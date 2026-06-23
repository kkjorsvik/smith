# Spec: Node provisioning (CA-reuse, add-agent bundle, setup scripts)

Status: draft for review
Roadmap item: operational tooling (not a functional roadmap item)

## Problem

Standing up a node is currently manual and has a sharp edge: `gencerts` always
mints a **fresh CA** and overwrites `ca.crt`/`ca.key`. Re-running it to add a
third agent would invalidate the server cert and every existing agent's cert,
breaking cluster-wide mTLS. There is also no scripted way to install the
dependencies a fresh Ubuntu box needs (containerd, CNI plugins) or to place
files and run smith as a service.

Goal: make adding a node a two-step operation —

1. On the **control plane** (where the CA lives): `smith-server add-agent
   -host <name>` issues one leaf against the **existing** CA and emits a
   self-contained `.tar.gz` bundle.
2. On the **new box**: `scp` the bundle, `tar x…`, `sudo ./setup.sh`. Done.

## Part 1 — CA reuse in `gencerts`

`runGenCerts` becomes **load-or-create**:

- If `ca.crt` + `ca.key` already exist in `-out`, **load and reuse** them
  (log "reusing existing CA"). Only generate a new CA when none is present, or
  when `-force-ca` is passed (explicit, destructive — re-keys the whole
  cluster).
- Server + agent leaves are still issued/refreshed against whatever CA is in
  force.

Private keys are written `0600` (today they land `0644` — tightened here; the
README already promises `ca.key` stays put).

## Part 2 — `add-agent` subcommand

```
smith-server add-agent \
  -host   smith-agent-03.kkjorsvik.com \      # cert SAN (required)
  -id     smith-agent-03 \                     # default: first label of -host
  -addr   smith-agent-03.kkjorsvik.com:9000 \  # default: <host>:9000
  -server smith-server-01.kkjorsvik.com:9443 \ # default
  -out    /etc/smith/certs \                    # CA location (default)
  -binary bin/smith-agent \                     # agent binary to bundle
  -bundle ./smith-agent-03.tar.gz               # output (default: ./<id>.tar.gz)
```

It **requires an existing CA** in `-out` (fails clearly if absent — you bootstrap
the CA with `gencerts` on the server first). It issues one leaf, writes
`<id>.crt`/`<id>.key` into `-out`, and builds a gzip tarball under a top dir
`<id>/`:

| File | Mode | Purpose |
|------|------|---------|
| `ca.crt` | 0644 | CA cert (trust anchor; **not** the key) |
| `<id>.crt` / `<id>.key` | 0644 / 0600 | this agent's leaf |
| `agent.env` | 0600 | `SMITH_ID/ADDR/SERVER/CA/CERT/KEY` for the unit |
| `setup.sh` | 0755 | the agent setup script (embedded in smith-server) |
| `smith-agent.service` | 0644 | systemd unit (embedded) |
| `smith-agent` | 0755 | the binary (from `-binary`) |

`ca.key` is **never** bundled — it stays on the control plane. The setup script
and unit are `go:embed`ded into `smith-server` (source under
`internal/provision/assets/`) so the bundle is always self-consistent.

## Part 3 — Setup scripts

Both are idempotent, require root, and target Ubuntu (`apt`).

`scripts/setup-server.sh` (run by hand on the control plane):
1. `apt install -y containerd`; enable + start it.
2. Create `/etc/smith`, `/etc/smith/certs` (0700), `/var/lib/smith`.
3. Install the `smith-server` binary to `/usr/local/bin`.
4. Generate `/etc/smith/token` (`openssl rand -hex 32`, `chmod 600`) if absent.
5. Bootstrap the CA + server cert via `smith-server gencerts` (no `-hosts`) if
   `ca.crt` is absent — reuse-safe, so re-running never re-keys.
6. Install + enable `smith-server.service`.
7. Print the AWS-creds reminder (public `:443` cert needs Route 53 access) and
   the `add-agent` next step.

`internal/provision/assets/setup-agent.sh` (delivered in the bundle, run as
`./setup.sh`):
1. `apt install -y containerd`; enable + start it.
2. Download the **pinned** `containernetworking/plugins` release for the box's
   arch (`dpkg --print-architecture`) into `/opt/cni/bin`.
3. Create `/etc/smith/certs` (0700); copy `ca.crt`, `<id>.crt`, `<id>.key` in.
4. Install the `smith-agent` binary to `/usr/local/bin`.
5. Install `agent.env` → `/etc/smith/agent.env` and the systemd unit.
6. Enable + start `smith-agent.service`.

systemd units use `Restart=on-failure` and order after `containerd.service`. The
agent unit reads `EnvironmentFile=/etc/smith/agent.env` and expands the vars in
`ExecStart`, so one env file drives `-id/-addr/-server/-ca/-cert/-key`.

## What stays automatic (unchanged)

Once the agent starts it pulls its subnet, cross-node routes, services, ingress
rules, and the wildcard cert from the control plane on its own (items 1–9). No
per-node networking config. The server needs **no** CNI plugins (it runs no
workloads); only agents do.

## Testing checklist

1. `gencerts` twice into the same dir → CA fingerprint unchanged the second
   time; existing leaves still verify.
2. `add-agent -host smith-agent-03…` against an existing CA → bundle contains
   the 7 files; `<id>.key` is `0600`; `ca.key` is **absent**.
3. The issued leaf verifies against the existing `ca.crt` (openssl verify).
4. On a fresh Ubuntu VM: untar, `sudo ./setup.sh` → containerd + CNI installed,
   service active, node registers and appears in `GET /nodes`.
5. `setup-server.sh` re-run is a no-op (token + CA preserved).

## Out of scope / future

- Non-Ubuntu / non-apt distros.
- Auto-publishing binaries (the bundle carries the binary the operator built).
- Rotating an agent's leaf (delete + re-`add-agent` for now).
- A server-side HTTP endpoint for cert issuance — kept as a local CLI so the CA
  key is never reachable over the network.

## Decisions (locked 2026-06-23)

1. **Deps**: containerd via `apt`; CNI plugins from a pinned upstream release
   tarball into `/opt/cni/bin`.
2. **Delivery**: `add-agent` bundles certs + setup script + unit + the
   `smith-agent` binary into one `.tar.gz`. One `scp`.
3. **Run mode**: systemd units for both roles, enabled + started; agent driven
   by an `EnvironmentFile`.
