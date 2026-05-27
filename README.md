# release-wave-gcp

[`ippoan/ci-dashboard`](https://github.com/ippoan/ci-dashboard) (Cloudflare Worker) から GCP Cloud Run の **traffic 操作** を代行する Cloud Run proxy (Go service)。

設計の親 issue: [ippoan/ci-dashboard#137](https://github.com/ippoan/ci-dashboard/issues/137) (Release Wave: ci-dashboard 主導の cross-repo coordinated release)。

## 役割

Release Wave 機構の **GCP 側 executor**。CF Worker からは GCP SA key を持てない (= `ippoan/secrets-inventory` 系の 2 段モデル参照) ため、本 service が Cloud Run 上で attached SA + ADC (metadata server) で credential を取り、Cloudflare Worker からは shared secret 経由で叩く。

```
[ci-dashboard (Cloudflare Worker)]
       │  HTTPS + X-Release-Wave-API-Key header
       ▼
[release-wave-gcp (Cloud Run service)]
       │  Application Default Credentials (metadata server)
       ▼
[Cloud Run Admin API (run.googleapis.com/v2)]
```

| 認証境界 | 方式 | 鍵 |
|---|---|---|
| operator → ci-dashboard | CF Access (Google OAuth) | 0 |
| ci-dashboard → release-wave-gcp | `X-Release-Wave-API-Key` header (32 byte shared secret、constant-time 比較) | Cloudflare Secrets Store + Google Secret Manager に同値を投入 |
| release-wave-gcp → GCP API | ADC (metadata server) | **0 (JSON key を発行しない)** |

## エンドポイント

| method | path | 認証 | 説明 |
|---|---|---|---|
| GET | `/health` | 不要 | Cloud Run liveness 用 |
| POST | `/cloudrun/flip-traffic` | `X-Release-Wave-API-Key` 必須 | service の traffic を `to_revision_tag` が指す revision に 100% flip |
| POST | `/cloudrun/rollback` | 同上 | service の traffic を `to_revision` (revision name) に 100% 戻す |
| POST | `/cloudrun/stage-check` | 同上 | service の現状 (latest ready revision / terminal condition / traffic) を返す |

### `POST /cloudrun/flip-traffic`

request:

```json
{
  "project": "cloudsql-sv",
  "region": "asia-northeast1",
  "service": "rust-alc-api",
  "to_revision_tag": "pending-v1-42-0"
}
```

response (200):

```json
{ "ok": true, "operation": "projects/.../operations/lro-..." }
```

- 内部的に `PATCH https://run.googleapis.com/v2/projects/{p}/locations/{r}/services/{s}?updateMask=traffic` を `traffic: [{ type: REVISION, tag, percent: 100 }]` で叩く
- 戻り値は GCP の long-running operation の resource name。caller は必要に応じて poll する (本 proxy は待たない)
- `project` / `region` / `service` に `/` `?` `#` を含むとリクエスト全体を 400 で reject (URL inject 防止)
- upstream エラーは 502 で固定文言を返す。詳細は service log にのみ出力 (値漏れ防止)

### `POST /cloudrun/rollback`

request:

```json
{
  "project": "cloudsql-sv",
  "region": "asia-northeast1",
  "service": "rust-alc-api",
  "to_revision": "rust-alc-api-00041-zzz"
}
```

response (200): flip-traffic と同形式。

- 内部は flip-traffic と同じ PATCH (`updateMask=traffic`) だが、`Tag` ではなく `Revision` (full or short revision name) で参照する
- caller (ci-dashboard DO) は flip 前の latest revision を記録しておき、ここに渡す
- DB が contract migration 後の状態だと旧 code が動かないので、rollback 安全性は caller 側 (issue #137 の `rollback.safe` flag) で別途判定する

### `POST /cloudrun/stage-check`

request:

```json
{
  "project": "cloudsql-sv",
  "region": "asia-northeast1",
  "service": "rust-alc-api"
}
```

response (200):

```json
{
  "ok": true,
  "status": {
    "name": "projects/.../services/rust-alc-api",
    "latest_ready_revision": "rust-alc-api-00042-abc",
    "latest_created_revision": "rust-alc-api-00042-abc",
    "traffic": [{ "type": "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", "revision": "...", "percent": 100 }],
    "terminal_condition": { "type": "Ready", "state": "CONDITION_SUCCEEDED" },
    "ready": true
  }
}
```

- 内部的に `GET https://run.googleapis.com/v2/projects/{p}/locations/{r}/services/{s}` を叩く
- `ready` は `terminalCondition.type == "Ready"` かつ `state == "CONDITION_SUCCEEDED"` のときだけ true (= 派生 boolean)
- ci-dashboard DO の `alarm()` が stage barrier の poll に使う用途

## 設計上の意図

| 項目 | 採用 | 不採用 / 理由 |
|---|---|---|
| Cloud Run v2 **REST** を直接叩く | ✓ | SDK (`cloud.google.com/go/run/apiv2`) を入れず最小依存。endpoint を struct field で差し替えれば `httptest.Server` で簡単に mock できる |
| traffic 指定は **tag** ベース | ✓ | release-wave-handler.yml が `--no-traffic --tag pending-...` で stage → 本 proxy が同 tag で flip、という対称になる。revision 名直接指定だと stage と flip の責務分割が壊れる |
| operation を **待たない** | ✓ | proxy 自体は CPU throttling が効く Cloud Run。LRO poll は ci-dashboard DO の `alarm()` で行う方が正しい layering |

## 別 repo にした理由

[ippoan/ci-dashboard#137](https://github.com/ippoan/ci-dashboard/issues/137) 「なぜ GCP 側を別 repo にするか」参照。要点:

- 認証層の分離 (CF Worker → Cloud Run → GCP の 2 段)
- lifecycle が違う (CF Workers 秒 / Cloud Run 分)
- IAM スコープ最小化 (GCP creds は本 service の SA だけ)
- secrets-inventory + secrets-inventory-gcp の 2 段モデル踏襲

## 環境構成

| env | Cloud Run service | trigger |
|---|---|---|
| staging (live) | `release-wave-gcp-staging` | `main` push / PR (non-draft) |
| production | `release-wave-gcp` | `v*` tag push |

親 issue と同じく **staging を実運用環境** とする。production は後段で導入。

## ローカル開発

```bash
go vet ./...
go test ./... -race
go build .

# ローカル run (実 GCP を叩く場合は ADC が必要)
gcloud auth application-default login
RELEASE_WAVE_API_KEY=dev-key ./release-wave-gcp
```

## 次フェーズ

- Cloud Run deploy job を CI に組み込み (= GCP setup 完了後、`ippoan/ci-workflows/cloud-run-deploy.yml` 経由)
- ci-dashboard DO `ReleaseWave` + MCP tools (issue #137 Phase 3) からの実呼び出し試験
