# Podman integration

This example registers selected Podman container network namespaces with
`netns-mcast-relay`. It contains:

- an OCI `poststart`/`poststop` hook for container lifecycle changes;
- a one-shot reconciler that restores registrations after relay restarts;
- a hardened system service template for a rootless Podman user.

Copy these files and adapt paths, user/group ownership, socket locations, and
hardening to the host. The defaults target rootless Podman.

## Requirements

- `relay`, `bash`, `curl`, and `jq`;
- Podman REST API socket;
- systemd;
- a system group allowed to write to the relay control socket.

The Podman API socket grants full control over its Podman account. Keep it as a
local Unix socket. Do not expose it over TCP without strong mutual TLS and a
clear security boundary.

## Install

Enable the rootless Podman user's API socket and lingering:

```bash
systemctl --user enable --now podman.socket
sudo loginctl enable-linger "$USER"
```

Create the relay socket group, add the Podman user, and install the example
files:

```bash
sudo groupadd --system --force netns-mcast-relay
sudo usermod --append --groups netns-mcast-relay "$USER"
sudo install -m 0755 relay /usr/local/bin/relay
sudo install -D -m 0755 netns-mcast-relay-hook.sh \
  /usr/local/libexec/netns-mcast-relay/netns-mcast-relay-hook.sh
sudo install -D -m 0755 reconcile-podman.sh \
  /usr/local/libexec/netns-mcast-relay/reconcile-podman.sh
sudo install -D -m 0644 'netns-mcast-relay@.service' \
  '/etc/systemd/system/netns-mcast-relay@.service'
sudo install -d -m 0755 /etc/containers/oci/hooks.d
sed 's|@HOOK_PATH@|/usr/local/libexec/netns-mcast-relay/netns-mcast-relay-hook.sh|g' \
  netns-mcast-relay-hook.json.in | \
  sudo tee /etc/containers/oci/hooks.d/netns-mcast-relay.json >/dev/null
```

Configure the rootless Podman user to load `/etc/containers/oci/hooks.d`. After
starting a new login session to acquire group membership, enable the relay
instance named with that user's numeric UID:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now "netns-mcast-relay@$(id -u).service"
```

Podman's implicit hook search paths are deprecated. Set `hooks_dir` explicitly
in the applicable `containers.conf` when the host does not already do so:

```toml
[engine]
hooks_dir=["/etc/containers/oci/hooks.d"]
```

## Opt in containers

Set this OCI annotation on each container that needs multicast relay:

```text
io.github.mortezaprk.netns-mcast-relay=true
```

Quadlet example:

```ini
[Container]
Annotation=io.github.mortezaprk.netns-mcast-relay=true
```

Podman CLI example:

```bash
podman run --annotation io.github.mortezaprk.netns-mcast-relay=true ...
```

The hook registers new containers and removes stopped containers. On every
relay start, `ExecStartPost` queries running containers through the Podman API,
selects the same annotation, reads each current PID, and restores registrations.

## Rootful Podman

Rootful Podman is not the default. To use it, enable the system socket and
override the Podman socket path for instance `0`:

```ini
[Service]
Environment=PODMAN_SOCKET=/run/podman/podman.sock
```

Save that drop-in under `netns-mcast-relay@0.service.d`, run `sudo systemctl
daemon-reload`, then enable `podman.socket` and
`netns-mcast-relay@0.service` as system units.

## Customize

Environment variables accepted by `reconcile-podman.sh`:

- `RELAY_SOCKET`
- `PODMAN_SOCKET`
- `PODMAN_API_VERSION`
- `RELAY_ANNOTATION`
- `RELAY_ANNOTATION_VALUE`
- `RECONCILE_ATTEMPTS`
- `RECONCILE_RETRY_SECONDS`

If the annotation key or value changes, update the OCI hook template and
systemd environment together.
