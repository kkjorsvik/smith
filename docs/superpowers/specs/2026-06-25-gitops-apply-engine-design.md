# GitOps Apply Engine — Design

**Date:** 2026-06-25
**Branch:** `gitops-manifests`
**Status:** Approved design, pre-implementation
**Builds on:** `2026-06-25-gitops-manifest-schema-design.md` (the manifest format) and the
`internal/manifest` package (`Parse` + `Resolve`).

## Purpose

Turn a directory of GitOps app-bundle manifests into live cluster state by
resolving each bundle and POSTing the resulting resources to the existing
control-plane API. This is the first piece that makes the manifests *do*
something: `smithctl apply ../smith-cluster/apps` reconciles the cluster to match
the files.

The control-plane API is already idempotent upsert — workloads by `id`, services
by `name` (preserving the existing ClusterIP/NodePort allocation), ingresses by
`host` — so re-applying is safe and does not churn allocations. The apply engine
is therefore a straightforward resolve-then-POST loop; the design work is in
safety (validate before mutating), ergonomics (config, dry-run), and clean
boundaries.

## Scope

In scope: a new operator CLI `smithctl` with an `apply` command (plus
`--dry-run`), a small HTTP API client, and config loading.

Out of scope (each its own later spec):

- **Pruning** — removing cluster resources that are absent from the directory.
  Apply only creates/updates; it never deletes.
- **SOPS secret overlays** — `*.sops.yaml` files are recognized and skipped, not
  decrypted/merged.
- **Diff against live state** — and therefore created-vs-updated-vs-noop
  reporting. The API returns `201` on every upsert, so apply reports "applied".

## Anchoring decisions (locked during brainstorming)

1. **New `cmd/smithctl` client binary.** A dedicated operator CLI (kubectl-style),
   pure HTTP + bearer, with no containerd/CNI/netlink dependencies — builds
   anywhere and runs from the workstation. Chosen over a `smith-server apply`
   subcommand, which would drag the heavy, Linux-only server dependencies into a
   command that only POSTs JSON.
2. **Validate-all, then apply, with `--dry-run`.** Parse+Resolve every bundle
   first; if any file is invalid, abort before POSTing anything (a typo never
   leaves half-applied state). Then POST in dependency order. `--dry-run` stops
   after resolve and prints the plan. Fail-fast on the first POST error.
3. **kubeconfig-style config file.** `~/.config/smith/config` (YAML) holds
   `server` and `token`, overridable with `--config`. Chosen over flags/env so
   there is a single, simple source of connection settings.

## Architecture

Three new units, each with one responsibility:

```
cmd/smithctl ──> internal/apply ──> internal/manifest (Parse/Resolve)
                      │
                      └─(Cluster interface)─> internal/client (real HTTP)
                                               └─ tests use a fake Cluster
```

- **`cmd/smithctl/main.go`** — thin CLI. Parses
  `smithctl [--config PATH] apply [--dry-run] <dir>`, loads config, builds the
  client, calls the apply orchestrator, prints errors and sets the exit code. No
  business logic.
- **`internal/client`** — the API client and config loader.
  - `Config{Server, Token string}` and `LoadConfig(path string) (Config, error)`.
    Default path `~/.config/smith/config`; YAML parsed with `sigs.k8s.io/yaml`.
    Both fields required; a missing file or empty field yields an actionable
    error.
  - `Client{baseURL, token string; http *http.Client}` with
    `ApplyWorkload(types.Workload) error`, `ApplyService(types.Service) error`,
    `ApplyIngress(types.Ingress) error`. Each POSTs JSON with
    `Authorization: Bearer <token>`.
- **`internal/apply`** — the orchestrator.
  `Apply(dir string, cluster Cluster, dryRun bool, out io.Writer) error`.
  Depends on `internal/manifest` and on a small `Cluster` interface (the three
  Apply methods), so it is unit-testable with a fake that records calls — no real
  HTTP in tests.

  ```go
  type Cluster interface {
      ApplyWorkload(types.Workload) error
      ApplyService(types.Service) error
      ApplyIngress(types.Ingress) error
  }
  ```
  `internal/client.Client` satisfies `Cluster`.

## Config file

```yaml
# ~/.config/smith/config   (override with --config)
server: https://smith-server-01.kkjorsvik.com
token: <bearer>
```

`LoadConfig` errors if the file is missing (`no config at <path>; set server and
token`), if it fails to parse, or if `server` or `token` is empty.

## Apply algorithm

```
smithctl apply <dir> [--dry-run]
 1. List <dir>/*.yaml, EXCLUDING *.sops.yaml (secret overlays, handled later).
    Non-recursive (flat apps/ layout). Empty set -> error "no manifests found in <dir>".
 2. For every file (sorted for deterministic order): read -> manifest.Parse ->
    App.Resolve. Any failure -> abort with "<file>: <error>", having POSTed NOTHING.
 3. If --dry-run: print the resolved plan (workloads, then services, then
    ingresses that WOULD be applied) and return. No POSTs.
 4. Otherwise POST in dependency order across ALL resolved bundles:
       a. all workloads   (services reference them by workload_id)
       b. all services    (ingresses reference them by name)
       c. all ingresses
    The first POST error stops the run immediately and is reported as
    "<kind> <name>: <error>".
 5. Print one line per applied resource: "applied workload postgres", etc.
```

Notes:

- **Dependency order** matters for a first-time apply: a service references a
  workload and an ingress references a service. POSTing workloads, then services,
  then ingresses ensures referenced resources exist first. (The control plane
  resolves endpoints asynchronously in its reconcile loop, so strict ordering is
  belt-and-suspenders, but it keeps the operator-visible sequence sensible.)
- **`*.sops.yaml` is skipped**, not treated as a bundle. Until the SOPS step
  exists, secret env is applied out-of-band as today.
- **No pruning:** resources present in the cluster but absent from `<dir>` are
  left untouched.

## HTTP client & error handling

- Standard `net/http` with normal TLS — the API serves a valid certificate, so
  there is no `--insecure` flag in v1. The base URL has any trailing slash
  trimmed.
- Each Apply method marshals the `types.*` value to JSON and POSTs to
  `/workloads`, `/services`, or `/ingresses` with `Content-Type: application/json`
  and the bearer header.
- A non-2xx response becomes an error including the status code and the response
  body (the API returns useful messages, e.g. validation failures).
- `401` maps to a specific `unauthorized (check token in <config path>)` error so
  a stale token is immediately obvious.

## Error handling summary

| Condition | Behavior |
|---|---|
| Config missing / `server` or `token` empty | Abort before any work, actionable message |
| `<dir>` missing or unreadable | Abort with the read error (e.g. "read dir <dir>: ...") |
| `<dir>` has no non-secret `*.yaml` | Error "no manifests found in <dir>" |
| Any file fails Parse/Resolve | Abort before any POST; report offending file + error |
| `--dry-run` | Print plan, POST nothing, exit 0 |
| A POST returns non-2xx | Stop immediately, report `<kind> <name>: <status> <body>` |
| `401` from any POST | Report unauthorized + config path |

## Testing

- **`internal/client`**
  - `httptest.Server`: assert method, path, `Authorization` header, and JSON body
    for `ApplyWorkload`/`ApplyService`/`ApplyIngress`; assert a non-2xx maps to an
    error carrying status+body; assert `401` maps to the unauthorized error.
  - `LoadConfig`: valid file; missing file; empty `server`; empty `token`;
    `--config` override path.
- **`internal/apply`** (fake `Cluster` recording calls)
  - Validate-all-aborts: a directory with one invalid file produces an error and
    **zero** recorded Apply calls.
  - Dependency ordering: with multiple bundles, all workloads are applied before
    any service, and all services before any ingress.
  - `--dry-run`: zero recorded calls, plan written to `out`.
  - Fail-fast: a fake that errors on the second workload stops the run and does
    not proceed to services/ingresses.
  - `*.sops.yaml` is skipped (a `foo.sops.yaml` alongside `foo.yaml` is not parsed
    as a second bundle).
  - "No manifests found" on an empty/secret-only directory.
- **`cmd/smithctl`** — kept thin; covered by a build smoke test only.

## File structure

- Create `cmd/smithctl/main.go` — CLI entry and flag parsing.
- Create `internal/client/config.go` — `Config` + `LoadConfig`.
- Create `internal/client/client.go` — `Client` + Apply methods.
- Create `internal/client/client_test.go`, `internal/client/config_test.go`.
- Create `internal/apply/apply.go` — `Cluster` interface + `Apply`.
- Create `internal/apply/apply_test.go`.

## Relationship to the broader GitOps roadmap

1. Manifest format — **done** (`internal/manifest`).
2. **Apply engine (this spec).**
3. Owner-labels + pruning — track which resources git owns so deleting a file
   removes the resource; needs `smithctl diff` against live state.
4. SOPS secret overlays — decrypt `*.sops.yaml` and merge over workload env.
5. Optional in-cluster pull-loop with drift correction.
