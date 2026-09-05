#!/usr/bin/env bash

# Best-effort OCI hook for registering opted-in Podman network namespaces.
# Relay availability must never block a container lifecycle transition.

set -u

readonly RELAY_SOCKET="${RELAY_SOCKET:-/run/netns-mcast-relay/control.sock}"

log() {
  logger -t netns-mcast-relay-hook -- "$*" 2>/dev/null || true
}

request() {
  local method="$1"
  local url="$2"
  local body="${3:-}"
  local args=(
    --silent
    --show-error
    --fail
    --connect-timeout 1
    --max-time 3
    --unix-socket "$RELAY_SOCKET"
    --request "$method"
  )

  if [[ -n "$body" ]]; then
    args+=(
      --header 'Content-Type: application/json'
      --data "$body"
    )
  fi

  curl "${args[@]}" "$url" >/dev/null
}

state="$(cat)" || {
  log "could not read OCI state"
  exit 0
}

container_id="$(jq -r '.id // empty' <<<"$state" 2>/dev/null)"
status="$(jq -r '.status // empty' <<<"$state" 2>/dev/null)"

if [[ ! "$container_id" =~ ^[a-f0-9]{64}$ ]]; then
  log "ignoring invalid container ID"
  exit 0
fi

url="http://relay/v1/containers/${container_id}/namespace"

case "$status" in
  running)
    pid="$(jq -r '.pid // empty' <<<"$state" 2>/dev/null)"
    if [[ ! "$pid" =~ ^[1-9][0-9]*$ ]]; then
      log "container=${container_id} missing valid PID at poststart"
      exit 0
    fi

    body="$(jq -cn --arg path "/proc/${pid}/ns/net" '{path: $path}')"
    if request PUT "$url" "$body"; then
      log "container=${container_id} pid=${pid} namespace registration accepted"
    else
      log "container=${container_id} pid=${pid} namespace registration failed"
    fi
    ;;

  stopped)
    if request DELETE "$url"; then
      log "container=${container_id} namespace removal accepted"
    else
      log "container=${container_id} namespace removal failed"
    fi
    ;;
esac

exit 0
