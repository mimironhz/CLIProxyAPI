# Root / Relay cutover

This runbook switches ChatGPT Desktop from the monolithic CPA listener to the
split topology without changing Desktop's configured base URL:

```text
ChatGPT Desktop -> Root 127.0.0.1:8317
                         |-- stock -> chatgpt.com/backend-api/codex
                         `-- third party -> authenticated CPA Relay 127.0.0.1:8318

OrbStack clients -> 192.168.139.3:8318 -> CPA Relay 127.0.0.1:8318
```

The live `192.168.139.3:8317` bridge must be stopped before Root starts. If it
remains active, it exposes the Desktop bearer-validation boundary to OrbStack.

## Cutover candidate

Use the exact private deployment directory recorded in the handoff manifest.
Verify every SHA-256 in `manifest.sha256` before loading a job. Do not rebuild
or edit an artifact in place; create a new versioned directory instead.

### Current handoff: delegated payload compatibility

Frozen but inactive Root + Relay candidate:

```text
/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260810T055724Z-plaintext-delegation-safe
```

Scope:

- Restart `com.user.cliproxy-relay` and `com.user.cliproxy-root` together.
- Do not touch `com.user.cliproxy-orbstack-relay-v2`.
- Roll back Root to `20260805T052624Z-b6ff2fbc`.
- Roll back Relay to `20260810T034432Z-398f082c`.
- Root and Relay configuration files are carried forward byte-for-byte.

Change:

- Root maps the reserved `collaboration` namespace to
  `collaboration-optimize` before removing the `message.encrypted` marker from
  `spawn_agent`, `followup_task`, and `send_message`, then restores the original
  namespace on official HTTP/SSE and WebSocket responses. This avoids official
  reserved-schema validation while keeping Desktop's tool names unchanged.
- Stock-only multi-agent traffic, unrelated message tools, tool descriptions,
  model lists, and opaque tool arguments are unchanged.
- xAI and DeepSeek convert Codex-only `agent_message` into a standard user
  message without enabling the optional broad multi-agent optimization.
- Codex Desktop can still label a plaintext `<codex_delegation>` task envelope
  as `encrypted_content` after the parent tool call. DeepSeek and xAI promote
  only a structurally verified envelope with a UUID source thread and non-empty
  input into model-visible `input_text`. Opaque base64, arbitrary plaintext,
  malformed envelopes, invalid thread IDs, and unrelated encrypted parts are
  removed.

This pairing is required. Relay alone cannot recover an already sealed task,
and Root alone would still send `agent_message` to an incompatible worker.

### Root + Relay activation

Run only from an external Terminal after this preparing task has completed.
Never invoke `launchctl bootout` for Root from a Codex task routed through Root:
that necessarily disconnects the task's own `/v1/responses` stream. The bundled
helper waits for launchd's asynchronous removal transaction, retries transient
bootstrap error 5, and automatically restores both rollback bundles if either
candidate service fails health. Choose a quiet interval with no active
delegated turns.

```bash
CANDIDATE=/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260810T055724Z-plaintext-delegation-safe
ROOT_ROLLBACK=/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260805T052624Z-b6ff2fbc
RELAY_ROLLBACK=/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260810T034432Z-398f082c
```

1. Verify all immutable artifacts:

   ```bash
   (cd "$CANDIDATE" && shasum -a 256 -c manifest.sha256)
   (cd "$ROOT_ROLLBACK" && shasum -a 256 -c manifest.sha256)
   (cd "$RELAY_ROLLBACK" && shasum -a 256 -c manifest.sha256)
   ```

2. Preflight the external activation helper. It validates all three manifests,
   requires the expected rollback baseline, and does not change services:

   ```bash
   "$CANDIDATE/activation/activate.zsh" --preflight
   ```

3. Run the helper from that external Terminal. It activates and health-checks
   Relay first, then Root, and leaves the bridge untouched:

   ```bash
   "$CANDIDATE/activation/activate.zsh" --activate
   ```

4. Verify the installed job targets, both loopback health endpoints, and the
   unchanged bridge PID:

   ```bash
   test "$(plutil -extract ProgramArguments.0 raw -o - "$HOME/Library/LaunchAgents/com.user.cliproxy-root.plist")" = "$CANDIDATE/bin/root-proxy"
   test "$(plutil -extract ProgramArguments.0 raw -o - "$HOME/Library/LaunchAgents/com.user.cliproxy-relay.plist")" = "$CANDIDATE/bin/cli-proxy-api-relay"
   curl -fsS http://127.0.0.1:8317/healthz | jq -e '.status == "ok"'
   curl -fsS http://127.0.0.1:8318/healthz | jq -e '.status == "ok"'
   test "$(lsof -nP -iTCP@192.168.139.3:8318 -sTCP:LISTEN -t)" = "$BRIDGE_PID"
   ```

5. From Codex Desktop, create a fresh `deepseek-v4-flash/max` subagent with a
   unique sentinel in a long task body. Require the worker to repeat that
   sentinel, then send a follow-up with a second sentinel and require that one.
   Repeat a short delegated probe with `grok-4.5/high`; require completion with
   no HTTP 422. Do not accept a generic acknowledgement as payload proof.

6. If either probe fails, roll back both jobs from the same external Terminal:

   ```bash
   "$CANDIDATE/activation/activate.zsh" --rollback
   ```

### Rejected candidate: semantic payload loss

The candidate below passed HTTP health and official reserved-schema validation,
but a real DeepSeek worker received only the short `Payload:` envelope and
returned `PAYLOAD_NOT_VISIBLE`. The full task remained in a plaintext
`<codex_delegation>` part mislabeled as `encrypted_content`. It was rolled back
on 2026-08-10 and must not be reused:

```text
/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260810T051259Z-reserved-schema-safe
```

### Rejected candidate: do not activate

The candidate below was activated briefly, rejected by the official upstream,
and rolled back on 2026-08-10. It must not be reused:

```text
/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260810T044233Z-delegation-compat
```

It removed `message.encrypted` in place from the reserved `collaboration`
namespace. Official Codex returned HTTP 400 with `Function
'collaboration.followup_task' is reserved for use by this model and must match
the configured schema.` Ten consecutive official requests failed with the same
249-byte response; after paired rollback, official requests returned HTTP 200.

### Previous live handoff (Relay-only): xAI `agent_message` compatibility

Frozen Relay-only candidate:

```text
/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260810T034432Z-398f082c
```

Scope:

- **Restart only** `com.user.cliproxy-relay`
- **Do not touch** `com.user.cliproxy-root` or `com.user.cliproxy-orbstack-relay-v2`
- Live Root remains `20260805T052624Z-b6ff2fbc` (multi-agent v2 advertisement)
- Rollback Relay is `20260807T031713Z-90e19091` (previous live Relay)

Change:

- Codex delegated turns may contain the Codex-only `agent_message` input item
- For Grok/xAI upstream only, the proxy converts it to a user `message`
- Nested `encrypted_content` parts become ordinary `input_text`
- Broader multi-agent optimization remains optional
- `relay.yaml` is carried forward unchanged from the previous live Relay bundle

Defect this fixes:

Grok's strict Responses `ModelInput` decoder rejects `agent_message`, returning
HTTP 422 before inference. The existing compatibility rewrite was gated behind
`codex.optimize-multi-agent-v2`, so delegated Grok turns failed whenever that
optional setting was disabled.

### Previous Relay-only activation (already completed)

Restart only Relay after verifying candidate hashes. Preserve Root and bridge
process identity throughout.

```bash
RELAY_CANDIDATE=/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260810T034432Z-398f082c
RELAY_ROLLBACK=/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260807T031713Z-90e19091
ROOT_LIVE=/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260805T052624Z-b6ff2fbc
```

1. Verify the candidate and immutable references:

   ```bash
   (cd "$RELAY_CANDIDATE" && shasum -a 256 -c manifest.sha256)
   (cd "$RELAY_ROLLBACK" && shasum -a 256 -c manifest.sha256)
   # Root is not restarted; confirm the live Root binary still matches the pin.
   shasum -a 256 "$ROOT_LIVE/bin/root-proxy" "$RELAY_CANDIDATE/bin/root-proxy"
   ```

   Expect the two `root-proxy` hashes to be identical
   (`b6ff2fbcb5473773f7d3eb0507049ce343a6624af9eaecc64b45b00ab35c46b0`).

2. Record pre-cutover PIDs and listeners. Require Root on `127.0.0.1:8317`, Relay
   on `127.0.0.1:8318`, and the only bridge on `192.168.139.3:8318`:

   ```bash
   lsof -nP -iTCP@127.0.0.1:8317 -sTCP:LISTEN
   lsof -nP -iTCP@127.0.0.1:8318 -sTCP:LISTEN
   lsof -nP -iTCP@192.168.139.3:8318 -sTCP:LISTEN
   ps -p "$(lsof -nP -iTCP@127.0.0.1:8317 -sTCP:LISTEN -t)" -o pid=,command=
   ps -p "$(lsof -nP -iTCP@127.0.0.1:8318 -sTCP:LISTEN -t)" -o pid=,command=
   ```

3. Replace only Relay:

   ```bash
   ROOT_PID=$(lsof -nP -iTCP@127.0.0.1:8317 -sTCP:LISTEN -t)
   BRIDGE_PID=$(lsof -nP -iTCP@192.168.139.3:8318 -sTCP:LISTEN -t)
   launchctl bootout "gui/$(id -u)/com.user.cliproxy-relay"
   test -z "$(lsof -nP -iTCP@127.0.0.1:8318 -sTCP:LISTEN)"
   install -m 600 "$RELAY_CANDIDATE/launchd/com.user.cliproxy-relay.plist" \
     "$HOME/Library/LaunchAgents/com.user.cliproxy-relay.plist"
   launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.user.cliproxy-relay.plist"
   ```

4. Verify identity and topology:

   ```bash
   # New Relay binary/config
   shasum -a 256 "$(plutil -extract ProgramArguments.0 raw -o - "$HOME/Library/LaunchAgents/com.user.cliproxy-relay.plist")"
   # Expect: 2d97b07d2643ec8bb7d78f502f5ffc012e475ec476a331ef86da1ce922336602

   # Root + bridge PIDs must be unchanged
   test "$(lsof -nP -iTCP@127.0.0.1:8317 -sTCP:LISTEN -t)" = "$ROOT_PID"
   test "$(lsof -nP -iTCP@192.168.139.3:8318 -sTCP:LISTEN -t)" = "$BRIDGE_PID"

   # Catalog through Root (Desktop path)
   curl -fsS -H 'Authorization: Bearer desktop-preflight' \
     'http://127.0.0.1:8317/v1/models?client_version=0.146.0' \
     | jq -r '.models[].slug'

   # Direct Relay smoke: grok-4.5 must complete
   set -a; . "$ROOT_LIVE/root/.env"; set +a
   curl -fsSN -H "Authorization: Bearer $CPA_RELAY_API_KEY" \
     -H 'Content-Type: application/json' \
     --data '{"model":"grok-4.5","stream":true,"input":"Reply exactly RELAY_READY_OK."}' \
     http://127.0.0.1:8318/v1/responses | rg -q '"type":"response.completed"'
   ```

5. Delegation acceptance:

   - Send a `grok-4.5` delegated turn containing `agent_message` through Root.
   - Require `response.completed` and no HTTP 422.
   - Check the Relay request log structurally: upstream input must contain a user
     `message` with `input_text`, never `agent_message` or nested
     `encrypted_content`.

6. If Relay verification fails, roll back Relay only:

   ```bash
   launchctl bootout "gui/$(id -u)/com.user.cliproxy-relay"
   install -m 600 "$RELAY_ROLLBACK/launchd/com.user.cliproxy-relay.plist" \
     "$HOME/Library/LaunchAgents/com.user.cliproxy-relay.plist"
   launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.user.cliproxy-relay.plist"
   ```

   Reverify the rollback Relay binary hash
   (`75bc1116f02f4a3aaef730ec3fbb37c3f8b027042c21cb0cdb9a218acd7f2d40`) and that
   Root/bridge PIDs never changed.

### Previous Root-only multi-agent v2 candidate (already live)

```text
/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260805T052624Z-b6ff2fbc
```

This Root-only bundle remains the live Root process. It is not part of the
Relay activation above; it is listed so rollback and dependency pins stay
discoverable.

### Historical Root-only readable-logging candidate

```text
/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260730T070219Z-c7a1cca9
```

Preserved for history. Do not use it as the current handoff target.

## Stable log and state layout

No mutable file belongs inside a versioned bundle. A bundle is an immutable,
hash-verified rollback artifact; anything that grows or is rewritten at runtime
must live under a bundle-independent path, or its location silently changes at
every cutover and its writes invalidate `manifest.sha256`.

Root always satisfied this through the absolute `logging.directory` in
`root.yaml`. Relay and the bridge did not, and were corrected in
`20260731T074512Z-72df0b9e`. The stable roots are now:

```text
/Users/dwolf/.local/state/cliproxyapi/root/
  bootstrap.{stdout,stderr}.log   launchd capture
  logs/                           root.log, access.ndjson, stock-traffic.ndjson
/Users/dwolf/.local/state/cliproxyapi/relay/
  bootstrap.{stdout,stderr}.log   launchd capture; the primary Relay log
  logs/                           error-api-*.log
  static/                         management assets
  auth/                           credentials
/Users/dwolf/.local/state/cliproxyapi/bridge/
  orbstack-relay-v2.log           socat capture
```

Relay has four sinks, not one, and only the first is a launchd path. Because
`logging-to-file: false`, its logrus output goes to stdout, so launchd's capture
*is* the application log. The other three follow `ResolveLogDirectory`
(`internal/logging/global_logger.go:139`) and `managementasset`, neither of
which has a config key — both derive from `util.WritablePath()`, then the
working directory, then `auth-dir`. Setting `WRITABLE_PATH` in the Relay plist
pins `logs/` and `static/` deterministically; without it they fell back to
`<auth-dir>/logs` and `<cwd>/static`, i.e. back inside the bundle. Setting
`WRITABLE_PATH` also redirects Postgres/object/git store local paths, which is
inert here because Relay uses the file store.

Two consequences. Relay's launchd capture never rotates and is no longer
implicitly reset by each cutover, so it needs a `newsyslog.d` entry before it
matters. And `auth-dir` is stable, so a credential refresh no longer reports
manifest drift against the bundle — the drift caveat in the Root-only
activation below applies to bundles staged before this change.

The production catalog intentionally contains:

- stock: `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`
- Relay: `deepseek-v4-flash`, `grok-4.5`

`deepseek-v4-pro` was removed from both `root.yaml` and `relay.yaml` on
2026-07-31 because DeepSeek's Responses API does not serve it: the request is
rejected with HTTP `400` and "Codex integration with deepseek-v4-pro will be
available starting early August 2026." Restoring it means adding the model back
to `routing.relay-models` and `routing.relay-model-providers` in `root.yaml` and
to the `deepseek` provider's `models` list in `relay.yaml` — nothing else, since
the provider already speaks the Responses API for every model it carries.

The Relay reaches DeepSeek through `api: responses` on its
`openai-compatibility` entry, so requests go to `/responses` instead of being
translated into `/chat/completions`. Three consequences are worth knowing during
verification. Reasoning arrives as the item's own `content` and is rewritten
into the `summary` form Codex renders, then sealed into `encrypted_content` as
before. A replayed reasoning item is unsealed back into `content`, which
DeepSeek does read, so the chain of thought survives a tool call. And the stream
ends on `response.completed` with no `data: [DONE]` marker, so a body that stops
earlier is reported as a truncated turn rather than an empty success.

`kimi-k3` was the original Relay entry and was removed in
`20260731T074512Z-72df0b9e`. Removal is by exclusion, not deletion:
`relay.yaml` now lists all seven registry Kimi models under
`oauth-excluded-models.kimi`, so the Kimi credential remains present but serves
nothing. Re-enabling `kimi-k3` means dropping that one exclusion entry and
restoring both `routing.relay-models` and `routing.relay-model-providers` in
`root.yaml`. Any Desktop thread still pinned to `kimi-k3` cannot be resumed
while the exclusion stands.

`grok-4.5` is live on the Relay arm via xAI OAuth. It is no longer excluded
from the production catalog. Image-generation Grok models remain excluded under
`oauth-excluded-models.xai`.

The Root config uses `websocket.mode: http-fallback`. The installed Codex client
attempts WebSocket, accepts Root's authenticated HTTP `426`, and switches the
session to HTTP/SSE. `first-message` remains an experimental opt-in for the
turn-aware WebSocket controller.

Synthesized Relay entries omit OpenAI's optional `comp_hash`. This prevents an
ordinary model switch from forcing an old-provider compact and lets Codex replay
full history across stock and Relay. A genuine context-window downshift on a
very large thread can still require provider-bound compaction and must start a
new chain.

## Historical: Root-only readable logging activation

Preserved as the completed Root logging cutover. Do not run it for the
current Relay `inspect_image` handoff.


Run this only from an external Terminal after the Codex task that prepared the
candidate has durably completed. Do not run it in-band through the serving Root.

1. Verify the new candidate and both immutable references:

   ```bash
   ROOT_CANDIDATE=/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260730T070219Z-c7a1cca9
   ROOT_ROLLBACK=/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260730T051458Z-30d98ff9
   RELAY_CANDIDATE=/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260730T063922Z-33d64b1e
   (cd "$ROOT_CANDIDATE" && shasum -a 256 -c manifest.sha256)
   (cd "$ROOT_ROLLBACK" && shasum -a 256 -c manifest.sha256)
   (cd "$RELAY_CANDIDATE" && shasum -a 256 -c manifest.sha256)
   ```

   A mutable Relay auth file can legitimately drift from its old manifest after
   credential refresh. That does not authorize replacing, copying, or restarting
   Relay; verify the pinned binary/config/plist identities in
   `dependencies/relay.json` separately.

2. Record the current Root, Relay, and bridge listener PIDs and executable paths.
   Require Root on `127.0.0.1:8317`, Relay on `127.0.0.1:8318`, and the only
   bridge on `192.168.139.3:8318`. Then replace only Root:

   ```bash
   launchctl bootout "gui/$(id -u)/com.user.cliproxy-root"
   test -z "$(lsof -nP -iTCP@127.0.0.1:8317 -sTCP:LISTEN)"
   install -m 600 "$ROOT_CANDIDATE/launchd/com.user.cliproxy-root.plist" "$HOME/Library/LaunchAgents/com.user.cliproxy-root.plist"
   launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.user.cliproxy-root.plist"
   ```

3. Verify the new Root executable and config hashes, loopback-only `8317`, the
   unchanged Relay/bridge PIDs and hashes, health, catalog, one stock turn, and a
   fresh `root.stock-traffic.v2` record with `payload_encoding: "utf-8"`. The
   existing active traffic file may contain older v1 records before the new v2
   records. Leave Relay and both bridge labels untouched.

4. If Root verification fails, roll back Root only:

   ```bash
   launchctl bootout "gui/$(id -u)/com.user.cliproxy-root"
   install -m 600 "$ROOT_ROLLBACK/launchd/com.user.cliproxy-root.plist" "$HOME/Library/LaunchAgents/com.user.cliproxy-root.plist"
   launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.user.cliproxy-root.plist"
   ```

   Reverify the rollback binary/config hashes and `8317`; do not boot out,
   bootstrap, reload, or edit Relay or the OrbStack bridge.

## Initial split-deployment reference

The following Phase A/Phase B procedure documents the completed initial split.
Do not rerun it for the Root-only readable-logging activation above.

Its commands are preserved as the historical record of what was actually run and
are deliberately not rewritten. They assert the then-current `kimi-k3` catalog
and the then-current bundle-scoped log paths, both of which are now wrong; see
the catalog and stable-layout sections above for the current expectations.

### Phase A: stage Relay without Desktop impact

Run this phase from Terminal. Set `DEPLOYMENT_DIR` to the manifest directory and
read the Relay key from its private Root environment file without printing it:

```bash
DEPLOYMENT_DIR=/absolute/private/deployment/path
set -a
. "$DEPLOYMENT_DIR/root/.env"
set +a
```

1. Verify the manifest, permissions, and free ports:

   ```bash
   (cd "$DEPLOYMENT_DIR" && shasum -a 256 -c manifest.sha256)
   stat -f '%Sp %N' "$DEPLOYMENT_DIR/relay/relay.yaml" "$DEPLOYMENT_DIR/root/root.yaml" "$DEPLOYMENT_DIR/root/.env"
   lsof -nP -iTCP:8318 -sTCP:LISTEN
   ```

   All three secret-bearing files must be `-rw-------`, and the final command
   must return no listener before staging.

2. Install the inactive Relay and bridge plists, then load Relay first:

   ```bash
   install -m 600 "$DEPLOYMENT_DIR/launchd/com.user.cliproxy-relay.plist" "$HOME/Library/LaunchAgents/com.user.cliproxy-relay.plist"
   install -m 600 "$DEPLOYMENT_DIR/launchd/com.user.cliproxy-orbstack-relay-v2.plist" "$HOME/Library/LaunchAgents/com.user.cliproxy-orbstack-relay-v2.plist"
   launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.user.cliproxy-relay.plist"
   ```

3. Prove the authenticated Relay boundary:

   ```bash
   test "$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:8318/v1/models)" = 401
   curl -fsS -H "Authorization: Bearer $CPA_RELAY_API_KEY" http://127.0.0.1:8318/v1/models | jq -e '[.data[].id] == ["kimi-k3"]'
   curl -fsSN -H "Authorization: Bearer $CPA_RELAY_API_KEY" -H 'Content-Type: application/json' \
     --data '{"model":"kimi-k3","stream":true,"input":"Reply exactly RELAY_READY_OK."}' \
     http://127.0.0.1:8318/v1/responses | rg -q '"type":"response.completed"'
   ```

   Do not proceed unless the authenticated turn reaches `response.completed`.

4. Load the new OrbStack bridge and repeat the authenticated model and Kimi
   checks from the OrbStack client:

   ```bash
   launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.user.cliproxy-orbstack-relay-v2.plist"
   ```

5. Update Hermes to `http://192.168.139.3:8318/v1`, install the same CPA key in
   its mode-`0600` config, gracefully reload `hermes-gateway.service`, and prove
   an authenticated Kimi turn. Confirm no Hermes connection still uses port
   `8317`.

### Phase B: switch Desktop

Only run this phase from an external Terminal during a quiescent window. Do not
run it from a Codex task whose connection traverses the serving `8317` proxy.

1. Install the inactive Root plist:

   ```bash
   install -d -m 700 /Users/dwolf/.local/state/cliproxyapi/root
   install -m 600 "$DEPLOYMENT_DIR/launchd/com.user.cliproxy-root.plist" "$HOME/Library/LaunchAgents/com.user.cliproxy-root.plist"
   ```

2. Remove the old OrbStack exposure before freeing loopback `8317`:

   ```bash
   launchctl bootout "gui/$(id -u)/com.user.cliproxy-orbstack-relay"
   test -z "$(lsof -nP -iTCP@192.168.139.3:8317 -sTCP:LISTEN)"
   ```

3. Stop the old monolithic CPA and start Root:

   ```bash
   launchctl bootout "gui/$(id -u)/com.user.cliproxy-api"
   test -z "$(lsof -nP -iTCP@127.0.0.1:8317 -sTCP:LISTEN)"
   launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.user.cliproxy-root.plist"
   ```

   `bootout` lasts only for the current login session. Both old plists remain
   installed for rollback with `RunAtLoad`, so without the following step they
   restart at the next login: `com.user.cliproxy-api` would race Root for
   loopback `8317`, and `com.user.cliproxy-orbstack-relay` would re-bind
   `192.168.139.3:8317` and forward OrbStack straight into Root. Persist the
   removal in the per-user override database:

   ```bash
   launchctl disable "gui/$(id -u)/com.user.cliproxy-api"
   launchctl disable "gui/$(id -u)/com.user.cliproxy-orbstack-relay"
   launchctl print-disabled "gui/$(id -u)" | rg 'cliproxy-(api|orbstack-relay)"'
   ```

   Both labels must report `=> disabled`. This is state, not a file: it survives
   reboot and is not undone by reinstalling a plist.

4. Verify identity and topology before opening Desktop:

   ```bash
   launchctl print "gui/$(id -u)/com.user.cliproxy-root"
   lsof -nP -iTCP:8317 -sTCP:LISTEN
   lsof -nP -iTCP:8318 -sTCP:LISTEN
   curl -fsS http://127.0.0.1:8317/healthz
   curl -fsS -H 'Authorization: Bearer desktop-preflight' 'http://127.0.0.1:8317/v1/models?client_version=0.146.0' | jq -e '[.models[].slug] == ["gpt-5.6-sol","gpt-5.6-terra","gpt-5.6-luna","kimi-k3"]'
   ```

   The only `8317` listener must be `127.0.0.1`; the bridge must expose only
   `192.168.139.3:8318`.

5. Run the exact installed Codex client through Root for one stock turn and one
   Kimi turn. Confirm the client logs one `426` WebSocket fallback per process,
   both turns reach `turn.completed`, and Root never falls back across provider
   arms. Then verify a stock-to-Kimi and Kimi-to-stock model change in Desktop.

6. Verify native logging without printing credentials:

   ```bash
   ROOT_LOG_DIR=/Users/dwolf/.local/state/cliproxyapi/root/logs
   stat -f '%Sp %N' "$ROOT_LOG_DIR" "$ROOT_LOG_DIR/root.log" "$ROOT_LOG_DIR/access.ndjson" "$ROOT_LOG_DIR/stock-traffic.ndjson"
   jq -e 'select(.schema == "root.access.v1")' "$ROOT_LOG_DIR/access.ndjson" >/dev/null
   jq -s -e 'length > 0 and all(.schema == "root.stock-traffic.v1" or .schema == "root.stock-traffic.v2") and any(.schema == "root.stock-traffic.v2" and .kind == "end" and .capture_complete == true) and any(.schema == "root.stock-traffic.v2" and .payload_encoding == "utf-8" and has("payload_text"))' "$ROOT_LOG_DIR/stock-traffic.ndjson" >/dev/null
   set -a
   . "$DEPLOYMENT_DIR/root/.env"
   set +a
   ! rg --search-zip -F --quiet "$CPA_RELAY_API_KEY" "$ROOT_LOG_DIR"
   ```

   The directory must be `drwx------` and each active file `-rw-------`.
   Access records must cover health, model discovery, `426`, stock, and Relay
   requests. Traffic records must name only configured stock models. Inspect a
   selected v2 `payload_text` field with `jq '.payload_text'` so untrusted control
   bytes stay escaped; use `jq -r` only when redirecting exact content to a
   private file. Decode `payload_base64` only for binary fallback records. Do not
   copy the complete traffic file into tickets or chat.

## Initial split rollback

Use launchd `bootout`/`bootstrap`; do not `kill` a KeepAlive job.

1. Boot out `com.user.cliproxy-root` and verify loopback `8317` is free.
2. Restore the snapshotted `com.user.cliproxy-api` binary, config, and plist only
   if their hashes differ from the rollback manifest. Preserve the executable
   mode explicitly:

   ```bash
   install -m 700 "$DEPLOYMENT_DIR/rollback/cli-proxy-api.live" /Users/dwolf/Projects/CLIProxyAPI-mimironhz/cli-proxy-api
   install -m 600 "$DEPLOYMENT_DIR/rollback/config.yaml.live" /Users/dwolf/Projects/CLIProxyAPI-mimironhz/config.yaml
   install -m 600 "$DEPLOYMENT_DIR/rollback/com.user.cliproxy-api.plist.live" "$HOME/Library/LaunchAgents/com.user.cliproxy-api.plist"
   ```
3. Re-enable the label before bootstrapping it. Phase B disabled it in the
   per-user override database, and `bootstrap` on a disabled label does not
   bring the job up:

   ```bash
   launchctl enable "gui/$(id -u)/com.user.cliproxy-api"
   ```

   Then bootstrap `com.user.cliproxy-api`, verify its exact binary/config hashes
   and `GET /healthz`, and reopen Desktop.
4. For a Desktop-only rollback, leave Hermes on the functioning authenticated
   Relay `8318`; the Root job must remain booted out.
5. For a full rollback, first restore Hermes's old `8317` config and reload it.
   Then remove the new bridge and Relay before restoring the old bridge:

   ```bash
   launchctl bootout "gui/$(id -u)/com.user.cliproxy-orbstack-relay-v2"
   launchctl bootout "gui/$(id -u)/com.user.cliproxy-relay"
   test -z "$(lsof -nP -iTCP:8318 -sTCP:LISTEN)"
   launchctl enable "gui/$(id -u)/com.user.cliproxy-orbstack-relay"
   launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.user.cliproxy-orbstack-relay.plist"
   ```

   The `enable` is required for the same reason as step 3.

6. The three new plist files may remain installed as inactive staged artifacts,
   but booting them out is not sufficient: all three carry `RunAtLoad`, so a
   retained file does return as a process and a listener at the next login.
   Disable each label, then verify it is both absent and disabled:

   ```bash
   for label in com.user.cliproxy-root com.user.cliproxy-relay com.user.cliproxy-orbstack-relay-v2; do
     launchctl disable "gui/$(id -u)/$label"
     ! launchctl print "gui/$(id -u)/$label"
     launchctl print-disabled "gui/$(id -u)" | rg -q "\"$label\" => disabled"
   done
   ```

   Retaining the files is intentional; the `disable` state is what keeps them
   inert. Re-enable a label before any later attempt to bootstrap it.

7. Native Root logs are not deleted during rollback. They remain private under
   `/Users/dwolf/.local/state/cliproxyapi/root/logs` for diagnosis and expire by
   the configured retention policy. Remove them only through a separately
   authorized sensitive-data cleanup.

Desktop-only rollback is complete when old CPA health and identity are restored.
Full rollback additionally requires the recorded pre-cutover listeners, process
paths, and artifact hashes, with no listener on `8318`.

Either way, confirm the surviving login-time state explicitly. Every installed
`com.user.cliproxy-*` label must be either loaded on purpose or disabled; a
label that is merely booted out is a reboot away from returning:

```bash
for f in "$HOME"/Library/LaunchAgents/com.user.cliproxy-*.plist; do
  label=$(plutil -extract Label raw -o - "$f")
  launchctl list | awk '{print $3}' | grep -qx "$label" && continue
  launchctl print-disabled "gui/$(id -u)" | rg -q "\"$label\" => disabled" \
    || echo "HAZARD: $label is neither loaded nor disabled"
done
```

Never inspect these plists with `plutil -extract` without `-o -`; omitting it
rewrites the plist in place with the extracted value and destroys the artifact.
