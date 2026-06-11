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
