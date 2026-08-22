#!/bin/bash

set -euo pipefail

RELAY_SOCKET="${RELAY_SOCKET:-${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/netns-relay.sock}"
RELAY_LABEL_KEY="${RELAY_LABEL_KEY:-homelab.relaymulticast}"
RELAY_LABEL_VALUE="${RELAY_LABEL_VALUE:-true}"
RESYNC_INTERVAL="${RESYNC_INTERVAL:-30}"

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "Missing required command: $1" >&2
    exit 1
  }
}

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

relay_get_namespaces() {
  curl --silent --show-error --fail \
    --unix-socket "$RELAY_SOCKET" \
    http://relay/v1/namespaces | jq -r '.namespaces[]?'
}

relay_add_namespace() {
  local ns_path="$1"
  curl --silent --show-error --fail \
    --unix-socket "$RELAY_SOCKET" \
    -X POST \
    -H 'Content-Type: application/json' \
    --data "$(jq -cn --arg path "$ns_path" '{path: $path}')" \
    http://relay/v1/namespaces/add >/dev/null
}

relay_remove_namespace() {
  local ns_path="$1"
  curl --silent --show-error --fail \
    --unix-socket "$RELAY_SOCKET" \
    -X POST \
    -H 'Content-Type: application/json' \
    --data "$(jq -cn --arg path "$ns_path" '{path: $path}')" \
    http://relay/v1/namespaces/remove >/dev/null
}

discover_labeled_container_ids() {
  local container_ids=()
  local network_ids=()
  local cid

  mapfile -t container_ids < <(podman ps --filter "label=${RELAY_LABEL_KEY}=${RELAY_LABEL_VALUE}" --format '{{.ID}}' || true)
  mapfile -t network_ids < <(podman network ls --filter "label=${RELAY_LABEL_KEY}=${RELAY_LABEL_VALUE}" --format '{{.ID}}' || true)

  declare -A seen=()
  for cid in "${container_ids[@]:-}"; do
    [[ -n "$cid" ]] || continue
    seen["$cid"]=1
  done

  local net_id
  for net_id in "${network_ids[@]:-}"; do
    [[ -n "$net_id" ]] || continue
    while IFS= read -r attached; do
      [[ -n "$attached" ]] || continue
      seen["$attached"]=1
    done < <(podman network inspect "$net_id" | jq -r '.[0].containers // .[0].Containers // {} | keys[]?')
  done

  printf '%s\n' "${!seen[@]}" | sort
}

discover_desired_namespaces() {
  local cid pid ns_path
  while IFS= read -r cid; do
    [[ -n "$cid" ]] || continue
    pid="$(podman inspect --format '{{.State.Pid}}' "$cid" 2>/dev/null || true)"
    [[ -n "$pid" && "$pid" != "0" ]] || continue
    ns_path="/proc/${pid}/ns/net"
    [[ -e "$ns_path" ]] || continue
    echo "$ns_path"
  done < <(discover_labeled_container_ids)
}

reconcile_once() {
  local desired current

  declare -A desired_set=()
  declare -A current_set=()

  while IFS= read -r desired; do
    [[ -n "$desired" ]] || continue
    desired_set["$desired"]=1
  done < <(discover_desired_namespaces)

  while IFS= read -r current; do
    [[ -n "$current" ]] || continue
    current_set["$current"]=1
  done < <(relay_get_namespaces)

  for ns_path in "${!desired_set[@]}"; do
    if [[ -z "${current_set[$ns_path]:-}" ]]; then
      log "Adding namespace: $ns_path"
      relay_add_namespace "$ns_path"
    fi
  done

  for ns_path in "${!current_set[@]}"; do
    if [[ -z "${desired_set[$ns_path]:-}" ]]; then
      log "Removing namespace: $ns_path"
      relay_remove_namespace "$ns_path"
    fi
  done
}

start_event_monitor() {
  local flag_file="$1"
  podman events --format json 2>/dev/null | while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    local event_type
    event_type="$(jq -r '.Type // .type // empty' <<<"$line" 2>/dev/null || true)"
    if [[ "$event_type" == "container" || "$event_type" == "network" || -z "$event_type" ]]; then
      : >"$flag_file"
    fi
  done
}

main() {
  require_cmd curl
  require_cmd jq
  require_cmd podman

  if [[ ! -S "$RELAY_SOCKET" ]]; then
    echo "Relay control socket not found: $RELAY_SOCKET" >&2
    exit 1
  fi

  local tmpdir
  tmpdir="$(mktemp -d)"
  local event_flag="$tmpdir/events.flag"

  cleanup() {
    trap - EXIT INT TERM
    if [[ -n "${event_pid:-}" ]]; then
      kill "$event_pid" >/dev/null 2>&1 || true
    fi
    rm -rf "$tmpdir"
  }
  trap cleanup EXIT INT TERM

  start_event_monitor "$event_flag" &
  event_pid=$!

  log "Initial reconcile"
  reconcile_once

  local seconds_since_resync=0
  while true; do
    if [[ -f "$event_flag" ]]; then
      rm -f "$event_flag"
      log "Podman event detected; reconciling"
      reconcile_once || log "Reconcile after event failed; will retry"
      seconds_since_resync=0
      continue
    fi

    sleep 1
    seconds_since_resync=$((seconds_since_resync + 1))
    if (( seconds_since_resync >= RESYNC_INTERVAL )); then
      log "Periodic reconcile"
      reconcile_once || log "Periodic reconcile failed; will retry"
      seconds_since_resync=0
    fi
  done
}

main "$@"
