package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// cloudRunClient は Cloud Run Admin v2 への境界。テストでは fake を差し込む。
//
// 戻り値 operationName は GCP が返す long-running operation の resource name
// (`projects/.../operations/...`)。caller は必要に応じて poll するが、本 proxy
// では待たない (= fire-and-return)。
type cloudRunClient interface {
	// FlipTraffic は service の traffic を toRevisionTag が指す revision に 100% 振る。
	// release-wave-handler の stage (`--no-traffic --tag pending-...`) と対称。
	FlipTraffic(ctx context.Context, fullServiceName, toRevisionTag string) (operationName string, err error)
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
// 対応を読むのに使う)。flip は GET で tag→revision を解決してから Revision で送る。
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
		return "", fmt.Errorf("cloud run patch returned %d: %s", resp.StatusCode, string(snippet))
	}

	var lro lroResponse
	if err := json.Unmarshal(raw, &lro); err != nil {
		return "", fmt.Errorf("parse lro response: %w", err)
	}
	return lro.Name, nil
}

// FlipTraffic は traffic を tag で参照される revision に 100% 振る。
//
// Cloud Run v2 の TrafficTarget は type=TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION の
// とき `revision` (フル revision 名) が必須。`tag` フィールドは「その target に
// tag を *付与* する」用途で、既存 tag による revision *選択* には使えない
// (tag だけ指定して type=REVISION にすると Cloud Run が
// "traffic[0].revision: must be specified if and only if traffic type is
// TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION" で 400 を返す)。
// release-wave-handler は `--no-traffic --tag pending-...` で no-traffic revision
// に tag を付けているので、現 service の traffic[] を GET して tag → revision 名を
// 解決してから revision 指定で 100% flip する。
func (c *liveCloudRun) FlipTraffic(ctx context.Context, fullServiceName, toRevisionTag string) (string, error) {
	if fullServiceName == "" {
		return "", errors.New("fullServiceName is empty")
	}
	if toRevisionTag == "" {
		return "", errors.New("toRevisionTag is empty")
	}
	svc, err := c.GetService(ctx, fullServiceName)
	if err != nil {
		return "", fmt.Errorf("resolve revision tag %q: %w", toRevisionTag, err)
	}
	revision := ""
	for _, t := range svc.Traffic {
		if t.Tag == toRevisionTag && t.Revision != "" {
			revision = t.Revision
			break
		}
	}
	if revision == "" {
		return "", fmt.Errorf(
			"revision tag %q not found in service traffic (deploy が --tag %s で no-traffic revision を作っているか確認)",
			toRevisionTag, toRevisionTag,
		)
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
		return nil, fmt.Errorf("cloud run get returned %d: %s", resp.StatusCode, string(snippet))
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
