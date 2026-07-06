# CLAUDE.md

Claude Code 向けの本リポジトリ作業ルール。

設計の親 issue: ippoan/ci-dashboard#137
参照モデル: `ippoan/secrets-inventory-gcp` (同じ 2 段構成)

## Worktree / branch 命名規則

形式: `<issue-number>-<type>-<short-description>` (`type`: `feat`|`fix`|`refactor`|`infra`、
`short-description` は半角小文字英数字とハイフン)。`issue-number` は必須 — 先に issue を
立ててから branch を作る。`claude/...` で自動採番された場合は対応する issue を作成し
上記形式に rename / 再切り出しする。

## PR description / commit message のキーワード

- 使用禁止: `Closes #N` / `Fixes #N` / `Resolves #N` (release 時の close 確認と不整合)
- 使用推奨: `Refs #N` / `Related to #N` / `Part of #N`

## このリポジトリの方針

- 本 service は `ippoan/ci-dashboard` から呼ばれる Cloud Run **traffic 操作のみ**の薄い proxy。
  **値を持たない / 値を返さない**
- **GCP の JSON key を一切発行しない**: runtime は attached SA + ADC、deploy は WIF + GitHub OIDC
- 呼び出しは `X-Release-Wave-API-Key` header (32 byte shared secret、constant-time 比較) で認証
- runtime SA IAM role: `roles/run.admin` または traffic 更新のみの custom role
- write 系は **traffic update のみ許可**。flip は `latestReadyRevision` を anchor にする
  (Refs ippoan/ci-dashboard#248)。delete / create / image push / SA 変更 / service 自体の
  作成削除は **やらない**
- upstream エラーは解釈せず 502 でラップ。値漏れ防止のため response body は固定文言、
  詳細は log にだけ出す

## 主要コマンド

```bash
go vet ./...
go test ./... -race
go build .
```

環境構成・デプロイ手順・ローカル起動・テスト方針・branch 命名例・IAM role 将来計画は
`release-wave-gcp-map` skill を参照。
