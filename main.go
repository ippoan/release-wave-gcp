// Command release-wave-gcp は Cloud Run の traffic 操作を Cloudflare Worker
// (ci-dashboard) に代理する Cloud Run service。
//
// 設計の親 issue: ippoan/ci-dashboard#137
// 2 段モデル (CF Worker → Cloud Run proxy → GCP API) の理由は親 issue 参照。
// 参考: ippoan/secrets-inventory + ippoan/secrets-inventory-gcp の 2 段構成。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	cloudrunproxy "github.com/ippoan/go-cloudrun-proxy"
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

func main() {
	apiKey := cloudrunproxy.MustEnv("RELEASE_WAVE_API_KEY")
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
// health / auth / JSON helper の skeleton は共有 lib (go-cloudrun-proxy) を使う
// (Refs ippoan/go-cloudrun-proxy#1)。
func newMuxWith(client cloudRunClient, apiKey string) *http.ServeMux {
	requireAPIKey := func(next http.Handler) http.Handler {
		return cloudrunproxy.RequireAPIKey("X-Release-Wave-API-Key", apiKey, next)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", cloudrunproxy.HandleHealth("release-wave-gcp"))
	mux.Handle("/cloudrun/flip-traffic", requireAPIKey(handleFlipTraffic(client)))
	mux.Handle("/cloudrun/rollback", requireAPIKey(handleRollback(client)))
	mux.Handle("/cloudrun/stage-check", requireAPIKey(handleStageCheck(client)))
	return mux
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
			cloudrunproxy.WriteJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req flipTrafficRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			cloudrunproxy.WriteJSONError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := validatePathSegments(req.Project, req.Region, req.Service); err != nil {
			cloudrunproxy.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		opName, err := client.FlipTraffic(r.Context(),
			fullServiceName(req.Project, req.Region, req.Service))
		if err != nil {
			log.Printf("flip-traffic upstream error: %v", err)
			// StatusFromGRPC: REST 由来の plain error は従来互換の 502。gRPC code を
			// 持つ error が混ざる構成になった時に 403/404 等へ自動で分解される。
			cloudrunproxy.WriteJSONError(w, cloudrunproxy.StatusFromGRPC(err), "cloud run upstream error")
			return
		}
		cloudrunproxy.WriteJSON(w, http.StatusOK, trafficResponse{Ok: true, Operation: opName})
	})
}

// handleRollback は POST /cloudrun/rollback の handler。
// body: { project, region, service, to_revision }
// → traffic を to_revision (full or short revision name) に 100% 戻す。
// caller (ci-dashboard DO) は flip 前の latest revision を記録しておき、ここに渡す。
func handleRollback(client cloudRunClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			cloudrunproxy.WriteJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req rollbackRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			cloudrunproxy.WriteJSONError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := validatePathSegments(req.Project, req.Region, req.Service); err != nil {
			cloudrunproxy.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(req.ToRevision) == "" {
			cloudrunproxy.WriteJSONError(w, http.StatusBadRequest, "to_revision is required")
			return
		}

		opName, err := client.Rollback(r.Context(),
			fullServiceName(req.Project, req.Region, req.Service),
			req.ToRevision)
		if err != nil {
			log.Printf("rollback upstream error: %v", err)
			// StatusFromGRPC: REST 由来の plain error は従来互換の 502。gRPC code を
			// 持つ error が混ざる構成になった時に 403/404 等へ自動で分解される。
			cloudrunproxy.WriteJSONError(w, cloudrunproxy.StatusFromGRPC(err), "cloud run upstream error")
			return
		}
		cloudrunproxy.WriteJSON(w, http.StatusOK, trafficResponse{Ok: true, Operation: opName})
	})
}

// handleStageCheck は POST /cloudrun/stage-check の handler。
// body: { project, region, service }
// → service の現状 (latest ready revision / terminal condition / traffic) を返す。
// release-wave stage 完了 barrier の poll 用。
func handleStageCheck(client cloudRunClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			cloudrunproxy.WriteJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req stageCheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			cloudrunproxy.WriteJSONError(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := validatePathSegments(req.Project, req.Region, req.Service); err != nil {
			cloudrunproxy.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		status, err := client.GetService(r.Context(),
			fullServiceName(req.Project, req.Region, req.Service))
		if err != nil {
			log.Printf("stage-check upstream error: %v", err)
			// StatusFromGRPC: REST 由来の plain error は従来互換の 502。gRPC code を
			// 持つ error が混ざる構成になった時に 403/404 等へ自動で分解される。
			cloudrunproxy.WriteJSONError(w, cloudrunproxy.StatusFromGRPC(err), "cloud run upstream error")
			return
		}
		cloudrunproxy.WriteJSON(w, http.StatusOK, stageCheckResponse{Ok: true, Status: status})
	})
}
