# GitOps Diff & Prune — Design

**Date:** 2026-06-25
**Branch:** `gitops-manifests`
**Status:** Approved design, pre-implementation
**Builds on:** the manifest format, the apply engine (`internal/apply`, `internal/client`,
`cmd/smithctl`), and the SOPS overlays.

## Purpose

Close the GitOps loop: make git the *complete* source of truth by detecting and
removing cluster resources that the manifests no longer declare. Today `apply`
only creates/updates — deleting an app's file leaves its workload running
forever. This adds a read-only `smithctl diff` (preview the delta vs. live) and
an opt-in `smithctl apply --prune` (delete the drift).

## Anchoring decisions (locked during brainstorming)

1. **Git is the source of truth (no ownership marker).** A live resource of a
   kind smith manages (`workload`/`service`/`ingress`) that is not declared in
   `<dir>` is drift and a prune candidate. Chosen over an owner-label/annotation
   (which would require adding a labels concept across types + store + API) and
   over a client state file. Matches the "the whole cluster is a git repo" goal;
   no control-plane changes.
2. **Separate `diff` command + `apply --prune` flag.** `smithctl diff` is a
   first-class read-only preview verb; `apply --prune` executes deletes. Prune is
   opt-in; plain `apply` never deletes.

## Command surface

- **`smithctl diff <dir>`** *(new, read-only)* — GETs live cluster state, resolves
  the manifests, prints the delta per kind, and touches nothing.
- **`smithctl apply --prune <dir>`** — applies the manifests as today, then
  deletes the live resources git no longer declares.
- **`smithctl apply --prune --dry-run <dir>`** — prints the full plan including
  the deletes; executes nothing.
- **`smithctl apply <dir>`** — unchanged; never deletes.

## Safety model

The separate `diff` command is the safety rail: the intended workflow is "run
`diff`, eyeball the deletes, then `apply --prune`." Therefore `apply --prune`
does **not** add an interactive prompt — it stays non-interactive and
CI-friendly (for the future Woodpecker-push model). Guardrails: prune is opt-in,
deletes are always shown by `diff` and by `apply --prune --dry-run`, and only the
three managed kinds are ever listed or deleted.

## Diff/prune engine

Because "git is truth," diff compares resource **identities**, not contents:
workload `id`, service `name`, ingress `host`. A useful consequence: **`diff`
never decrypts** — it resolves bundles for their keys only, so it needs no
`sops`/`age`.

- **Per kind, compute key sets:** `desired` = keys from the resolved manifests;
  `live` = keys from the GET. Then `create = desired − live`,
  `delete = live − desired`, `inSync = desired ∩ live`.
- **No deep "update" diff in v1.** Field-level change detection is unreliable
  because live resources carry server-assigned fields (a service's `cluster_ip`
  and `node_port`) the manifest does not — naive comparison is all false
  positives. Since `apply` is an idempotent upsert that re-applies every desired
  resource anyway, the high-value signals are creates and deletes; resources
  present in both are shown as `= in sync`. Field-level diff is a clean future
  add.
- **`diff` output** groups by kind, e.g.:
  ```
  workloads:
    + create   foo
    - delete   nginx-test
    = in sync  deployops, postgres
  services:
    = in sync  deployops, postgres
  ingresses:
    = in sync  deployops.kkjorsvik.com
  ```
  Within each line/group, keys are sorted for deterministic output.
- **Prune execution** deletes the `delete` set in **reverse-dependency order:
  ingresses → services → workloads** (remove front-facing routing first, the
  backing workload last). Each delete is reported (`pruned workload nginx-test`).
- **Apply order with prune:** apply all desired resources **first**, then delete
  the drift — so a rename brings the replacement up before the old resource is
  removed.

## Architecture

- **`internal/client`** gains six methods, reusing the existing bearer-auth and
  non-2xx-error handling:
  - `ListWorkloads() ([]types.Workload, error)`,
    `ListServices() ([]types.Service, error)`,
    `ListIngresses() ([]types.Ingress, error)` — GET, decode the JSON array.
  - `DeleteWorkload(id string) error`, `DeleteService(name string) error`,
    `DeleteIngress(host string) error` — DELETE `/{kind}/{key}`, expect 2xx.
- **`internal/apply`**:
  - The `Cluster` interface grows from three to nine methods (adds the three List
    and three Delete). The real `client.Client` satisfies it; the test fake
    implements all nine.
  - `Diff(dir string, cluster Cluster, out io.Writer) error` — resolves manifests
    for their keys (no decryptor), GETs live, prints create/delete/in-sync per
    kind.
  - `Apply` gains a `prune bool` parameter:
    `Apply(dir string, cluster Cluster, dec Decryptor, dryRun, prune bool, out io.Writer) error`.
    When `prune` is set it applies desired first, then (unless dryRun) deletes the
    `live − desired` set in reverse-dependency order; under dryRun it prints the
    deletes and removes nothing.
  - A shared helper `delta(desired, live []string) (create, del, inSync []string)`
    computes the three sets (sorted), used by both `Diff` and the prune path.
- **`cmd/smithctl`**: add the `diff` subcommand and a `--prune` bool flag on the
  `apply` subcommand.

The `Cluster` interface intentionally stays a single 9-method interface (apply +
list + delete on one cluster connection) rather than splitting; the real client
and the test fake each implement the whole thing.

## Client method semantics

- **List**: GET `/workloads` (etc.) with the bearer header; on 2xx decode the
  JSON array into `[]types.Workload` (etc.); non-2xx maps to the existing
  status+body error; 401 to the existing unauthorized error.
- **Delete**: DELETE `/workloads/{id}` (path-escaped key); 2xx → nil; non-2xx →
  status+body error. A 404 on delete is treated as an error (the resource was
  expected to exist); this is acceptable because prune only deletes keys it just
  observed in the live list.

## Error handling summary

| Condition | Behavior |
|---|---|
| `diff`/`apply` GET of live state fails | Abort with the client error (nothing deleted) |
| Manifest resolve error (in `diff` or `apply`) | Abort before any GET/delete, report file + error |
| A prune DELETE returns non-2xx | Stop the prune, report `prune <kind> <key>: <status> <body>` |
| `apply --prune` where apply itself fails | Existing apply fail-fast applies; prune is not reached |

## Testing

- **`internal/client`** (`httptest`):
  - Each List returns a JSON array and is decoded; assert method `GET`, path, and
    auth header; non-2xx → error.
  - Each Delete asserts method `DELETE`, path `/workloads/{id}` (etc.), 2xx → nil,
    non-2xx → error including status+body.
- **`internal/apply`** (fake `Cluster` seeded with "live" resources and recording
  deletes):
  - `Diff` — a live extra workload shows `- delete`; a git-only one shows
    `+ create`; in-both shows `= in sync`; **zero deletes recorded** (read-only).
  - `Apply --prune` — extra live workload/service/ingress are deleted;
    git-declared resources are not; deletes happen in order ingresses → services →
    workloads; desired resources are applied before any delete.
  - `Apply --prune --dry-run` — deletes are printed, **zero** deletes and **zero**
    applies recorded.
  - `Apply` without `--prune` — never deletes even when live has extras.
  - `delta` helper — direct unit test of create/delete/inSync set computation and
    sorting.
- **`cmd/smithctl`** — kept thin; covered by a build smoke test (the new `diff`
  command and `--prune` flag wiring).

## File structure

- `internal/client/client.go` *(modify)* — add the three List and three Delete
  methods plus a small `get`/`delete` helper alongside the existing `post`.
- `internal/client/client_test.go` *(modify)* — List/Delete tests.
- `internal/apply/apply.go` *(modify)* — extend `Cluster`; add `Diff`, the
  `prune` parameter, the `delta` helper, and prune execution.
- `internal/apply/apply_test.go` *(modify)* — extend the fake `Cluster` with
  list/delete; add diff and prune tests; update existing `Apply` call sites for
  the new `prune` parameter.
- `cmd/smithctl/main.go` *(modify)* — add the `diff` subcommand and the `--prune`
  flag.

## Scope

**In scope:** `smithctl diff`, `smithctl apply --prune`, and the GET/DELETE client
methods.

**Out of scope:** deep field-level "update" diffing, label/annotation-based
ownership, interactive confirmation prompts, and the in-cluster pull-loop.

## Relationship to the broader GitOps roadmap

1. Manifest format — done.
2. Apply engine — done.
3. SOPS secret overlays — done.
4. **Diff & prune (this spec)** — closes the loop; git becomes the complete
   source of truth.
5. Optional in-cluster pull-loop with drift correction.

With this, GitOps is functionally complete for the homelab: author manifests +
encrypted overlays in git, `diff` to preview, `apply --prune` to converge.
