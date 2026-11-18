# llmgw

Ollama / LM Studio / llama.cpp / vLLM などのローカルLLMの前段に置く、軽量なLLMゲートウェイです。単一バイナリと SQLite ファイル1つで動きます。LiteLLM Proxy（Docker Compose + Postgres）の置き換えを目的にしています。

- **3系統のAPIを同じポートで提供**します：OpenAI互換（`/v1/chat/completions` ほか）、Ollama ネイティブ（`/api/chat` ほか）、Anthropic互換（`/v1/messages`）。どの形式でも、同じ Virtual Key と同じモデル名で同じモデルを呼べます。
- **モデルエイリアス**：クライアントが指定する公開名を「プロバイダ＋実モデル名」に対応付けます。モデルを差し替えるときはエイリアスの向き先を変えるだけです。フォールバック先も設定できます。
- **Virtual Key**：発行、再発行（猶予付き）、無効化、許可するモデル、期限、RPM 上限、同時実行数の上限を設定できます。
- **段階別タイムアウト**（キュー待ち／接続／初回トークン／無通信／全体）と keepalive で、モデルのロードや長い prefill の間も接続を保ちます。
- **プロンプトログ**（本文の保存・検索・JSONL エクスポート）、**管理画面**と **Playground**、ヘルスチェック、同時実行制御、パラメータ上書き、バックアップがあります。

課金、予算、チーム管理、キャッシュ、ガードレールなどは対象外です。

---

## 目次

1. [クイックスタート](#クイックスタート)
2. [基本の概念](#基本の概念)
3. [クライアントの接続設定](#クライアントの接続設定)
4. [タイムアウトの考え方と経路上の整合](#タイムアウトの考え方と経路上の整合)
5. [Traefik の設定例](#traefik-の設定例)
6. [運用](#運用)
7. [LiteLLM からの移行](#litellm-からの移行)
8. [環境変数](#環境変数)
9. [API の対応状況](#api-の対応状況)
10. [開発](#開発)
11. [ライセンス](#ライセンス)

---

## クイックスタート

### Docker Compose

```sh
cp .env.example .env          # LLMGW_SECRET_KEY（必須）と LLMGW_ADMIN_PASSWORD を設定
docker compose up -d --build
docker compose logs llmgw     # LLMGW_ADMIN_PASSWORD 未設定なら初期パスワードが一度だけ出る
```

[docker-compose.yml](docker-compose.yml) は Traefik 配下に置く想定のサンプルです。ホストで動いている Ollama には `http://host.docker.internal:11434` で接続できます（`extra_hosts` で設定済み）。

### バイナリ

```sh
go build -o llmgw ./cmd/llmgw
export LLMGW_SECRET_KEY='長いランダム文字列'   # プロバイダAPIキーの暗号化に使う。後から変えない
export LLMGW_COOKIE_SECURE=false               # http://localhost で管理画面を使う場合
./llmgw serve
```

### 初期設定（管理画面）

1. `http://localhost:4000/admin/` を開き、`admin` でログインします。
2. **プロバイダ**を追加します（例：種別 `ollama`、`http://localhost:11434`）。追加すると疎通確認が行われ、上流のモデル一覧が取得されます。
3. **エイリアス**を追加します（公開モデル名 → プロバイダ＋上流モデル名）。
4. **キー**を発行します。平文のキーは発行したときに一度だけ表示されます。
5. **Playground** で、ここまでの設定で実際に応答が返るか確認します。

```sh
curl http://localhost:4000/v1/chat/completions -H "Authorization: Bearer sk-gw-..." \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3","messages":[{"role":"user","content":"こんにちは"}]}'
```

---

## 基本の概念

| 概念 | 内容 |
|---|---|
| **プロバイダ** | 上流のLLMサーバーです。種別は `ollama`（ネイティブAPI `/api/chat` などを使う）と `openai_compatible`（LM Studio・llama.cpp・vLLM など。ベースURLは `http://host:1234/v1` の形で、パスを省くと `/v1` を補う）の2つです。APIキーは暗号化して保存します。上流が Anthropic の `/v1/messages` を持っている場合は「Anthropic互換」を有効にでき、追加時に自動で判定します。 |
| **エイリアス** | クライアントが指定するモデル名です。`name:tag` の形でも構いません。Ollama クライアントからは `:latest` を省略できます。ターゲット（プロバイダ＋上流モデル名）を優先順に複数登録でき、タイムアウトとパラメータ上書きもここで設定します。 |
| **Virtual Key** | `sk-gw-...` の形のキーです。DB には SHA-256 だけを保存します。許可するエイリアス（空なら全部）、期限、ログモード、RPM 上限、同時実行数の上限を持ちます。 |
| **変換とパススルー** | 受信した形式を上流がそのまま扱える場合は、モデル名の書き換えとパラメータ上書きだけを行って転送します（パススルー）。扱えない場合は内部の共通形式を経由して変換します。上流へは常にストリーミングで要求します。 |

---

## クライアントの接続設定

認証は `Authorization: Bearer <key>` または `x-api-key: <key>` で行います。

### Open WebUI

- **Ollama として接続**する場合：管理者設定の「接続」で、Ollama API の URL を `http://llmgw:4000` にし、API キー欄に Virtual Key を入れます。モデル一覧には、そのキーで使えるエイリアスだけが出ます。モデルの pull や削除はゲートウェイが拒否します（403）。
- **OpenAI として接続**する場合：URL を `http://llmgw:4000/v1`、キーを Virtual Key にします。環境変数で設定するなら `OPENAI_API_BASE_URL=http://llmgw:4000/v1`、`OPENAI_API_KEY=sk-gw-...` です。
- 長い生成で途中で切れる場合は、`AIOHTTP_CLIENT_TIMEOUT` を延ばしてください（下の[タイムアウト整合表](#経路上のタイムアウト整合表)を参照）。

### Claude Code

```sh
export ANTHROPIC_BASE_URL=http://llmgw:4000
export ANTHROPIC_AUTH_TOKEN=sk-gw-...             # Virtual Key（Authorization: Bearer として送られる）
export ANTHROPIC_MODEL=qwen3-coder                 # メインモデル（エイリアス名）
export ANTHROPIC_DEFAULT_HAIKU_MODEL=qwen3-coder   # 軽量タスク用（未設定だと claude-haiku-... が送られる）
export API_TIMEOUT_MS=1800000                      # モデルのロードと長い prefill を待てるように
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
claude
```

- Claude Code は `claude-sonnet-4-5` のような Anthropic のモデル名を送ってきます。**その名前でエイリアスを作れば、任意のローカルモデルに向けられます**（`ANTHROPIC_MODEL` を設定しない場合）。
- Claude Code のシステムプロンプトとツール定義は1万トークンを超えます。エイリアスの overrides で `num_ctx` を十分に大きくしてください（例 `{"num_ctx": 65536}`）。初回のロードと prefill に数分かかることがあります。待っている間、ゲートウェイは `ping` を15秒ごとに送ります。
- `thinking` を指定しない要求では、Anthropic API と同じく思考をオフにします。Ollama には `think:false` を、パススルーでは `thinking: {"type":"disabled"}` を送ります。

### OpenAI SDK

```python
from openai import OpenAI
client = OpenAI(base_url="http://llmgw:4000/v1", api_key="sk-gw-...")
client.chat.completions.create(model="qwen3", messages=[{"role": "user", "content": "hi"}])
```

```js
import OpenAI from "openai";
const client = new OpenAI({ baseURL: "http://llmgw:4000/v1", apiKey: "sk-gw-..." });
```

LiteLLM と同じく `/v1` を付けないパス（`/chat/completions`、`/models` など）でも受け付けます。

### Anthropic SDK

```python
import anthropic
client = anthropic.Anthropic(base_url="http://llmgw:4000", api_key="sk-gw-...")
client.messages.create(model="qwen3", max_tokens=512, messages=[{"role": "user", "content": "hi"}])
```

### Ollama クライアント

```python
import ollama
client = ollama.Client(host="http://llmgw:4000", headers={"Authorization": "Bearer sk-gw-..."})
client.chat(model="qwen3", messages=[{"role": "user", "content": "hi"}])
```

`ollama` CLI は認証ヘッダを送れないため、使えません。

---

## タイムアウトの考え方と経路上の整合

ローカルLLMへの1リクエストは、待ち方の違う段階を順に通ります。ゲートウェイは、段階ごとに別のタイマーで監視します。

| 段階 | 設定名 | 既定 | 超過時 |
|---|---|---|---|
| ゲートウェイ内の同時実行枠の空き待ち | `queue_timeout` | 120秒 | 503 + `Retry-After` |
| 上流へのTCP接続〜リクエスト送信 | `connect_timeout` | 10秒 | 次のターゲットへフォールバック |
| 送信〜最初のトークン（モデルのロードと prefill を含む） | `ttft_timeout` | 300秒 | 504（フォールバックは任意） |
| 生成中の無通信 | `idle_timeout` | 120秒 | ストリーム内エラー |
| 全体（キュー待ちを除く） | `max_duration` | 1800秒 | ストリーム内エラー |

- 値は「全体設定 → プロバイダ → エイリアス」の順に上書きされます。各値の実効値と、どの階層から来たかは、エイリアスの編集画面で確認できます。`0` は無制限です。
- ストリーミングでは、応答ヘッダを「最初のトークン」か「最初の keepalive（15秒）」の早い方で返します。それより前に失敗すれば通常の HTTP エラーになり、それより後の失敗はストリーム内のエラーになります。keepalive は OpenAI 形式では `: keepalive`、Anthropic 形式では `event: ping` です。Ollama（NDJSON）では送りません。
- 非ストリーミングの要求も、上流へはストリーミングで要求します。そのため TTFT と無通信の監視は同じように効きます。途中で失敗した場合、途中までの結果は返さずに 504 にします。途中までの結果はログに残します。
- タイムアウトやクライアントの切断では上流との接続を閉じ（Ollama は接続が切れると生成を止める）、閉じた時点で同時実行枠を解放します。

### 経路上のタイムアウト整合表

クライアント → プロキシ（Traefik／Cloudflare／nginx）→ llmgw → 上流 のどこか1か所でも短すぎると、そこで切れます。目安は次のとおりです。

| レイヤ | 設定 | 既定値 | 推奨 | 理由 |
|---|---|---|---|---|
| OpenAI SDK（Python/JS） | `timeout` | 600秒（接続5秒） | 非ストリーミングで長い生成をするなら `max_duration` 以上 | ストリーミングは keepalive が流れるので既定のままで足りる |
| Anthropic SDK | `timeout` | 600秒 | 同上 | 長い生成は `messages.stream()` を使う |
| Claude Code | `API_TIMEOUT_MS` | 600000（10分） | `ttft_timeout` 以上。長い作業では 1800000 程度 | 初回のロード＋1万トークン超の prefill に数分かかる |
| Open WebUI | `AIOHTTP_CLIENT_TIMEOUT` | 300秒 | `max_duration` 以上（例 1800） | リクエスト全体の上限なので、長い生成が5分で切れる |
| Traefik v3 | `entryPoints.*.transport.respondingTimeouts.readTimeout` | 60秒 | 大きな画像や長いプロンプトを送るなら 120秒以上 | リクエスト本文の受信時間の上限。応答のストリーミングには影響しない |
| Traefik v3 | `respondingTimeouts.writeTimeout` / `idleTimeout` | 0 / 180秒 | 既定のまま | 応答時間に上限はない |
| Cloudflare（プロキシ経由） | 最初の応答バイトまでの待ち | 100秒（Enterprise は変更可） | `queue_timeout` を 90秒以下にする。非ストリーミングはキーの `nonstream_keepalive` を有効にする | ストリーミングは15秒で応答ヘッダが出るので問題ない。キュー待ちと非ストリーミングでは応答が始まらず 524 になる |
| nginx | `proxy_read_timeout` / `proxy_buffering` | 60秒 / on | 非ストリーミングで長く待つなら延ばす / SSE 用に off | ゲートウェイは `X-Accel-Buffering: no` を返す。keepalive 15秒で無通信判定は回避できる |
| llmgw | `LLMGW_READ_TIMEOUT` | 120秒 | 大きな本文を送るなら延ばす | リクエスト本文の受信時間の上限 |
| llmgw | `queue_timeout` | 120秒 | 前段で最初のバイトまでの時間を制限するプロキシより短く | キュー待ちの間は何も送らない |
| llmgw | `ttft_timeout` | 300秒 | ロード時間＋最長 prefill 以上 | 大きなモデルの初回ロードと長いプロンプトに数分かかる |
| llmgw | `keepalive_interval` | 15秒 | 経路上で最も短い無通信タイムアウトより短く | |
| Ollama | `OLLAMA_KEEP_ALIVE`（またはエイリアスの `keep_alive`） | 5分 | よく使うモデルは長め（例 `30m`、`-1`=常駐） | アンロードされると次の要求でロード時間が TTFT に乗る |
| Ollama | `OLLAMA_LOAD_TIMEOUT` | 5分 | `ttft_timeout` 以下 | ロードが止まったときは Ollama 側で諦めさせる |
| Ollama | `OLLAMA_NUM_PARALLEL` | モデル依存 | プロバイダの `max_concurrency` と同じ値にする | 待ちを Ollama 内部ではなくゲートウェイで行わせ、時間とログに表れるようにする |
| llama.cpp server | `--timeout` | 600秒 | `max_duration` 以上 | サーバー側の読み書きタイムアウト |

---

## Traefik の設定例

[docker-compose.yml](docker-compose.yml) のラベルが基本形です。

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.llmgw.rule=Host(`llm.example.com`)
  - traefik.http.routers.llmgw.entrypoints=websecure
  - traefik.http.routers.llmgw.tls.certresolver=letsencrypt
  - traefik.http.services.llmgw.loadbalancer.server.port=4000
```

- llmgw 側では `LLMGW_TRUSTED_PROXIES` に Traefik のネットワーク（例 `172.16.0.0/12`）を指定してください。ログに実際のクライアント IP が記録されるようになります。
- **管理画面を forward auth で守る**場合は、`/admin` 用のルーターにミドルウェアを付け、`LLMGW_ADMIN_TRUST_HEADER=X-Forwarded-User` を設定します。このヘッダは `LLMGW_TRUSTED_PROXIES` からの接続でだけ信頼します。

  ```yaml
  - traefik.http.routers.llmgw-admin.rule=Host(`llm.example.com`) && PathPrefix(`/admin`)
  - traefik.http.routers.llmgw-admin.middlewares=auth@file
  - traefik.http.routers.llmgw-admin.service=llmgw
  ```
- 大きなリクエスト本文を受けるなら、静的設定でエントリポイントの `readTimeout` を延ばします。

  ```yaml
  entryPoints:
    websecure:
      address: ":443"
      transport:
        respondingTimeouts:
          readTimeout: 300s
  ```
- Traefik は SSE をバッファしないので、ストリーミングのための追加設定は要りません。

---

## 運用

### 同時実行制御とキュー

- キーとプロバイダの `max_concurrency` で同時実行数を制限できます（空欄は無制限）。超えた分は FIFO で待ち、`queue_timeout` を過ぎると 503 と `Retry-After` を返します。Anthropic 形式では `overloaded_error` です。
- 枠は、クライアントへの応答が終わった時点ではなく、**上流との接続を閉じた時点**で解放します。待ち時間はログの `queue_ms` に記録します。
- GPU 1台を共有するなら、プロバイダの `max_concurrency` を Ollama の `OLLAMA_NUM_PARALLEL` に合わせてください。

### パラメータ上書き（エイリアス）

```json
{
  "defaults":  {"temperature": 0.7},
  "overrides": {"num_ctx": 32768, "keep_alive": "30m", "max_tokens": 4096, "think": false},
  "system_prompt": {"mode": "prepend", "text": "日本語で答えてください。"}
}
```

- `defaults` はクライアントが指定しなかった場合だけ、`overrides` は常に適用します。ただし `overrides.max_tokens` は**上限**として扱います。
- 共通の名前は `temperature`、`top_p`、`top_k`、`seed`、`max_tokens`、`stop`、`presence_penalty`、`frequency_penalty`、`keep_alive`、`think` です。それ以外（`num_ctx`、`repeat_penalty`、`min_p` など）は Ollama の `options` として扱います。
- どの形式で受けても適用されます。Anthropic 形式のリクエストに `num_ctx` のような Anthropic で表せない項目を適用する場合は、パススルーではなく変換で送ります。

### フォールバック

ターゲットを上から順に試します。次へ進むのは、接続に失敗したとき（`connect_timeout` を含む）と、応答が始まる前に 5xx が返ったときだけです。`ttft_timeout` で失敗したときは、エイリアスの「TTFT タイムアウト時にフォールバック」を有効にしている場合だけ次へ進みます。トークンを返し始めた後はフォールバックしません。

### ヘルスチェック

有効なプロバイダを `health_check_interval`（既定60秒）ごとに確認し、ダッシュボードに表示します。状態が変わったときは構造化ログにも出します。

### プロンプトログ

- キーの `log_mode` で保存内容を決めます。`full` は本文も保存、`metadata_only` は本文なし、`none` は記録しません。
- 保存する本文は「クライアントから受けたリクエスト」と「クライアントに返したレスポンス」で、どちらも受けた形式のままです。ストリーミングは、組み立てた完全な応答として保存します。中断した場合は、途中までの結果に `interrupted` を付けて保存します。
- base64 画像は `[image: 123KB]` に、長い数値配列（埋め込みベクトルなど）は `[N numbers]` に置き換えます。本文は1件あたり1MB（`log_body_max_bytes`）までで、超えた分は切り詰めます。
- `log_retention_days`（既定30日）を過ぎたログは自動で削除します。管理画面で検索・閲覧でき、JSONL でエクスポートできます。

### バックアップと復元

```sh
llmgw backup /data/backup-$(date +%F).db          # 稼働中でも可（VACUUM INTO による一貫したコピー）
docker compose exec llmgw llmgw backup /data/backup.db
```

管理画面の「設定」からもダウンロードできます。復元するときは、停止した状態で DB ファイルを置き換えます。このとき `LLMGW_SECRET_KEY` は同じ値でなければなりません（違うとプロバイダのAPIキーを復号できません）。

### 管理者パスワードを忘れたとき

```sh
llmgw reset-admin-password                        # 生成したパスワードを表示する
llmgw reset-admin-password -password '12文字以上'
docker compose exec llmgw llmgw reset-admin-password
```

稼働中でも実行できます。そのアカウントの既存セッションは無効になります。

---

## LiteLLM からの移行

1. **llmgw を起動します**。LiteLLM と同じポート（4000）を使えば、クライアント側の設定変更は base URL のホスト名だけで済みます。
2. **`config.yaml` の `model_list` を取り込みます**。先に dry-run で内容を確認してから実行します。

   ```sh
   docker compose run --rm -v "$PWD/litellm/config.yaml:/config.yaml:ro" llmgw import-litellm -dry-run /config.yaml
   docker compose run --rm -v "$PWD/litellm/config.yaml:/config.yaml:ro" llmgw import-litellm /config.yaml
   ```

   - `model_name` がそのままエイリアス名になります。同じ `model_name` の複数デプロイと `fallbacks` は、ターゲットの優先順に変換します。
   - `ollama/`・`ollama_chat/` は Ollama プロバイダに、`openai/`・`hosted_vllm/`・`lm_studio/` などは OpenAI互換プロバイダになります。クラウドのプロバイダ（`anthropic/` など）は取り込まず、警告を出します。
   - `api_key: os.environ/NAME` は実行時の環境変数から読みます。`temperature` や `num_ctx` などはエイリアスの `defaults` に入れます。
   - **`api_base` に `localhost` と書いてある場合は注意してください**。これは LiteLLM コンテナ自身を指していた値なので、llmgw から届くアドレス（`host.docker.internal` など）に管理画面で直してください。
   - 既にあるエイリアスは飛ばします。`-replace` を付けると置き換えます。
3. **Virtual Key を引き継ぎます**。LiteLLM はキーを `sha256(key)` の16進で保存しており、llmgw と同じ形式です。そのため**再発行なしで、同じキーがそのまま使えます**。

   ```sh
   docker compose exec -T db psql -U <user> -d <db> -At -c \
     "SELECT row_to_json(t) FROM (SELECT token, key_name, key_alias, models, expires, blocked, rpm_limit, max_parallel_requests FROM \"LiteLLM_VerificationToken\") t" > keys.jsonl
   docker compose run --rm -v "$PWD/keys.jsonl:/keys.jsonl:ro" llmgw import-litellm-keys /keys.jsonl
   ```

   引き継ぐ項目は、名前（`key_alias`）、許可モデル（`models`。空または `all-proxy-models` なら全許可）、期限、ブロック状態、RPM 上限、並列数の上限です。**マスターキー（`LITELLM_MASTER_KEY`）は DB に入っていないので引き継げません**。必要なら管理画面で新しいキーを発行してください。予算、チーム、利用額は対象外です。
4. **管理画面と Playground で動作を確認します**。プロバイダの `max_concurrency` やエイリアスの `num_ctx` なども、この時点で調整します。
5. **Traefik のルーターを llmgw に切り替えます**。OpenAI互換のパスは `/v1` 付きでも無しでも受け付けるので、クライアントの base URL はそのまま使えます。
6. 問題がなければ、LiteLLM と Postgres を止めます。LiteLLM 固有の管理API（`/key/generate`、`/spend` など）は提供していません。

---

## 環境変数

| 変数 | 既定 | 説明 |
|---|---|---|
| `LLMGW_SECRET_KEY` | **必須** | プロバイダAPIキーの暗号化（AES-GCM）に使う鍵。未設定なら起動しない。後から変えない |
| `LLMGW_LISTEN` | `:4000` | 待ち受けアドレス |
| `LLMGW_DB_PATH` | `data/llmgw.db`（Docker では `/data/llmgw.db`） | SQLite ファイル |
| `LLMGW_ADMIN_PASSWORD` | ランダム生成 | 初回起動時の管理者パスワード。未設定なら生成して起動ログに一度だけ出す |
| `LLMGW_ADMIN_TRUST_HEADER` | 無効 | このヘッダがあれば管理画面を認証済みとみなす（forward auth 用。信頼するプロキシからの接続に限る） |
| `LLMGW_TRUSTED_PROXIES` | なし | `X-Forwarded-For` を信頼するプロキシ（IP/CIDR、カンマ区切り） |
| `LLMGW_COOKIE_SECURE` | `true` | 管理画面の Cookie の Secure 属性。平文HTTPで使う場合は `false` |
| `LLMGW_MAX_BODY_BYTES` | 64MB | APIリクエスト本文の上限 |
| `LLMGW_READ_TIMEOUT` / `LLMGW_READ_HEADER_TIMEOUT` | `120s` / `10s` | リクエスト本文／ヘッダの受信タイムアウト |
| `LLMGW_SHUTDOWN_TIMEOUT` | `30s` | グレースフルシャットダウンの待ち時間 |
| `LLMGW_OLLAMA_VERSION` | `0.13.0` | `GET /api/version` が返すバージョン |
| `LLMGW_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`（ログは JSON で標準出力） |

タイムアウトの全体既定値、keepalive の間隔、ログの保持期間、ヘルスチェックの間隔などは DB に保存し、管理画面の「設定」で変えます。

---

## API の対応状況

| 系統 | エンドポイント |
|---|---|
| OpenAI互換 | `POST /v1/chat/completions`、`POST /v1/completions`、`POST /v1/embeddings`、`GET /v1/models`（`/v1` 無しでも可） |
| Ollama | `POST /api/chat`、`/api/generate`、`/api/embed`、`/api/embeddings`、`GET /api/tags`、`POST /api/show`、`GET /api/ps`、`GET /api/version`。モデル管理系（pull/push/create/copy/delete/blobs）は常に 403 |
| Anthropic互換 | `POST /v1/messages`、`POST /v1/messages/count_tokens` |
| 管理 | `/admin/`（画面）、`/admin/api/...`（REST） |
| その他 | `GET /healthz`、`GET /`（`Ollama is running`） |

変換の細かい規則と管理APIの一覧は [docs/api.md](docs/api.md) にあります。

### CLI

```
llmgw serve                                   ゲートウェイを起動（既定）
llmgw backup <path>                           オンラインバックアップ
llmgw reset-admin-password [-username U] [-password P]
llmgw import-litellm [-dry-run] [-replace] <config.yaml>
llmgw import-litellm-keys [-dry-run] <keys.jsonl>
llmgw sql "<SQL>"                             DB に直接 SQL を実行（調査用）
llmgw healthcheck                             /healthz を確認（Docker HEALTHCHECK 用）
llmgw version
```

---

## 開発

```sh
go test ./...                       # ユニットテストと e2e（上流はモック、タイマーは差し替え可能な時計）
go test -race ./...                 # Windows では CGO が要るため、Linux コンテナでの実行を推奨
```

- 変換のテストは、受信3形式 × 送信3形式 × ストリームの有無を、テキスト・画像・ツール呼び出し・thinking を含めて網羅しています（`internal/server/matrix_e2e_test.go`）。
- タイムアウトのテストは、遅延や停止を再現できるモック上流と、差し替え可能な時計を使います。実際の時間は待ちません。
- 実際のモデルと公式 SDK を使う結合テストは [scripts/integration](scripts/integration/README.md) にあります（任意実行）。
- 仕様は [SPEC.md](SPEC.md) にあります。

## ライセンス

[NYSL Version 0.9982](LICENSE)（煮るなり焼くなり好きにしろライセンス）です。
