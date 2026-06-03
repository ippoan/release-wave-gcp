// Command release-wave-gcp は Cloud Run の traffic 操作を Cloudflare Worker
// (ci-dashboard) に代理する Cloud Run service。
//
// 設計の親 issue: ippoan/ci-dashboard#137
// 2 段モデル (CF Worker → Cloud Run proxy → GCP API) の理由は親 issue 参照。
// 参考: ippoan/secrets-inventory + ippoan/secrets-inventory-gcp の 2 段構成。
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultCloudRunEndpoint = "https://run.googleapis.com"

// 共通 request: project / region / service の 3 path segment + endpoint 固有 field。
type flipTrafficRequest struct {
	Project string `json:"project"`
	Region  string `json:"region"`
	Service string `json:"service"`
}

type rollbackRequest struct {
	Project    string `json:"project"`
	Region     string `json:"region"`
	Service    string `json:"service"`
	ToRevision string `json:"to_revision"`
}

type stageCheckRequest struct {
	Project string `json:"project"`
	Region  string `json:"region"`
	Service string `json:"service"`
}

type trafficResponse struct {
	Ok        bool   `json:"ok"`
	Operation string `json:"operation,omitempty"`
}

type stageCheckResponse struct {
	Ok     bool           `json:"ok"`
	Status *serviceStatus `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func main() {
	apiKey := mustEnv("RELEASE_WAVE_API_KEY")
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	endpoint := os.Getenv("CLOUD_RUN_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultCloudRunEndpoint
	}

	ctx := context.Background()
	cr, err := newLiveCloudRun(ctx, endpoint)
	if err != nil {
		log.Fatalf("init cloud run client: %v", err)
	}

	mux := newMuxWith(cr, apiKey)
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("release-wave-gcp listening on :%s (endpoint=%s)", port, endpoint)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}

// newMuxWith は handler 構築を main bootstrap から分離してテスト可能にする。
func newMuxWith(client cloudRunClient, apiKey string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.Handle("/cloudrun/flip-traffic", requireAPIKey(apiKey, handleFlipTraffic(client)))
	mux.Handle("/cloudrun/rollback", requireAPIKey(apiKey, handleRollback(client)))
	mux.Handle("/cloudrun/stage-check", requireAPIKey(apiKey, handleStageCheck(client)))
	return mux
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"service":"release-wave-gcp"}`))
}

func requireAPIKey(expected string, next http.Handler) http.Handler {
	expectedBytes := []byte(expected)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Release-Wave-API-Key")
		if subtle.ConstantTimeCompare([]byte(got), expectedBytes) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validatePathSegments は project / region / service の 3 field を検証する共通ヘルパ。
// `/` `?` `#` を含むと URL inject になり得るので reject。
func validatePathSegments(project, region, service string) error {
	if strings.TrimSpace(project) == "" {
		return errors.New("project is required")
	}
	if strings.TrimSpace(region) == "" {
		return errors.New("region is required")
	}
	if strings.TrimSpace(service) == "" {
		return errors.New("service is required")
	}
	for name, v := range map[string]string{
		"project": project,
		"region":  region,
		"service": service,
	} {
		if strings.ContainsAny(v, "/?#") {
			return fmt.Errorf("%s must not contain '/', '?', or '#'", name)
		}
	}
	return nil
}

func fullServiceName(project, region, service string) string {
	return fmt.Sprintf("projects/%s/locations/%s/services/%s", project, region, service)
}

// handleFlipTraffic は POST /cloudrun/flip-traffic の handler。
// body: { project, region, service }
// → Cloud Run service の traffic を「最新の ready revision」(= 直前に no-traffic
// deploy で上がった revision) に 100% flip する。揮発する revision tag には依存
// しない (Refs ippoan/ci-dashboard#248)。後方互換のため body に残っている
// to_revision_tag は無視される。
func handleFlipTraffic(client cloudRunClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req flipTrafficRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := validatePathSegments(req.Project, req.Region, req.Service); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		opName, err := client.FlipTraffic(r.Context(),
			fullServiceName(req.Project, req.Region, req.Service))
		if err != nil {
			log.Printf("flip-traffic upstream error: %v", err)
			writeJSONError(w, http.StatusBadGateway, "cloud run upstream error")
			return
		}
		writeJSON(w, http.StatusOK, trafficResponse{Ok: true, Operation: opName})
	})
}

// handleRollback は POST /cloudrun/rollback の handler。
// body: { project, region, service, to_revision }
// → traffic を to_revision (full or short revision name) に 100% 戻す。
// caller (ci-dashboard DO) は flip 前の latest revision を記録しておき、ここに渡す。
func handleRollback(client cloudRunClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req rollbackRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := validatePathSegments(req.Project, req.Region, req.Service); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(req.ToRevision) == "" {
			writeJSONError(w, http.StatusBadRequest, "to_revision is required")
			return
		}

		opName, err := client.Rollback(r.Context(),
			fullServiceName(req.Project, req.Region, req.Service),
			req.ToRevision)
		if err != nil {
			log.Printf("rollback upstream error: %v", err)
			writeJSONError(w, http.StatusBadGateway, "cloud run upstream error")
			return
		}
		writeJSON(w, http.StatusOK, trafficResponse{Ok: true, Operation: opName})
	})
}

// handleStageCheck は POST /cloudrun/stage-check の handler。
// body: { project, region, service }
// → service の現状 (latest ready revision / terminal condition / traffic) を返す。
// release-wave stage 完了 barrier の poll 用。
func handleStageCheck(client cloudRunClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req stageCheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := validatePathSegments(req.Project, req.Region, req.Service); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		status, err := client.GetService(r.Context(),
			fullServiceName(req.Project, req.Region, req.Service))
		if err != nil {
			log.Printf("stage-check upstream error: %v", err)
			writeJSONError(w, http.StatusBadGateway, "cloud run upstream error")
			return
		}
		writeJSON(w, http.StatusOK, stageCheckResponse{Ok: true, Status: status})
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("env %s is required", key)
	}
	return v
}
