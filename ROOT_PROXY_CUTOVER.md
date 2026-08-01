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
The current Root-only bundle contains one versioned binary, a mode-`0600` config
and `.env`, one inactive launchd plist, immutable dependency/rollback references,
and verification evidence. It contains no live logs. Verify every SHA-256 in
`manifest.sha256` before loading a job. Do not rebuild or edit an artifact in
place; create a new versioned directory instead.

Frozen Root-only readable-logging candidate:

```text
/Users/dwolf/.local/state/cliproxyapi/root-relay-cutover/20260730T070219Z-c7a1cca9
```

This bundle changes Root only. It contains no Relay binary, config, credential,
or mutable auth state. It pins the running Relay dependency
`20260730T063922Z-33d64b1e` and the Root rollback
`20260730T051458Z-30d98ff9` by immutable manifest and artifact hashes. At the
time of writing, Root still serves from the rollback candidate, while Relay and
the OrbStack bridge remain live and must not be restarted for this upgrade.

The pinned Relay dependency added `remote-management` to `relay.yaml`. Without a
management secret the API routes are never registered — `internal/api/server.go`
only calls `registerManagementRoutes()` when
`cfg.RemoteManagement.SecretKey != "" || MANAGEMENT_PASSWORD || --tui`. Because
`/management.html` is served unconditionally, the symptom of omitting it is a
control panel that loads and then fails login with `HTTP 404: Management API not
found`. Root exposes no management surface at all, so `8318` is the only host
for it. It also repoints `auth-dir` into its own directory so the superseded
bundle is not mutated by credential refreshes.

**`allow-remote: false` does not confine management to this host.** The OrbStack
socat bridge terminates the VM's connection and opens a fresh loopback one, so
the Relay sees every bridged request as `127.0.0.1` and treats it as a local
client. Verified: with the management key, `/v0/management/config` returns `200`
from the OrbStack VM. Management is therefore reachable from any OrbStack VM and
is guarded only by the bcrypt secret. This was accepted knowingly. Two
consequences follow. The secret is the entire boundary, so it must stay strong
and out of the VM's reach. And `AuthenticateManagementKey` bans by client IP
after 5 failures for 30 minutes, so bridged brute-force attempts are attributed
to `127.0.0.1` and will lock out local administration too. To get a genuinely
loopback-only window, boot out `com.user.cliproxy-orbstack-relay-v2` first.

The generated password is stored mode `0600`, outside the tracked manifest, at
`relay/management-password.txt`. Move it into a password manager and remove the
file. Note that `relay/auth/kimi-apikey.json` is manifest-tracked but mutable at
runtime; a credential refresh will make `manifest.sha256` report it as changed,
which is expected drift rather than tampering.

The Root-only candidate retains native application, request-access, and
stock-request-response logs and changes their payload policy from base64-only
v1 to readable-auto v2. Active and rotated files live outside the frozen
bundle under `/Users/dwolf/.local/state/cliproxyapi/root/logs`: `root.log`,
`access.ndjson`, and `stock-traffic.ndjson`. The latter contains complete stock
conversation/tool payloads and must be treated as sensitive. Root never sends a
direct Relay request or response body to that sink; history replayed later as
part of a stock request is, correctly, part of the stock request capture.

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
- Relay: `deepseek-v4-flash`

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

`grok-4.5` is implemented by the routing layer but is excluded from the first
cutover catalog because the current xAI account returns HTTP `402` with an
exhausted usage balance. Add it only after an authenticated direct Relay smoke
completes successfully.

The Root config uses `websocket.mode: http-fallback`. The installed Codex client
attempts WebSocket, accepts Root's authenticated HTTP `426`, and switches the
session to HTTP/SSE. `first-message` remains an experimental opt-in for the
turn-aware WebSocket controller.

Synthesized Relay entries omit OpenAI's optional `comp_hash`. This prevents an
ordinary model switch from forcing an old-provider compact and lets Codex replay
full history across stock and Relay. A genuine context-window downshift on a
very large thread can still require provider-bound compaction and must start a
new chain.

## Root-only readable logging activation

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
