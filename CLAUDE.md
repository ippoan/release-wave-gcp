# CLAUDE.md

Claude Code 向けの本リポジトリ作業ルール。

設計の親 issue: [ippoan/ci-dashboard#137](https://github.com/ippoan/ci-dashboard/issues/137)
参照モデル: `ippoan/secrets-inventory-gcp` (同じ「CF Worker → Cloud Run → GCP API」2 段構成)

## Worktree / branch 命名規則

形式: `<issue-number>-<type>-<short-description>`

- `issue-number`: 必須。先に issue を立ててから worktree / branch を作る
- `type`: `feat` | `fix` | `refactor` | `infra`
- `short-description`: 半角小文字英数字とハイフン

例:

- `1-feat-cloudrun-flip-traffic`
- `2-feat-cloudrun-rollback`

Claude Code が自動採番する `claude/...` で実装に入る場合は、対応する issue を作成し上記の形式で rename / 再切り出しする。

## PR description / commit message のキーワード

- 使用禁止: `Closes #N` / `Fixes #N` / `Resolves #N`
  - PR auto-merge が走った瞬間に issue が自動 close されるため、release 時の close 確認 UI と整合しない
- 使用推奨: `Refs #N` / `Related to #N` / `Part of #N`

PR テンプレートは `.github/pull_request_template.md` で `Refs` を強制する。

## このリポジトリの方針

- 本 service は親 service (`ippoan/ci-dashboard`) から呼ばれる **Cloud Run proxy**。Cloud Run の **traffic 操作のみ**を行う薄い proxy
- **値を持たない / 値を返さない**。route も traffic state ぐらいで、secret や config の保持は親 (ci-dashboard) に任せる
- **GCP の JSON key を一切発行しない**:
  - runtime: Cloud Run の attached SA + ADC (metadata server)
  - deploy: WIF (Workload Identity Federation + GitHub OIDC trust)
- 親 service からの呼び出しは `X-Release-Wave-API-Key` header (32 byte shared secret、constant-time 比較) で認証
- runtime SA に付与する IAM role は以下:
  - `roles/run.admin` または、より絞った custom role (= `run.services.update` の minimal set) で **Cloud Run service の traffic 更新のみ**を許可
  - **(将来) `roles/run.viewer`** — `/cloudrun/stage-check` 追加時に
- write 系のうち以下のみ許可:
  - **traffic update (`PATCH services?updateMask=traffic`)** — `--no-traffic` で stage 済みの **最新 ready revision** に 100% traffic を flip する用途。flip は揮発する revision tag に依存せず `latestReadyRevision` を anchor にする (Refs ippoan/ci-dashboard#248)
  - delete / create / image push / SA 変更 / Cloud Run service 自体の作成削除は **やらない**
- upstream エラーは proxy が解釈せず status code とともに 502 でラップする。値漏れ防止のため response body は固定文言、詳細は log にだけ出す

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
