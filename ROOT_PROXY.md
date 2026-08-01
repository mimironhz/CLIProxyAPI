# Root Proxy

Root Proxy is the lightweight, ChatGPT Desktop-facing routing boundary. It is
separate from the full CLIProxyAPI server (the Relay) and does not run provider
translation, thinking, tool-rewrite, retry, or credential-selection pipelines.

This milestone implements the Desktop Responses WebSocket controller plus its
HTTP/SSE and compaction fallback paths. Production configuration defaults to
HTTP/SSE; the completed first-message WebSocket controller remains available as
an explicit experimental opt-in:

```text
ChatGPT Desktop
  -> Root 127.0.0.1:8317
     -> stock model -> chatgpt.com/backend-api/codex
     -> relay model -> 127.0.0.1:8318/v1
```

The client begins a turn with `response.create`, matching the documented
[Responses WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode).
Root reads one bounded complete application message, selects an exact route
from disjoint model sets, and establishes one upstream connection. A later
`response.create` can change arms after the prior response reaches a terminal
event. Relay provider identity is part of that logical target: an xAI-to-Kimi
change also waits for the terminal event and opens a fresh Relay connection,
while two models explicitly mapped to the same provider may reuse it. A
self-contained request is handed off without closing the Desktop socket. A
cross-provider request that references provider-local response or conversation
state receives an in-band `previous_response_not_found` error so Desktop can
replay full context without forwarding the foreign identifier.
When Desktop supplies a just-in-time attestation header, a Relay-to-stock
change closes the transport with `1012` instead; Desktop reconnects and creates
a fresh attestation for the new official handshake. Ordinary conversation state
is preserved by the client's full replay in either case.
The number of upgraded sockets that have not established an upstream route is
capped, including sockets waiting for their first message and sockets dialing
an upstream. When the cap is reached, the oldest unestablished socket is closed
with `1013` and its dial is canceled so stale clients or stalled handshakes
cannot exhaust the pool indefinitely. Root does not add read, write, idle, or
upstream-handshake deadlines.

HTTP `POST /responses` accepts only explicit `stream: true` and relays SSE bytes
without parsing or reframing them. Root bounds both the raw request and the
decoded inspection copy. It supports identity and zstd request encoding; an
unchanged zstd Responses request is forwarded in its original compressed byte
representation. `/responses/compact` is unary JSON, rejects streaming, and
returns the upstream body unchanged. Before any stock HTTP request, Root removes
only non-portable third-party reasoning state; it fails closed rather than
discard foreign compaction state and lose conversation history. Every Relay
compaction item is attributed from its provider-specific wire representation
and must match the exact provider configured for the target model. Same-provider
xAI or Kimi compaction continues; GPT, unknown, malformed, mixed, or
cross-provider compaction fails closed. Relay compaction creation also fails
closed when its model has no provider mapping. Consequently, ordinary model
changes are recoverable, but changing providers after opaque compaction requires
a new conversation chain. When a request carries both opaque compaction and a
provider-local response ID, the compaction portability error takes precedence
because replaying ordinary history cannot make that opaque state portable.
This attribution validates the known Kimi marker or Grok transport shape; it
does not cryptographically prove that an opaque blob is decryptable upstream.
An unusually large thread may still require a real context-window downshift
compaction before changing to a smaller-context provider. That compaction is
not portable and remains fail-closed; start a new chain for that boundary.

## Model discovery

`GET /v1/models` and `GET /backend-api/codex/models` return the complete Codex
`{"models":[...]}` catalog shape expected by the installed Desktop client. Root
synthesizes the response once at startup from the exact configured allowlists
and the repository's validated Codex metadata templates; it does not query
Relay or expose the Relay key. Stock models retain their curated metadata.
Relay models use conservative metadata, expose no speed tier, and advertise
hosted search only when their configured `xai`, `kimi` or `deepseek` provider
supports the Relay shim. They deliberately omit the optional OpenAI `comp_hash`: inheriting
the fallback GPT template's hash would make Codex run an old-model compaction
on every stock-to-Relay switch and then send that provider-bound opaque state
to Relay. With no asserted compatibility hash, ordinary model changes replay
the portable full history and Root removes only provider-local reasoning state.

Configuration order is authoritative: all stock entries precede Relay entries,
and the first stock entry becomes Desktop's default. In `http-fallback` mode,
every entry advertises `prefer_websockets: false`; explicit `first-message` mode
advertises `true`. The response has a deterministic strong `ETag` and supports
`If-None-Match`. The only accepted query parameter is the optional, single
`client_version` value sent by Codex.

This catalog is an allowlist, not live provider-availability evidence. Root does
not infer quota or authentication health from Relay `/v1/models`, and it never
substitutes a different model when the exact Desktop-requested model is
unavailable. Eligibility and fallback among task-execution candidates belong to
the caller that owns those candidates; inference dispatch remains exact and
single-attempt.

This matches the source tag for installed `codex-cli 0.146.0-alpha.3.1`:

- [the client requests `<base>/models?client_version=...` and reads `ETag`](https://github.com/openai/codex/blob/ff75c5b939c477c49eb1bd5248da6dab71b109d1/codex-rs/codex-api/src/endpoint/models.rs#L31-L78)
- [a non-empty ChatGPT catalog becomes the model source of truth](https://github.com/openai/codex/blob/ff75c5b939c477c49eb1bd5248da6dab71b109d1/codex-rs/models-manager/src/manager.rs#L394-L435)
- [the decoded Codex `ModelInfo` contract](https://github.com/openai/codex/blob/ff75c5b939c477c49eb1bd5248da6dab71b109d1/codex-rs/protocol/src/openai_models.rs#L368-L451)
- [a missing `comp_hash` does not force hash-change compaction](https://github.com/openai/codex/blob/ff75c5b939c477c49eb1bd5248da6dab71b109d1/codex-rs/core/src/session/turn.rs#L853-L859)
- [required model-switch compaction runs with the previous model](https://github.com/openai/codex/blob/ff75c5b939c477c49eb1bd5248da6dab71b109d1/codex-rs/core/src/session/turn.rs#L883-L974)
- [the resume-and-switch contract compacts with the old model before the new-model turn](https://github.com/openai/codex/blob/ff75c5b939c477c49eb1bd5248da6dab71b109d1/codex-rs/core/tests/suite/compact.rs#L3088-L3205)

## Native logging

Root has three independent native log surfaces under `logging`:

- `logging-to-file` writes structured application and debug events to
  `root.log`. The top-level `debug` setting controls only the application log
  level.
- `request-access-log` writes one metadata-only `root.access.v1` JSON record to
  `access.ndjson` for every HTTP request or WebSocket session, including health,
  model discovery, validation failures, HTTP `426`, Relay traffic, and unknown
  paths. Records include a Root-generated request ID, method, path without query
  values, status, duration, byte counts, transport, route, and model when known.
- `stock-request-response-log` writes exact stock traffic to
  `stock-traffic.ndjson`. `stock-payload-format: base64`, the backward-compatible
  default, emits `root.stock-traffic.v1`. `stock-payload-format: auto` emits
  `root.stock-traffic.v2`: unencoded valid UTF-8 uses `payload_text`, while zstd,
  binary WebSocket, invalid UTF-8, or a read chunk split inside a UTF-8 rune uses
  `payload_base64`. It begins capture only after a model is classified as stock.
  For HTTP it records the decoded inspection request; for WebSocket it records
  the exact received application message. Both transports also record the
  normalized bytes attempted on the official arm and the exact response bytes
  Root reads. A stock request rejected after classification also records the
  exact Root-generated JSON error. Failed official handshakes remain visible as
  incomplete exchanges.
  WebSocket frames include their opcode and are split into one exchange per
  stock turn. Relay
  request and response bodies are never sent to this logger, including during a
  stock-to-Relay-to-stock handoff.

`payload_text` is a JSON string: quotes, controls, and newlines are escaped on
disk so each event remains one valid JSONL record. Inspect it with
`jq '.payload_text'`, which keeps control bytes escaped. Do not send untrusted
payloads directly to a terminal with `jq -r`; use raw output only when redirecting
an exact reconstruction to a private file. Decoding either payload field returns
the exact original bytes covered by the recorded hashes. Base64 parts are at
most 256 KiB; readable parts are at most 128 KiB and split only at UTF-8 rune
boundaries. The smaller
text bound accounts for worst-case JSON escaping under the supported 1 MiB
minimum rotation size. Every part carries `payload_id`, `payload_part`,
`payload_parts`, `payload_offset`, the whole-payload size/hash, and its own
size/hash. A large payload can cross rotated files. Merge compressed backups,
uncompressed backups, and the active file before reconstructing one
`payload_id`. Reconstruction must fail closed unless there is exactly one record
for every ordinal from 1 through `payload_parts`, offsets are contiguous from
zero, each decoded part matches `payload_part_bytes` and
`payload_part_sha256`, the total matches `payload_bytes`, and the joined bytes
match `payload_sha256`. Never use a plain sort-and-concatenate pipeline: retention
can remove a part, and conflicting duplicate ordinals must be treated as
corruption rather than silently selected.

An existing active file can contain v1 records followed by v2 records after a
Root-only upgrade. Consumers must branch on each record's `schema` and
`payload_encoding`; Root never changes the representation within one payload.

An exchange end record separates upstream outcome from capture integrity:
`outcome` can be `completed`, `rejected`, `failed`, `incomplete`, or
`canceled` (plus `reconnect_required` for a fresh-attestation handoff), while
`capture_complete` is true only after an upstream terminal or a definitive
Root-generated response was captured and every prior log write succeeded. A write failure latches
`capture_error` and can never produce a false complete marker.
`capture_complete` is a write-time statement, not a permanent-retention claim:
the configured age, backup-count, or total-size policy can later remove old
parts. The Desktop controller normally has one outstanding turn; if an
experimental WebSocket client pipelines stock turns, response frames are
attributed to the oldest outstanding exchange.

Root never records request or response headers in access or traffic logs. This
keeps OAuth values, the Relay CPA key, cookies, attestation, and account headers
out of the native log schema. Exact stock bodies necessarily contain prompts,
tool calls and results, opaque reasoning/compaction state, and any secrets the
user placed in message content. Traffic capture is therefore explicit opt-in
and must be handled as sensitive conversation data.

The configured directory must be a real private directory, not a symlink. Root
creates it as `0700`, precreates active files as `0600`, rotates each stream by
`max-file-size-mb`, and enforces backup age/count plus a total Root-owned size
cap. `max-total-size-mb` must be at least `max-file-size-mb` multiplied by the
number of enabled active files, so the protected current files cannot exceed
the total cap by configuration. Access or traffic logging cannot be enabled
unless `logging-to-file` is also enabled. Initialization and unsafe permissions
fail startup. A later disk
write failure is logged once and inference continues without retrying or
changing either upstream arm. Use an absolute runtime directory outside an
immutable cutover bundle so normal log writes do not invalidate its manifest;
launchd stdout/stderr should remain as bootstrap-failure fallbacks.

## Security boundary

- Root and Relay must both use loopback listeners. Only Relay should be exposed
  through the OrbStack bridge, on port `8318`.
- The official ChatGPT OAuth-backed destination is compiled in and cannot be
  redirected through configuration.
- The Relay URL must be loopback, must use an explicit port, and cannot point
  back to Root's port.
- Root rejects an inbound hop marker and adds one on the Relay hop. This is
  defense in depth, not the sole loop guard: cutover must also verify that no
  Relay provider base URL points back to Root.
- `routing.relay-model-providers` accepts exact `xai`, `kimi` and `deepseek`
  family names.
  It is backward compatible for ordinary requests, but an unclassified Relay
  model cannot create or consume compaction, and distinct unclassified model
  names are treated as separate logical targets.
- The Relay CPA key is loaded only from the environment variable named by
  `relay.api-key-env`; inline keys are rejected as unknown configuration.
- Every inference route requires one inbound Desktop Bearer value. Stock
  requests preserve it. Relay requests validate and then discard it, along with account IDs,
  cookies, API-key variants, organization, project, proxy, Origin, and
  WebSocket negotiation headers, before injecting only the Relay CPA key.
- Model discovery also requires one Desktop Bearer value and applies the shared
  Origin policy, but never forwards credentials to either upstream.
- Root validates Bearer syntax but does not cryptographically authenticate the
  ChatGPT token. Loopback binding and the Origin policy remain the actual local
  trust boundary.
- Both upstream arms start from a positive metadata-header allowlist. Headers
  and credentials are never logged. Exact stock bodies are recorded only when
  the explicitly sensitive `stock-request-response-log` option is enabled;
  Relay bodies are never recorded by Root.
- Managed residency, FedRAMP, and attestation headers are preserved only on the
  official arm. Attestation and all Desktop account credentials are stripped
  from Relay requests.
- Browser Origins are denied unless explicitly listed. Requests without an
  Origin, as produced by native clients, are accepted.
- HTTP upstream requests are attempted exactly once. Root does not retry, follow
  redirects, or fall back to the other arm. Relay HTTP bypasses environment
  proxies; the protected official host uses the repository's Chrome-like uTLS
  transport and the environment's HTTPS proxy policy. Unsupported or malformed
  proxy URLs fail Root startup rather than falling back to a direct connection.
- Root retains only Cloudflare infrastructure cookies for official ChatGPT
  HTTP and WebSocket requests. It never stores or forwards ChatGPT account or
  session cookies.
- Official HTTP inherits the environment HTTPS proxy and the host's system trust
  store. A CA configured only inside Desktop is not visible to Root and needs a
  separate Root configuration before that environment can cut over.

Root disables `permessage-deflate` on both WebSocket legs. Its transparency
contract is therefore application-message payload bytes, type, ordering,
streaming, cancellation, and legal close semantics—not raw frame identity.

## WebSocket cutover policy

`websocket.mode: http-fallback` is the production-safe default. Root answers a
Responses WebSocket `GET` with HTTP `426` before upgrading or dialing either
upstream. The installed ChatGPT Desktop client treats that exact status as an
immediate, session-sticky switch to streaming HTTP Responses, so the model in
the POST body is available before Root selects an upstream. The selected
upstream's HTTP status and response metadata then flow through the ordinary
HTTP/SSE path.

This policy is necessary because the installed client does not reveal the
model in its WebSocket upgrade request. Its handshake contains authentication,
attestation, session/thread identity, compatibility, and beta headers, but the
model is first serialized in the later `response.create` application message.
The client can also preconnect without sending any request frame. Delaying the
downstream upgrade cannot solve that ordering: a conforming WebSocket client
waits for the server's `101` before sending application data.

The behavior is verified against the source tag matching the installed
`codex-cli 0.146.0-alpha.3.1` binary:

- [handshake header construction has no model](https://github.com/openai/codex/blob/ff75c5b939c477c49eb1bd5248da6dab71b109d1/codex-rs/core/src/client.rs#L1061-L1093)
- [preconnect opens a socket before a request exists](https://github.com/openai/codex/blob/ff75c5b939c477c49eb1bd5248da6dab71b109d1/codex-rs/core/src/client.rs#L1240-L1280)
- [HTTP 426 activates sticky HTTP fallback](https://github.com/openai/codex/blob/ff75c5b939c477c49eb1bd5248da6dab71b109d1/codex-rs/core/tests/suite/websocket_fallback.rs#L29-L78)

`websocket.mode: first-message` explicitly opts into the turn-aware WebSocket
controller. It preserves application-message bytes, turn boundaries, and
cross-provider handoff behavior, but it cannot retroactively replay an
upstream handshake status or attach successful upgrade-only
`X-Reasoning-Included`, `X-Models-Etag`, or `OpenAI-Model` headers to an already
upgraded downstream socket. Keep this mode experimental for stock models until
Desktop supplies a trusted pre-upgrade model/route hint, uses a separate stock
endpoint, or defines equivalent post-upgrade metadata events.

## Run locally

Copy the example outside source control, set the Relay key, then start Root:

```bash
install -d -m 700 "$HOME/.config/cli-proxy-api"
cp config.root-proxy.example.yaml "$HOME/.config/cli-proxy-api/root-proxy.yaml"
export CPA_RELAY_API_KEY='replace-with-a-configured-relay-key'
go run ./cmd/root-proxy --config "$HOME/.config/cli-proxy-api/root-proxy.yaml"
```

If the key is stored in the repository working directory's `.env`, that file
must be a regular file with mode `0600`. Root's YAML contains only the name of
the environment variable, not the key itself.

The example enables all three native files. If request/response capture is not
needed, set `stock-request-response-log: false`; leave the access log enabled
for cutover and operational diagnosis.

Available endpoints in this milestone:

- `GET /healthz`
- `GET /v1/responses` and `/backend-api/codex/responses`: HTTP `426` in the
  default `http-fallback` mode; WebSocket upgrade in explicit `first-message`
  mode
- `POST /v1/responses` and `/backend-api/codex/responses` with `stream: true`
- `POST /v1/responses/compact` and `/backend-api/codex/responses/compact`
- `GET` or `HEAD /v1/models` and `/backend-api/codex/models`, with an optional
  single `client_version` query parameter

Native non-streaming HTTP Responses remain deliberately deferred. Desktop uses
the implemented streaming HTTP/SSE path after the cutover-safe `426` fallback.
