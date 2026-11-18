#!/usr/bin/env python3
"""Integration test of llmgw with the official SDKs (openai, anthropic, ollama).

Runs against a live gateway in front of a real local model. Not run in CI by
default; see scripts/integration/README.md.

Environment:
  LLMGW_BASE_URL     gateway URL (default http://localhost:4000)
  LLMGW_KEY          virtual key (required)
  LLMGW_MODEL        chat alias that supports tool calling (required)
  LLMGW_EMBED_MODEL  embedding alias (optional; embedding tests are skipped if unset)
  LLMGW_TIMEOUT      per-request timeout in seconds (default 600)
"""
import json
import os
import sys
import time
import traceback

BASE = os.environ.get("LLMGW_BASE_URL", "http://localhost:4000").rstrip("/")
KEY = os.environ.get("LLMGW_KEY", "")
MODEL = os.environ.get("LLMGW_MODEL", "")
EMBED = os.environ.get("LLMGW_EMBED_MODEL", "")
TIMEOUT = float(os.environ.get("LLMGW_TIMEOUT", "600"))

if not KEY or not MODEL:
    sys.exit("set LLMGW_KEY and LLMGW_MODEL")

WEATHER_SCHEMA = {
    "type": "object",
    "properties": {"city": {"type": "string", "description": "City name"}},
    "required": ["city"],
}
TESTS = []
results = []


def check(name):
    def deco(fn):
        def run():
            t0 = time.time()
            try:
                detail = fn() or ""
                results.append((name, True))
                print(f"PASS {name} ({time.time() - t0:.1f}s) {detail}", flush=True)
            except Exception as e:  # noqa: BLE001
                results.append((name, False))
                print(f"FAIL {name}: {e!r}", flush=True)
                traceback.print_exc()
        TESTS.append(run)
        return run
    return deco


# ---------------------------------------------------------------- OpenAI SDK
import openai  # noqa: E402

oa = openai.OpenAI(base_url=BASE + "/v1", api_key=KEY, timeout=TIMEOUT, max_retries=0)
OA_TOOLS = [{"type": "function", "function": {"name": "get_weather", "description": "Get weather", "parameters": WEATHER_SCHEMA}}]


@check("openai: models.list contains alias")
def _():
    ids = [m.id for m in oa.models.list().data]
    assert MODEL in ids, ids
    return f"{len(ids)} models"


@check("openai: chat (non-stream)")
def _():
    r = oa.chat.completions.create(model=MODEL, reasoning_effort="none", max_tokens=50,
                                   messages=[{"role": "user", "content": "Reply with exactly: pong"}])
    text = r.choices[0].message.content or ""
    assert "pong" in text.lower(), text
    assert r.usage and r.usage.completion_tokens > 0
    return repr(text[:40])


@check("openai: chat (stream, include_usage)")
def _():
    chunks, usage = [], None
    stream = oa.chat.completions.create(model=MODEL, stream=True, stream_options={"include_usage": True},
                                        reasoning_effort="none", max_tokens=60,
                                        messages=[{"role": "user", "content": "Count from 1 to 5, separated by spaces."}])
    for c in stream:
        if c.usage:
            usage = c.usage
        if c.choices and c.choices[0].delta.content:
            chunks.append(c.choices[0].delta.content)
    text = "".join(chunks)
    assert "3" in text, text
    assert usage is not None
    return f"{len(chunks)} chunks"


@check("openai: tool call round trip")
def _():
    msgs = [{"role": "user", "content": "What is the weather in Tokyo? Use the tool."}]
    r = oa.chat.completions.create(model=MODEL, reasoning_effort="none", messages=msgs, tools=OA_TOOLS)
    tc = r.choices[0].message.tool_calls
    assert tc and tc[0].function.name == "get_weather", r.choices[0].message
    args = json.loads(tc[0].function.arguments)
    assert "tokyo" in args.get("city", "").lower(), args
    msgs += [r.choices[0].message.model_dump(exclude_none=True),
             {"role": "tool", "tool_call_id": tc[0].id, "content": "sunny, 25C"}]
    r2 = oa.chat.completions.create(model=MODEL, reasoning_effort="none", messages=msgs, tools=OA_TOOLS)
    assert "25" in (r2.choices[0].message.content or ""), r2.choices[0].message
    return "ok"


@check("openai: unknown model -> NotFoundError")
def _():
    try:
        oa.chat.completions.create(model="no-such-model", messages=[{"role": "user", "content": "x"}])
    except openai.NotFoundError:
        return "404"
    raise AssertionError("expected NotFoundError")


@check("openai: bad key -> AuthenticationError")
def _():
    bad = openai.OpenAI(base_url=BASE + "/v1", api_key="sk-gw-invalid", max_retries=0)
    try:
        bad.models.list()
    except openai.AuthenticationError:
        return "401"
    raise AssertionError("expected AuthenticationError")


if EMBED:
    @check("openai: embeddings")
    def _():
        r = oa.embeddings.create(model=EMBED, input=["hello", "world"])
        assert len(r.data) == 2 and len(r.data[0].embedding) > 10
        return f"dim={len(r.data[0].embedding)}"

# ------------------------------------------------------------- Anthropic SDK
import anthropic  # noqa: E402

an = anthropic.Anthropic(base_url=BASE, api_key=KEY, timeout=TIMEOUT, max_retries=0)
AN_TOOLS = [{"name": "get_weather", "description": "Get weather", "input_schema": WEATHER_SCHEMA}]


@check("anthropic: messages.create")
def _():
    m = an.messages.create(model=MODEL, max_tokens=50, messages=[{"role": "user", "content": "Reply with exactly: pong"}])
    text = "".join(b.text for b in m.content if b.type == "text")
    assert "pong" in text.lower(), m
    assert m.usage.output_tokens > 0
    return repr(text[:40])


@check("anthropic: messages.stream")
def _():
    with an.messages.stream(model=MODEL, max_tokens=80, system="You are terse.",
                            messages=[{"role": "user", "content": "Count from 1 to 5, separated by spaces."}]) as s:
        text = "".join(s.text_stream)
        final = s.get_final_message()
    assert "3" in text, text
    assert final.usage.output_tokens > 0
    return repr(text[:40])


@check("anthropic: tool_use round trip")
def _():
    msgs = [{"role": "user", "content": "What is the weather in Tokyo? Use the tool."}]
    m = an.messages.create(model=MODEL, max_tokens=300, tools=AN_TOOLS, messages=msgs)
    tu = [b for b in m.content if b.type == "tool_use"]
    assert m.stop_reason == "tool_use" and tu and tu[0].name == "get_weather", m
    msgs += [{"role": "assistant", "content": [b.model_dump(exclude_none=True) for b in m.content]},
             {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tu[0].id, "content": "sunny, 25C"}]}]
    m2 = an.messages.create(model=MODEL, max_tokens=300, tools=AN_TOOLS, messages=msgs)
    text = "".join(b.text for b in m2.content if b.type == "text")
    assert "25" in text, m2
    return "ok"


@check("anthropic: count_tokens")
def _():
    r = an.messages.count_tokens(model=MODEL, messages=[{"role": "user", "content": "hello world"}])
    assert r.input_tokens > 0
    return f"{r.input_tokens} tokens"


@check("anthropic: unknown model -> NotFoundError")
def _():
    try:
        an.messages.create(model="no-such-model", max_tokens=5, messages=[{"role": "user", "content": "x"}])
    except anthropic.NotFoundError:
        return "404"
    raise AssertionError("expected NotFoundError")

# ---------------------------------------------------------------- Ollama SDK
import ollama  # noqa: E402

ol = ollama.Client(host=BASE, headers={"Authorization": "Bearer " + KEY}, timeout=TIMEOUT)


@check("ollama: list")
def _():
    names = [m.model for m in ol.list().models]
    want = MODEL if ":" in MODEL else MODEL + ":latest"
    assert want in names, names
    return f"{len(names)} models"


@check("ollama: show")
def _():
    r = ol.show(MODEL)
    return f"capabilities={getattr(r, 'capabilities', None)}"


@check("ollama: chat (non-stream)")
def _():
    r = ol.chat(model=MODEL, think=False, messages=[{"role": "user", "content": "Reply with exactly: pong"}])
    assert "pong" in r.message.content.lower(), r
    assert r.done and r.eval_count
    return repr(r.message.content[:40])


@check("ollama: chat (stream)")
def _():
    parts = [c.message.content for c in ol.chat(model=MODEL, think=False, stream=True,
                                                 messages=[{"role": "user", "content": "Count from 1 to 5, separated by spaces."}])]
    text = "".join(parts)
    assert "3" in text, text
    return f"{len(parts)} chunks"


def get_weather(city: str) -> str:
    """Get weather for a city.

    Args:
      city: City name
    """
    return "sunny"


@check("ollama: tool call")
def _():
    r = ol.chat(model=MODEL, think=False, tools=[get_weather],
                messages=[{"role": "user", "content": "What is the weather in Tokyo? Use the tool."}])
    tc = r.message.tool_calls
    assert tc and tc[0].function.name == "get_weather", r.message
    return f"args={dict(tc[0].function.arguments)}"


@check("ollama: generate")
def _():
    r = ol.generate(model=MODEL, prompt="Reply with exactly: pong", think=False)
    assert "pong" in r.response.lower(), r
    return repr(r.response[:40])


@check("ollama: pull is blocked (403)")
def _():
    try:
        ol.pull("llama3")
    except ollama.ResponseError as e:
        assert e.status_code == 403, e
        return "403"
    raise AssertionError("expected 403")


if EMBED:
    @check("ollama: embed")
    def _():
        r = ol.embed(model=EMBED, input=["hello", "world"])
        assert len(r.embeddings) == 2
        return f"dim={len(r.embeddings[0])}"


if __name__ == "__main__":
    print(f"gateway={BASE} model={MODEL} embed={EMBED or '-'}", flush=True)
    for t in TESTS:
        t()
    failed = [n for n, ok in results if not ok]
    print(f"\n{len(results) - len(failed)}/{len(results)} passed")
    for n in failed:
        print("  failed:", n)
    sys.exit(1 if failed else 0)
