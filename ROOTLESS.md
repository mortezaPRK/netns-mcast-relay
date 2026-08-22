# Running Without Root/Sudo

## The Problem

The `setns()` syscall requires `CAP_SYS_ADMIN` to switch between network
namespaces. Source-preserving UDP injection requires `CAP_NET_RAW`. These are
Linux kernel security restrictions, even when accessing your own rootless
containers.

## Solution: Use Capabilities Instead of Sudo

Instead of running the entire relay with `sudo`, grant only the required capability to the binary:

### One-time setup:
```bash
sudo setcap cap_sys_admin,cap_net_raw+ep ./relay
```

### Then run as regular user:
```bash
./relay \
    --ns=/proc/6399/ns/net \
    --ns=/proc/6434/ns/net \
    --group=224.0.0.251:5353 \
    --verbose
```

## Why This is Better Than Sudo

✅ **No root password needed** - Can be automated in scripts  
✅ **More constrained than full root** - No access to other root operations like package management or user administration  
✅ **Works with rootless Podman** - No sudo required for container namespaces

## Security Considerations

⚠️ **CAP_SYS_ADMIN and CAP_NET_RAW are high-risk capabilities**

While better than full root, `CAP_SYS_ADMIN` grants far more than just namespace switching:
- Mount operations
- Kernel module loading  
- Performance and observability operations
- Various kernel configuration changes

`CAP_NET_RAW` permits raw and packet sockets, including source-address
spoofing. Relay uses it to retain original multicast packet identity.

**Risk**: Any relay bug or supply-chain compromise can become a host-level compromise path.

### Hardening Recommendations

For production deployments, consider additional isolation:
- Run relay in dedicated service account with minimal permissions
- Restrict filesystem access (no write access outside runtime directories)
- Apply seccomp/AppArmor/SELinux profile
- Never load untrusted extensions or plugins
- Consider dedicated namespace helper process via systemd unit with tighter confinement  

## Rootless Podman Example

With rootless Podman containers, the namespace files are owned by your user:

```bash
# Get container PIDs
PID_A=$(podman inspect container-a --format '{{.State.Pid}}')
PID_B=$(podman inspect container-b --format '{{.State.Pid}}')

# Namespace files are owned by you (no root)
ls -l /proc/$PID_A/ns/net  # owned by your user

# Run relay without sudo (after setcap)
./relay --ns=/proc/$PID_A/ns/net --ns=/proc/$PID_B/ns/net
```

## Runtime Control + Podman Labels (Bash)

Start relay with Unix control socket:

```bash
./relay \
  --group=224.0.0.251:5353 \
  --control-socket="${XDG_RUNTIME_DIR}/netns-relay.sock" \
  --verbose
```

Then run watcher script to keep namespaces in sync with Podman labels:

```bash
export RELAY_SOCKET="${XDG_RUNTIME_DIR}/netns-relay.sock"
export RELAY_LABEL_KEY="homelab.relaymulticast"
export RELAY_LABEL_VALUE="true"
./scripts/watch_podman_relay.sh
```

Control API semantics:
- Add/remove calls return `202 Accepted` when intent is queued; apply is asynchronous.
- Namespace path is normalized to clean absolute path before enqueue.
- Failure payload shape is JSON: `{ "error": { "message": "..." } }`.
- Mutating request body limit is 4 KiB.

## Alternative: Run with Sudo (Not Recommended)

If you can't use capabilities (e.g., filesystem doesn't support extended attributes), you can use sudo:

```bash
sudo ./relay --ns=/proc/6399/ns/net --ns=/proc/6434/ns/net
```

But this is less secure and requires root privileges.

## Verification

Check if capability is set:
```bash
getcap ./relay
# Output: ./relay = cap_sys_admin,cap_net_raw+ep
```

After every rebuild/reinstall of the binary, run `setcap` again because file capabilities are not preserved across replaced binaries.

Check if relay is running without root:
```bash
ps aux | grep relay
# Should show your username, not root
```

## Security Note

Only grant capabilities to binaries you trust. The relay binary:
- Switches namespaces and injects source-preserving multicast packets
- Does not escalate privileges
- Does not modify system configuration
- Is open source and auditable

See the Security Considerations section above for deployment hardening recommendations.
