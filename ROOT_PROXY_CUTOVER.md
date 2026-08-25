# Root / Relay deployment state

This runbook manages the local split proxy without accumulating an unbounded deployment history.

```text
Codex Desktop -> Root 127.0.0.1:8317
                       |-- stock -> chatgpt.com/backend-api/codex
                       `-- third party -> Relay 127.0.0.1:8318

OrbStack clients -> 192.168.139.3:8318 -> Relay 127.0.0.1:8318
```

The repository helper is `scripts/root-relay-cutover.zsh`. Run any operation that restarts Root from an external Terminal during a quiet interval. Restarting Root from a Codex task routed through Root disconnects that task, and the helper refuses to restart Root while it sees established inbound clients.

## State model

`$HOME/.local/state/cliproxyapi/root-relay-cutover/deployment-state.json` is the sole reachability index. It records exactly two generations for each service: `active` and `rollback`.

The managed layout is:

```text
root-relay-cutover/
  deployment-state.json
  20...-root-.../                 # immutable Root bundle
  20...-relay-.../                # immutable Relay bundle
  runtime/
    root/root.yaml                # mutable installed Root config
    root/.env                     # mutable installed Root environment
    relay/relay.yaml              # mutable installed Relay config
  rollback-snapshots/
    root/<generation>/            # exact pre-switch config, plist, and .env
    relay/<generation>/           # exact pre-switch config and plist
  receipts/
    root/current.{json,log}
    relay/current.{json,log}
    failed/latest.{json,log}
  transactions/                   # empty outside an interrupted operation
```

Relay keeps its existing working directory at `$HOME/.local/state/cliproxyapi/relay`; only its installed config moves to the stable runtime path. Service logs remain outside immutable bundles and must have a finite `logs-max-total-size-mb` policy in Relay configuration. Receipts overwrite fixed filenames.

Immutable bundles are never edited in place. A successful activation snapshots the exact installed service files, advances the selected service's `active` and `rollback` slots atomically, verifies both services and the OrbStack bridge, and then removes every unreferenced bundle and rollback snapshot. The garbage collector also removes legacy loose activation scripts and the old `activation-logs` directory.

## Safety properties

- Root and Relay activate independently; a Relay-only change never restarts Root.
- The other service and `192.168.139.3:8318` bridge must retain their listener PIDs during a single-service activation.
- Runtime config is copied from the candidate and is never a symlink into an immutable bundle.
- Rollback restores the exact files captured before the preceding switch, including Root's `.env`.
- A pending transaction fails closed. Inspect its journal and reconcile live jobs, runtime files, and `deployment-state.json` before removing it.
- Garbage collection derives its keep set only from the four state slots. It first moves candidates to a private Trash quarantine, re-verifies live state, and deletes the quarantine only after verification succeeds.
- Exit status `3` means the service and state change committed but post-commit finalization failed. Inspect the current receipt first; if cleanup did not complete, run `--gc-dry-run` and `--gc` after investigating.

## Candidate contract

Candidates are service-specific direct children of `root-relay-cutover` with a UTC deployment basename matching `20*T*Z-*`; paired bundles that duplicate both binaries and both configs are rejected for new activations. The basename contract is also the garbage collector's discovery boundary, so a bundle that cannot be collected cannot be accepted.

A Root candidate contains:

```text
manifest.json                      # includes "service": "root"
manifest.sha256
bin/root-proxy
root/root.yaml
root/.env
launchd/com.user.cliproxy-root.plist
```

A Relay candidate contains:

```text
manifest.json                      # includes "service": "relay"
manifest.sha256
bin/cli-proxy-api-relay
relay/relay.yaml
launchd/com.user.cliproxy-relay.plist
```

`manifest.sha256` must cover exactly `manifest.json` plus the binary, config, plist, and Root `.env` where applicable. The bundle may contain no extra files or symlinks. The helper verifies this closed payload set before preflight or activation and normalizes the candidate plist to the immutable binary plus stable runtime config.

## One-time migration

Migration preserves exact live config bytes even when an old whole-bundle manifest no longer matches because its config was edited after activation. It uses the named legacy active bundles only to identify the running binaries, normalizes the named legacy rollback bundle plists onto stable runtime paths for generation 1, restarts Relay and then Root, writes state, and prunes deeper history. Subsequent snapshots preserve the exact installed plist bytes.

Run this only from an external Terminal when Root has no active clients:

```bash
HELPER=/Users/dwolf/Projects/CLIProxyAPI-mimironhz/scripts/root-relay-cutover.zsh
CUTOVER=$HOME/.local/state/cliproxyapi/root-relay-cutover

"$HELPER" --migrate-state \
  --root-active "$CUTOVER/<current-root-bundle>" \
  --root-rollback "$CUTOVER/<previous-root-bundle>" \
  --relay-active "$CUTOVER/<current-relay-bundle>" \
  --relay-rollback "$CUTOVER/<previous-relay-bundle>"
```

Migration refuses to overwrite an existing state file. Verify the result with:

```bash
"$HELPER" --status
"$HELPER" --gc-dry-run
```

## Normal activation

Preflight is read-only:

```bash
"$HELPER" --service relay --candidate "$CUTOVER/<relay-candidate>" --preflight
"$HELPER" --service root --candidate "$CUTOVER/<root-candidate>" --preflight
```

Activate only the service carried by the candidate. Automatic pruning is the default:

```bash
"$HELPER" --service relay --candidate "$CUTOVER/<relay-candidate>" --activate
"$HELPER" --service root --candidate "$CUTOVER/<root-candidate>" --activate
```

Use `--no-prune` only for a diagnosed cleanup problem; it is not a normal deployment option.

## Rollback

Rollback swaps the selected service's two slots and captures the pre-rollback live files as the new immediate rollback:

```bash
"$HELPER" --service relay --rollback
"$HELPER" --service root --rollback
```

Root rollback has the same quiet-client requirement as Root activation.

## Garbage collection and inspection

Preview and apply reachability-based cleanup with:

```bash
"$HELPER" --gc-dry-run
"$HELPER" --gc
"$HELPER" --status
```

`--gc` is safe to repeat. It does not delete the stable runtime directory, state, receipts, pending transactions, service logs, installed plists, or any of the four state-referenced bundles.

## Interrupted operations

Do not delete a non-empty `transactions` entry just to clear the guard. Read `journal.json`, compare the active bundle recorded in state with the programs reported by `launchctl print`, verify both health endpoints, and compare the stable runtime files with the transaction's `before-*` and `staged-*` copies. Restore or commit the intended side first, verify the bridge, and only then remove the reconciled transaction and run `--gc`.
