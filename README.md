# netns-mcast-relay

A lightweight multicast relay between Linux network namespaces.

## Overview

`netns-mcast-relay` enables multicast traffic (mDNS, SSDP) to flow between Linux network namespaces, particularly useful for rootless container environments like Podman with pasta.

## Features

- Relay multicast packets between two network namespaces
- Support for configurable multicast groups
- Self-echo suppression so relayed packets can't loop back through the multicast loopback that real local listeners depend on
- Channel-based architecture for extensibility
- Structured logging with slog

## Installation

```bash
go install github.com/mortezaPRK/netns-mcast-relay/cmd/relay@latest
```

### Grant Required Capability

Relay needs `CAP_SYS_ADMIN` to switch network namespaces and `CAP_NET_RAW` to
preserve source address, source port, and TTL. Instead of using sudo, grant
capabilities once:

```bash
sudo setcap cap_sys_admin,cap_net_raw+ep $(which relay)
# Or if built locally:
sudo setcap cap_sys_admin,cap_net_raw+ep ./relay
```

**Note**: Both capabilities are security-sensitive. See [ROOTLESS.md](ROOTLESS.md) for security considerations and hardening recommendations.

## Usage

```bash
relay \
    --ns=/proc/1/ns/net \
    --ns=/proc/12345/ns/net \
    --group=224.0.0.251:5353 \
    --group=239.255.255.250:1900 \
    --verbose
```

### Flags

- `--ns`: Path to namespace (can be specified multiple times for initial namespaces)
- `--group`: Multicast group address and port (can be specified multiple times)
- `--verbose`: Enable debug logging
- `--control-socket`: Unix socket path for runtime namespace control API (optional)
- `--control-timeout`: Timeout for control API operations

### Default Groups

If no groups are specified, the relay defaults to:
- mDNS: `224.0.0.251:5353`
- SSDP: `239.255.255.250:1900`

## Example: Home Assistant with Podman

```bash
# Start Home Assistant container (rootless)
podman run -d --name home-assistant homeassistant/home-assistant:latest

# Get container PID
CONTAINER_PID=$(podman inspect home-assistant --format '{{.State.Pid}}')

# Grant capability to relay (one-time setup)
sudo setcap cap_sys_admin,cap_net_raw+ep ./relay

# Start relay (no sudo needed!)
./relay --ns=/proc/1/ns/net --ns=/proc/${CONTAINER_PID}/ns/net --verbose
```

## Dynamic Runtime Control (Unix Socket)

Start relay with control API enabled:

```bash
./relay \
  --group=224.0.0.251:5353 \
  --control-socket="${XDG_RUNTIME_DIR}/netns-relay.sock" \
  --verbose
```

When the relay runs as root but a rootless Podman hook needs access, grant a
specific group access without changing the secure defaults:

```bash
sudo ./relay \
  --group=224.0.0.251:5353 \
  --ns=/proc/1/ns/net \
  --control-socket=/run/netns-mcast-relay.sock \
  --control-socket-group=pi \
  --control-socket-mode=0660 \
  --verbose
```

Use bash + curl to add/remove namespaces while relay stays running:

```bash
# Add namespace (returns 202 Accepted: intent queued, apply is async)
curl --unix-socket "${XDG_RUNTIME_DIR}/netns-relay.sock" -fsS -X POST \
  -H 'Content-Type: application/json' \
  --data "{\"path\":\"/proc/${CONTAINER_PID}/ns/net\"}" \
  http://relay/v1/namespaces/add

# List active namespaces
curl --unix-socket "${XDG_RUNTIME_DIR}/netns-relay.sock" -fsS \
  http://relay/v1/namespaces

# Remove namespace (returns 202 Accepted: intent queued, apply is async)
curl --unix-socket "${XDG_RUNTIME_DIR}/netns-relay.sock" -fsS -X POST \
  -H 'Content-Type: application/json' \
  --data "{\"path\":\"/proc/${CONTAINER_PID}/ns/net\"}" \
  http://relay/v1/namespaces/remove
```

OCI hooks can register a namespace under the stable Podman container ID. This
allows `poststop` to remove the registration even though its OCI state no longer
contains the PID:

```bash
CONTAINER_ID="$(podman inspect --format '{{.Id}}' systemd-esphome)"
CONTAINER_PID="$(podman inspect --format '{{.State.Pid}}' systemd-esphome)"

curl --unix-socket /run/netns-mcast-relay.sock -fsS -X PUT \
  -H 'Content-Type: application/json' \
  --data "{\"path\":\"/proc/${CONTAINER_PID}/ns/net\"}" \
  "http://relay/v1/containers/${CONTAINER_ID}/namespace"

curl --unix-socket /run/netns-mcast-relay.sock -fsS -X DELETE \
  "http://relay/v1/containers/${CONTAINER_ID}/namespace"
```

Notes:
- `POST /v1/namespaces/add` and `POST /v1/namespaces/remove` accept intent only and reply `202` with `{ "accepted": true, "applied": false, "mode": "intent", ... }`.
- Container-ID `PUT` and `DELETE` operations are also asynchronous intents and
  require a full 64-character lowercase hexadecimal ID. Repeating `DELETE` for
  an unknown ID is accepted as an idempotent no-op.
- Namespace paths are normalized to clean absolute paths before enqueue.
- Error responses use JSON envelope: `{ "error": { "message": "..." } }`.
- Mutating endpoint request body limit is 4 KiB.

## Label-Driven Podman Automation

Use `scripts/watch_podman_relay.sh` to reconcile relay namespaces from Podman labels:

```bash
export RELAY_SOCKET="${XDG_RUNTIME_DIR}/netns-relay.sock"
export RELAY_LABEL_KEY="homelab.relaymulticast"
export RELAY_LABEL_VALUE="true"
./scripts/watch_podman_relay.sh
```

The watcher includes containers with either:
- container label `homelab.relaymulticast=true`
- attachment to a network labeled `homelab.relaymulticast=true`

## Architecture

The relay uses a channel-based pipeline architecture:

```
Namespace A Socket → Channel → Middleware chain → Channel → Namespace B Socket
Namespace B Socket → Channel → Middleware chain → Channel → Namespace A Socket
```

Relay uses two loop barriers. Synchronous relay-wide guard drops a packet copy
returning through another namespace before fanout. This matters when two
multi-network containers share one network: destination-socket suppression
alone has a race because send workers run asynchronously. Per-socket guard
also drops direct `IP_MULTICAST_LOOP` echoes. Relay-wide window is 250 ms, so
normal mDNS retransmissions remain visible.

Sockets join and transmit on every up, multicast-capable interface in each
namespace. Relay emits source-preserved IPv4 copy using raw UDP injection for
strict local daemons such as Avahi, plus locally sourced copy for physical
devices that reject off-link query sources. Both copies retain incoming TTL.

## Requirements

- Linux kernel with network namespace support
- Go 1.26.3+ for building
- `CAP_SYS_ADMIN` and `CAP_NET_RAW` capabilities (see [ROOTLESS.md](ROOTLESS.md))
  - Either via `setcap cap_sys_admin,cap_net_raw+ep` (recommended)
  - Or run with sudo (not recommended)

## Contrib

- [`contrib/podman`](contrib/podman): OCI hook and systemd integration for
  opted-in Podman containers, including relay restart reconciliation through
  the Podman Unix socket.

## License

[MIT](LICENSE)
