package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

type fakeCloudRun struct {
	calledWith struct {
		fullServiceName string
		toRevisionTag   string
	}
	returnOp  string
	returnErr error
	calls     int
}

func (f *fakeCloudRun) FlipTraffic(_ context.Context, fullServiceName, toRevisionTag string) (string, error) {
	f.calls++
	f.calledWith.fullServiceName = fullServiceName
	f.calledWith.toRevisionTag = toRevisionTag
	if f.returnErr != nil {
		return "", f.returnErr
	}
	return f.returnOp, nil
}

const testAPIKey = "test-key-32-bytes-of-shared-secret"

func newTestServer(updater cloudRunTrafficUpdater) *httptest.Server {
	return httptest.NewServer(newMuxWith(updater, testAPIKey))
}

func do(t *testing.T, srv *httptest.Server, method, path string, headers map[string]string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return string(b)
}

func TestHealth(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	resp := do(t, srv, http.MethodGet, "/health", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"service":"release-wave-gcp"`) {
		t.Fatalf("body: %s", body)
	}
}

func TestFlipTrafficRequiresAPIKey(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", nil, `{}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestFlipTrafficWrongAPIKey(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	headers := map[string]string{"X-Release-Wave-API-Key": "wrong"}
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", headers, `{}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestFlipTrafficRejectsGET(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	headers := map[string]string{"X-Release-Wave-API-Key": testAPIKey}
	resp := do(t, srv, http.MethodGet, "/cloudrun/flip-traffic", headers, "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestFlipTrafficInvalidJSON(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	headers := map[string]string{"X-Release-Wave-API-Key": testAPIKey}
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", headers, `not json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestFlipTrafficMissingFields(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	headers := map[string]string{"X-Release-Wave-API-Key": testAPIKey}

	cases := []struct {
		name string
		body string
	}{
		{"missing project", `{"region":"r","service":"s","to_revision_tag":"t"}`},
		{"missing region", `{"project":"p","service":"s","to_revision_tag":"t"}`},
		{"missing service", `{"project":"p","region":"r","to_revision_tag":"t"}`},
		{"missing to_revision_tag", `{"project":"p","region":"r","service":"s"}`},
		{"empty project", `{"project":"  ","region":"r","service":"s","to_revision_tag":"t"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", headers, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status: got %d, body=%s", resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

func TestFlipTrafficRejectsPathInjection(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	headers := map[string]string{"X-Release-Wave-API-Key": testAPIKey}
	// service field に '/' を入れて URL inject を試みる
	body := `{"project":"p","region":"r","service":"s/../evil","to_revision_tag":"t"}`
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", headers, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestFlipTrafficHappyPath(t *testing.T) {
	fake := &fakeCloudRun{returnOp: "projects/p/locations/r/operations/op-123"}
	srv := newTestServer(fake)
	defer srv.Close()
	headers := map[string]string{"X-Release-Wave-API-Key": testAPIKey}
	body := `{"project":"cloudsql-sv","region":"asia-northeast1","service":"rust-alc-api","to_revision_tag":"pending-v1-42-0"}`
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", headers, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	var got flipTrafficResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Ok || got.Operation != "projects/p/locations/r/operations/op-123" {
		t.Fatalf("response: %+v", got)
	}

	wantFull := "projects/cloudsql-sv/locations/asia-northeast1/services/rust-alc-api"
	if fake.calledWith.fullServiceName != wantFull {
		t.Fatalf("fullServiceName: got %s want %s", fake.calledWith.fullServiceName, wantFull)
	}
	if fake.calledWith.toRevisionTag != "pending-v1-42-0" {
		t.Fatalf("toRevisionTag: got %s", fake.calledWith.toRevisionTag)
	}
}

func TestFlipTrafficUpstreamError(t *testing.T) {
	fake := &fakeCloudRun{returnErr: errors.New("upstream boom")}
	srv := newTestServer(fake)
	defer srv.Close()
	headers := map[string]string{"X-Release-Wave-API-Key": testAPIKey}
	body := `{"project":"p","region":"r","service":"s","to_revision_tag":"t"}`
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", headers, body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	body2 := readBody(t, resp)
	// upstream の生エラー文言は response に漏らさず固定文言を返す
	if strings.Contains(body2, "upstream boom") {
		t.Fatalf("upstream error leaked into response: %s", body2)
	}
}

// ----- liveCloudRun (REST 経路) のテスト -----

type staticTokenSource struct{ tok string }

func (s *staticTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: s.tok}, nil
}

func newTestLiveCloudRun(endpoint string) *liveCloudRun {
	return &liveCloudRun{
		httpClient: &http.Client{},
		tokenSrc:   &staticTokenSource{tok: "test-token"},
		endpoint:   endpoint,
	}
}

func TestLiveCloudRunFlipTraffic_Success(t *testing.T) {
	var captured struct {
		method string
		path   string
		query  string
		auth   string
		ctype  string
		body   patchTrafficBody
	}
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.query = r.URL.RawQuery
		captured.auth = r.Header.Get("Authorization")
		captured.ctype = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured.body)
		_, _ = w.Write([]byte(`{"name":"projects/p/locations/r/operations/lro-1"}`))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	op, err := cr.FlipTraffic(context.Background(),
		"projects/p/locations/r/services/s", "pending-v1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if op != "projects/p/locations/r/operations/lro-1" {
		t.Fatalf("op: %s", op)
	}
	if captured.method != http.MethodPatch {
		t.Fatalf("method: %s", captured.method)
	}
	if captured.path != "/v2/projects/p/locations/r/services/s" {
		t.Fatalf("path: %s", captured.path)
	}
	if captured.query != "updateMask=traffic" {
		t.Fatalf("query: %s", captured.query)
	}
	if captured.auth != "Bearer test-token" {
		t.Fatalf("auth: %s", captured.auth)
	}
	if captured.ctype != "application/json" {
		t.Fatalf("content-type: %s", captured.ctype)
	}
	if len(captured.body.Traffic) != 1 {
		t.Fatalf("traffic len: %d", len(captured.body.Traffic))
	}
	tt := captured.body.Traffic[0]
	if tt.Tag != "pending-v1" || tt.Percent != 100 ||
		tt.Type != "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION" {
		t.Fatalf("traffic target: %+v", tt)
	}
}

func TestLiveCloudRunFlipTraffic_Non2xx(t *testing.T) {
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"PermissionDenied"}}`))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	_, err := cr.FlipTraffic(context.Background(),
		"projects/p/locations/r/services/s", "tag")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("err should mention 403: %v", err)
	}
}

func TestLiveCloudRunFlipTraffic_EmptyInputs(t *testing.T) {
	cr := newTestLiveCloudRun("http://unused")
	if _, err := cr.FlipTraffic(context.Background(), "", "tag"); err == nil {
		t.Fatalf("expected error for empty service")
	}
	if _, err := cr.FlipTraffic(context.Background(), "projects/p/locations/r/services/s", ""); err == nil {
		t.Fatalf("expected error for empty tag")
	}
}

func TestLiveCloudRunFlipTraffic_HTTPError(t *testing.T) {
	// endpoint を即座に閉じることで TCP レベルの error を発生させる
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	cr := newTestLiveCloudRun(url)
	_, err := cr.FlipTraffic(context.Background(),
		"projects/p/locations/r/services/s", "tag")
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestLiveCloudRunFlipTraffic_BadJSONResponse(t *testing.T) {
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<not json>`))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	_, err := cr.FlipTraffic(context.Background(),
		"projects/p/locations/r/services/s", "tag")
	if err == nil {
		t.Fatalf("expected parse error")
	}
}

// ----- 一貫性チェック -----

func TestPatchTrafficBodyShape(t *testing.T) {
	body := patchTrafficBody{
		Traffic: []trafficTarget{
			{Type: "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", Tag: "x", Percent: 100},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(buf)
	want := `{"traffic":[{"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION","tag":"x","percent":100}]}`
	if got != want {
		t.Fatalf("body shape:\n got=%s\nwant=%s", got, want)
	}
}

