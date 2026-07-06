---
name: release-wave-gcp-map
generated-from: release-wave-gcp:5bb89945662a6f997c8c159c92c08f60dd7b2e83
paths: [main.go, cloudrun.go]
description: ippoan/release-wave-gcp (Go / Cloud Run、ci-dashboard から Cloud Run の traffic 操作を代行する薄い proxy) の構造ナビゲーション。flip-traffic / rollback / stage-check の 3 endpoint と認証境界・GCP key 0 個運用の guardrail を 1 枚にまとめる。flip は latestReadyRevision を anchor (揮発 revision tag 非依存, Refs ci-dashboard#248)。トリガー:「release-wave-gcp」「flip-traffic」「rollback」「stage-check」「Cloud Run traffic」「release wave」「latestReadyRevision」「revision tag flip」「X-Release-Wave-API-Key」等。
---

# release-wave-gcp-map — ippoan/release-wave-gcp 構造ナビゲーション

Go の Cloud Run service。`ci-dashboard` (Cloudflare Worker) から呼ばれ、Cloud Run v2 REST API の
**traffic 操作だけ**を ADC (metadata server) 経由で代行する **値を持たない薄い proxy**。

> ここは索引 (pointer)。細部は repo 側が正。frontmatter の `generated-from` が現在の
> repo tree-sha とズレたら session-start hook が再生成を促す → その時 tree-sha を更新する。

## 区画 (フラットな単一 package、`main`)

| ファイル | 主要 symbol | 役割 |
|---|---|---|
| `main.go` | `main` / `newMuxWith` / `requireAPIKey` / `handleHealth` / `handleFlipTraffic` / `handleRollback` / `handleStageCheck` | HTTP layer。route 登録 + API key 認証 middleware + 各 handler |
| `cloudrun.go` | `liveCloudRun` / `newLiveCloudRun` / `updateTraffic` / `FlipTraffic` / `Rollback` / `GetService` | Cloud Run Admin API (v2 REST) client。`cloudRunTrafficUpdater` interface 実装 |
| `main_test.go` | `TestFlipTraffic_*` / `TestLiveCloudRun*` 他 | HTTP layer は fake updater、live 側は `httptest.Server` で GCP mock |
| `Dockerfile` / `.dockerignore` | — | Cloud Run image |
| `coverage_100.toml` | — | カバレッジ 100% gate 設定 |

## entrypoint / route (main.go の `newMuxWith`)

| method | path | 認証 | handler → client メソッド |
|---|---|---|---|
| GET | `/health` | 不要 | `handleHealth`（`/healthz` は GFE reserved のため避ける） |
| POST | `/cloudrun/flip-traffic` | `X-Release-Wave-API-Key` | `handleFlipTraffic` → `FlipTraffic` (service の latestReadyRevision に 100% flip。body は `{project,region,service}` のみ) |
| POST | `/cloudrun/rollback` | 同上 | `handleRollback` → `Rollback` (revision 名で 100% 戻す) |
| POST | `/cloudrun/stage-check` | 同上 | `handleStageCheck` → `GetService` (latest ready / terminal condition / traffic) |

- 内部は **GET で `latestReadyRevision` を解決** → `PATCH run.googleapis.com/v2/.../services?updateMask=traffic` に **revision 名指定**で 100% (flip/rollback) / `GET .../services` (stage-check)。`to_revision_tag` は受けても**無視**される (`main.go`)。
- LRO は **待たない**（caller の ci-dashboard DO `alarm()` が poll）。

## gotcha (CLAUDE.md / README 由来)

- **GCP の JSON key を一切発行しない**: runtime = Cloud Run attached SA + ADC、deploy = WIF + GitHub OIDC。
- **値を持たない / 返さない**。upstream エラーは proxy が解釈せず **502 + 固定文言**でラップし、詳細は log のみ（値漏れ防止、test で regression 固定）。
- **`project` / `region` / `service` に `/ ? #` を含むと 400 reject**（URL injection 防止）。
- **flip は service の `latestReadyRevision` を anchor** に 100% 切替。揮発する revision tag (`pending-...`) には依存しない (Refs ci-dashboard#248: 旧 tag ベースは tag→flip の間に main-push が入ると pending tag が外れ flip 不能になる事故があり廃止)。stage 側は no-traffic deploy で新 revision を上げるだけで flip 用 tag 付けは不要。
- runtime SA の write 権限は **traffic update のみ**（`roles/run.admin` or `run.services.update` 最小 custom role）。delete / create / image push / SA 変更はしない。
- `Closes/Fixes/Resolves #N` 禁止 → `Refs #N`（release 時の目視 close 用、auto-close させない）。

## CCoW / CI から見た立ち位置

- staging (`release-wave-gcp-staging`) を **実運用環境**として扱う。production (`release-wave-gcp`、`v*` tag) は当面未使用。
- deploy は caller workflow で **Docker build + GHCR push** → `ci-workflows/cloud-run-deploy.yml` reusable で AR remote-repo 経由 digest-pinned deploy。
- 実呼び出し元は `ci-workflows/release-wave-handler.yml`（cloudrun path で本 service の 3 endpoint を叩く）。設計の親 issue は `ci-dashboard#137`。

## 関連

- `secrets-inventory-gcp-map` — 同じ「CF Worker → Cloud Run → GCP API」2 段モデルの姉妹 repo（参照モデル）
- `repo-map` / `cross-repo-symbol-index` — この per-repo map の運用方針 (generated-from 鮮度 hook)

## CLAUDE.md から移設 (2026-07-06)

### branch 命名例

例:

- `1-feat-cloudrun-flip-traffic`
- `2-feat-cloudrun-rollback`

### PR テンプレート

PR テンプレートは `.github/pull_request_template.md` で `Refs` を強制する。

### runtime SA IAM role 詳細

- runtime SA に付与する IAM role は以下:
  - `roles/run.admin` または、より絞った custom role (= `run.services.update` の minimal set) で **Cloud Run service の traffic 更新のみ**を許可
  - **(将来) `roles/run.viewer`** — `/cloudrun/stage-check` 追加時に

## 環境

親 issue (`ippoan/ci-dashboard#137`) で staging = 実運用環境という方針を採るので、本 repo もそれに揃える。

| env | Cloud Run service | trigger |
|---|---|---|
| staging (live) | `release-wave-gcp-staging` | `main` push / PR (non-draft) |
| production | `release-wave-gcp` | `v*` tag push |

PR を上げると staging に auto-deploy される。production は当面未使用。

## デプロイ

ippoan の Cloud Run deploy 標準パターンに揃える: caller workflow で **Docker build + GHCR push** → `ippoan/ci-workflows/.github/workflows/cloud-run-deploy.yml` reusable で **AR remote-repo (pull-through cache) 経由で digest-pinned deploy**。

> **GCP key 0 個運用** は runtime / deploy 両側で達成:
>
> - **Runtime** (Cloud Run → Cloud Run Admin API): attached SA + ADC (metadata server)
> - **Deploy** (GitHub Actions → GCP): WIF + GitHub OIDC trust

### GCP 側 (one-time、`cloudsql-sv` project)

`secrets-inventory-gcp` の setup を再利用しつつ、Deployer SA / WIF binding と Runtime SA を新規作成する。

```bash
PROJECT=cloudsql-sv
POOL=gh-actions-pool             # 既存
PROVIDER=github                   # 既存
REGION=asia-northeast1

# 1) Runtime SA (Cloud Run attached)
gcloud iam service-accounts create release-wave-runtime \
  --project="$PROJECT" \
  --display-name="Release Wave (Cloud Run runtime)"

# 2) Runtime SA に Cloud Run service の traffic 更新権限を grant
#    range: 本 proxy が触る Cloud Run service の存在する project に絞る。
#    現状は cloudsql-sv (rust-alc-api 等) のみ。
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:release-wave-runtime@$PROJECT.iam.gserviceaccount.com" \
  --role="roles/run.admin"

# 3) Deployer SA (GitHub Actions → Cloud Run deploy)
#    既存の staging-deploy@cloudsql-sv を流用する。
#    本 repo (ippoan/release-wave-gcp) からの impersonate を許可する binding を追加:
PROJECT_NUMBER=$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')
gcloud iam service-accounts add-iam-policy-binding \
  staging-deploy@$PROJECT.iam.gserviceaccount.com \
  --project="$PROJECT" \
  --role="roles/iam.workloadIdentityUser" \
  --member="principalSet://iam.googleapis.com/projects/$PROJECT_NUMBER/locations/global/workloadIdentityPools/$POOL/attribute.repository/ippoan/release-wave-gcp"

# 4) Shared API key を Secret Manager に投入
openssl rand -hex 32 | gcloud secrets create RELEASE_WAVE_GCP_API_KEY_STAGING \
  --project="$PROJECT" --replication-policy=automatic --data-file=-

# 5) runtime SA に上記 secret 限定の accessor binding
gcloud secrets add-iam-policy-binding RELEASE_WAVE_GCP_API_KEY_STAGING \
  --project="$PROJECT" \
  --member="serviceAccount:release-wave-runtime@$PROJECT.iam.gserviceaccount.com" \
  --role="roles/secretmanager.secretAccessor"

# 6) 同 key を Cloudflare Secrets Store 側にも同値投入 (ci-dashboard が叩く側)
```

### Cloud Run service の初回作成 (user 手動、1 回だけ)

`cloud-run-deploy.yml` reusable は `gcloud run services update` を叩く設計 = service 既存前提:

```bash
gcloud run deploy release-wave-gcp-staging \
  --project=cloudsql-sv \
  --region=asia-northeast1 \
  --image=asia-northeast1-docker.pkg.dev/cloudsql-sv/ghcr/ippoan/release-wave-gcp:latest \
  --service-account=release-wave-runtime@cloudsql-sv.iam.gserviceaccount.com \
  --allow-unauthenticated \
  --ingress=all \
  --update-secrets=RELEASE_WAVE_API_KEY=RELEASE_WAVE_GCP_API_KEY_STAGING:latest
```

これ以降は CI workflow が image を update する。

### GitHub repo 側 (Settings → Secrets and variables → Actions)

**Variables (plain text, repo-level):**

- `GCP_REGION` = `asia-northeast1`
- `GCP_PROJECT_ID_STAGING` = `cloudsql-sv`
- `GCP_WIF_PROVIDER` = `projects/<PROJECT_NUMBER>/locations/global/workloadIdentityPools/gh-actions-pool/providers/github`
- `GCP_WIF_SERVICE_ACCOUNT_STAGING` = `staging-deploy@cloudsql-sv.iam.gserviceaccount.com`

org-level vars に既に登録されていればそれを流用する。

vars が空のままなら CI workflow の `deploy-staging` job (= **後続 PR で追加予定**) は `if:` で skip され、PR の必須 check は `ci` / `build-image` だけで通る。

## ローカル開発

```bash
go vet ./...
go test ./... -race
go build .

# ローカル run (実 GCP を叩く場合は ADC が必要)
gcloud auth application-default login
RELEASE_WAVE_API_KEY=dev-key ./release-wave-gcp

# 別 shell から health check
curl http://localhost:8080/health

# flip-traffic を叩く (ADC + Cloud Run Admin API への接続が要る、本番 service に注意)
# flip は service の最新 ready revision (= no-traffic deploy で上がった revision) に
# 100% 振る。revision tag の指定は不要 (Refs ippoan/ci-dashboard#248)。
curl -X POST http://localhost:8080/cloudrun/flip-traffic \
  -H 'X-Release-Wave-API-Key: dev-key' \
  -H 'Content-Type: application/json' \
  -d '{"project":"cloudsql-sv","region":"asia-northeast1","service":"rust-alc-api"}'
```

## テスト方針

- HTTP layer は `newMuxWith(updater, apiKey)` で構築し、`cloudRunTrafficUpdater` interface を fake に差し替えてテスト
- `liveCloudRun` 側は endpoint を struct field にしているので `httptest.Server` で GCP API を mock 可能。token source も `oauth2.TokenSource` interface で固定 token に差し替え
- カバレッジ目標は 100% (除外: `main()` bootstrap / `mustEnv` log.Fatal / `newLiveCloudRun` 内の `google.DefaultTokenSource` 呼び出し)
- 値漏れ regression は test で固定: upstream error 文言が response body に echo されない、log のみに出ること
