# Codex Desktop / codex-cli dual provider

Point Codex's **built-in `openai` provider** at this proxy and switch between
ChatGPT models and Grok models by changing only the model slug. No ChatGPT.app
ASAR patch, no custom `model_provider`, no sticky-provider surgery.

| Model slug | Upstream | Credential |
|---|---|---|
| `gpt-5.6-sol`, other Codex slugs | `chatgpt.com/backend-api/codex` | Codex OAuth (`--codex-login`), or the client's own token via passthrough |
| `grok-4.5`, `grok-4.3`, … | `cli-chat-proxy.grok.com` | xAI OAuth (`--xai-login`) |

Routing is by model slug through the normal credential registry — nothing
Codex-specific is hard-coded.

## 1. Authenticate

```bash
go build -o cli-proxy-api ./cmd/server

./cli-proxy-api --codex-login   # ChatGPT account
./cli-proxy-api --xai-login     # SuperGrok / Grok Build account
```

Credentials land in `auth-dir` (default `~/.cli-proxy-api`). Either arm works on
its own; add both for the dual provider.

## 2. Configure the proxy

```yaml
port: 8317
auth-dir: "~/.cli-proxy-api"

# Codex Desktop sends its own ChatGPT bearer, which is not a proxy API key.
# Leaving api-keys empty disables inbound auth entirely, so that bearer is
# simply ignored. Keep the listener on localhost when you do this.
api-keys: []
ws-auth: false

routing:
  # Keep a multi-turn conversation pinned to one credential.
  session-affinity: true
```

Then run it:

```bash
./cli-proxy-api --config config.yaml
```

## 3. Point Codex at it

In `~/.codex/config.toml`, at the root (before any `[table]`):

```toml
openai_base_url = "http://127.0.0.1:8317/v1"
# leave model_provider unset — the built-in `openai` provider is what we hijack
```

Codex's built-in provider prefers the Responses **WebSocket** transport and
falls back to HTTP POST. Both are served:

- `GET  /v1/responses`  → WebSocket upgrade
- `POST /v1/responses`  → SSE
- The same trio is also mounted under `/backend-api/codex` for clients that
  expect the native ChatGPT path.

Grok slugs appear in the Codex model list automatically: any model in the
registry that has no explicit entry in
`internal/registry/models/codex_client_models.json` is published using the
default client template.

## 4. Verify

```bash
curl -s localhost:8317/v1/models | jq -r '.models[].slug' | grep -i grok

codex -m grok-4.5   exec --skip-git-repo-check "Reply with exactly: pong"
codex -m gpt-5.6-sol exec --skip-git-repo-check "Reply with exactly: pong"
```

Then relaunch ChatGPT.app and run one turn on each arm to exercise the
WebSocket path.

## Optional: pass the client's own ChatGPT token upstream

By default the Codex arm uses the credential stored by `--codex-login`. Set the
flag below to forward the **calling client's** ChatGPT OAuth bearer instead, so
ChatGPT usage is billed to whichever account Codex Desktop is signed in as:

```yaml
codex:
  passthrough-client-token: true
```

Behaviour:

- Only **JWT-shaped** bearers are forwarded. A value configured in `api-keys` is
  also presented as a bearer, and would never be mistaken for a ChatGPT token.
- Passthrough applies **only** to the Codex executors. xAI requests always use
  the xAI credential.
- When it fires, the request is treated as OAuth even if the selected credential
  is an api-key entry: `Originator` is set and the client's
  `Chatgpt-Account-Id` wins over the stored credential's.
- Credential selection still needs *some* Codex credential to select. To run
  passthrough without storing a ChatGPT login, declare a placeholder:

  ```yaml
  codex-api-key:
    - api-key: "passthrough-placeholder"
      base-url: "https://chatgpt.com/backend-api/codex"
  codex:
    passthrough-client-token: true
  ```

  The placeholder's key is never sent; the client's bearer replaces it.
  `base-url` is **required**: `SanitizeCodexKeys` drops entries without one, and
  it does so silently — the symptom is no `gpt-*` models in `/v1/models`.

## Reasoning effort

The proxy forwards `reasoning.effort` as the client sends it; it only clamps a
level that the model's registry entry does not list. The single source of truth
for what actually went upstream is the proxy's own debug line (`debug: true`):

```
thinking: original config from request | provider=codex model=gpt-5.6-sol mode=level budget=0 level=max
thinking: processed config to apply    | provider=codex model=gpt-5.6-sol mode=level budget=0 level=max
```

Three Codex behaviours regularly look like a proxy bug and are not:

- **Ultra is not an effort on the wire.** Selecting Ultra sends
  `reasoning.effort: "max"` and expresses the rest as client-side multi-agent
  delegation, so the proxy only ever sees `max`. This matters because
  `/v1/models?client_version=…` *does* advertise `ultra` while the registry does
  not accept it — a deliberate split (upstream PR #4160 kept it in the catalog,
  and issue #4463 was closed as intended: for Codex `ultra` is a working mode,
  not a reasoning level). The mismatch is unreachable from Codex; it only
  affects clients that consume the shared catalog literally.
- **Effort is bound at turn start**, and every spawned subagent thread keeps its
  own copy. Changing it mid-turn does not affect the running turn or its live
  subagents, so the proxy keeps logging the old level until the next turn.
- **The composer cannot display `max`.** Codex Desktop's model controls default
  to `[low, medium, high, xhigh, ultra]`, so a thread sitting on `max` renders
  as a neighbouring rung ("Extra High") while still sending `max`.

When a report disagrees with the proxy log, the client-side state is worth
checking before touching `internal/thinking/`:

| Where | What it holds |
|---|---|
| `~/.codex/state_5.sqlite` | `threads(model, reasoning_effort)`, `thread_spawn_edges` |
| `~/.codex/logs_2.sqlite` | span fields `codex.turn.reasoning_effort` / `codex.request.reasoning_effort`, and the `turn.id` still in flight |
| `~/.codex/sessions/<date>/rollout-*.jsonl` | `turn_context` per turn |

Those `codex.*.reasoning_effort` spans are Codex's internal turn label, **not**
the request body — an Ultra turn tags them `ultra` while sending `max`. Only the
proxy's `thinking:` lines show the field that was actually sent.

## Grok arm and deferred tools

Codex's `tool_search` tool is how the model reaches **deferred** tools — tools
that are not in the base tool list, such as the Node REPL
(`mcp__node_repl__js`) that `chrome:control-chrome` and similar skills need.

xAI rejects the hosted `tool_search` tool type, so it is forwarded as a plain
function and Grok's call is converted back into a `tool_search_call` item
(`execution: client`) that Codex Desktop's own loader resolves. Alongside that:

- `defer_loading` is stripped from every forwarded tool — Grok has no deferred
  loading, so a tool that keeps the flag stays invisible and the model
  re-searches for it forever.
- Tools the client loads are harvested out of `tool_search_output` history items
  and merged back into the request's `tools` array; they arrive nowhere else.
- Whole-number floats on integer-typed tool arguments (`timeout_ms: 1000.0`) are
  rewritten to integers, which Codex parses as `i32`/`i64`.

**Client-side caveat:** the Node REPL is additionally gated by Codex's own
config. If `~/.codex/config.toml` has `[features] js_repl = false`, Codex never
exposes the `js` tool at all and no proxy behaviour can help. Set:

```toml
[features]
js_repl = true
```

## Operational note

While `openai_base_url` is set, **all** Codex model calls go through this proxy —
stock and Grok alike. If it is down, both arms fail. To roll back, remove
`openai_base_url` and Codex talks to ChatGPT directly again.
