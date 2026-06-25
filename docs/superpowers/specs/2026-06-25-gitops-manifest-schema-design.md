# GitOps Manifest Schema — Design

**Date:** 2026-06-25
**Branch:** `gitops-manifests`
**Status:** Approved design, pre-implementation

## Purpose

Define the on-disk manifest format that lets a smith cluster's desired state
live in a git repository ("cluster-as-repo" / GitOps). The repo becomes the
source of truth; a future apply engine reconciles cluster reality to match the
files.

This document specifies **only the file format** — the bytes on disk and the
rules for interpreting them. The following are explicitly **out of scope** and
get their own specs:

- The apply / diff / prune engine and owner-labeling.
- The `smith apply` / `smith diff` CLI.
- SOPS key management and the decrypt mechanism.
- Push (CI runs apply) vs. pull (in-cluster controller) delivery.

Keeping that boundary tight is deliberate: the format can be designed,
reviewed, and even validated without any of the machinery above existing.

## Enabling property

smith's build-plane (Forgejo) runs **off-cluster**. The repo holding cluster
state therefore does not depend on the cluster being up — there is no
chicken-and-egg bootstrap. The cluster can be wiped and rebuilt from the repo.

## Anchoring decisions (locked during brainstorming)

1. **App-bundle, smith-native.** One file describes one app, with
   `workload`/`service`/`ingress` as sections. Cross-references are implicit
   from the shared app `name`. Chosen over Kubernetes-style
   `apiVersion/kind/metadata/spec` envelopes (more boilerplate) and over a
   fully-flat "everything is lists" model (loses implicit naming).
2. **Singular + list escape hatch.** Each app has exactly one `workload`.
   `service`/`ingress` are singular by default (implicit naming) but
   `services:`/`ingresses:` lists are allowed when an app genuinely needs
   several. The common case stays clean; multi-port apps remain expressible.
3. **Encrypted companion file for secrets.** The schema never models a secret.
   `env:` is always just `env:`. An optional SOPS-encrypted sibling
   (`<app>.sops.yaml`) carries secret env values and is decrypted + merged over
   `workload.env` at apply time. Secrets are an apply-layer overlay, not a
   schema concept — so the structural validator needs no SOPS.

## Scope of GitOps management

**Apps only.** Nodes self-register and are not git-managed. Control-plane
outputs (`cluster_ip`, `node_port` assignment) are never authored — with the
single exception of an explicit `node_port` *pin* on a service.

## Repo layout

```
smith-cluster/                 # a Forgejo repo
  apps/
    deployops.yaml             # app bundle (committed, plaintext)
    deployops.sops.yaml        # encrypted env overlay (optional sibling)
    postgres.yaml
    postgres.sops.yaml
    gitea.yaml                 # list-mode example, no secrets
```

- **One app per file.** The `name:` field inside is authoritative; the filename
  is cosmetic (conventionally matches `name`).
- The `.sops.yaml` sibling is optional, has the same `env:` shape, and is
  decrypted + merged over `workload.env` at apply time.

## Schema

### Top level

| Field | Required | Notes |
|---|---|---|
| `name` | yes | App identity. Owner key for pruning; default name for the workload and singular service. |
| `workload` | yes | Exactly one per app (always singular). |
| `service` / `services` | no | Singular **or** a list. Omit for non-networked apps. |
| `ingress` / `ingresses` | no | Singular **or** a list. Omit for TCP-only apps (e.g. Postgres). |

`service` and `services` are mutually exclusive; likewise `ingress` and
`ingresses`. Supplying both the singular and list form of the same resource is
an error.

### `workload`

Mirrors `types.Workload`, minus `id` (implicit, `= name`). Field names match the
existing JSON tags exactly (snake_case), so a section unmarshals into the
existing struct with no translation layer.

```yaml
workload:
  image: ...              # required
  args: [...]             # optional
  replicas: 2             # default 1
  max_unavailable: 1      # default 1
  env: { KEY: value }     # plaintext only; secrets via .sops overlay
  resources: { cpu_millicores: 500, memory_mb: 512 }
  volumes: [ { name: data, path: /var/lib/postgresql/data } ]
  ports: [ { host_port: 0, container_port: 8080 } ]   # usually unneeded when a service exists
  health_check: { type: http, url: "http://localhost:8080/healthz", interval: 10s, threshold: 3 }
```

### `service`

Mirrors `types.Service`, minus `workload_id` (implicit) and `cluster_ip` /
`node_port` assignment (control-plane outputs). `node_port` may appear only as
an explicit pin.

```yaml
service:
  name: ...          # optional in singular mode (defaults to app name); REQUIRED in list mode
  port: 8080         # required
  target_port: 8080  # optional, defaults to port
  protocol: tcp      # default tcp
  node_port: 30000   # optional pin for a stable NodePort; omit = auto-assign
```

### `ingress`

```yaml
ingress:
  host: deployops.kkjorsvik.com   # required
  service: ...                    # optional singular (the one service); REQUIRED when >1 service
```

## Naming, defaults & cross-reference resolution

All cross-references are implicit and derived from `name`. The apply layer
resolves a bundle into the three API resources using these rules:

| Resolved field | Singular mode | List mode |
|---|---|---|
| `workload.id` | `= name` (always; one workload per app) | same |
| `service.name` | `= name` (override allowed) | **required** per entry |
| `service.workload_id` | `= name` | `= name` (still one workload) |
| `ingress.service` | `=` the sole service's name | **required** ref per entry |

The `ingress.service` defaulting rule is **count-based, not form-based**: it may
be omitted only when the bundle declares *exactly one* service total (whether
written as singular `service:` or a one-entry `services:` list). With zero
services an ingress is an error (nothing to route to); with two or more,
`ingress.service` is required on every ingress.

**Defaults:** `replicas: 1`, `max_unavailable: 1`, `protocol: tcp`,
`target_port: port`.

**Validation implied by the format** (enforced at apply time; defined here):

- An `ingress` requires a target `service` to exist. When `ingress.service` is
  given it must match a service declared in the same bundle; when omitted the
  bundle must declare exactly one service (see the count-based rule above).
- `volumes` present ⇒ `replicas` must be 1 (existing single-writer rule from
  `types`/API validation).
- `cluster_ip` may never appear. `node_port` may appear only as an explicit pin
  on a service.
- The singular and list form of a resource are mutually exclusive (see Top
  level).

## Secrets overlay

- The `.sops.yaml` sibling is keyed identically to the workload's `env:` — a
  flat `env:` map of `KEY: value`.
- At apply time: decrypt the sibling, then `merge(workload.env, decrypted.env)`
  with **the encrypted overlay winning** on key conflicts. A plaintext
  placeholder can thus be safely overridden, or simply omitted.
- The schema never names a secret; from the format's point of view there is
  only `env`. Structural validation of a bundle therefore requires no SOPS —
  only the applier decrypts.
- Exactly one overlay file per app, sibling-named `<app>.sops.yaml`. No partial
  or multiple overlays per app — this keeps the merge unambiguous.

## Worked examples

All three shapes the schema supports:

```yaml
# apps/postgres.yaml — stateful, TCP-only (no ingress)
name: postgres
workload:
  image: git.kkjorsvik.com/kydovik/postgres:18
  volumes: [ { name: data, path: /var/lib/postgresql/data } ]
  # replicas defaults to 1 (required anyway because volumes present)
service:
  port: 5432
  node_port: 30000        # pinned so the provision-db NodePort is stable
```

```yaml
# apps/deployops.yaml — workload + service + ingress, with secret overlay
name: deployops
workload:
  image: git.kkjorsvik.com/kydovik/deployops:2026.06.24
  replicas: 2
  env: { DB_HOST: 10.23.0.1, DB_NAME: deployops, LOG_LEVEL: info }
  resources: { cpu_millicores: 500, memory_mb: 512 }
service:  { port: 8080, target_port: 8080 }
ingress:  { host: deployops.kkjorsvik.com }
# apps/deployops.sops.yaml (encrypted) supplies env.JWT_SECRET, env.OPSCTL_SECRET_KEY
```

```yaml
# apps/gitea.yaml — list mode: two services, ingress refs one explicitly
name: gitea
workload:
  image: git.kkjorsvik.com/kydovik/gitea:1.22
  volumes: [ { name: data, path: /data } ]
services:
  - { name: gitea-http, port: 3000, target_port: 3000 }
  - { name: gitea-ssh,  port: 22,   target_port: 22 }
ingresses:
  - { host: git.kkjorsvik.com, service: gitea-http }
```

## Relationship to the broader GitOps roadmap

This format is the foundation; downstream specs build on it in this order:

1. **Declarative apply (no prune)** — parse bundles, resolve names/refs, POST to
   the existing `/workloads`, `/services`, `/ingresses` APIs.
2. **Owner-labels + pruning** — track which resources git owns (keyed by app
   `name`) so deleting a file removes the resource.
3. **SOPS secrets** — the decrypt-and-merge overlay defined here.
4. **Optional pull-loop** — in-cluster controller that corrects drift.

Each step is independently useful.
