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

type flipTrafficRequest struct {
	Project       string `json:"project"`
	Region        string `json:"region"`
	Service       string `json:"service"`
	ToRevisionTag string `json:"to_revision_tag"`
}

type flipTrafficResponse struct {
	Ok        bool   `json:"ok"`
	Operation string `json:"operation,omitempty"`
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
func newMuxWith(updater cloudRunTrafficUpdater, apiKey string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.Handle("/cloudrun/flip-traffic", requireAPIKey(apiKey, handleFlipTraffic(updater)))
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

// handleFlipTraffic は POST /cloudrun/flip-traffic の handler。
// body: { project, region, service, to_revision_tag }
// → Cloud Run service の traffic を to_revision_tag が指す revision に 100% flip。
func handleFlipTraffic(updater cloudRunTrafficUpdater) http.Handler {
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
		if err := validateFlipTrafficRequest(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		fullName := fmt.Sprintf("projects/%s/locations/%s/services/%s",
			req.Project, req.Region, req.Service)

		opName, err := updater.FlipTraffic(r.Context(), fullName, req.ToRevisionTag)
		if err != nil {
			// upstream エラーは 502 にラップ。値漏れ防止のため詳細は log にだけ
			// 出して response には固定文言を返す。
			log.Printf("flip-traffic upstream error: %v", err)
			writeJSONError(w, http.StatusBadGateway, "cloud run upstream error")
			return
		}

		writeJSON(w, http.StatusOK, flipTrafficResponse{Ok: true, Operation: opName})
	})
}

// validateFlipTrafficRequest は 4 field の存在 + 軽い shape 検証。
// project / region / service は GCP resource name の path segment に入るため、
// `/` を含むと URL inject になり得るので reject する。
func validateFlipTrafficRequest(req *flipTrafficRequest) error {
	if strings.TrimSpace(req.Project) == "" {
		return errors.New("project is required")
	}
	if strings.TrimSpace(req.Region) == "" {
		return errors.New("region is required")
	}
	if strings.TrimSpace(req.Service) == "" {
		return errors.New("service is required")
	}
	if strings.TrimSpace(req.ToRevisionTag) == "" {
		return errors.New("to_revision_tag is required")
	}
	for name, v := range map[string]string{
		"project": req.Project,
		"region":  req.Region,
		"service": req.Service,
	} {
		if strings.ContainsAny(v, "/?#") {
			return fmt.Errorf("%s must not contain '/', '?', or '#'", name)
		}
	}
	return nil
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
