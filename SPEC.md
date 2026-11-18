# 開発プロンプト：ローカルLLM向け軽量ゲートウェイ「llmgw」

あなたはこのリポジトリで、Ollama / LM Studio などのローカルLLMプロバイダの前段に置く軽量LLMゲートウェイを実装する。現在 LiteLLM Proxy（Docker Compose + Postgres + Traefik）で運用している用途を置き換えることが目的。LiteLLMの多機能さは不要で、**シンプル・単一バイナリ・運用が軽いこと**を最優先とする。

不明点や仕様の曖昧さに気づいたら、実装前に質問すること。各フェーズの終わりで動作確認手順を示し、次フェーズに進む前に私の確認を待つこと。

---

## 1. スコープ

### 作るもの
1. **Virtual Key 管理**：発行・再発行（ローテーション）・無効化・有効化・削除
2. **プロバイダ管理**：Ollama、OpenAI互換API（LM Studio、llama.cpp server、vLLM など）の登録・編集・削除・疎通確認
3. **モデルエイリアス**：クライアントが指定する公開モデル名 → 実プロバイダ＋実モデル名へのマッピング。差し替えはエイリアスの向き先変更だけで完了する
4. **Playground**：管理画面からキーとエイリアスを選んでチャットを試せる画面（ストリーミング表示）
5. **プロンプトログ**：リクエスト／レスポンス本文の収集と閲覧・検索
6. **管理画面**：上記すべてをWeb UIから操作

### 作らないもの（明示的に除外）
- 組織・チーム・一般ユーザー管理（アカウントは管理者のみ）
- 課金・予算・コスト計算・利用量ダッシュボード
- クラウドLLMプロバイダ固有の対応（OpenAI互換で繋がるものは結果的に使えて構わないが、専用実装はしない）
- キャッシュ、ガードレール、プロンプト管理、評価機能

---

## 2. 技術スタック

- **言語**：Go（最新安定版）。標準 `net/http` 中心で、依存は最小限
- **DB**：SQLite（`modernc.org/sqlite` を使い CGO 不要にする）。WALモード。マイグレーションはアプリ起動時に自動適用
- **管理UI**：サーバー側テンプレート + htmx（または小さなSPA）を `go:embed` でバイナリに同梱。外部CDNには依存しない
- **配布**：単一バイナリ + 最小Dockerイメージ（distroless または alpine）。`docker-compose.yml` のサンプルと Traefik ラベル例を同梱
- **設定**：環境変数（必要最小限）＋ DB に保存する動的設定。設定ファイルは任意

技術選定に強い異論があれば実装前に提案してよい。

---

## 3. 公開API（クライアント向け）

3系統のAPIを同一ポートで提供する。どの系統から来たリクエストも、同じ Virtual Key・同じエイリアスで処理する（例：同じキーで OpenAI 形式でも Anthropic 形式でも同じモデルを呼べる）。

### 3.1 認証（全系統共通）
- `Authorization: Bearer <virtual key>` と `x-api-key: <virtual key>` の両方を受け付ける
- 認証エラー時は、リクエストを受けた系統のエラー形式で返す（3.5参照）

### 3.2 OpenAI互換API

| エンドポイント | 内容 |
|---|---|
| `POST /v1/chat/completions` | `stream: true` の SSE に対応 |
| `POST /v1/completions` | レガシー |
| `POST /v1/embeddings` | |
| `GET /v1/models` | **そのキーが利用可能なエイリアスのみ**を返す |

### 3.3 Ollama ネイティブAPI

Open WebUI の Ollama 接続や Ollama 専用クライアントを、ゲートウェイの URL に向けるだけで使えるようにする。

| エンドポイント | 内容 |
|---|---|
| `POST /api/chat` | ストリーミングは **NDJSON**（SSE ではない）。`stream` のデフォルトは `true` である点に注意 |
| `POST /api/generate` | 同上 |
| `POST /api/embed` | 現行の埋め込みAPI |
| `POST /api/embeddings` | 旧埋め込みAPI（互換のため） |
| `GET /api/tags` | キーが利用可能なエイリアスを Ollama のモデル一覧形式で返す |
| `POST /api/show` | エイリアスの情報を返す。上流が Ollama ならその応答を元にし、そうでなければ最小限の項目で組み立てる |
| `GET /api/ps` | 任意。上流 Ollama の状態をエイリアス名に読み替えて返す |
| `GET /api/version` | ゲートウェイ固定値を返す（認証不要でよい。クライアントの接続確認に使われるため） |

要件：
- **モデル管理系エンドポイント（`/api/pull`、`/api/push`、`/api/create`、`/api/copy`、`/api/delete` 等）は中継せず 403 を返す**。キーを持つクライアントが上流のモデルを消したり大容量のダウンロードを始めたりできないようにするため
- Ollama のモデル名は `name:tag` 形式なので、エイリアス名にコロンを使えるようにする。タグ省略時は `:latest` を補って照合する
- `/api/tags` の `size`、`digest`、`details` 等は、上流 Ollama から取れればその値、取れなければ妥当なダミー値で埋め、クライアントが壊れないことを優先する
- `options`（`num_ctx`、`temperature` 等）、`keep_alive`、`format`、`think`、`images`、`tools` を透過・変換する

### 3.4 Anthropic互換API

Claude Code 等の Anthropic SDK 系クライアントから、`ANTHROPIC_BASE_URL` をゲートウェイに向けてローカルモデルを使えるようにする。

| エンドポイント | 内容 |
|---|---|
| `POST /v1/messages` | Messages API。ストリーミングは Anthropic 形式の SSE イベント列（`message_start` → `content_block_start` → `content_block_delta` → `content_block_stop` → `message_delta` → `message_stop`、および `ping`） |
| `POST /v1/messages/count_tokens` | 上流で正確に数えられない場合は概算値を返す（文字数ベース等）。Claude Code がこのエンドポイントを呼ぶため 404 にはしない |

要件：
- `system`（文字列 / ブロック配列の両方）、`messages` のコンテンツブロック（`text`、`image`、`tool_use`、`tool_result`）、`tools`、`tool_choice`、`stop_sequences`、`max_tokens`、`temperature`、`top_p`、`top_k` に対応
- `thinking` が指定され上流が推論出力（Ollama の `thinking`、OpenAI互換の `reasoning_content` 等）を返す場合は `thinking` ブロックに変換する。対応しない上流では無視する
- `cache_control` 等の Anthropic 固有でローカルでは意味のないフィールドはエラーにせず無視する
- `anthropic-version`、`anthropic-beta` ヘッダは受け付けるが、上流が Anthropic 互換で直接中継する場合以外は解釈しなくてよい
- `stop_reason`（`end_turn`、`max_tokens`、`tool_use`、`stop_sequence`）と `usage` を上流の結果から正しく対応付ける
- Claude Code は `claude-sonnet-...` のようなモデル名を送ってくるため、その名前でエイリアスを作れば任意のローカルモデルに向けられることを README に記載する

### 3.5 エラー形式
系統ごとにそれぞれの形式で返す。
- OpenAI互換：`{"error": {"message", "type", "code"}}`
- Ollama：`{"error": "message"}`
- Anthropic：`{"type": "error", "error": {"type", "message"}}`（`invalid_request_error`、`authentication_error`、`permission_error`、`not_found_error`、`rate_limit_error`、`api_error`、`overloaded_error`）

### 3.6 変換層の設計

受信側（クライアントが使う API 形式）と送信側（上流プロバイダの API 形式）を分離し、組み合わせごとに処理する。

- **受信アダプタ**：OpenAI互換 / Ollama ネイティブ / Anthropic
- **送信アダプタ**：Ollama ネイティブ（`/api/chat` 等） / OpenAI互換 / Anthropic互換（上流が `/v1/messages` を持つ場合）
- 受信形式と同じ形式を上流がサポートしていれば **変換せずパススルー**（モデル名の書き換えとパラメータ上書きのみ）する。そうでなければ内部共通形式を経由して変換する
- 内部共通形式は OpenAI Chat Completions のスキーマを土台にし、変換で失われる情報（Ollama の `options`、Anthropic の `thinking` 等）は拡張フィールドで保持する
- **Ollama プロバイダへの送信はネイティブAPI（`/api/chat`、`/api/generate`、`/api/embed`）を基本とする**。`num_ctx`、`keep_alive` 等の Ollama 固有オプションを確実に効かせるため
- ストリーミングは受信形式ごとのストリーム形式（SSE / NDJSON / Anthropic SSE イベント）に**逐次変換**し、バッファリングしない。クライアント切断・タイムアウト時の扱いは 3.7 に従う
- ツール呼び出しの変換（特にストリーミング中の引数断片の組み立て、`tool_call_id` ⇔ `tool_use_id` の対応、Ollama のツール呼び出しにIDがない場合の採番）を丁寧に実装する
- 未知のフィールドは、パススルー時はそのまま透過し、変換時は捨ててよい（ログに警告は不要）
- 変換ロジックは HTTP から切り離した純粋関数として実装し、テーブル駆動テストを厚めに書く

### 3.7 タイムアウトと長時間推論

ローカルLLMの1リクエストは「キュー待ち → 接続 → モデルロード＋prefill → 生成」と、待ち時間の性質が違う段階を通る。タイムアウトを1本の値にせず、段階ごとに分けて監視する。

#### 段階別タイムアウト

| 名前 | 対象 | デフォルト | 設定単位 | 超過時 |
|---|---|---|---|---|
| `connect_timeout` | 上流へのTCP接続〜リクエスト送信完了 | 10秒 | プロバイダ | 応答開始前の失敗としてフォールバック対象 |
| `queue_timeout` | ゲートウェイ内の同時実行枠の空き待ち | 120秒 | プロバイダ | 503 + `Retry-After` |
| `ttft_timeout` | 上流へ送信後、最初のトークン（または最初の有効チャンク）が届くまで。モデルロードとprefillを含む | 300秒 | エイリアス | 504 |
| `idle_timeout` | 生成開始後、上流から次のチャンクが届くまでの無通信時間 | 120秒 | エイリアス | 応答開始後のエラーとして処理（後述） |
| `max_duration` | リクエスト全体の上限（暴走防止）。キュー待ちは含めない | 1800秒 | エイリアス | 応答開始後のエラーとして処理 |

- 値は「全体デフォルト（設定画面）→ プロバイダ → エイリアス」の順に上書きする。未設定は上位の値を継承する
- `0` は無制限を意味する（`max_duration` 以外は非推奨として管理画面で警告表示）
- 推論出力（thinking / reasoning）のチャンクも「トークン」として扱い、TTFT・無通信の判定に含める。上流のkeepalive的な空チャンクは含めない

#### 上流へは常にストリーミングで要求する
- クライアントが非ストリーミング（`stream: false`）を要求した場合も、ゲートウェイから上流へは常にストリーミングで要求し、ゲートウェイ内で組み立ててから一括で返す
- これにより非ストリーミングでも `ttft_timeout` / `idle_timeout` が同じように効き、`ttft_ms` も常に記録できる
- 埋め込み（embeddings）はストリーミングがないため、`ttft_timeout` を応答全体のタイムアウトとして使う

#### Go サーバー側の注意
- `http.Server.WriteTimeout` はストリーミングを含むレスポンス全体にかかるため **0（無効）にする**。リクエスト単位の制御は context と `http.ResponseController`（`SetWriteDeadline` 等）で行う
- `ReadHeaderTimeout` は短め（10秒）に設定し、`ReadTimeout` はリクエスト本文の受信用として長め（画像付き・長文プロンプトを考慮して 120秒程度、設定可能）にする
- 上流向け `http.Client` には全体の `Timeout` を設定しない（ストリーミングが途中で切れるため）。`connect_timeout` は `net.Dialer` のタイムアウトで実装し、`ttft_timeout` / `idle_timeout` / `max_duration` は自前のタイマーで管理する（上流によっては応答ヘッダだけ先に返してトークンを待たせるため、`ResponseHeaderTimeout` では TTFT を測れない）

#### keepalive（途中経路での切断防止）
- ストリーミング応答では、応答ヘッダを返した後、トークンが出るまで（ロード・prefill中）および生成中に無通信が続く間、15秒間隔（設定可能）で keepalive を送る
  - OpenAI互換：SSE コメント行 `: keepalive`
  - Anthropic互換：`event: ping`
  - Ollama ネイティブ（NDJSON）：送らない（安全な手段がないため）
- ストリーミング時は、上流の応答を待たずにキュー確保後すぐ応答ヘッダを返してよい。ただしその場合、TTFTタイムアウトやフォールバック不能な上流エラーはストリーム内エラーとして返すことになるため、**応答ヘッダの送出は「最初のトークン受信時」か「最初の keepalive 送出時」のどちらか早い方**とし、それまでに失敗した場合は通常のHTTPエラーで返す
- 非ストリーミング応答の keepalive（JSON本体の前に空白を定期送出する方式）は、ステータス200を先に確定させてしまう副作用があるため、キー単位のオプション（デフォルト無効）とする。有効時にエラーになった場合は、200のまま受信形式のエラーJSONを本文で返す

#### キャンセルとGPUの解放
- タイムアウト・クライアント切断のどちらでも、context をキャンセルして上流との接続を確実に閉じる（Ollama は接続断で生成を中止する）
- **同時実行枠（キー単位・プロバイダ単位）は、クライアントへの応答終了時ではなく、上流との接続が閉じた時点で解放する**。裏で生成が続いている間に次のリクエストを上流へ送り込まないため
- 上流が接続断後も処理を続ける実装の可能性に備え、解放の遅延は発生しうるものとして扱い、テストで上流の接続クローズを検証する

#### フォールバックとの関係
- フォールバック対象は「接続失敗（`connect_timeout` 含む）」と「応答開始前の 5xx」のみ
- `ttft_timeout` 超過はデフォルトでフォールバック対象外（別の上流でもモデルロードからやり直しになり待ち時間が倍増するため）。エイリアス設定 `fallback_on_ttft_timeout`（デフォルト false）で有効化できる
- フォールバック時も `max_duration` は最初の試行開始時点から通算する

#### タイムアウト時のエラー応答
- 応答開始前：504（`queue_timeout` は 503 + `Retry-After`）を受信形式のエラー形式で返す
  - Anthropic 形式では 504 相当として `api_error`、`queue_timeout` は `overloaded_error`（HTTP 529 ではなく 503 を使う）
- 応答開始後（ストリーム途中）：受信形式ごとのエラー表現を送ってから接続を閉じる
  - OpenAI互換：`data: {"error": {...}}` を送り、`[DONE]` は送らない
  - Anthropic互換：`event: error` に `{"type":"error","error":{"type":"api_error","message":"..."}}`
  - Ollama ネイティブ：`{"error": "..."}` の行
- 非ストリーミングで応答組み立て中にタイムアウトした場合、途中までの生成結果は返さず 504 とする（ログには途中結果を保存する）
- エラーメッセージにはどの段階のタイムアウトか（例：`timeout: ttft exceeded 300s`）を含める

## 4. データモデル

### admin
- 管理者は1名（将来複数でも困らない構造にしておく）
- パスワードは argon2id でハッシュ化
- 初回起動時、環境変数 `LLMGW_ADMIN_PASSWORD` から作成。未設定ならランダム生成してログに一度だけ出力

### providers
- `id`, `name`, `type`（`ollama` / `openai_compatible`）, `base_url`, `api_key`（任意・**暗号化保存**）, `connect_timeout`, `queue_timeout`, `max_concurrency`（プロバイダ単位の同時実行上限）, `enabled`, `created_at`, `updated_at`
- `supports_anthropic`（bool）：上流が `/v1/messages` を持つか。true なら Anthropic 形式のリクエストを変換せずパススルーする。疎通確認時に自動判定できればする
- 疎通確認：Ollama は `/api/tags`、OpenAI互換は `/v1/models` を叩き、取得できたモデル一覧を管理画面に表示（エイリアス作成時の候補にする）
- 定期ヘルスチェック（間隔は設定可能、デフォルト60秒）。状態を管理画面に表示

### aliases
- `id`, `name`（公開モデル名・一意）, `enabled`, `description`
- **targets**（1エイリアスに複数可）：`provider_id`, `upstream_model`, `priority`
  - 先頭ターゲットが接続失敗・応答開始前の 5xx の場合、次の priority にフォールバック（ストリーム開始後のフォールバックはしない。条件の詳細は 3.7）
- **タイムアウト設定**：`ttft_timeout`, `idle_timeout`, `max_duration`, `fallback_on_ttft_timeout`（いずれも未設定ならプロバイダ／全体デフォルトを継承）
- **パラメータ上書き**（JSON）：受信形式に関係なく、内部共通形式の段階で適用する
  - `defaults`：クライアントが指定しなかった場合のみ適用（例：`temperature`）
  - `overrides`：常に強制適用（例：Ollama の `num_ctx`、`keep_alive`、`max_tokens` 上限）
  - `system_prompt`（任意）：先頭に追加 or 置換を選択
- エイリアスの向き先変更は即時反映（再起動不要）

### virtual_keys
- `id`, `name`（用途メモ）, `key_prefix`（表示用、例 `sk-gw-abcd…`）, `key_hash`（SHA-256）, `status`（`active` / `disabled`）, `allowed_aliases`（空なら全許可）, `expires_at`（任意）, `log_mode`（`full` / `metadata_only` / `none`）, `max_concurrency`（任意）, `rpm_limit`（任意）, `last_used_at`, `created_at`, `updated_at`
- キー形式：`sk-gw-` + 暗号学的乱数（base62で40文字程度）
- **平文キーは発行・再発行時に一度だけ表示し、DBにはハッシュのみ保存**
- 再発行：同じキーレコード（名前・権限・設定）を保ったまま新しいキーに差し替え、旧キーは即時無効。オプションで猶予期間（旧キーを N 分間併用可）を指定できると望ましい
- 無効化／有効化：論理的な停止。削除：物理削除（ログ上はキー名をスナップショットとして残す）
- 認証はメモリキャッシュ（短TTL）でDB負荷を抑えつつ、無効化・削除は即時反映されるようキャッシュを破棄する

### request_logs
- `id`, `created_at`, `key_id`, `key_name`（スナップショット）, `alias`, `provider_id`, `upstream_model`, `endpoint`, `inbound_format`（`openai` / `ollama` / `anthropic`）, `upstream_format`, `passthrough`（bool）, `stream`, `status_code`, `error`, `queue_ms`（キュー待ち時間）, `latency_ms`, `ttft_ms`（上流送信から初回トークンまで。非ストリーミング要求でも記録）, `timeout_phase`（`queue` / `connect` / `ttft` / `idle` / `max_duration`、タイムアウトでなければ null）, `cancelled_by_client`（bool）, `attempts`（フォールバックを含む試行の履歴：プロバイダ・結果・所要時間）, `prompt_tokens`, `completion_tokens`（上流が返す場合のみ）, `client_ip`, `user_agent`
- `request_body`, `response_body`：キーの `log_mode` が `full` のときのみ保存
  - 保存するのは「クライアントから受けたリクエスト」と「クライアントに返したレスポンス」（受信形式のまま）。変換のデバッグ用に、上流へ送ったリクエストも保存するオプション（デフォルト無効）を設ける
  - ストリーミング時はチャンクを組み立てて最終的な完全レスポンス（content、tool_calls / tool_use、thinking）として保存
  - タイムアウト・切断で中断した場合も、そこまでに生成された部分を「中断」の印付きで保存する
  - 画像の base64 データは保存時に `[image: 123KB]` のようなプレースホルダに置換
  - 1レコードあたりの本文サイズ上限（デフォルト 1MB）を設け、超過分は切り詰めて印を付ける
- ログ書き込みは非同期（チャネル + バッチ書き込み）にし、リクエストのレイテンシに影響させない
- **保持期間**（デフォルト30日、設定可能）を過ぎたログは定期削除
- 補足：トークン数・レイテンシは統計目的ではなくデバッグ用のログ属性として記録するのみ。集計ダッシュボードは作らない

### audit_logs
- 管理操作（キー発行・再発行・削除、プロバイダ／エイリアス変更、ログイン成功・失敗）を記録。シンプルな一覧表示のみ

---

## 5. 管理画面

- パス：`/admin`（公開APIと同一ポート、パスで分離）
- 認証：パスワードログイン → HttpOnly / Secure / SameSite=Strict の Cookie セッション。CSRF対策。ログイン試行のレート制限
- Traefik の forward auth 等で前段認証する運用も想定し、`LLMGW_ADMIN_TRUST_HEADER`（指定ヘッダがあれば認証済みとみなす）をオプションで用意。デフォルト無効
- 画面一覧：
  1. **ダッシュボード**：プロバイダの稼働状態、直近エラー（統計グラフは不要）
  2. **キー**：一覧（名前・prefix・状態・許可エイリアス・最終使用日時・期限）、発行、再発行、無効化/有効化、削除、編集
  3. **プロバイダ**：一覧・追加・編集・削除・疎通確認・上流モデル一覧表示
  4. **エイリアス**：一覧・追加・編集（ターゲット、パラメータ上書き）・削除。各エイリアスから「Playgroundで試す」
  5. **Playground**：API形式（OpenAI / Ollama / Anthropic）の切り替え、キー（平文を入力 or 管理者権限で任意キーとして実行を選択）、エイリアス、システムプロンプト、温度等のパラメータ、ストリーミング表示、送信したリクエストJSON / 受信レスポンスの確認、`curl` コマンドのコピー（選んだ形式で生成）
  6. **ログ**：一覧（日時・キー・エイリアス・ステータス・レイテンシ）、絞り込み（期間・キー・エイリアス・ステータス・本文の部分一致）、詳細（リクエスト／レスポンス本文の整形表示）、JSONLエクスポート
  7. **設定**：ログ保持期間、本文サイズ上限、ヘルスチェック間隔、タイムアウトの全体デフォルト、keepalive 間隔、管理者パスワード変更
  - エイリアス・プロバイダの編集画面では、各タイムアウトの「実効値（どの階層から継承したか）」を表示する
  8. **監査ログ**
- 管理用 REST API（`/admin/api/...`）も UI と同じ機能で提供する（UIはこのAPIを使う形が望ましい）

---

## 6. 非機能要件

- **タイムアウト**：3.7 のとおり段階別に制御する
- **同時実行制御**：GPU 1台を複数キーで共有する前提のため、キー単位 `max_concurrency` と、プロバイダ単位の `max_concurrency` を設ける。超過時はFIFOでキュー待ちし、`queue_timeout` 超過で 503 + `Retry-After`。枠の解放タイミングは 3.7 に従う
- **秘密情報**：プロバイダAPIキーは `LLMGW_SECRET_KEY`（環境変数）由来の鍵で AES-GCM 暗号化して保存。ログやエラーメッセージにキー・ヘッダを出さない
- **リバースプロキシ配下**：`X-Forwarded-For` を信頼するプロキシを設定で指定
- **ログ出力**：構造化ログ（JSON）を標準出力へ
- **バックアップ**：SQLite ファイル1つで完結。オンラインバックアップ用の管理APIまたはCLIサブコマンド（`llmgw backup <path>`）
- **テスト**：エイリアス解決、キー認証、パラメータ上書き、ストリーミング中継とログ組み立て、フォールバックについてユニットテスト。上流はモックサーバー（`httptest`）で代替
- **タイムアウトテスト**：遅延・停止を自在に再現できるモック上流（ロード遅延、prefill遅延、生成途中の停止、接続断後も処理継続する上流）を用意し、各タイムアウトの発火、keepalive 送出、受信形式ごとのエラー表現、フォールバック条件、同時実行枠の解放タイミング、ログの `timeout_phase` を検証する。タイマーはテストで差し替え可能にし、実時間待ちのテストにしない
- **変換テスト**：受信3形式 × 送信3形式の組み合わせについて、非ストリーム／ストリーム、テキスト／画像／ツール呼び出し／thinking を網羅するテーブル駆動テスト。実際の Ollama・LM Studio・公式 SDK（`openai`、`anthropic`、`ollama` の Python パッケージ）を使った結合テスト用スクリプトも用意する（CIでは任意実行）
- **セキュリティ**：キー比較は定数時間、ハッシュ比較で行う。管理画面に適切なセキュリティヘッダ

---

## 7. 実装フェーズ

各フェーズ完了時に、ビルド・テスト結果と手動確認手順（curl例）を提示して止まること。

1. **骨格**：プロジェクト構成、設定読み込み、SQLite + マイグレーション、`/healthz`、Dockerfile、compose サンプル
2. **中継の中核（OpenAI互換）**：内部共通形式、送信アダプタ（Ollama ネイティブ / OpenAI互換）、上流への常時ストリーミング、段階別タイムアウトとキャンセル、keepalive、プロバイダ・エイリアス（DB直接投入でよい）、`/v1/chat/completions`（非ストリーム／ストリーム）、`/v1/models`、`/v1/embeddings`、エラー正規化
3. **Virtual Key**：発行・検証・無効化・削除・再発行、許可エイリアス、期限、管理API
4. **Ollama ネイティブAPI**：受信アダプタ、NDJSON ストリーム、`/api/tags`・`/api/show` 等、モデル管理系の遮断
5. **Anthropic互換API**：受信アダプタ、Anthropic SSE イベント変換、ツール呼び出し・thinking、`count_tokens`、Anthropic 送信アダプタ（パススルー）。Claude Code から実際に使えることを確認
6. **プロンプトログ**：非同期書き込み、ストリーム組み立て、画像置換、サイズ上限、保持期間の自動削除
7. **管理画面**：ログイン、キー・プロバイダ・エイリアス・ログ・設定・監査ログ画面、Playground
8. **運用機能**：ヘルスチェック、フォールバック（タイムアウト条件含む）、同時実行制御とキュー（枠の解放タイミング含む）、パラメータ上書き、バックアップ
9. **仕上げ**：README（導入手順、Traefik設定例、各クライアントの接続設定例〔Open WebUI、Claude Code、OpenAI SDK〕、**経路上のタイムアウト整合表**〔クライアント（各SDK、Claude Code の `API_TIMEOUT_MS`、Open WebUI）、Traefik、Cloudflare 等のプロキシ、ゲートウェイ、上流（Ollama の `keep_alive` 等）の各タイムアウトを、どの値にそろえるべきかの一覧〕、LiteLLMからの移行手順）、テスト拡充

---

## 8. LiteLLM からの移行

- 既存クライアントは base URL を差し替えるだけで動くこと（OpenAI互換の挙動を維持）
- 現行 LiteLLM の `config.yaml` の `model_list` を読み込み、プロバイダとエイリアスを生成するインポート機能（CLIサブコマンド）を用意する
- 可能であれば既存 Virtual Key を再発行なしで引き継ぎたい。LiteLLM の DB に保存されているキーのハッシュ形式を調査し、互換が取れるなら「ハッシュのインポート」機能を実装する。互換が取れない場合はその旨を報告すること