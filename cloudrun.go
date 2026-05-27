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

// cloudRunTrafficUpdater は Cloud Run service の traffic split を更新する境界。
// テストでは fake を差し込む。
//
// 戻り値 operationName は GCP が返す long-running operation の resource name
// (`projects/.../operations/...`)。caller は必要に応じて poll するが、本 proxy
// では待たない (= fire-and-return)。
type cloudRunTrafficUpdater interface {
	FlipTraffic(ctx context.Context, fullServiceName, toRevisionTag string) (operationName string, err error)
}

// liveCloudRun は Cloud Run Admin v2 REST API (PATCH services?updateMask=traffic)
// を ADC (oauth2/google.DefaultTokenSource) で叩く実装。SDK を避けて REST
// 直接にしているのは依存を最小化しテスト時に endpoint を httptest.Server で
// 差し替えやすくするため。
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

// trafficTarget は v2 API の TrafficTarget JSON 構造。Tag で revision を
// 指す方式 (= release-wave の stage 時に `--tag pending-...` で打った tag を
// flip で参照する)。
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

// FlipTraffic は service の traffic を to_revision_tag が指す revision に 100%
// 振る。`fullServiceName` は `projects/{p}/locations/{r}/services/{s}` 形式。
//
// GCP の v2 API ドキュメント:
//
//	PATCH https://run.googleapis.com/v2/{name}?updateMask=traffic
//	body: { traffic: [{ type, tag, percent }] }
//
// 200 で long-running operation を返す。
func (c *liveCloudRun) FlipTraffic(ctx context.Context, fullServiceName, toRevisionTag string) (string, error) {
	if fullServiceName == "" {
		return "", errors.New("fullServiceName is empty")
	}
	if toRevisionTag == "" {
		return "", errors.New("toRevisionTag is empty")
	}

	body := patchTrafficBody{
		Traffic: []trafficTarget{
			{
				Type:    "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION",
				Tag:     toRevisionTag,
				Percent: 100,
			},
		},
	}
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
		// upstream エラーは proxy が解釈せず status code とともに返す。
		// 値漏れ防止のため body は 1KB に切る。
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
