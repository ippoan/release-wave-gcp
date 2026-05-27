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
	// FlipTraffic
	flipCalledWith struct {
		fullServiceName string
		toRevisionTag   string
	}
	flipReturnOp  string
	flipReturnErr error
	flipCalls     int

	// Rollback
	rollbackCalledWith struct {
		fullServiceName string
		toRevision      string
	}
	rollbackReturnOp  string
	rollbackReturnErr error
	rollbackCalls     int

	// GetService
	getServiceCalledWith string
	getServiceReturn     *serviceStatus
	getServiceErr        error
	getServiceCalls      int
}

func (f *fakeCloudRun) FlipTraffic(_ context.Context, fullServiceName, toRevisionTag string) (string, error) {
	f.flipCalls++
	f.flipCalledWith.fullServiceName = fullServiceName
	f.flipCalledWith.toRevisionTag = toRevisionTag
	if f.flipReturnErr != nil {
		return "", f.flipReturnErr
	}
	return f.flipReturnOp, nil
}

func (f *fakeCloudRun) Rollback(_ context.Context, fullServiceName, toRevision string) (string, error) {
	f.rollbackCalls++
	f.rollbackCalledWith.fullServiceName = fullServiceName
	f.rollbackCalledWith.toRevision = toRevision
	if f.rollbackReturnErr != nil {
		return "", f.rollbackReturnErr
	}
	return f.rollbackReturnOp, nil
}

func (f *fakeCloudRun) GetService(_ context.Context, fullServiceName string) (*serviceStatus, error) {
	f.getServiceCalls++
	f.getServiceCalledWith = fullServiceName
	if f.getServiceErr != nil {
		return nil, f.getServiceErr
	}
	return f.getServiceReturn, nil
}

const testAPIKey = "test-key-32-bytes-of-shared-secret"

func newTestServer(client cloudRunClient) *httptest.Server {
	return httptest.NewServer(newMuxWith(client, testAPIKey))
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

func authHeader() map[string]string {
	return map[string]string{"X-Release-Wave-API-Key": testAPIKey}
}

// ===== /health =====

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

// ===== auth (1 経路で代表) =====

func TestRequireAPIKey_Missing(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	for _, path := range []string{"/cloudrun/flip-traffic", "/cloudrun/rollback", "/cloudrun/stage-check"} {
		resp := do(t, srv, http.MethodPost, path, nil, `{}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status got %d", path, resp.StatusCode)
		}
	}
}

func TestRequireAPIKey_Wrong(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	headers := map[string]string{"X-Release-Wave-API-Key": "wrong"}
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", headers, `{}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

// ===== /cloudrun/flip-traffic =====

func TestFlipTraffic_RejectsGET(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	resp := do(t, srv, http.MethodGet, "/cloudrun/flip-traffic", authHeader(), "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestFlipTraffic_InvalidJSON(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", authHeader(), `not json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestFlipTraffic_MissingFields(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
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
			resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", authHeader(), tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status: got %d, body=%s", resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

func TestFlipTraffic_PathInjection(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	body := `{"project":"p","region":"r","service":"s/../evil","to_revision_tag":"t"}`
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", authHeader(), body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestFlipTraffic_HappyPath(t *testing.T) {
	fake := &fakeCloudRun{flipReturnOp: "projects/p/locations/r/operations/op-123"}
	srv := newTestServer(fake)
	defer srv.Close()
	body := `{"project":"cloudsql-sv","region":"asia-northeast1","service":"rust-alc-api","to_revision_tag":"pending-v1-42-0"}`
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", authHeader(), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	var got trafficResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Ok || got.Operation != "projects/p/locations/r/operations/op-123" {
		t.Fatalf("response: %+v", got)
	}

	wantFull := "projects/cloudsql-sv/locations/asia-northeast1/services/rust-alc-api"
	if fake.flipCalledWith.fullServiceName != wantFull {
		t.Fatalf("fullServiceName: got %s want %s", fake.flipCalledWith.fullServiceName, wantFull)
	}
	if fake.flipCalledWith.toRevisionTag != "pending-v1-42-0" {
		t.Fatalf("toRevisionTag: got %s", fake.flipCalledWith.toRevisionTag)
	}
}

func TestFlipTraffic_UpstreamError(t *testing.T) {
	fake := &fakeCloudRun{flipReturnErr: errors.New("upstream boom")}
	srv := newTestServer(fake)
	defer srv.Close()
	body := `{"project":"p","region":"r","service":"s","to_revision_tag":"t"}`
	resp := do(t, srv, http.MethodPost, "/cloudrun/flip-traffic", authHeader(), body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if strings.Contains(readBody(t, resp), "upstream boom") {
		t.Fatalf("upstream error leaked into response")
	}
}

// ===== /cloudrun/rollback =====

func TestRollback_RejectsGET(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	resp := do(t, srv, http.MethodGet, "/cloudrun/rollback", authHeader(), "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestRollback_InvalidJSON(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	resp := do(t, srv, http.MethodPost, "/cloudrun/rollback", authHeader(), `nope`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestRollback_MissingFields(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	cases := []struct {
		name string
		body string
	}{
		{"missing project", `{"region":"r","service":"s","to_revision":"rev-1"}`},
		{"missing region", `{"project":"p","service":"s","to_revision":"rev-1"}`},
		{"missing service", `{"project":"p","region":"r","to_revision":"rev-1"}`},
		{"missing to_revision", `{"project":"p","region":"r","service":"s"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, srv, http.MethodPost, "/cloudrun/rollback", authHeader(), tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status: got %d, body=%s", resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

func TestRollback_HappyPath(t *testing.T) {
	fake := &fakeCloudRun{rollbackReturnOp: "projects/p/locations/r/operations/roll-1"}
	srv := newTestServer(fake)
	defer srv.Close()
	body := `{"project":"cloudsql-sv","region":"asia-northeast1","service":"rust-alc-api","to_revision":"rust-alc-api-00042-abc"}`
	resp := do(t, srv, http.MethodPost, "/cloudrun/rollback", authHeader(), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	var got trafficResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Ok || got.Operation != "projects/p/locations/r/operations/roll-1" {
		t.Fatalf("response: %+v", got)
	}

	wantFull := "projects/cloudsql-sv/locations/asia-northeast1/services/rust-alc-api"
	if fake.rollbackCalledWith.fullServiceName != wantFull {
		t.Fatalf("fullServiceName: got %s", fake.rollbackCalledWith.fullServiceName)
	}
	if fake.rollbackCalledWith.toRevision != "rust-alc-api-00042-abc" {
		t.Fatalf("toRevision: got %s", fake.rollbackCalledWith.toRevision)
	}
}

func TestRollback_UpstreamError(t *testing.T) {
	fake := &fakeCloudRun{rollbackReturnErr: errors.New("upstream boom")}
	srv := newTestServer(fake)
	defer srv.Close()
	body := `{"project":"p","region":"r","service":"s","to_revision":"rev"}`
	resp := do(t, srv, http.MethodPost, "/cloudrun/rollback", authHeader(), body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if strings.Contains(readBody(t, resp), "upstream boom") {
		t.Fatalf("upstream error leaked")
	}
}

// ===== /cloudrun/stage-check =====

func TestStageCheck_RejectsGET(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	resp := do(t, srv, http.MethodGet, "/cloudrun/stage-check", authHeader(), "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestStageCheck_InvalidJSON(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	resp := do(t, srv, http.MethodPost, "/cloudrun/stage-check", authHeader(), `nope`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
}

func TestStageCheck_MissingFields(t *testing.T) {
	srv := newTestServer(&fakeCloudRun{})
	defer srv.Close()
	cases := []struct {
		name string
		body string
	}{
		{"missing project", `{"region":"r","service":"s"}`},
		{"missing region", `{"project":"p","service":"s"}`},
		{"missing service", `{"project":"p","region":"r"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, srv, http.MethodPost, "/cloudrun/stage-check", authHeader(), tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status: got %d", resp.StatusCode)
			}
		})
	}
}

func TestStageCheck_HappyPath(t *testing.T) {
	fake := &fakeCloudRun{
		getServiceReturn: &serviceStatus{
			Name:                "projects/p/locations/r/services/s",
			LatestReadyRevision: "s-00042-abc",
			Ready:               true,
			TerminalCondition:   &condition{Type: "Ready", State: "CONDITION_SUCCEEDED"},
		},
	}
	srv := newTestServer(fake)
	defer srv.Close()
	body := `{"project":"p","region":"r","service":"s"}`
	resp := do(t, srv, http.MethodPost, "/cloudrun/stage-check", authHeader(), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	var got stageCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Ok || got.Status == nil || !got.Status.Ready {
		t.Fatalf("response: %+v", got)
	}
	if got.Status.LatestReadyRevision != "s-00042-abc" {
		t.Fatalf("latest_ready_revision: got %s", got.Status.LatestReadyRevision)
	}
	if fake.getServiceCalledWith != "projects/p/locations/r/services/s" {
		t.Fatalf("fullServiceName: got %s", fake.getServiceCalledWith)
	}
}

func TestStageCheck_UpstreamError(t *testing.T) {
	fake := &fakeCloudRun{getServiceErr: errors.New("upstream boom")}
	srv := newTestServer(fake)
	defer srv.Close()
	body := `{"project":"p","region":"r","service":"s"}`
	resp := do(t, srv, http.MethodPost, "/cloudrun/stage-check", authHeader(), body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if strings.Contains(readBody(t, resp), "upstream boom") {
		t.Fatalf("upstream error leaked")
	}
}

// ===== liveCloudRun (REST 経路) =====

type staticTokenSource struct{ tok string }

func (s *staticTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: s.tok}, nil
}

type errTokenSource struct{}

func (errTokenSource) Token() (*oauth2.Token, error) {
	return nil, errors.New("token source boom")
}

func newTestLiveCloudRun(endpoint string) *liveCloudRun {
	return &liveCloudRun{
		httpClient: &http.Client{},
		tokenSrc:   &staticTokenSource{tok: "test-token"},
		endpoint:   endpoint,
	}
}

// --- updateTraffic / FlipTraffic ---

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
	if tt.Revision != "" {
		t.Fatalf("Revision should be empty when flipping by tag: %s", tt.Revision)
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
		t.Fatalf("err: %v", err)
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

func TestLiveCloudRunFlipTraffic_TokenError(t *testing.T) {
	gcp := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer gcp.Close()
	cr := &liveCloudRun{
		httpClient: &http.Client{},
		tokenSrc:   errTokenSource{},
		endpoint:   gcp.URL,
	}
	_, err := cr.FlipTraffic(context.Background(),
		"projects/p/locations/r/services/s", "tag")
	if err == nil || !strings.Contains(err.Error(), "get token") {
		t.Fatalf("err: %v", err)
	}
}

func TestLiveCloudRunFlipTraffic_LongErrorBodyTrimmed(t *testing.T) {
	bigBody := strings.Repeat("X", 2048)
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(bigBody))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	_, err := cr.FlipTraffic(context.Background(),
		"projects/p/locations/r/services/s", "tag")
	if err == nil {
		t.Fatalf("expected error")
	}
	msg := err.Error()
	xCount := 0
	for i := len(msg) - 1; i >= 0 && msg[i] == 'X'; i-- {
		xCount++
	}
	if xCount > 1024 || xCount < 100 {
		t.Fatalf("body should be trimmed to ~1024 X but got %d", xCount)
	}
}

func TestLiveCloudRunFlipTraffic_InvalidURL(t *testing.T) {
	cr := &liveCloudRun{
		httpClient: &http.Client{},
		tokenSrc:   &staticTokenSource{tok: "t"},
		endpoint:   "http://control\x7fchar",
	}
	_, err := cr.FlipTraffic(context.Background(),
		"projects/p/locations/r/services/s", "tag")
	if err == nil || !strings.Contains(err.Error(), "build request") {
		t.Fatalf("err: %v", err)
	}
}

func TestLiveCloudRunFlipTraffic_HTTPError(t *testing.T) {
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

// --- Rollback ---

func TestLiveCloudRunRollback_Success(t *testing.T) {
	var capturedBody patchTrafficBody
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &capturedBody)
		_, _ = w.Write([]byte(`{"name":"operations/roll-1"}`))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	op, err := cr.Rollback(context.Background(),
		"projects/p/locations/r/services/s", "s-00041-zzz")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if op != "operations/roll-1" {
		t.Fatalf("op: %s", op)
	}
	if len(capturedBody.Traffic) != 1 {
		t.Fatalf("traffic len")
	}
	tt := capturedBody.Traffic[0]
	if tt.Revision != "s-00041-zzz" || tt.Percent != 100 ||
		tt.Type != "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION" {
		t.Fatalf("traffic target: %+v", tt)
	}
	if tt.Tag != "" {
		t.Fatalf("Tag should be empty when rolling back by revision: %s", tt.Tag)
	}
}

func TestLiveCloudRunRollback_EmptyInputs(t *testing.T) {
	cr := newTestLiveCloudRun("http://unused")
	if _, err := cr.Rollback(context.Background(), "", "rev"); err == nil {
		t.Fatalf("expected error for empty service")
	}
	if _, err := cr.Rollback(context.Background(), "projects/p/locations/r/services/s", ""); err == nil {
		t.Fatalf("expected error for empty revision")
	}
}

// --- GetService ---

func TestLiveCloudRunGetService_Success(t *testing.T) {
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v2/projects/p/locations/r/services/s" {
			t.Errorf("path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"name": "projects/p/locations/r/services/s",
			"latestReadyRevision": "s-00042-abc",
			"latestCreatedRevision": "s-00042-abc",
			"traffic": [{"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION","revision":"s-00042-abc","percent":100}],
			"terminalCondition": {"type":"Ready","state":"CONDITION_SUCCEEDED","message":"Ready"}
		}`))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	got, err := cr.GetService(context.Background(), "projects/p/locations/r/services/s")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !got.Ready {
		t.Fatalf("ready should be true")
	}
	if got.LatestReadyRevision != "s-00042-abc" {
		t.Fatalf("latest_ready_revision: %s", got.LatestReadyRevision)
	}
	if got.TerminalCondition == nil || got.TerminalCondition.State != "CONDITION_SUCCEEDED" {
		t.Fatalf("terminal condition: %+v", got.TerminalCondition)
	}
	if len(got.Traffic) != 1 || got.Traffic[0].Revision != "s-00042-abc" {
		t.Fatalf("traffic: %+v", got.Traffic)
	}
}

func TestLiveCloudRunGetService_NotReady(t *testing.T) {
	// terminalCondition は "Ready" だが state が SUCCEEDED 以外なら Ready=false
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"name": "projects/p/locations/r/services/s",
			"terminalCondition": {"type":"Ready","state":"CONDITION_PENDING","message":"still rolling out"}
		}`))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	got, err := cr.GetService(context.Background(), "projects/p/locations/r/services/s")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Ready {
		t.Fatalf("ready should be false when state=CONDITION_PENDING")
	}
}

func TestLiveCloudRunGetService_NoTerminalCondition(t *testing.T) {
	// terminalCondition 自体が無い場合は Ready=false
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"projects/p/locations/r/services/s"}`))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	got, err := cr.GetService(context.Background(), "projects/p/locations/r/services/s")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Ready {
		t.Fatalf("ready should be false")
	}
	if got.TerminalCondition != nil {
		t.Fatalf("terminal condition should be nil")
	}
}

func TestLiveCloudRunGetService_NonReadyOtherType(t *testing.T) {
	// terminalCondition.type が "Ready" 以外 (例: "RoutesReady") なら Ready=false
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"terminalCondition": {"type":"RoutesReady","state":"CONDITION_SUCCEEDED"}
		}`))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	got, err := cr.GetService(context.Background(), "projects/p/locations/r/services/s")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Ready {
		t.Fatalf("ready should be false when type!=Ready")
	}
}

func TestLiveCloudRunGetService_Non2xx(t *testing.T) {
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	_, err := cr.GetService(context.Background(), "projects/p/locations/r/services/s")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err: %v", err)
	}
}

func TestLiveCloudRunGetService_LongErrorBodyTrimmed(t *testing.T) {
	bigBody := strings.Repeat("Y", 2048)
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(bigBody))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	_, err := cr.GetService(context.Background(), "projects/p/locations/r/services/s")
	if err == nil {
		t.Fatalf("expected error")
	}
	msg := err.Error()
	yCount := 0
	for i := len(msg) - 1; i >= 0 && msg[i] == 'Y'; i-- {
		yCount++
	}
	if yCount > 1024 || yCount < 100 {
		t.Fatalf("body should be trimmed to ~1024 Y but got %d", yCount)
	}
}

func TestLiveCloudRunGetService_EmptyName(t *testing.T) {
	cr := newTestLiveCloudRun("http://unused")
	if _, err := cr.GetService(context.Background(), ""); err == nil {
		t.Fatalf("expected error")
	}
}

func TestLiveCloudRunGetService_TokenError(t *testing.T) {
	cr := &liveCloudRun{
		httpClient: &http.Client{},
		tokenSrc:   errTokenSource{},
		endpoint:   "http://unused",
	}
	_, err := cr.GetService(context.Background(), "projects/p/locations/r/services/s")
	if err == nil || !strings.Contains(err.Error(), "get token") {
		t.Fatalf("err: %v", err)
	}
}

func TestLiveCloudRunGetService_InvalidURL(t *testing.T) {
	cr := &liveCloudRun{
		httpClient: &http.Client{},
		tokenSrc:   &staticTokenSource{tok: "t"},
		endpoint:   "http://control\x7fchar",
	}
	_, err := cr.GetService(context.Background(), "projects/p/locations/r/services/s")
	if err == nil || !strings.Contains(err.Error(), "build request") {
		t.Fatalf("err: %v", err)
	}
}

func TestLiveCloudRunGetService_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	cr := newTestLiveCloudRun(url)
	_, err := cr.GetService(context.Background(), "projects/p/locations/r/services/s")
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestLiveCloudRunGetService_BadJSONResponse(t *testing.T) {
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<not json>`))
	}))
	defer gcp.Close()

	cr := newTestLiveCloudRun(gcp.URL)
	_, err := cr.GetService(context.Background(), "projects/p/locations/r/services/s")
	if err == nil {
		t.Fatalf("expected parse error")
	}
}

// ===== 一貫性 =====

func TestPatchTrafficBodyShape_ByTag(t *testing.T) {
	body := patchTrafficBody{
		Traffic: []trafficTarget{
			{Type: "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", Tag: "x", Percent: 100},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"traffic":[{"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION","tag":"x","percent":100}]}`
	if string(buf) != want {
		t.Fatalf("got=%s want=%s", string(buf), want)
	}
}

func TestPatchTrafficBodyShape_ByRevision(t *testing.T) {
	body := patchTrafficBody{
		Traffic: []trafficTarget{
			{Type: "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", Revision: "r-1", Percent: 100},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"traffic":[{"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION","revision":"r-1","percent":100}]}`
	if string(buf) != want {
		t.Fatalf("got=%s want=%s", string(buf), want)
	}
}
