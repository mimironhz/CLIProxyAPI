#!/bin/zsh

set -u
setopt pipefail
umask 077

script_name=${0:t}

usage() {
  print -u2 -r -- "Usage: $script_name --candidate DIR --root-rollback DIR --relay-rollback DIR [--preflight | --activate | --rollback]"
}

mode=
candidate=
root_rollback=
relay_rollback=

while (( $# > 0 )); do
  case "$1" in
    --candidate)
      (( $# >= 2 )) || { usage; exit 2; }
      candidate=$2
      shift 2
      ;;
    --root-rollback)
      (( $# >= 2 )) || { usage; exit 2; }
      root_rollback=$2
      shift 2
      ;;
    --relay-rollback)
      (( $# >= 2 )) || { usage; exit 2; }
      relay_rollback=$2
      shift 2
      ;;
    --preflight|--activate|--rollback)
      [[ -z "$mode" ]] || { print -u2 -r -- "Specify exactly one operation."; usage; exit 2; }
      mode=$1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      print -u2 -r -- "Unknown argument: $1"
      usage
      exit 2
      ;;
  esac
done

if [[ -z "$mode" || -z "$candidate" || -z "$root_rollback" || -z "$relay_rollback" ]]; then
  usage
  exit 2
fi

canonical_dir() {
  local directory=$1
  [[ "$directory" == /* && -d "$directory" ]] || {
    print -u2 -r -- "Bundle directory must be an existing absolute path: $directory"
    return 1
  }
  (cd "$directory" && pwd -P)
}

candidate=$(canonical_dir "$candidate") || exit 2
root_rollback=$(canonical_dir "$root_rollback") || exit 2
relay_rollback=$(canonical_dir "$relay_rollback") || exit 2

user_home_dir=${HOME:?HOME must identify the current launchd user home}
root_plist=${CLIPROXY_ROOT_PLIST:-"$user_home_dir/Library/LaunchAgents/com.user.cliproxy-root.plist"}
relay_plist=${CLIPROXY_RELAY_PLIST:-"$user_home_dir/Library/LaunchAgents/com.user.cliproxy-relay.plist"}
log_dir=${CLIPROXY_CUTOVER_LOG_DIR:-"$user_home_dir/.local/state/cliproxyapi/root-relay-cutover/activation-logs"}
domain="gui/$(id -u)"
root_label=com.user.cliproxy-root
relay_label=com.user.cliproxy-relay
bridge_address=${CLIPROXY_BRIDGE_ADDRESS:-192.168.139.3:8318}

verify_commands() {
  local command_name
  for command_name in awk basename chmod curl date head install jq launchctl lsof mkdir sed shasum sleep tee; do
    command -v "$command_name" >/dev/null || {
      print -u2 -r -- "Required command not found: $command_name"
      return 1
    }
  done
}

verify_commands || exit 1

if [[ "$user_home_dir" != /* || "$root_plist" != /* || "$relay_plist" != /* || "$log_dir" != /* ]]; then
  print -u2 -r -- "Home, plist, and log paths must be absolute."
  exit 2
fi
if [[ "${root_plist:t}" != com.user.cliproxy-root.plist || "${relay_plist:t}" != com.user.cliproxy-relay.plist ]]; then
  print -u2 -r -- "Plist overrides must retain the expected launchd filenames."
  exit 2
fi

mkdir -p "$log_dir" || exit 1
chmod 700 "$log_dir" || exit 1
run_id=$(date -u '+%Y%m%dT%H%M%SZ')
candidate_name=$(basename "$candidate")
run_slug=$(print -r -- "$candidate_name" | sed 's/[^A-Za-z0-9._-]/_/g')
log_file="$log_dir/$run_id-$run_slug.log"
status_file="$log_dir/$run_id-$run_slug.json"
exec > >(tee -a "$log_file") 2>&1 || exit 1

timestamp() {
  date -u '+%Y-%m-%dT%H:%M:%SZ'
}

job_program() {
  launchctl print "$domain/$1" 2>/dev/null | awk -F' = ' '/^[[:space:]]*program = / { print $2; exit }'
}

listener_pid() {
  lsof -nP -iTCP@"$1" -sTCP:LISTEN -t 2>/dev/null | head -n 1
}

wait_job_absent() {
  local label=$1
  local attempt=1
  while (( attempt <= 40 )); do
    if ! launchctl print "$domain/$label" >/dev/null 2>&1; then
      sleep 1
      return 0
    fi
    sleep 0.25
    (( attempt++ ))
  done
  print -r -- "$(timestamp) job did not unload: $label"
  return 1
}

stop_job() {
  local label=$1
  if launchctl print "$domain/$label" >/dev/null 2>&1; then
    launchctl bootout "$domain/$label" || return 1
  fi
  wait_job_absent "$label"
}

bootstrap_job() {
  local label=$1
  local plist=$2
  local attempt=1
  local output
  while (( attempt <= 12 )); do
    if output=$(launchctl bootstrap "$domain" "$plist" 2>&1); then
      return 0
    fi
    print -r -- "$(timestamp) bootstrap attempt $attempt failed for $label: $output"
    sleep 1
    (( attempt++ ))
  done
  return 1
}

wait_health() {
  local url=$1
  local attempt=1
  while (( attempt <= 40 )); do
    if curl -fsS "$url" | jq -e '.status == "ok"' >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
    (( attempt++ ))
  done
  print -r -- "$(timestamp) health check failed: $url"
  return 1
}

verify_bundle_files() {
  [[ -f "$candidate/manifest.sha256" ]] || return 1
  [[ -x "$candidate/bin/root-proxy" ]] || return 1
  [[ -x "$candidate/bin/cli-proxy-api-relay" ]] || return 1
  [[ -f "$candidate/launchd/com.user.cliproxy-root.plist" ]] || return 1
  [[ -f "$candidate/launchd/com.user.cliproxy-relay.plist" ]] || return 1
  [[ -f "$root_rollback/manifest.sha256" ]] || return 1
  [[ -x "$root_rollback/bin/root-proxy" ]] || return 1
  [[ -f "$root_rollback/launchd/com.user.cliproxy-root.plist" ]] || return 1
  [[ -f "$relay_rollback/manifest.sha256" ]] || return 1
  [[ -x "$relay_rollback/bin/cli-proxy-api-relay" ]] || return 1
  [[ -f "$relay_rollback/launchd/com.user.cliproxy-relay.plist" ]] || return 1
}

verify_manifests() {
  (cd "$candidate" && shasum -a 256 -c manifest.sha256) || return 1
  (cd "$root_rollback" && shasum -a 256 -c manifest.sha256) || return 1
  (cd "$relay_rollback" && shasum -a 256 -c manifest.sha256) || return 1
}

verify_baseline() {
  [[ "$(job_program "$root_label")" == "$root_rollback/bin/root-proxy" ]] || return 1
  [[ "$(job_program "$relay_label")" == "$relay_rollback/bin/cli-proxy-api-relay" ]] || return 1
  wait_health http://127.0.0.1:8317/healthz || return 1
  wait_health http://127.0.0.1:8318/healthz || return 1
  [[ -n "$(listener_pid "$bridge_address")" ]]
}

switch_relay() {
  print -r -- "$(timestamp) switching Relay"
  stop_job "$relay_label" || return 1
  install -m 600 "$candidate/launchd/com.user.cliproxy-relay.plist" "$relay_plist" || return 1
  bootstrap_job "$relay_label" "$relay_plist" || return 1
  wait_health http://127.0.0.1:8318/healthz || return 1
  [[ "$(job_program "$relay_label")" == "$candidate/bin/cli-proxy-api-relay" ]]
}

switch_root() {
  print -r -- "$(timestamp) switching Root"
  stop_job "$root_label" || return 1
  install -m 600 "$candidate/launchd/com.user.cliproxy-root.plist" "$root_plist" || return 1
  bootstrap_job "$root_label" "$root_plist" || return 1
  wait_health http://127.0.0.1:8317/healthz || return 1
  [[ "$(job_program "$root_label")" == "$candidate/bin/root-proxy" ]]
}

rollback_both() {
  print -r -- "$(timestamp) rolling back Root and Relay"
  stop_job "$root_label" || true
  stop_job "$relay_label" || true
  install -m 600 "$root_rollback/launchd/com.user.cliproxy-root.plist" "$root_plist" || return 1
  install -m 600 "$relay_rollback/launchd/com.user.cliproxy-relay.plist" "$relay_plist" || return 1
  bootstrap_job "$relay_label" "$relay_plist" || return 1
  wait_health http://127.0.0.1:8318/healthz || return 1
  bootstrap_job "$root_label" "$root_plist" || return 1
  wait_health http://127.0.0.1:8317/healthz || return 1
  [[ "$(job_program "$root_label")" == "$root_rollback/bin/root-proxy" ]] || return 1
  [[ "$(job_program "$relay_label")" == "$relay_rollback/bin/cli-proxy-api-relay" ]]
}

write_status() {
  local result=$1
  local detail=$2
  jq -n \
    --arg timestamp "$(timestamp)" \
    --arg result "$result" \
    --arg detail "$detail" \
    --arg root_program "$(job_program "$root_label")" \
    --arg relay_program "$(job_program "$relay_label")" \
    --arg root_pid "$(listener_pid 127.0.0.1:8317)" \
    --arg relay_pid "$(listener_pid 127.0.0.1:8318)" \
    --arg bridge_pid "$(listener_pid "$bridge_address")" \
    '{timestamp: $timestamp, result: $result, detail: $detail, root_program: $root_program, relay_program: $relay_program, root_pid: $root_pid, relay_pid: $relay_pid, bridge_pid: $bridge_pid}' \
    >"$status_file" || return 1
  chmod 600 "$status_file"
}

preflight() {
  verify_bundle_files || return 1
  verify_manifests || return 1
  verify_baseline || return 1
  print -r -- "$(timestamp) preflight passed"
}

case "$mode" in
  --preflight)
    if preflight; then
      write_status preflight_passed "No services changed."
      exit 0
    fi
    write_status preflight_failed "No services changed."
    exit 1
    ;;
  --activate)
    ;;
  --rollback)
    if ! verify_bundle_files || ! verify_manifests; then
      write_status rollback_preflight_failed "No services changed."
      exit 1
    fi
    if rollback_both; then
      write_status rolled_back "Manual rollback completed."
      exit 0
    fi
    write_status rollback_failed "Manual rollback did not fully recover."
    exit 1
    ;;
esac

bridge_pid_before=$(listener_pid "$bridge_address")
if ! preflight; then
  write_status preflight_failed "No services changed."
  exit 1
fi

if switch_relay && [[ "$(listener_pid "$bridge_address")" == "$bridge_pid_before" ]] && switch_root && [[ "$(listener_pid "$bridge_address")" == "$bridge_pid_before" ]]; then
  write_status activated "Relay then Root activated; bridge unchanged."
  print -r -- "$(timestamp) activation completed"
  exit 0
fi

if rollback_both; then
  write_status rolled_back "Activation failed; both services restored automatically."
  print -u2 -r -- "$(timestamp) activation failed and rollback completed"
  exit 1
fi

write_status rollback_failed "Activation failed and automatic rollback did not fully recover."
print -u2 -r -- "$(timestamp) activation and rollback both failed"
exit 1
