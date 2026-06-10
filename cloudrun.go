package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcCodeFromHTTP は Cloud Run Admin REST の non-2xx status を gRPC code に
// 逆変換する。handler 側に既配線の cloudrunproxy.StatusFromGRPC が outbound
// status に戻すための carry (Refs #11)。round-trip が一意な code だけ対応し、
// それ以外は Unknown (= StatusFromGRPC で従来互換の 502 に倒れる)。
func grpcCodeFromHTTP(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusBadRequest:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusConflict:
		return codes.AlreadyExists
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	case http.StatusGatewayTimeout:
		return codes.DeadlineExceeded
	default:
		return codes.Unknown
	}
}

// cloudRunClient は Cloud Run Admin v2 への境界。テストでは fake を差し込む。
//
// 戻り値 operationName は GCP が返す long-running operation の resource name
// (`projects/.../operations/...`)。caller は必要に応じて poll するが、本 proxy
// では待たない (= fire-and-return)。
type cloudRunClient interface {
	// FlipTraffic は service の traffic を「最新の ready revision」(= no-traffic
	// deploy で上がった、まだ traffic を受けていない revision) に 100% 振る。
	// rollback が revision 名指定で traffic を変えるのと対称で、揮発しやすい
	// Cloud Run revision tag (pending-...) には依存しない (Refs ippoan/ci-dashboard#248)。
	FlipTraffic(ctx context.Context, fullServiceName string) (operationName string, err error)
	// Rollback は traffic を toRevision (full revision name) に 100% 振る。
	// flip 後に問題が見つかった時、旧 revision に明示的に戻す用途。
	Rollback(ctx context.Context, fullServiceName, toRevision string) (operationName string, err error)
	// GetService は service の現状 (latest ready revision, terminal condition,
	// traffic 配分) を返す。release-wave stage barrier の poll に使う。
	GetService(ctx context.Context, fullServiceName string) (*serviceStatus, error)
}

// liveCloudRun は Cloud Run Admin v2 REST API を ADC (oauth2/google.DefaultTokenSource)
// で叩く実装。SDK を避けて REST 直接にしているのは依存を最小化しテスト時に
// endpoint を httptest.Server で差し替えやすくするため。
type liveCloudRun struct {
	httpClient *http.Client
	tokenSrc   oauth2.TokenSource
	endpoint   string // 通常 "https://run.googleapis.com"、test では httptest.Server.URL
}

func newLiveCloudRun(ctx context.Context, endpoint string) (*liveCloudRun, error) {
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("default token source: %w", err)
	}
	return &liveCloudRun{
		httpClient: &http.Client{},
		tokenSrc:   ts,
		endpoint:   endpoint,
	}, nil
}

// trafficTarget は v2 API の TrafficTarget JSON 構造。
// PATCH 送信時 type=REVISION なら Revision (full revision name) が必須。
// Tag は「その target に tag を付与する」用途 (GET 応答では tag→revision の
// 対応を読むのに使う)。flip / rollback は Revision (revision 名) を直接指定する。
type trafficTarget struct {
	Type     string `json:"type"`
	Tag      string `json:"tag,omitempty"`
	Revision string `json:"revision,omitempty"`
	Percent  int32  `json:"percent"`
}

type patchTrafficBody struct {
	Traffic []trafficTarget `json:"traffic"`
}

type lroResponse struct {
	Name string `json:"name"`
}

// condition は v2 API の TerminalCondition / GoogleCloudRunV2Condition 形式。
type condition struct {
	Type    string `json:"type"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// serviceStatus は GET service v2 response の必要部分。
//
// `Ready` は本 proxy 側で計算した派生 boolean。terminalCondition.type == "Ready"
// かつ state == "CONDITION_SUCCEEDED" の時のみ true。
type serviceStatus struct {
	Name                  string          `json:"name"`
	LatestReadyRevision   string          `json:"latest_ready_revision,omitempty"`
	LatestCreatedRevision string          `json:"latest_created_revision,omitempty"`
	Traffic               []trafficTarget `json:"traffic,omitempty"`
	TerminalCondition     *condition      `json:"terminal_condition,omitempty"`
	Ready                 bool            `json:"ready"`
}

// updateTraffic は PATCH /v2/{name}?updateMask=traffic の共通実装。
// FlipTraffic / Rollback から traffic targets を構築して呼び出す。
func (c *liveCloudRun) updateTraffic(ctx context.Context, fullServiceName string, targets []trafficTarget) (string, error) {
	body := patchTrafficBody{Traffic: targets}
	// json.Marshal は本 struct (string + int32 のみ) では実行時 fail し得ないので
	// err は無視する (= unreachable branch を test で外して 100% gate に乗せる)。
	buf, _ := json.Marshal(body)

	url := fmt.Sprintf("%s/v2/%s?updateMask=traffic", c.endpoint, fullServiceName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	tok, err := c.tokenSrc.Token()
	if err != nil {
		return "", fmt.Errorf("get token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		snippet := raw
		if len(snippet) > 1024 {
			snippet = snippet[:1024]
		}
		// status.Errorf で upstream HTTP status を gRPC code として carry する。
		// メッセージ (snippet 含む) は handler が log にだけ出す — response body
		// は固定文言のまま (値漏れ防止規約)。
		return "", status.Errorf(grpcCodeFromHTTP(resp.StatusCode),
			"cloud run patch returned %d: %s", resp.StatusCode, string(snippet))
	}

	var lro lroResponse
	if err := json.Unmarshal(raw, &lro); err != nil {
		return "", fmt.Errorf("parse lro response: %w", err)
	}
	return lro.Name, nil
}

// FlipTraffic は traffic を「最新の ready revision」に 100% 振る。
//
// no-traffic deploy (= `--no-traffic` で 0% 配置 + 現 live を pin) で上がった
// revision は、その service の最新 ready revision になる。flip はそれを GET で
// 取り、revision 名指定で 100% に切り替えるだけ — rollback が revision 名で
// traffic を戻すのと完全に対称。
//
// 以前は deploy が付けた Cloud Run revision tag (pending-<tag>) を GET で
// 解決していたが、この tag は次の deploy が traffic を全置換するたびに消える
// (揮発する) ため flip が "tag not found" で落ちていた。最新 ready revision を
// 直接 anchor にすることで揮発タグ依存を撤廃する (Refs ippoan/ci-dashboard#248)。
func (c *liveCloudRun) FlipTraffic(ctx context.Context, fullServiceName string) (string, error) {
	if fullServiceName == "" {
		return "", errors.New("fullServiceName is empty")
	}
	svc, err := c.GetService(ctx, fullServiceName)
	if err != nil {
		return "", fmt.Errorf("get service for flip: %w", err)
	}
	revision := svc.LatestReadyRevision
	if revision == "" {
		return "", errors.New("service has no latest ready revision to flip to")
	}
	// Cloud Run v2 の GET service は latestReadyRevision を full resource name
	// (projects/.../services/<svc>/revisions/<rev>) で返すが、traffic[].revision
	// は **短い revision 名** (<svc>-NNNNN-xxx) のみを受け付ける。full path を
	// そのまま渡すと "Revision target ... invalid. Expected a revision prefixed
	// with '<svc>-'" で 400 になるため、最後のパスセグメントを取り出す
	// (Refs ippoan/ci-dashboard#248)。
	if idx := strings.LastIndex(revision, "/"); idx >= 0 {
		revision = revision[idx+1:]
	}
	return c.updateTraffic(ctx, fullServiceName, []trafficTarget{
		{
			Type:     "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION",
			Revision: revision,
			Percent:  100,
		},
	})
}

// Rollback は traffic を revision name (full or short) に 100% 振る。
// flip 後の rollback では「以前 100% を受けていた revision」を caller (ci-dashboard
// DO) が記録しておき、本 method に渡す。
func (c *liveCloudRun) Rollback(ctx context.Context, fullServiceName, toRevision string) (string, error) {
	if fullServiceName == "" {
		return "", errors.New("fullServiceName is empty")
	}
	if toRevision == "" {
		return "", errors.New("toRevision is empty")
	}
	return c.updateTraffic(ctx, fullServiceName, []trafficTarget{
		{
			Type:     "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION",
			Revision: toRevision,
			Percent:  100,
		},
	})
}

// GetService は GET /v2/{name} で service の現状を取り、ready 判定を付けて返す。
// stage barrier の poll 用 (= release-wave-handler が stage 完了 callback を投げる
// 前に「最新 revision が ready か」を確認する用途、または ci-dashboard DO が
// alarm() で進捗を見る用途)。
func (c *liveCloudRun) GetService(ctx context.Context, fullServiceName string) (*serviceStatus, error) {
	if fullServiceName == "" {
		return nil, errors.New("fullServiceName is empty")
	}

	url := fmt.Sprintf("%s/v2/%s", c.endpoint, fullServiceName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	tok, err := c.tokenSrc.Token()
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		snippet := raw
		if len(snippet) > 1024 {
			snippet = snippet[:1024]
		}
		return nil, status.Errorf(grpcCodeFromHTTP(resp.StatusCode),
			"cloud run get returned %d: %s", resp.StatusCode, string(snippet))
	}

	// v2 API は JSON で camelCase。本 proxy の output は snake_case に揃える
	// (= flip-traffic レスポンスと一貫させる) ため、まず camelCase で受けて
	// serviceStatus に詰め直す。
	var raw_resp struct {
		Name                  string          `json:"name"`
		LatestReadyRevision   string          `json:"latestReadyRevision"`
		LatestCreatedRevision string          `json:"latestCreatedRevision"`
		Traffic               []trafficTarget `json:"traffic"`
		TerminalCondition     *condition      `json:"terminalCondition"`
	}
	if err := json.Unmarshal(raw, &raw_resp); err != nil {
		return nil, fmt.Errorf("parse service response: %w", err)
	}

	ready := raw_resp.TerminalCondition != nil &&
		raw_resp.TerminalCondition.Type == "Ready" &&
		raw_resp.TerminalCondition.State == "CONDITION_SUCCEEDED"

	return &serviceStatus{
		Name:                  raw_resp.Name,
		LatestReadyRevision:   raw_resp.LatestReadyRevision,
		LatestCreatedRevision: raw_resp.LatestCreatedRevision,
		Traffic:               raw_resp.Traffic,
		TerminalCondition:     raw_resp.TerminalCondition,
		Ready:                 ready,
	}, nil
}
