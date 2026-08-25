#!/bin/zsh

set -u
setopt pipefail
umask 077

script_name=${0:t}
operation=
service=
candidate=
prune_after_success=true
root_active=
root_rollback=
relay_active=
relay_rollback=

usage() {
  print -u2 -r -- "Usage:"
  print -u2 -r -- "  $script_name --migrate-state --root-active DIR --root-rollback DIR --relay-active DIR --relay-rollback DIR"
  print -u2 -r -- "  $script_name --service root|relay --candidate DIR --preflight"
  print -u2 -r -- "  $script_name --service root|relay --candidate DIR --activate [--no-prune]"
  print -u2 -r -- "  $script_name --service root|relay --rollback [--no-prune]"
  print -u2 -r -- "  $script_name --gc | --gc-dry-run | --status"
}

set_operation() {
  [[ -z "$operation" ]] || { print -u2 -r -- "Specify exactly one operation."; usage; exit 2; }
  operation=$1
}

while (( $# > 0 )); do
  case "$1" in
    --service)
      (( $# >= 2 )) || { usage; exit 2; }
      service=$2
      shift 2
      ;;
    --candidate)
      (( $# >= 2 )) || { usage; exit 2; }
      candidate=$2
      shift 2
      ;;
    --root-active)
      (( $# >= 2 )) || { usage; exit 2; }
      root_active=$2
      shift 2
      ;;
    --root-rollback)
      (( $# >= 2 )) || { usage; exit 2; }
      root_rollback=$2
      shift 2
      ;;
    --relay-active)
      (( $# >= 2 )) || { usage; exit 2; }
      relay_active=$2
      shift 2
      ;;
    --relay-rollback)
      (( $# >= 2 )) || { usage; exit 2; }
      relay_rollback=$2
      shift 2
      ;;
    --preflight) set_operation preflight; shift ;;
    --activate) set_operation activate; shift ;;
    --rollback) set_operation rollback; shift ;;
    --migrate-state) set_operation migrate; shift ;;
    --gc) set_operation gc; shift ;;
    --gc-dry-run) set_operation gc-dry-run; shift ;;
    --status) set_operation status; shift ;;
    --no-prune) prune_after_success=false; shift ;;
    --help|-h) usage; exit 0 ;;
    *) print -u2 -r -- "Unknown argument: $1"; usage; exit 2 ;;
  esac
done

[[ -n "$operation" ]] || { usage; exit 2; }
case "$operation" in
  preflight|activate)
    [[ "$service" == root || "$service" == relay ]] || { usage; exit 2; }
    [[ -n "$candidate" ]] || { usage; exit 2; }
    ;;
  rollback)
    [[ "$service" == root || "$service" == relay ]] || { usage; exit 2; }
    [[ -z "$candidate" ]] || { usage; exit 2; }
    ;;
  migrate)
    [[ -n "$root_active" && -n "$root_rollback" && -n "$relay_active" && -n "$relay_rollback" ]] || { usage; exit 2; }
    ;;
  gc|gc-dry-run|status)
    [[ -z "$service" && -z "$candidate" ]] || { usage; exit 2; }
    ;;
esac

user_home_dir=${HOME:?HOME must identify the current launchd user home}
cutover_root=${CLIPROXY_CUTOVER_ROOT:-"$user_home_dir/.local/state/cliproxyapi/root-relay-cutover"}
state_file=${CLIPROXY_DEPLOYMENT_STATE:-"$cutover_root/deployment-state.json"}
runtime_root=${CLIPROXY_RUNTIME_ROOT:-"$cutover_root/runtime"}
transaction_root=${CLIPROXY_TRANSACTION_ROOT:-"$cutover_root/transactions"}
snapshot_root=${CLIPROXY_SNAPSHOT_ROOT:-"$cutover_root/rollback-snapshots"}
receipt_root=${CLIPROXY_RECEIPT_ROOT:-"$cutover_root/receipts"}
lock_dir=${CLIPROXY_CUTOVER_LOCK:-"$cutover_root/.cutover-lock"}
trash_root=${CLIPROXY_TRASH_ROOT:-"$user_home_dir/.Trash"}
root_plist=${CLIPROXY_ROOT_PLIST:-"$user_home_dir/Library/LaunchAgents/com.user.cliproxy-root.plist"}
relay_plist=${CLIPROXY_RELAY_PLIST:-"$user_home_dir/Library/LaunchAgents/com.user.cliproxy-relay.plist"}
relay_work_dir=${CLIPROXY_RELAY_WORK_DIR:-"$user_home_dir/.local/state/cliproxyapi/relay"}
domain="gui/$(id -u)"
root_label=com.user.cliproxy-root
relay_label=com.user.cliproxy-relay
bridge_address=${CLIPROXY_BRIDGE_ADDRESS:-192.168.139.3:8318}

fail() {
  print -u2 -r -- "$1"
  return 1
}

timestamp() {
  date -u '+%Y-%m-%dT%H:%M:%SZ'
}

run_id() {
  date -u '+%Y%m%dT%H%M%SZ'
}

verify_commands() {
  local command_name
  for command_name in awk basename chmod curl date dirname find head install jq launchctl lsof mkdir mv plutil rg rm sed shasum sleep sort stat; do
    command -v "$command_name" >/dev/null || return 1
  done
}

verify_commands || { print -u2 -r -- "A required command is unavailable."; exit 1; }
for absolute_path in "$user_home_dir" "$cutover_root" "$state_file" "$runtime_root" "$transaction_root" "$snapshot_root" "$receipt_root" "$lock_dir" "$trash_root" "$root_plist" "$relay_plist" "$relay_work_dir"; do
  [[ "$absolute_path" == /* ]] || { print -u2 -r -- "Deployment paths must be absolute: $absolute_path"; exit 2; }
done

initialize_layout() {
  mkdir -p "$cutover_root" "$runtime_root/root" "$runtime_root/relay" "$transaction_root" "$snapshot_root/root" "$snapshot_root/relay" "$receipt_root/root" "$receipt_root/relay" "$receipt_root/failed" "$trash_root" || return 1
  chmod 700 "$cutover_root" "$runtime_root" "$runtime_root/root" "$runtime_root/relay" "$transaction_root" "$snapshot_root" "$snapshot_root/root" "$snapshot_root/relay" "$receipt_root" "$receipt_root/root" "$receipt_root/relay" "$receipt_root/failed" || return 1
}

lock_held=false
release_lock() {
  if [[ "$lock_held" == true ]]; then
    rm -f "$lock_dir/pid" 2>/dev/null || true
    rmdir "$lock_dir" 2>/dev/null || true
    lock_held=false
  fi
}

acquire_lock() {
  mkdir "$lock_dir" 2>/dev/null || fail "Cutover lock exists; inspect it instead of assuming it is stale."
  print -r -- "$$" >"$lock_dir/pid" || { rmdir "$lock_dir" 2>/dev/null || true; return 1; }
  chmod 600 "$lock_dir/pid"
  lock_held=true
}

trap release_lock EXIT
trap 'release_lock; exit 130' HUP INT TERM

ensure_no_pending_transaction() {
  local pending
  pending=$(find "$transaction_root" -mindepth 1 -maxdepth 1 -type d -print -quit)
  [[ -z "$pending" ]] || fail "Pending cutover transaction requires operator recovery: $pending"
}

canonical_bundle() {
  local requested=$1
  local canonical
  local parent
  local root_canonical
  [[ "$requested" == /* && -d "$requested" && ! -L "$requested" ]] || return 1
  canonical=$(cd "$requested" && pwd -P) || return 1
  root_canonical=$(cd "$cutover_root" && pwd -P) || return 1
  parent=$(dirname "$canonical")
  [[ "$parent" == "$root_canonical" ]] || return 1
  [[ "${canonical:t}" == 20*T*Z-* ]] || return 1
  print -r -- "$canonical"
}

canonical_snapshot() {
  local service_name=$1
  local requested=$2
  local canonical
  local parent
  local service_snapshot_root
  [[ "$requested" == /* && -d "$requested" && ! -L "$requested" ]] || return 1
  canonical=$(cd "$requested" && pwd -P) || return 1
  service_snapshot_root=$(cd "$snapshot_root/$service_name" && pwd -P) || return 1
  parent=$(dirname "$canonical")
  [[ "$parent" == "$service_snapshot_root" ]] || return 1
  print -r -- "$canonical"
}

service_label() {
  [[ "$1" == root ]] && print -r -- "$root_label" || print -r -- "$relay_label"
}

service_plist() {
  [[ "$1" == root ]] && print -r -- "$root_plist" || print -r -- "$relay_plist"
}

service_binary_name() {
  [[ "$1" == root ]] && print -r -- root-proxy || print -r -- cli-proxy-api-relay
}

service_binary() {
  print -r -- "$2/bin/$(service_binary_name "$1")"
}

service_bundle_config() {
  [[ "$1" == root ]] && print -r -- "$2/root/root.yaml" || print -r -- "$2/relay/relay.yaml"
}

service_bundle_env() {
  [[ "$1" == root ]] && print -r -- "$2/root/.env" || print -r -- ""
}

service_bundle_plist() {
  [[ "$1" == root ]] && print -r -- "$2/launchd/com.user.cliproxy-root.plist" || print -r -- "$2/launchd/com.user.cliproxy-relay.plist"
}

service_runtime_dir() {
  [[ "$1" == root ]] && print -r -- "$runtime_root/root" || print -r -- "$relay_work_dir"
}

service_runtime_config() {
  [[ "$1" == root ]] && print -r -- "$runtime_root/root/root.yaml" || print -r -- "$runtime_root/relay/relay.yaml"
}

service_runtime_env() {
  [[ "$1" == root ]] && print -r -- "$runtime_root/root/.env" || print -r -- ""
}

service_health_url() {
  [[ "$1" == root ]] && print -r -- http://127.0.0.1:8317/healthz || print -r -- http://127.0.0.1:8318/healthz
}

service_listener() {
  [[ "$1" == root ]] && print -r -- 127.0.0.1:8317 || print -r -- 127.0.0.1:8318
}

sha256_file() {
  shasum -a 256 "$1" | awk '{print $1}'
}

job_program() {
  launchctl print "$domain/$(service_label "$1")" 2>/dev/null | awk -F' = ' '/^[[:space:]]*program = / {print $2; exit}'
}

loaded_arguments() {
  launchctl print "$domain/$(service_label "$1")" 2>/dev/null | awk '/^[[:space:]]*arguments = \{/ {inside=1; next} inside && /^[[:space:]]*\}/ {exit} inside {sub(/^[[:space:]]+/, ""); print}'
}

loaded_working_dir() {
  launchctl print "$domain/$(service_label "$1")" 2>/dev/null | awk -F' = ' '/^[[:space:]]*working directory = / {print $2; exit}'
}

listener_pid() {
  lsof -nP -iTCP@"$1" -sTCP:LISTEN -t 2>/dev/null | head -n 1
}

wait_job_absent() {
  local label=$1
  local attempt=1
  while (( attempt <= 40 )); do
    if ! launchctl print "$domain/$label" >/dev/null 2>&1; then
      sleep 0.25
      return 0
    fi
    sleep 0.25
    (( attempt++ ))
  done
  return 1
}

stop_service() {
  local service_name=$1
  local label
  label=$(service_label "$service_name")
  if launchctl print "$domain/$label" >/dev/null 2>&1; then
    launchctl bootout "$domain/$label" || return 1
  fi
  wait_job_absent "$label"
}

bootstrap_service() {
  local service_name=$1
  local plist_file=$2
  local attempt=1
  local output
  while (( attempt <= 12 )); do
    if output=$(launchctl bootstrap "$domain" "$plist_file" 2>&1); then
      return 0
    fi
    print -u2 -r -- "$(timestamp) bootstrap attempt $attempt failed for $(service_label "$service_name"): $output"
    sleep 1
    (( attempt++ ))
  done
  return 1
}

wait_health() {
  local service_name=$1
  local attempt=1
  while (( attempt <= 40 )); do
    if curl --noproxy '*' -fsS "$(service_health_url "$service_name")" | jq -e '.status == "ok"' >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
    (( attempt++ ))
  done
  return 1
}

root_has_inbound_connection() {
  local root_pid
  root_pid=$(listener_pid 127.0.0.1:8317)
  [[ -n "$root_pid" ]] || return 1
  lsof -nP -a -p "$root_pid" -iTCP -sTCP:ESTABLISHED 2>/dev/null | awk 'NR > 1 && $0 ~ /127\.0\.0\.1:8317->/ {found=1} END {exit found ? 0 : 1}'
}

copy_atomic() {
  local source_file=$1
  local destination=$2
  local mode=$3
  local temporary="${destination}.next.$$"
  mkdir -p "$(dirname "$destination")" || return 1
  install -m "$mode" "$source_file" "$temporary" || return 1
  mv "$temporary" "$destination"
}

normalize_plist() {
  local template=$1
  local output=$2
  local service_name=$3
  local program=$4
  install -m 600 "$template" "$output" || return 1
  plutil -replace ProgramArguments.0 -string "$program" "$output" || return 1
  plutil -replace ProgramArguments.2 -string "$(service_runtime_config "$service_name")" "$output" || return 1
  plutil -replace WorkingDirectory -string "$(service_runtime_dir "$service_name")" "$output" || return 1
  plutil -lint "$output" >/dev/null || return 1
  verify_plist_arguments "$service_name" "$output" "$program" "$(service_runtime_config "$service_name")"
}

verify_plist_arguments() {
  local service_name=$1
  local plist_file=$2
  local program=$3
  local config_file=$4
  local arguments
  [[ "$(plutil -extract Label raw -o - "$plist_file")" == "$(service_label "$service_name")" ]] || return 1
  arguments=$(plutil -extract ProgramArguments json -o - "$plist_file") || return 1
  if [[ "$service_name" == root ]]; then
    jq -e --arg program "$program" --arg config "$config_file" '. == [$program, "--config", $config]' >/dev/null <<<"$arguments"
  else
    jq -e --arg program "$program" --arg config "$config_file" '. == [$program, "--config", $config, "--local-model"]' >/dev/null <<<"$arguments"
  fi
}

installed_config() {
  plutil -extract ProgramArguments.2 raw -o - "$(service_plist "$1")"
}

installed_working_dir() {
  plutil -extract WorkingDirectory raw -o - "$(service_plist "$1")"
}

verify_loaded_plist_parity() {
  local service_name=$1
  local installed_arguments
  installed_arguments=$(plutil -extract ProgramArguments json -o - "$(service_plist "$service_name")" | jq -r '.[]') || return 1
  [[ "$(loaded_arguments "$service_name")" == "$installed_arguments" ]] || return 1
  [[ "$(loaded_working_dir "$service_name")" == "$(installed_working_dir "$service_name")" ]]
}

verify_candidate_manifest() {
  local service_name=$1
  local bundle=$2
  local expected_checksum_paths
  local expected_files
  local actual_checksum_paths
  local actual_files
  [[ -z "$(find "$bundle" -type l -print -quit)" ]] || return 1
  if [[ "$service_name" == root ]]; then
    expected_checksum_paths=$'bin/root-proxy\nlaunchd/com.user.cliproxy-root.plist\nmanifest.json\nroot/.env\nroot/root.yaml'
    expected_files=$'bin/root-proxy\nlaunchd/com.user.cliproxy-root.plist\nmanifest.json\nmanifest.sha256\nroot/.env\nroot/root.yaml'
  else
    expected_checksum_paths=$'bin/cli-proxy-api-relay\nlaunchd/com.user.cliproxy-relay.plist\nmanifest.json\nrelay/relay.yaml'
    expected_files=$'bin/cli-proxy-api-relay\nlaunchd/com.user.cliproxy-relay.plist\nmanifest.json\nmanifest.sha256\nrelay/relay.yaml'
  fi
  actual_checksum_paths=$(awk 'NF != 2 || length($1) != 64 || $1 ~ /[^0-9a-f]/ || $2 ~ /^\// || $2 ~ /(^|\/)\.\.($|\/)/ {bad=1} {print $2} END {exit bad}' "$bundle/manifest.sha256" | LC_ALL=C sort) || return 1
  [[ "$actual_checksum_paths" == "$expected_checksum_paths" ]] || return 1
  actual_files=$(cd "$bundle" && find . -type f -print | sed 's#^\./##' | LC_ALL=C sort) || return 1
  [[ "$actual_files" == "$expected_files" ]] || return 1
  (cd "$bundle" && shasum -a 256 -c manifest.sha256 >/dev/null)
}

verify_candidate() {
  local service_name=$1
  local bundle=$2
  local manifest_service
  [[ -f "$bundle/manifest.json" && ! -L "$bundle/manifest.json" && -f "$bundle/manifest.sha256" && ! -L "$bundle/manifest.sha256" ]] || return 1
  manifest_service=$(jq -r '.service // empty' "$bundle/manifest.json") || return 1
  [[ "$manifest_service" == "$service_name" ]] || return 1
  [[ -f "$(service_binary "$service_name" "$bundle")" && -x "$(service_binary "$service_name" "$bundle")" && ! -L "$(service_binary "$service_name" "$bundle")" ]] || return 1
  [[ -f "$(service_bundle_config "$service_name" "$bundle")" && ! -L "$(service_bundle_config "$service_name" "$bundle")" ]] || return 1
  [[ -f "$(service_bundle_plist "$service_name" "$bundle")" && ! -L "$(service_bundle_plist "$service_name" "$bundle")" ]] || return 1
  if [[ "$service_name" == root ]]; then
    [[ -f "$(service_bundle_env "$service_name" "$bundle")" && ! -L "$(service_bundle_env "$service_name" "$bundle")" ]] || return 1
    [[ ! -e "$bundle/bin/cli-proxy-api-relay" && ! -e "$bundle/relay" ]] || return 1
  else
    [[ ! -e "$bundle/bin/root-proxy" && ! -e "$bundle/root" ]] || return 1
  fi
  verify_plist_arguments "$service_name" "$(service_bundle_plist "$service_name" "$bundle")" "$(service_binary "$service_name" "$bundle")" "$(service_bundle_config "$service_name" "$bundle")" || return 1
  verify_candidate_manifest "$service_name" "$bundle"
}

deployment_id() {
  local bundle=$1
  local identifier
  identifier=$(jq -r '.deployment_id // empty' "$bundle/manifest.json" 2>/dev/null)
  [[ -n "$identifier" ]] && print -r -- "$identifier" || basename "$bundle"
}

verify_state_shape() {
  [[ -f "$state_file" ]] || return 1
  jq -e \
    --arg root_config "$(service_runtime_config root)" \
    --arg root_env "$(service_runtime_env root)" \
    --arg root_plist "$root_plist" \
    --arg relay_config "$(service_runtime_config relay)" \
    --arg relay_plist "$relay_plist" \
    '.schema == "cliproxy-cutover-state-v1" and (.generation | type == "number") and .generation >= 1 and (.root.active.bundle | type == "string") and (.root.rollback.bundle | type == "string") and (.relay.active.bundle | type == "string") and (.relay.rollback.bundle | type == "string") and .root.runtime_config == $root_config and .root.runtime_env == $root_env and .root.installed_plist == $root_plist and .relay.runtime_config == $relay_config and .relay.installed_plist == $relay_plist' \
    "$state_file" >/dev/null
}

state_value() {
  jq -r "$1" "$state_file"
}

verify_snapshot_slot() {
  local service_name=$1
  local slot=$2
  local snapshot
  snapshot=$(state_value ".${service_name}.${slot}.snapshot_dir // empty")
  [[ -n "$snapshot" ]] || return 1
  [[ "$snapshot" == "$(canonical_snapshot "$service_name" "$snapshot")" ]] || return 1
  [[ -f "$snapshot/config.yaml" && -f "$snapshot/service.plist" ]] || return 1
  [[ "$(sha256_file "$snapshot/config.yaml")" == "$(state_value ".${service_name}.${slot}.config_sha256")" ]] || return 1
  [[ "$(sha256_file "$snapshot/service.plist")" == "$(state_value ".${service_name}.${slot}.plist_sha256")" ]] || return 1
  if [[ "$service_name" == root ]]; then
    [[ -f "$snapshot/.env" ]] || return 1
    [[ "$(sha256_file "$snapshot/.env")" == "$(state_value ".${service_name}.${slot}.env_sha256")" ]] || return 1
  fi
}

verify_state_slot_bundle() {
  local service_name=$1
  local slot=$2
  local bundle
  local binary
  local snapshot
  bundle=$(state_value ".${service_name}.${slot}.bundle")
  [[ "$bundle" == "$(canonical_bundle "$bundle")" ]] || return 1
  binary=$(service_binary "$service_name" "$bundle")
  [[ -x "$binary" ]] || return 1
  [[ "$(sha256_file "$binary")" == "$(state_value ".${service_name}.${slot}.binary_sha256")" ]] || return 1
  snapshot=$(state_value ".${service_name}.${slot}.snapshot_dir // empty")
  if [[ "$slot" == rollback || -n "$snapshot" ]]; then
    verify_snapshot_slot "$service_name" "$slot" || return 1
  fi
}

verify_live_state() {
  local service_name
  local active_bundle
  local configured_path
  verify_state_shape || return 1
  for service_name in root relay; do
    verify_state_slot_bundle "$service_name" active || return 1
    verify_state_slot_bundle "$service_name" rollback || return 1
    active_bundle=$(state_value ".${service_name}.active.bundle")
    [[ "$(job_program "$service_name")" == "$(service_binary "$service_name" "$active_bundle")" ]] || return 1
    verify_loaded_plist_parity "$service_name" || return 1
    configured_path=$(plutil -extract ProgramArguments.2 raw -o - "$(service_plist "$service_name")") || return 1
    [[ "$configured_path" == "$(service_runtime_config "$service_name")" ]] || return 1
    [[ "$(installed_working_dir "$service_name")" == "$(service_runtime_dir "$service_name")" ]] || return 1
    wait_health "$service_name" || return 1
  done
  [[ -n "$(listener_pid "$bridge_address")" ]]
}

make_snapshot() {
  local service_name=$1
  local snapshot=$2
  local config_source=$3
  local plist_source=$4
  local program=$5
  local env_source=${6:-}
  local normalize_paths=${7:-false}
  mkdir -p "$snapshot" || return 1
  chmod 700 "$snapshot" || return 1
  install -m 600 "$config_source" "$snapshot/config.yaml" || return 1
  if [[ "$normalize_paths" == true ]]; then
    normalize_plist "$plist_source" "$snapshot/service.plist" "$service_name" "$program" || return 1
  else
    install -m 600 "$plist_source" "$snapshot/service.plist" || return 1
  fi
  if [[ "$service_name" == root ]]; then
    [[ -f "$env_source" ]] || return 1
    install -m 600 "$env_source" "$snapshot/.env" || return 1
  fi
}

slot_json() {
  local service_name=$1
  local bundle=$2
  local config_file=$3
  local plist_file=$4
  local snapshot=${5:-}
  local env_file=${6:-}
  local legacy_mismatch=${7:-false}
  local env_hash=""
  [[ "$service_name" == root ]] && env_hash=$(sha256_file "$env_file")
  jq -n \
    --arg bundle "$bundle" \
    --arg deployment_id "$(deployment_id "$bundle")" \
    --arg binary_sha256 "$(sha256_file "$(service_binary "$service_name" "$bundle")")" \
    --arg config_sha256 "$(sha256_file "$config_file")" \
    --arg plist_sha256 "$(sha256_file "$plist_file")" \
    --arg env_sha256 "$env_hash" \
    --arg snapshot_dir "$snapshot" \
    --argjson legacy_manifest_mismatch "$legacy_mismatch" \
    '{bundle:$bundle,deployment_id:$deployment_id,binary_sha256:$binary_sha256,config_sha256:$config_sha256,plist_sha256:$plist_sha256,env_sha256:$env_sha256,snapshot_dir:(if $snapshot_dir == "" then null else $snapshot_dir end),legacy_manifest_mismatch:$legacy_manifest_mismatch}'
}

write_state_atomic() {
  local source_file=$1
  local temporary="${state_file}.next.$$"
  install -m 600 "$source_file" "$temporary" || return 1
  mv "$temporary" "$state_file"
}

write_receipt() {
  local receipt_service=$1
  local result=$2
  local detail=$3
  local pruned_file=${4:-}
  local destination_dir
  local base_name
  local json_temp
  local log_temp
  local pruned_json='[]'
  if [[ "$receipt_service" == failed ]]; then
    destination_dir="$receipt_root/failed"
    base_name=latest
  else
    destination_dir="$receipt_root/$receipt_service"
    base_name=current
  fi
  json_temp="$destination_dir/$base_name.json.next.$$"
  log_temp="$destination_dir/$base_name.log.next.$$"
  if [[ -n "$pruned_file" && -s "$pruned_file" ]]; then
    pruned_json=$(jq -R -s 'split("\n") | map(select(length > 0))' "$pruned_file") || return 1
  fi
  jq -n \
    --arg timestamp "$(timestamp)" \
    --arg service "$receipt_service" \
    --arg result "$result" \
    --arg detail "$detail" \
    --arg root_program "$(job_program root)" \
    --arg relay_program "$(job_program relay)" \
    --arg root_pid "$(listener_pid 127.0.0.1:8317)" \
    --arg relay_pid "$(listener_pid 127.0.0.1:8318)" \
    --arg bridge_pid "$(listener_pid "$bridge_address")" \
    --argjson generation "$(jq -r '.generation // 0' "$state_file" 2>/dev/null || print -r -- 0)" \
    --argjson pruned "$pruned_json" \
    '{timestamp:$timestamp,service:$service,result:$result,detail:$detail,generation:$generation,root_program:$root_program,relay_program:$relay_program,root_pid:$root_pid,relay_pid:$relay_pid,bridge_pid:$bridge_pid,pruned:$pruned}' >"$json_temp" || return 1
  print -r -- "$(timestamp) $result: $detail" >"$log_temp" || return 1
  chmod 600 "$json_temp" "$log_temp"
  mv "$json_temp" "$destination_dir/$base_name.json" && mv "$log_temp" "$destination_dir/$base_name.log"
}

transaction_begin() {
  local transaction="$transaction_root/$1"
  mkdir "$transaction" || return 1
  chmod 700 "$transaction"
  print -r -- "$transaction"
}

backup_current_service() {
  local service_name=$1
  local transaction=$2
  local before="$transaction/before-$service_name"
  local config_source
  local work_dir
  mkdir "$before" || return 1
  verify_loaded_plist_parity "$service_name" || return 1
  config_source=$(installed_config "$service_name") || return 1
  [[ -f "$config_source" ]] || return 1
  install -m 600 "$config_source" "$before/config.yaml" || return 1
  install -m 600 "$(service_plist "$service_name")" "$before/service.plist" || return 1
  print -r -- "$(job_program "$service_name")" >"$before/program" || return 1
  chmod 600 "$before/program"
  if [[ "$service_name" == root ]]; then
    work_dir=$(installed_working_dir root) || return 1
    [[ -f "$work_dir/.env" ]] || return 1
    install -m 600 "$work_dir/.env" "$before/.env" || return 1
  fi
}

stage_service() {
  local service_name=$1
  local transaction=$2
  local bundle=$3
  local staged="$transaction/staged-$service_name"
  mkdir "$staged" || return 1
  install -m 600 "$(service_bundle_config "$service_name" "$bundle")" "$staged/config.yaml" || return 1
  normalize_plist "$(service_bundle_plist "$service_name" "$bundle")" "$staged/service.plist" "$service_name" "$(service_binary "$service_name" "$bundle")" || return 1
  if [[ "$service_name" == root ]]; then
    install -m 600 "$(service_bundle_env "$service_name" "$bundle")" "$staged/.env" || return 1
  fi
}

install_staged_service() {
  local service_name=$1
  local transaction=$2
  local staged="$transaction/staged-$service_name"
  if [[ "$service_name" == root ]] && root_has_inbound_connection; then
    return 75
  fi
  stop_service "$service_name" || return 1
  copy_atomic "$staged/config.yaml" "$(service_runtime_config "$service_name")" 600 || return 1
  if [[ "$service_name" == root ]]; then
    copy_atomic "$staged/.env" "$(service_runtime_env root)" 600 || return 1
  fi
  install -m 600 "$staged/service.plist" "$(service_plist "$service_name")" || return 1
  bootstrap_service "$service_name" "$(service_plist "$service_name")" || return 1
  wait_health "$service_name" || return 1
  verify_loaded_plist_parity "$service_name" || return 1
  [[ "$(job_program "$service_name")" == "$(plutil -extract ProgramArguments.0 raw -o - "$staged/service.plist")" ]]
}

restore_before_service() {
  local service_name=$1
  local transaction=$2
  local before="$transaction/before-$service_name"
  stop_service "$service_name" || true
  copy_atomic "$before/config.yaml" "$(service_runtime_config "$service_name")" 600 || return 1
  if [[ "$service_name" == root ]]; then
    copy_atomic "$before/.env" "$(service_runtime_env root)" 600 || return 1
  fi
  install -m 600 "$before/service.plist" "$(service_plist "$service_name")" || return 1
  bootstrap_service "$service_name" "$(service_plist "$service_name")" || return 1
  wait_health "$service_name" || return 1
  verify_loaded_plist_parity "$service_name" || return 1
  [[ "$(job_program "$service_name")" == "$(<"$before/program")" ]]
}

restore_before_pair() {
  local transaction=$1
  local expected_bridge_pid=$2
  local root_restored=false
  local relay_restored=false
  restore_before_service root "$transaction" && root_restored=true
  restore_before_service relay "$transaction" && relay_restored=true
  [[ "$root_restored" == true && "$relay_restored" == true && "$(listener_pid "$bridge_address")" == "$expected_bridge_pid" ]]
}

write_journal() {
  local transaction=$1
  local phase=$2
  local journal="$transaction/journal.json.next"
  jq -n --arg phase "$phase" --arg timestamp "$(timestamp)" --argjson generation "$(jq -r '.generation // 0' "$state_file" 2>/dev/null || print -r -- 0)" '{phase:$phase,timestamp:$timestamp,state_generation:$generation}' >"$journal" || return 1
  chmod 600 "$journal"
  mv "$journal" "$transaction/journal.json"
}

state_keep_bundles() {
  jq -r '[.root.active.bundle,.root.rollback.bundle,.relay.active.bundle,.relay.rollback.bundle] | unique[]' "$state_file" | sort
}

state_keep_snapshots() {
  jq -r '[.root.active.snapshot_dir,.root.rollback.snapshot_dir,.relay.active.snapshot_dir,.relay.rollback.snapshot_dir] | map(select(. != null)) | unique[]' "$state_file" | sort
}

gc_candidates() {
  local output_file=$1
  local keep_file=$2
  local keep_snapshot_file=$3
  local bundle_dir
  local service_name
  local snapshot_dir
  local loose_file
  state_keep_bundles >"$keep_file" || return 1
  state_keep_snapshots >"$keep_snapshot_file" || return 1
  : >"$output_file"
  while IFS= read -r bundle_dir; do
    [[ -L "$bundle_dir" ]] && return 1
    rg -Fxq "$bundle_dir" "$keep_file" || print -r -- "$bundle_dir" >>"$output_file"
  done < <(find "$cutover_root" -mindepth 1 -maxdepth 1 -type d -name '20*T*Z-*' -print | sort)
  for service_name in root relay; do
    while IFS= read -r snapshot_dir; do
      [[ -L "$snapshot_dir" ]] && return 1
      rg -Fxq "$snapshot_dir" "$keep_snapshot_file" || print -r -- "$snapshot_dir" >>"$output_file"
    done < <(find "$snapshot_root/$service_name" -mindepth 1 -maxdepth 1 -type d -print | sort)
  done
  while IFS= read -r loose_file; do
    [[ -L "$loose_file" ]] && return 1
    print -r -- "$loose_file" >>"$output_file"
  done < <(find "$cutover_root" -mindepth 1 -maxdepth 1 -type f -name 'activate-*.zsh' -print | sort)
  [[ ! -d "$cutover_root/activation-logs" ]] || print -r -- "$cutover_root/activation-logs" >>"$output_file"
}

restore_quarantine() {
  local quarantine=$1
  local entry
  [[ -d "$quarantine" ]] || return 0
  while IFS= read -r entry; do mv "$entry" "$cutover_root/${entry:t}" || return 1; done < <(find "$quarantine/bundles" -mindepth 1 -maxdepth 1 -print 2>/dev/null | sort)
  while IFS= read -r entry; do mv "$entry" "$cutover_root/${entry:t}" || return 1; done < <(find "$quarantine/loose" -mindepth 1 -maxdepth 1 -print 2>/dev/null | sort)
  while IFS= read -r entry; do mv "$entry" "$snapshot_root/root/${entry:t}" || return 1; done < <(find "$quarantine/snapshots/root" -mindepth 1 -maxdepth 1 -print 2>/dev/null | sort)
  while IFS= read -r entry; do mv "$entry" "$snapshot_root/relay/${entry:t}" || return 1; done < <(find "$quarantine/snapshots/relay" -mindepth 1 -maxdepth 1 -print 2>/dev/null | sort)
  if [[ -d "$quarantine/activation-logs" ]]; then mv "$quarantine/activation-logs" "$cutover_root/activation-logs" || return 1; fi
  rm -r "$quarantine"
}

run_gc() {
  local dry_run=$1
  local transaction=${2:-}
  local work_dir
  local candidates_file
  local keep_file
  local keep_snapshot_file
  local quarantine
  local item
  if [[ -n "$transaction" ]]; then work_dir=$transaction; else work_dir="$transaction_root/gc-$(run_id)-$$"; mkdir "$work_dir" || return 1; chmod 700 "$work_dir"; fi
  candidates_file="$work_dir/pruned.txt"
  keep_file="$work_dir/keep.txt"
  keep_snapshot_file="$work_dir/keep-snapshots.txt"
  gc_candidates "$candidates_file" "$keep_file" "$keep_snapshot_file" || return 1
  if [[ "$dry_run" == true ]]; then
    if [[ -s "$candidates_file" ]]; then print -r -- "GC candidates:"; sed 's/^/  /' "$candidates_file"; else print -r -- "GC candidates: none"; fi
    [[ -n "$transaction" ]] || rm -r "$work_dir"
    return 0
  fi
  [[ -s "$candidates_file" ]] || { [[ -n "$transaction" ]] || rm -r "$work_dir"; return 0; }
  quarantine="$trash_root/cliproxyapi-cutover-gc-$(run_id)-$$"
  [[ ! -e "$quarantine" ]] || return 1
  [[ "$(stat -f %d "$cutover_root")" == "$(stat -f %d "$trash_root")" ]] || return 1
  mkdir -m 700 "$quarantine" "$quarantine/bundles" "$quarantine/loose" "$quarantine/snapshots" "$quarantine/snapshots/root" "$quarantine/snapshots/relay" || return 1
  while IFS= read -r item; do
    case "$item" in
      "$cutover_root"/20*T*Z-*) mv "$item" "$quarantine/bundles/${item:t}" || { restore_quarantine "$quarantine"; return 1; } ;;
      "$cutover_root"/activate-*.zsh) mv "$item" "$quarantine/loose/${item:t}" || { restore_quarantine "$quarantine"; return 1; } ;;
      "$snapshot_root"/root/*) mv "$item" "$quarantine/snapshots/root/${item:t}" || { restore_quarantine "$quarantine"; return 1; } ;;
      "$snapshot_root"/relay/*) mv "$item" "$quarantine/snapshots/relay/${item:t}" || { restore_quarantine "$quarantine"; return 1; } ;;
      "$cutover_root"/activation-logs) mv "$item" "$quarantine/activation-logs" || { restore_quarantine "$quarantine"; return 1; } ;;
      *) restore_quarantine "$quarantine"; return 1 ;;
    esac
  done <"$candidates_file"
  if ! verify_live_state; then restore_quarantine "$quarantine" || true; return 1; fi
  rm -r "$quarantine" || return 1
  [[ -n "$transaction" ]] || rm -r "$work_dir"
}

build_state_after_activation() {
  local service_name=$1
  local bundle=$2
  local transaction=$3
  local snapshot=$4
  local active_json="$transaction/active.json"
  local rollback_json="$transaction/rollback.json"
  local state_next="$transaction/state.next.json"
  local previous_bundle
  previous_bundle=$(state_value ".${service_name}.active.bundle")
  slot_json "$service_name" "$bundle" "$transaction/staged-$service_name/config.yaml" "$transaction/staged-$service_name/service.plist" "" "$transaction/staged-$service_name/.env" false >"$active_json" || return 1
  slot_json "$service_name" "$previous_bundle" "$snapshot/config.yaml" "$snapshot/service.plist" "$snapshot" "$snapshot/.env" false >"$rollback_json" || return 1
  jq --arg service "$service_name" --arg timestamp "$(timestamp)" --slurpfile active "$active_json" --slurpfile rollback "$rollback_json" '.generation += 1 | .updated_at = $timestamp | .[$service].active = $active[0] | .[$service].rollback = $rollback[0]' "$state_file" >"$state_next" || return 1
  write_state_atomic "$state_next"
}

activate_service() {
  local service_name=$1
  local bundle=$2
  local transaction
  local generation
  local snapshot
  local other_service
  local other_pid_before
  local bridge_pid_before
  local install_status=0
  if [[ "$service_name" == root ]] && root_has_inbound_connection; then
    fail "Root has established inbound clients; activate it from a quiet external terminal."
    return 1
  fi
  transaction=$(transaction_begin "activate-$(run_id)-$service_name-$$") || return 1
  generation=$(( $(state_value '.generation') + 1 ))
  snapshot="$snapshot_root/$service_name/$generation"
  other_service=$([[ "$service_name" == root ]] && print -r -- relay || print -r -- root)
  other_pid_before=$(listener_pid "$(service_listener "$other_service")")
  bridge_pid_before=$(listener_pid "$bridge_address")
  backup_current_service "$service_name" "$transaction" || { rm -r "$transaction"; return 1; }
  stage_service "$service_name" "$transaction" "$bundle" || { rm -r "$transaction"; return 1; }
  make_snapshot "$service_name" "$snapshot" "$transaction/before-$service_name/config.yaml" "$transaction/before-$service_name/service.plist" "$(<"$transaction/before-$service_name/program")" "$transaction/before-$service_name/.env" || { rm -r "$transaction"; return 1; }
  if ! write_journal "$transaction" prepared; then
    rm -r "$snapshot" "$transaction" 2>/dev/null || true
    return 1
  fi
  if [[ "$service_name" == root ]] && root_has_inbound_connection; then
    rm -r "$snapshot" "$transaction" 2>/dev/null || true
    fail "Root acquired an inbound client during activation preparation; no service changed."
    return 1
  fi
  install_staged_service "$service_name" "$transaction" || install_status=$?
  if (( install_status == 75 )); then
    rm -r "$snapshot" "$transaction" 2>/dev/null || true
    fail "Root acquired an inbound client immediately before stop; no service changed."
    return 1
  fi
  if (( install_status != 0 )) || [[ "$(listener_pid "$(service_listener "$other_service")")" != "$other_pid_before" ]] || [[ "$(listener_pid "$bridge_address")" != "$bridge_pid_before" ]]; then
    if restore_before_service "$service_name" "$transaction" && [[ "$(listener_pid "$(service_listener "$other_service")")" == "$other_pid_before" ]] && [[ "$(listener_pid "$bridge_address")" == "$bridge_pid_before" ]]; then
      rm -r "$snapshot" "$transaction" 2>/dev/null || true
      write_receipt failed activation_failed "$service_name activation failed and pre-run files were restored." || true
    else
      write_receipt failed activation_recovery_failed "$service_name activation failed; recovery is incomplete and the transaction was retained." || true
    fi
    return 1
  fi
  if ! write_journal "$transaction" switched; then
    write_receipt failed activation_journal_failed "$service_name switched but its journal could not advance; the transaction was retained." || true
    return 1
  fi
  if ! build_state_after_activation "$service_name" "$bundle" "$transaction" "$snapshot"; then
    if restore_before_service "$service_name" "$transaction" && [[ "$(listener_pid "$(service_listener "$other_service")")" == "$other_pid_before" ]] && [[ "$(listener_pid "$bridge_address")" == "$bridge_pid_before" ]]; then
      rm -r "$snapshot" "$transaction" 2>/dev/null || true
      write_receipt failed state_commit_failed "$service_name switched but state commit failed; pre-run files were restored." || true
    else
      write_receipt failed state_commit_recovery_failed "$service_name state commit failed; recovery is incomplete and the transaction was retained." || true
    fi
    return 1
  fi
  write_journal "$transaction" committed || return 1
  if [[ "$prune_after_success" == true ]] && ! run_gc false "$transaction"; then
    write_receipt "$service_name" activated_cleanup_failed "$service_name activated; automatic cleanup failed and can be retried with --gc." || true
    rm -r "$transaction" 2>/dev/null || true
    return 3
  fi
  if ! write_receipt "$service_name" activated "$service_name activated with one immediate rollback generation." "$transaction/pruned.txt"; then
    rm -r "$transaction" 2>/dev/null || true
    return 3
  fi
  rm -r "$transaction"
}

rollback_service() {
  local service_name=$1
  local transaction
  local rollback_snapshot
  local generation
  local new_snapshot
  local old_active
  local old_rollback
  local new_rollback
  local new_active
  local state_next
  local other_service
  local other_pid_before
  local bridge_pid_before
  local install_status=0
  if [[ "$service_name" == root ]] && root_has_inbound_connection; then
    fail "Root has established inbound clients; roll it back from a quiet external terminal."
    return 1
  fi
  transaction=$(transaction_begin "rollback-$(run_id)-$service_name-$$") || return 1
  other_service=$([[ "$service_name" == root ]] && print -r -- relay || print -r -- root)
  other_pid_before=$(listener_pid "$(service_listener "$other_service")")
  bridge_pid_before=$(listener_pid "$bridge_address")
  rollback_snapshot=$(state_value ".${service_name}.rollback.snapshot_dir")
  generation=$(( $(state_value '.generation') + 1 ))
  new_snapshot="$snapshot_root/$service_name/$generation"
  backup_current_service "$service_name" "$transaction" || { rm -r "$transaction"; return 1; }
  mkdir "$transaction/staged-$service_name" || return 1
  install -m 600 "$rollback_snapshot/config.yaml" "$transaction/staged-$service_name/config.yaml" || return 1
  install -m 600 "$rollback_snapshot/service.plist" "$transaction/staged-$service_name/service.plist" || return 1
  if [[ "$service_name" == root ]]; then
    install -m 600 "$rollback_snapshot/.env" "$transaction/staged-$service_name/.env" || return 1
  fi
  make_snapshot "$service_name" "$new_snapshot" "$transaction/before-$service_name/config.yaml" "$transaction/before-$service_name/service.plist" "$(<"$transaction/before-$service_name/program")" "$transaction/before-$service_name/.env" || return 1
  if ! write_journal "$transaction" prepared; then
    rm -r "$new_snapshot" "$transaction" 2>/dev/null || true
    return 1
  fi
  if [[ "$service_name" == root ]] && root_has_inbound_connection; then
    rm -r "$new_snapshot" "$transaction" 2>/dev/null || true
    fail "Root acquired an inbound client during rollback preparation; no service changed."
    return 1
  fi
  install_staged_service "$service_name" "$transaction" || install_status=$?
  if (( install_status == 75 )); then
    rm -r "$new_snapshot" "$transaction" 2>/dev/null || true
    fail "Root acquired an inbound client immediately before stop; no service changed."
    return 1
  fi
  if (( install_status != 0 )) || [[ "$(listener_pid "$(service_listener "$other_service")")" != "$other_pid_before" ]] || [[ "$(listener_pid "$bridge_address")" != "$bridge_pid_before" ]]; then
    if restore_before_service "$service_name" "$transaction" && [[ "$(listener_pid "$(service_listener "$other_service")")" == "$other_pid_before" ]] && [[ "$(listener_pid "$bridge_address")" == "$bridge_pid_before" ]]; then
      rm -r "$new_snapshot" "$transaction" 2>/dev/null || true
      write_receipt failed rollback_failed "$service_name rollback failed and pre-run files were restored." || true
    else
      write_receipt failed rollback_recovery_failed "$service_name rollback failed; recovery is incomplete and the transaction was retained." || true
    fi
    return 1
  fi
  old_active="$transaction/old-active.json"
  old_rollback="$transaction/old-rollback.json"
  new_active="$transaction/new-active.json"
  new_rollback="$transaction/new-rollback.json"
  state_next="$transaction/state.next.json"
  if ! jq ".${service_name}.active" "$state_file" >"$old_active" || ! jq ".${service_name}.rollback" "$state_file" >"$old_rollback" || ! jq '.snapshot_dir = null' "$old_rollback" >"$new_active" || ! slot_json "$service_name" "$(jq -r '.bundle' "$old_active")" "$new_snapshot/config.yaml" "$new_snapshot/service.plist" "$new_snapshot" "$new_snapshot/.env" false >"$new_rollback" || ! jq --arg service "$service_name" --arg timestamp "$(timestamp)" --slurpfile active "$new_active" --slurpfile rollback "$new_rollback" '.generation += 1 | .updated_at = $timestamp | .[$service].active = $active[0] | .[$service].rollback = $rollback[0]' "$state_file" >"$state_next" || ! write_state_atomic "$state_next"; then
    if restore_before_service "$service_name" "$transaction" && [[ "$(listener_pid "$(service_listener "$other_service")")" == "$other_pid_before" ]] && [[ "$(listener_pid "$bridge_address")" == "$bridge_pid_before" ]]; then
      rm -r "$new_snapshot" "$transaction" 2>/dev/null || true
      write_receipt failed rollback_state_failed "$service_name rollback state commit failed; pre-run files were restored." || true
    else
      write_receipt failed rollback_state_recovery_failed "$service_name rollback state commit failed; recovery is incomplete and the transaction was retained." || true
    fi
    return 1
  fi
  write_journal "$transaction" committed || return 1
  if [[ "$prune_after_success" == true ]] && ! run_gc false "$transaction"; then
    write_receipt "$service_name" rolled_back_cleanup_failed "$service_name rolled back; automatic cleanup failed and can be retried with --gc." || true
    rm -r "$transaction" 2>/dev/null || true
    return 3
  fi
  if ! write_receipt "$service_name" rolled_back "$service_name active and rollback generations were swapped." "$transaction/pruned.txt"; then
    rm -r "$transaction" 2>/dev/null || true
    return 3
  fi
  rm -r "$transaction"
}

legacy_manifest_mismatch() {
  if (cd "$1" && shasum -a 256 -c manifest.sha256 >/dev/null 2>&1); then print -r -- false; else print -r -- true; fi
}

migration_slot_files() {
  slot_json "$1" "$2" "$3" "$4" "$5" "${6:-}" "$(legacy_manifest_mismatch "$2")"
}

migrate_state() {
  local root_active_bundle
  local root_rollback_bundle
  local relay_active_bundle
  local relay_rollback_bundle
  local transaction
  local root_pid_before
  local relay_pid_after
  local bridge_pid_before
  local root_install_status=0
  local root_rollback_snapshot="$snapshot_root/root/1"
  local relay_rollback_snapshot="$snapshot_root/relay/1"
  local root_active_json
  local root_rollback_json
  local relay_active_json
  local relay_rollback_json
  local state_next
  [[ ! -e "$state_file" ]] || return 1
  root_active_bundle=$(canonical_bundle "$root_active") || return 1
  root_rollback_bundle=$(canonical_bundle "$root_rollback") || return 1
  relay_active_bundle=$(canonical_bundle "$relay_active") || return 1
  relay_rollback_bundle=$(canonical_bundle "$relay_rollback") || return 1
  [[ "$(job_program root)" == "$(service_binary root "$root_active_bundle")" ]] || return 1
  [[ "$(job_program relay)" == "$(service_binary relay "$relay_active_bundle")" ]] || return 1
  (cd "$root_rollback_bundle" && shasum -a 256 -c manifest.sha256 >/dev/null) || return 1
  (cd "$relay_rollback_bundle" && shasum -a 256 -c manifest.sha256 >/dev/null) || return 1
  wait_health root && wait_health relay || return 1
  if root_has_inbound_connection; then
    fail "Root has established inbound clients; migrate from a quiet external terminal."
    return 1
  fi
  transaction=$(transaction_begin "migrate-$(run_id)-$$") || return 1
  backup_current_service root "$transaction" && backup_current_service relay "$transaction" || { rm -r "$transaction"; return 1; }
  root_pid_before=$(listener_pid 127.0.0.1:8317)
  bridge_pid_before=$(listener_pid "$bridge_address")
  mkdir "$transaction/staged-root" "$transaction/staged-relay" || return 1
  install -m 600 "$transaction/before-root/config.yaml" "$transaction/staged-root/config.yaml" || return 1
  install -m 600 "$transaction/before-root/.env" "$transaction/staged-root/.env" || return 1
  normalize_plist "$transaction/before-root/service.plist" "$transaction/staged-root/service.plist" root "$(service_binary root "$root_active_bundle")" || return 1
  install -m 600 "$transaction/before-relay/config.yaml" "$transaction/staged-relay/config.yaml" || return 1
  normalize_plist "$transaction/before-relay/service.plist" "$transaction/staged-relay/service.plist" relay "$(service_binary relay "$relay_active_bundle")" || return 1
  make_snapshot root "$root_rollback_snapshot" "$(service_bundle_config root "$root_rollback_bundle")" "$(service_bundle_plist root "$root_rollback_bundle")" "$(service_binary root "$root_rollback_bundle")" "$(service_bundle_env root "$root_rollback_bundle")" true || return 1
  make_snapshot relay "$relay_rollback_snapshot" "$(service_bundle_config relay "$relay_rollback_bundle")" "$(service_bundle_plist relay "$relay_rollback_bundle")" "$(service_binary relay "$relay_rollback_bundle")" "" true || return 1
  if ! write_journal "$transaction" prepared; then
    rm -r "$root_rollback_snapshot" "$relay_rollback_snapshot" "$transaction" 2>/dev/null || true
    return 1
  fi
  if ! install_staged_service relay "$transaction" || [[ "$(listener_pid 127.0.0.1:8317)" != "$root_pid_before" ]] || [[ "$(listener_pid "$bridge_address")" != "$bridge_pid_before" ]]; then
    if restore_before_service relay "$transaction" && [[ "$(listener_pid 127.0.0.1:8317)" == "$root_pid_before" ]] && [[ "$(listener_pid "$bridge_address")" == "$bridge_pid_before" ]]; then
      rm -r "$root_rollback_snapshot" "$relay_rollback_snapshot" "$transaction" 2>/dev/null || true
    else
      write_receipt failed migration_relay_recovery_failed "Relay migration failed; recovery is incomplete and the transaction was retained." || true
    fi
    return 1
  fi
  relay_pid_after=$(listener_pid 127.0.0.1:8318)
  if root_has_inbound_connection; then
    if restore_before_service relay "$transaction" && [[ "$(listener_pid 127.0.0.1:8317)" == "$root_pid_before" ]] && [[ "$(listener_pid "$bridge_address")" == "$bridge_pid_before" ]]; then
      rm -r "$root_rollback_snapshot" "$relay_rollback_snapshot" "$transaction" 2>/dev/null || true
      fail "Root acquired an inbound client during migration preparation; Root was not restarted."
    else
      write_receipt failed migration_relay_recovery_failed "Root acquired an inbound client and Relay recovery failed; the transaction was retained." || true
    fi
    return 1
  fi
  install_staged_service root "$transaction" || root_install_status=$?
  if (( root_install_status == 75 )); then
    if restore_before_service relay "$transaction" && [[ "$(listener_pid 127.0.0.1:8317)" == "$root_pid_before" ]] && [[ "$(listener_pid "$bridge_address")" == "$bridge_pid_before" ]]; then
      rm -r "$root_rollback_snapshot" "$relay_rollback_snapshot" "$transaction" 2>/dev/null || true
      fail "Root acquired an inbound client immediately before stop; Root was not restarted."
    else
      write_receipt failed migration_relay_recovery_failed "Root became busy and Relay recovery failed; the transaction was retained." || true
    fi
    return 1
  fi
  if (( root_install_status != 0 )) || [[ "$(listener_pid 127.0.0.1:8317)" == "$root_pid_before" ]] || [[ "$(listener_pid 127.0.0.1:8318)" != "$relay_pid_after" ]] || [[ "$(listener_pid "$bridge_address")" != "$bridge_pid_before" ]]; then
    if restore_before_pair "$transaction" "$bridge_pid_before"; then
      rm -r "$root_rollback_snapshot" "$relay_rollback_snapshot" "$transaction" 2>/dev/null || true
    else
      write_receipt failed migration_recovery_failed "Root migration failed; recovery is incomplete and the transaction was retained." || true
    fi
    return 1
  fi
  root_active_json="$transaction/root-active.json"
  root_rollback_json="$transaction/root-rollback.json"
  relay_active_json="$transaction/relay-active.json"
  relay_rollback_json="$transaction/relay-rollback.json"
  state_next="$transaction/state.next.json"
  if ! migration_slot_files root "$root_active_bundle" "$transaction/staged-root/config.yaml" "$transaction/staged-root/service.plist" "" "$transaction/staged-root/.env" >"$root_active_json" || ! migration_slot_files root "$root_rollback_bundle" "$root_rollback_snapshot/config.yaml" "$root_rollback_snapshot/service.plist" "$root_rollback_snapshot" "$root_rollback_snapshot/.env" >"$root_rollback_json" || ! migration_slot_files relay "$relay_active_bundle" "$transaction/staged-relay/config.yaml" "$transaction/staged-relay/service.plist" "" >"$relay_active_json" || ! migration_slot_files relay "$relay_rollback_bundle" "$relay_rollback_snapshot/config.yaml" "$relay_rollback_snapshot/service.plist" "$relay_rollback_snapshot" >"$relay_rollback_json" || ! jq -n \
    --arg updated_at "$(timestamp)" \
    --arg root_config "$(service_runtime_config root)" \
    --arg root_env "$(service_runtime_env root)" \
    --arg root_plist "$root_plist" \
    --arg root_receipt "$receipt_root/root/current.json" \
    --arg relay_config "$(service_runtime_config relay)" \
    --arg relay_plist "$relay_plist" \
    --arg relay_receipt "$receipt_root/relay/current.json" \
    --slurpfile root_active "$root_active_json" \
    --slurpfile root_rollback "$root_rollback_json" \
    --slurpfile relay_active "$relay_active_json" \
    --slurpfile relay_rollback "$relay_rollback_json" \
    '{schema:"cliproxy-cutover-state-v1",generation:1,updated_at:$updated_at,root:{active:$root_active[0],rollback:$root_rollback[0],runtime_config:$root_config,runtime_env:$root_env,installed_plist:$root_plist,current_receipt:$root_receipt},relay:{active:$relay_active[0],rollback:$relay_rollback[0],runtime_config:$relay_config,installed_plist:$relay_plist,current_receipt:$relay_receipt}}' >"$state_next" || ! write_state_atomic "$state_next"; then
    if restore_before_pair "$transaction" "$bridge_pid_before"; then
      rm -r "$root_rollback_snapshot" "$relay_rollback_snapshot" "$transaction" 2>/dev/null || true
      write_receipt failed migration_state_failed "Migration state commit failed; pre-run files were restored." || true
    else
      write_receipt failed migration_state_recovery_failed "Migration state commit failed; recovery is incomplete and the transaction was retained." || true
    fi
    return 1
  fi
  write_journal "$transaction" committed || return 1
  if ! write_receipt root migrated "Root migrated to stable runtime config and bounded state." || ! write_receipt relay migrated "Relay migrated to stable runtime config and bounded state."; then
    rm -r "$transaction" 2>/dev/null || true
    return 3
  fi
  if [[ "$prune_after_success" == true ]] && ! run_gc false "$transaction"; then
    write_receipt failed migrated_cleanup_failed "Migration succeeded but cleanup requires --gc." || true
    rm -r "$transaction" 2>/dev/null || true
    return 3
  fi
  rm -r "$transaction"
}

status_output() {
  verify_state_shape || return 1
  jq '{schema,generation,updated_at,root:{active:.root.active.bundle,rollback:.root.rollback.bundle,runtime_config:.root.runtime_config,current_receipt:.root.current_receipt},relay:{active:.relay.active.bundle,rollback:.relay.rollback.bundle,runtime_config:.relay.runtime_config,current_receipt:.relay.current_receipt}}' "$state_file"
}

case "$operation" in
  status) status_output || exit 1 ;;
  preflight)
    ensure_no_pending_transaction || exit 1
    verify_state_shape || exit 1
    candidate=$(canonical_bundle "$candidate") || exit 1
    verify_candidate "$service" "$candidate" || exit 1
    verify_live_state || exit 1
    print -r -- "$(timestamp) $service candidate preflight passed; no services or state changed."
    ;;
  activate)
    initialize_layout || exit 1
    acquire_lock || exit 1
    ensure_no_pending_transaction || exit 1
    verify_state_shape && verify_live_state || exit 1
    candidate=$(canonical_bundle "$candidate") || exit 1
    verify_candidate "$service" "$candidate" || exit 1
    activate_service "$service" "$candidate"; exit $?
    ;;
  rollback)
    initialize_layout || exit 1
    acquire_lock || exit 1
    ensure_no_pending_transaction || exit 1
    verify_state_shape && verify_live_state || exit 1
    rollback_service "$service"; exit $?
    ;;
  migrate)
    initialize_layout || exit 1
    acquire_lock || exit 1
    ensure_no_pending_transaction || exit 1
    migrate_state; exit $?
    ;;
  gc-dry-run)
    initialize_layout || exit 1
    verify_state_shape && verify_live_state || exit 1
    run_gc true; exit $?
    ;;
  gc)
    initialize_layout || exit 1
    acquire_lock || exit 1
    ensure_no_pending_transaction || exit 1
    verify_state_shape && verify_live_state || exit 1
    run_gc false; exit $?
    ;;
esac
