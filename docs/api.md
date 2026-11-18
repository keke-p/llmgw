# llmgw API リファレンス

## 公開API

### 共通

- 認証は `Authorization: Bearer <key>` または `x-api-key: <key>` で行います。認証エラーは、受けた系統のエラー形式で返します。
- エラーの形式は系統ごとに次のとおりです。
  - OpenAI：`{"error":{"message","type","code"}}`
  - Ollama：`{"error":"..."}`
  - Anthropic：`{"type":"error","error":{"type","message"}}`
- キュー待ちのタイムアウトは 503 と `Retry-After` を返します（Anthropic 形式では `overloaded_error`）。それ以外のタイムアウトは 504 で、メッセージにどの段階のタイムアウトかが入ります（例 `timeout: ttft exceeded 300s`）。
- 応答開始後のエラーは、形式ごとに次の表現で送って接続を閉じます。
  - OpenAI：`data: {"error":...}`。`[DONE]` は送りません。
  - Anthropic：`event: error`
  - Ollama：`{"error":"..."}` の行

### 変換の規則（要点）

| 項目 | 規則 |
|---|---|
| パススルー | 受けた形式を上流がそのまま扱える場合（OpenAI→OpenAI互換、Ollama→Ollama、Anthropic→Anthropic互換プロバイダ）は、モデル名の書き換え・ストリーム強制・パラメータ上書きだけを行って転送します。未知のフィールドもそのまま送ります |
| ツール呼び出し | ストリーム中に断片で届く引数は組み立てます。`tool_call_id` と `tool_use_id` は同じ値を使います。Ollama の呼び出しには ID がないので、ゲートウェイが振ります。Ollama の `tool_name` は、直前の呼び出しと名前で対応付けます |
| 画像 | OpenAI の data URL、Ollama の base64、Anthropic の `source.base64` を相互に変換します。Ollama には http(s) の画像URLを渡せないため 400 にします（ゲートウェイは画像を取得しに行きません） |
| 思考 | Ollama の `thinking`、OpenAI互換の `reasoning_content`／`reasoning`、Anthropic の `thinking` ブロックを相互に変換します。Anthropic 形式で `thinking` を指定しない要求は、思考オフとして扱います |
| `stop_reason` | `end_turn`／`max_tokens`／`tool_use`／`stop_sequence` と、`stop`／`length`／`tool_calls` を対応付けます |
| `/api/generate` → OpenAI互換 | system と user のチャットに変換します。`template`・`context`・`raw`・`suffix` は 400 にします |
| `/v1/completions` → Ollama | `/api/generate`（`prompt`/`suffix`）に変換し、既定で `think:false` を送ります |
| `count_tokens` | 上流が数えられればその値、そうでなければ文字数からの概算を返します |
| 無視するフィールド | `cache_control`、`metadata`、サーバーツール（`web_search` など）、`service_tier` など |

## 管理API（`/admin/api`）

ログインすると、HttpOnly の Cookie（`llmgw_session`、SameSite=Strict、Path=/admin）と CSRF トークンが返ります。GET 以外のリクエストには `X-CSRF-Token` ヘッダが必要です。別サイトからのリクエスト（Origin や Sec-Fetch-Site で判定）は拒否します。

```sh
B=http://localhost:4000
CSRF=$(curl -s -c cj.txt $B/admin/api/login -H 'Content-Type: application/json' -d '{"password":"..."}' | jq -r .csrf_token)
H=(-b cj.txt -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json')
curl -s "${H[@]}" $B/admin/api/keys -d '{"name":"open-webui","allowed_aliases":["qwen3"]}'
```

| メソッドとパス | 内容 |
|---|---|
| `POST /login`、`POST /logout`、`GET /session` | セッション。ログインの失敗は同じ IP から15分に5回まで |
| `POST /password` | パスワード変更（12文字以上）。他のセッションは無効になる |
| `GET/POST /keys`、`GET/PATCH/DELETE /keys/{id}` | キー。平文は作成時の応答でだけ返す。編集できる項目：`name`、`allowed_aliases`、`expires_at`、`log_mode`、`max_concurrency`、`rpm_limit`、`nonstream_keepalive`、`status` |
| `POST /keys/{id}/rotate` `{grace_minutes}` | 再発行（猶予0〜10080分） |
| `POST /keys/{id}/disable`、`/enable` | 無効化と有効化 |
| `GET/POST /providers`、`GET/PATCH/DELETE /providers/{id}` | プロバイダ。`api_key` は書き込み専用で、`null` を送ると削除。エイリアスが使っているプロバイダの削除は 409 |
| `POST /providers/{id}/test` | 疎通確認。`{"apply_anthropic": true}` を送ると、検出した Anthropic 互換かどうかを設定に反映する |
| `GET/POST /aliases`、`GET/PATCH/DELETE /aliases/{id}` | エイリアス。`targets` は配列の順が優先順。`effective` に実効タイムアウトと継承元が入る |
| `GET/PATCH /settings` | 設定。時間は ms で指定する |
| `GET /logs`、`GET /logs/{id}`、`GET /logs/export` | ログ。絞り込みは `from`、`to`、`key_id`、`alias`、`status`（`200`/`4xx`/`5xx`/`error`）、`q`、`limit`、`before_id` |
| `GET /audit-logs?limit=&before_id=` | 監査ログ |
| `POST /playground/{APIパス}?key_id=N` | 指定したキーとして API を実行する。`key_id` を省くと管理者として全エイリアスを使える |
| `GET /backup` | DB のバックアップをダウンロードする |

`LLMGW_ADMIN_TRUST_HEADER` を設定すると、信頼するプロキシからそのヘッダ付きで来たリクエストを認証済みとして扱います。この場合 CSRF トークンは使わず、別サイトからのリクエストの拒否と、本文を JSON に限ることで防ぎます。
