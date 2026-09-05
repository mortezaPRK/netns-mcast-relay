#!/usr/bin/env bash

# Restore registrations for opted-in containers after relay startup or restart.

set -euo pipefail

readonly RELAY_SOCKET="${RELAY_SOCKET:-/run/netns-mcast-relay/control.sock}"
readonly PODMAN_SOCKET="${PODMAN_SOCKET:-${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/podman/podman.sock}"
readonly PODMAN_API_VERSION="${PODMAN_API_VERSION:-v1.0.0}"
readonly RELAY_ANNOTATION="${RELAY_ANNOTATION:-io.github.mortezaprk.netns-mcast-relay}"
readonly RELAY_ANNOTATION_VALUE="${RELAY_ANNOTATION_VALUE:-true}"
readonly RECONCILE_ATTEMPTS="${RECONCILE_ATTEMPTS:-30}"
readonly RECONCILE_RETRY_SECONDS="${RECONCILE_RETRY_SECONDS:-1}"

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "Missing required command: $1" >&2
    exit 1
  }
}

podman_api() {
  local path="$1"
  curl \
    --silent \
    --show-error \
    --fail \
    --connect-timeout 2 \
    --max-time 10 \
    --unix-socket "$PODMAN_SOCKET" \
    "http://podman/${PODMAN_API_VERSION}/libpod${path}"
}

relay_api() {
  local method="$1"
  local path="$2"
  local body="${3:-}"
  local args=(
    --silent
    --show-error
    --fail
    --connect-timeout 2
    --max-time 10
    --unix-socket "$RELAY_SOCKET"
    --request "$method"
  )

  if [[ -n "$body" ]]; then
    args+=(
      --header 'Content-Type: application/json'
      --data "$body"
    )
  fi

  curl "${args[@]}" "http://relay${path}"
}

wait_for_apis() {
  local attempt
  for ((attempt = 1; attempt <= RECONCILE_ATTEMPTS; attempt++)); do
    if [[ -S "$RELAY_SOCKET" && -S "$PODMAN_SOCKET" ]] &&
      relay_api GET /v1/healthz >/dev/null 2>&1 &&
      podman_api /info >/dev/null 2>&1; then
      return 0
    fi
    sleep "$RECONCILE_RETRY_SECONDS"
  done

  echo "Relay or Podman API unavailable after ${RECONCILE_ATTEMPTS} attempts" >&2
  return 1
}

main() {
  require_cmd curl
  require_cmd jq

  if [[ ! "$RECONCILE_ATTEMPTS" =~ ^[1-9][0-9]*$ ]]; then
    echo "RECONCILE_ATTEMPTS must be a positive integer" >&2
    exit 1
  fi
  if [[ ! "$RECONCILE_RETRY_SECONDS" =~ ^[1-9][0-9]*$ ]]; then
    echo "RECONCILE_RETRY_SECONDS must be a positive integer" >&2
    exit 1
  fi

  wait_for_apis

  local containers
  containers="$(podman_api '/containers/json?all=false')"

  local container_id inspect annotation running pid namespace body
  local registered=0
  local failed=0
  while IFS= read -r container_id; do
    [[ -n "$container_id" ]] || continue
    if [[ ! "$container_id" =~ ^[a-f0-9]{64}$ ]]; then
      echo "Ignoring invalid container ID returned by Podman" >&2
      continue
    fi

    if ! inspect="$(podman_api "/containers/${container_id}/json")"; then
      echo "Container disappeared during reconciliation: ${container_id}" >&2
      continue
    fi

    annotation="$(jq -r --arg key "$RELAY_ANNOTATION" '.Config.Annotations[$key] // empty' <<<"$inspect")"
    [[ "$annotation" == "$RELAY_ANNOTATION_VALUE" ]] || continue

    running="$(jq -r '.State.Running // false' <<<"$inspect")"
    pid="$(jq -r '.State.Pid // empty' <<<"$inspect")"
    if [[ "$running" != "true" || ! "$pid" =~ ^[1-9][0-9]*$ ]]; then
      continue
    fi

    namespace="/proc/${pid}/ns/net"
    if [[ ! -e "$namespace" ]]; then
      echo "Container namespace disappeared during reconciliation: ${container_id}" >&2
      continue
    fi

    body="$(jq -cn --arg path "$namespace" '{path: $path}')"
    if relay_api PUT "/v1/containers/${container_id}/namespace" "$body" >/dev/null; then
      registered=$((registered + 1))
    else
      echo "Failed to register container namespace: ${container_id}" >&2
      failed=$((failed + 1))
    fi
  done < <(jq -r '.[] | .Id // empty' <<<"$containers")

  echo "Restored ${registered} Podman relay registration(s)"
  if ((failed > 0)); then
    return 1
  fi
}

main "$@"
