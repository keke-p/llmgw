# SDK 結合テスト

公式 SDK（`openai`・`anthropic`・`ollama` の Python パッケージ）を使い、実際のゲートウェイとローカルモデルに対して主要な呼び出しを一通り試します。CI では実行しません（任意実行）。

```sh
pip install -r scripts/integration/requirements.txt
export LLMGW_BASE_URL=http://localhost:4000
export LLMGW_KEY=sk-gw-...          # Virtual Key
export LLMGW_MODEL=qwen3            # ツール呼び出しに対応したモデルのエイリアス
export LLMGW_EMBED_MODEL=nomic      # 任意。未設定なら埋め込みのテストを省略
python scripts/integration/test_sdks.py
```

Docker で実行する例：

```sh
docker run --rm --network <ゲートウェイのネットワーク> -v "$PWD/scripts/integration:/t" \
  -e LLMGW_BASE_URL=http://llmgw:4000 -e LLMGW_KEY=sk-gw-... -e LLMGW_MODEL=qwen3 \
  python:3.12-slim sh -c "pip install -q -r /t/requirements.txt && python /t/test_sdks.py"
```

確認する内容：

- **OpenAI**：モデル一覧、非ストリーム、ストリーム（usage 付き）、ツール呼び出しの往復、404・401 で返る例外の型、埋め込み
- **Anthropic**：`messages.create`、`messages.stream`、`tool_use` の往復、`count_tokens`、404
- **Ollama**：`list`、`show`、`chat`、ストリーム、ツール呼び出し、`generate`、`pull` が 403 になること、`embed`
