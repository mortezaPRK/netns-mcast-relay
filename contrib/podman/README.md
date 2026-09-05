# Podman integration

This example registers selected Podman container network namespaces with
`netns-mcast-relay`. It contains:

- an OCI `poststart`/`poststop` hook for container lifecycle changes;
- a one-shot reconciler that restores registrations after relay restarts;
- a rootless Quadlet drop-in that enables explicit OCI hook discovery;
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

Run these commands as the rootless Podman user from the repository or release
archive root, with `relay` built or extracted there. The default socket paths
support one relay instance per host; do not start another instance on the same
socket. If replacing an existing relay, retain its unit/binary for rollback and
stop it before starting this instance.

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
sudo install -D -m 0755 contrib/podman/netns-mcast-relay-hook.sh \
  /usr/local/libexec/netns-mcast-relay/netns-mcast-relay-hook.sh
sudo install -D -m 0755 contrib/podman/reconcile-podman.sh \
  /usr/local/libexec/netns-mcast-relay/reconcile-podman.sh
sudo install -D -m 0644 'contrib/podman/netns-mcast-relay@.service' \
  '/etc/systemd/system/netns-mcast-relay@.service'
sudo install -d -m 0755 /etc/containers/oci/hooks.d
sed 's|@HOOK_PATH@|/usr/local/libexec/netns-mcast-relay/netns-mcast-relay-hook.sh|g' \
  contrib/podman/netns-mcast-relay-hook.json.in | \
  sudo tee /etc/containers/oci/hooks.d/netns-mcast-relay.json >/dev/null
```

Install the Quadlet drop-in for this user's numeric UID. Application repositories
only need the annotation; the relay installation owns hook discovery:

```bash
sudo install -D -m 0644 contrib/podman/50-netns-mcast-relay.conf \
  "/etc/containers/systemd/users/$(id -u)/container.d/50-netns-mcast-relay.conf"
systemctl --user daemon-reload
```

Existing containers pick up the hook when recreated. A `podman.service.d`
override alone does not affect Quadlet units, which invoke Podman directly.
Retain any other required hook directories when adapting the drop-in, and
remove conflicting old relay-specific overrides.

Acquire the new group membership before continuing. Both the login session and
the user systemd manager that launches Quadlets need it; a new login alone may
leave an existing lingering manager with old groups. A maintenance reboot after
group setup refreshes both. Then enable the instance named with the user's UID:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now "netns-mcast-relay@$(id -u).service"
```

For direct Podman CLI/API use outside Quadlet, set `hooks_dir` explicitly in the
applicable `containers.conf` when the host does not already do so, or pass
`--hooks-dir=/etc/containers/oci/hooks.d` before `run`:

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
podman --hooks-dir=/etc/containers/oci/hooks.d run \
  --annotation io.github.mortezaprk.netns-mcast-relay=true ...
```

The hook registers new containers and removes stopped containers. On every
relay start, `ExecStartPost` queries running containers through the Podman API,
selects the same annotation, reads each current PID, and restores registrations.

## Service sandbox and socket access

The relay runs as root with a dedicated `netns-mcast-relay` group and a `0660`
control socket in a `0750` runtime directory. Its capabilities include
`CAP_SYS_PTRACE` because opening host and rootless `/proc/<pid>/ns/net` paths
requires ptrace access checks in addition to namespace-switching privileges.

`ProtectHome=tmpfs` hides home directories and `/run/user`. The template exposes
only `/run/user/%i/podman` with `BindReadOnlyPaths`, allowing startup reconciliation
to reach the rootless API. A read-only bind does not make the Podman API read-only;
the relay/reconciler is trusted with that account's container control.

If `PODMAN_SOCKET` is overridden, update the bind path too. Keep the exposed
directory narrow and ensure it exists before starting the relay.

## Verify lifecycle and restart recovery

Recreate opted-in containers after installing the hook configuration. Check that
the relay is active and its namespace list includes their current PIDs:

```bash
CONTAINER=my-container # Replace with a running opted-in container name.
systemctl is-active "netns-mcast-relay@$(id -u).service"
podman inspect "$CONTAINER" --format '{{.Id}} {{.State.Pid}}'
curl --fail --unix-socket /run/netns-mcast-relay/control.sock \
  http://relay/v1/namespaces
```

Record the container ID and PID, then restart only the relay:

```bash
sudo systemctl restart "netns-mcast-relay@$(id -u).service"
sudo journalctl -u "netns-mcast-relay@$(id -u).service" -n 30 --no-pager
podman inspect "$CONTAINER" --format '{{.Id}} {{.State.Pid}}'
curl --fail --unix-socket /run/netns-mcast-relay/control.sock \
  http://relay/v1/namespaces
```

Expect unchanged container ID/PID, restored registrations in the journal, and
the container namespace back in the list. Registration is asynchronous: wait
for the namespace to appear. With disposable containers, also verify that an
unannotated container is excluded and stopping an annotated container removes
its registration. Test real LAN discovery separately from API registration.

On Fedora IoT/Pi, these sandbox settings were tested with the existing relay
binary and a host-specific `pi` socket group: startup, annotation selection,
stop-hook removal, and relay restart recovery passed while Home Assistant
remained running and SELinux stayed enforcing. Dedicated-group provisioning,
reboot, and end-to-end LAN discovery were not covered by that deployment test.

## Rootful Podman

Rootful Podman is not the default. To use it, enable the system socket and
override the Podman socket path for instance `0`:

```ini
[Service]
Environment=PODMAN_SOCKET=/run/podman/podman.sock
BindReadOnlyPaths=
BindReadOnlyPaths=/run/podman
```

Save that drop-in under `netns-mcast-relay@0.service.d`, run `sudo systemctl
daemon-reload`, then enable `podman.socket` and
`netns-mcast-relay@0.service` as system units.

The bind reset removes the rootless `/run/user/0/podman` path, which may not
exist. Configure hook discovery for rootful containers separately; the supplied
drop-in under `systemd/users/<UID>/container.d` applies only to rootless Quadlets.

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
