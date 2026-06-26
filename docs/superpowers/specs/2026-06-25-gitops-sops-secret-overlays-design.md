# GitOps SOPS Secret Overlays — Design

**Date:** 2026-06-25
**Branch:** `gitops-manifests`
**Status:** Approved design, pre-implementation
**Builds on:** the manifest format (`2026-06-25-gitops-manifest-schema-design.md`) and the
apply engine (`2026-06-25-gitops-apply-engine-design.md` / `internal/apply`, `internal/client`,
`cmd/smithctl`).

## Purpose

Let secret environment values live in the git-managed cluster repo as encrypted
companion files, decrypted and merged into a workload's env at apply time. This
makes a real `smithctl apply` safe for the existing secret-bearing apps
(`postgres`, `deployops`), which today cannot be applied from git because the
plaintext manifests deliberately omit their secrets — and `POST /workloads` is a
full upsert that would otherwise overwrite the live env with secret-less values
and break those apps.

The manifest format already reserved this shape: an optional `<app>.sops.yaml`
sibling with the same `env:` map as the workload, decrypted and merged over
`workload.env` with the overlay winning. This spec defines how the apply engine
performs that decrypt-and-merge.

## Anchoring decisions (locked during brainstorming)

1. **Shell out to the `sops` binary.** `smithctl` runs `sops --decrypt` as a
   subprocess rather than importing the Go SOPS library. Keeps the binary tiny
   (no new Go dependencies) and delegates age-key discovery to `sops`'s own
   conventions. Cost: the host running `smithctl apply` must have `sops` on
   `PATH`.
2. **`--dry-run` decrypts and redacts (full rehearsal).** Dry-run performs the
   exact same decrypt-and-merge as a real apply, proving the key works, but
   prints overlay-sourced keys as `KEY=(set from overlay)` — secret values are
   never written to stdout. This is the primary safety check before a real
   apply.

## Architecture

The overlay handling slots into the apply **pre-flight** (the existing
validate-all phase), so a decryption failure aborts before any POST — the same
guarantee the engine already gives for a bad manifest.

- **`internal/secrets`** *(new)* — `SopsDecryptor` implementing
  `Decrypt(path string) (map[string]string, error)`. It shells out to
  `sops --decrypt --output-type yaml <path>`, parses the `env:` map from stdout,
  and returns it. One responsibility: turn an encrypted overlay file into an env
  map.
- **`internal/apply`** *(extended)* — defines the `Decryptor` interface
  (consumer-side, like `Cluster`) and performs overlay discovery, merge, and
  redaction. `Apply` gains a `Decryptor` parameter. Unit tests inject a fake
  decryptor, so they need no real `sops` binary or age key.
- **`cmd/smithctl`** — wires `secrets.SopsDecryptor{}` into `apply.Apply`.

New signatures:

```go
// internal/apply
func Apply(dir string, cluster Cluster, dec Decryptor, dryRun bool, out io.Writer) error

type Decryptor interface {
    Decrypt(path string) (map[string]string, error)
}
```

## Pre-flight flow (per bundle, during validate-all, before any apply)

```
resolve bundle
  -> overlay := strip ".yaml" from the bundle file, add ".sops.yaml"
  -> if overlay file exists:
        env := dec.Decrypt(overlay)          // abort the whole run on error
        for k, v := range env:
            workload.Env[k] = v               // overlay wins; adds new keys
        record the overlay key set for this workload (for dry-run redaction)
```

The resolved-and-merged results are collected for every bundle before anything
is applied. Only then does the engine either print the plan (`--dry-run`) or POST
in dependency order (workloads → services → ingresses).

## Overlay discovery, merge & redaction

- **Discovery is filename-based:** for bundle file `apps/postgres.yaml`, the
  overlay is `apps/postgres.sops.yaml` (strip `.yaml`, append `.sops.yaml`). This
  matches the manifest-format spec. An overlay file with no matching bundle is
  ignored (nothing references it). `manifestFiles` continues to exclude
  `*.sops.yaml` from the bundle list, as today.
- **Decrypted overlay schema** is just the env shape, parsed strictly:
  ```yaml
  env:
    POSTGRES_PASSWORD: <decrypted value>
  ```
- **Merge:** for each key in the decrypted env, `workload.Env[k] = overlayEnv[k]`
  — overlay wins on conflicts and adds keys the base lacked. If `workload.Env` is
  nil it is initialized before merging.
- **Redaction:** the pre-flight records the set of overlay-sourced keys per
  workload. In `--dry-run`, `printPlan` prints each workload's env, showing
  overlay keys as `KEY=(set from overlay)` and base keys with their (non-secret)
  values. Secret values are never written to stdout, even though they are
  decrypted in memory for the rehearsal.

A real apply POSTs the fully merged env (secrets included); a dry-run decrypts to
prove the key works but prints only redacted key names.

## sops invocation & error handling

`SopsDecryptor.Decrypt(path)` runs `sops --decrypt --output-type yaml <path>` via
`os/exec`, capturing stdout and stderr. On success it parses stdout into
`struct{ Env map[string]string }` (strict YAML via `sigs.k8s.io/yaml`). Errors map
to clear, actionable messages, all raised during the pre-flight (so the cluster
is never left half-applied):

| Condition | Behavior |
|---|---|
| An overlay exists but `sops` is not on `PATH` | Abort: `<app>: found <file> but 'sops' is not installed (PATH)` |
| `sops` exits non-zero (wrong/missing key, malformed) | Abort: `<app>: decrypt <file>: <sops stderr>` |
| Decrypted output is not `{env: map}` | Abort: `<app>: overlay <file>: <parse error>` |

**Key discovery** is delegated entirely to `sops`: it reads the age private key
from `SOPS_AGE_KEY_FILE` or `~/.config/sops/age/keys.txt`. `smithctl` adds no key
configuration of its own.

## Operator workflow (documentation, not code)

Run once per operator / per repo:

```bash
# 1. one-time: generate an age keypair (prints the public key)
age-keygen -o ~/.config/sops/age/keys.txt

# 2. in the smith-cluster repo, .sops.yaml pins the recipient and which files to encrypt:
#    creation_rules:
#      - path_regex: \.sops\.yaml$
#        age: <age public key>

# 3. author/edit a secret overlay (opens decrypted in $EDITOR, re-encrypts on save):
sops apps/postgres.sops.yaml      # contents:  env: { POSTGRES_PASSWORD: ... }
```

The committed `*.sops.yaml` has ciphertext values, cleartext keys, and a `sops:`
metadata block. The private key in `~/.config/sops/age/keys.txt` is never
committed.

For the current cluster the overlays to create are:

| Overlay | Keys |
|---|---|
| `apps/postgres.sops.yaml` | `POSTGRES_PASSWORD` |
| `apps/deployops.sops.yaml` | `API_KEY_SECRET`, `JWT_SECRET`, `OPSCTL_SECRET_KEY`, `DATABASE_URL` |

## Testing

No real `sops` binary or age key is needed for the unit tests.

- **`internal/apply`** (fake `Decryptor` returning a canned env map):
  - Overlay present → its env is merged over the workload (overlay wins); the
    workload handed to `ApplyWorkload` carries the secret value.
  - Decrypt error → `Apply` aborts with **zero** applies (validate-all guarantee
    holds for secrets too).
  - `--dry-run` with an overlay → plan shows `KEY=(set from overlay)`, the secret
    value never appears in the output, and zero applies happen.
  - Orphan `<x>.sops.yaml` with no `<x>.yaml` → ignored (not applied, no error).
  - Bundle with no overlay → unchanged behavior.
- **`internal/secrets`**: the parse-env-from-stdout logic is unit-tested with
  canned `sops`-style YAML output. The actual `sops` exec is thin and covered by
  one integration test that `t.Skip`s when `sops` is not on `PATH`, so CI without
  sops still passes.

## File structure

- Create `internal/secrets/sops.go` — `SopsDecryptor` + `Decrypt` (exec sops,
  parse env) + the stdout-parsing helper.
- Create `internal/secrets/sops_test.go` — parse-helper unit tests + skipped
  integration test.
- Modify `internal/apply/apply.go` — add `Decryptor` interface, the
  `Decryptor` parameter, overlay discovery/merge, per-workload overlay-key
  tracking, and redacted env printing in `printPlan`.
- Modify `internal/apply/apply_test.go` — fake `Decryptor`; new overlay tests;
  update existing call sites to pass a no-op/fake decryptor.
- Modify `cmd/smithctl/main.go` — construct `secrets.SopsDecryptor{}` and pass it
  to `apply.Apply`.

## Scope

**In scope:** decrypt + merge + redacted dry-run, wired into `smithctl apply`.

**Out of scope:** pruning/delete, diff-vs-live, the in-cluster pull-loop, and
`smithctl` encrypting or editing secrets (that is `sops`'s job directly —
`smithctl` only ever decrypts at apply time).

## Relationship to the broader GitOps roadmap

1. Manifest format — done.
2. Apply engine — done.
3. **SOPS secret overlays (this spec)** — makes a real apply safe for
   secret-bearing apps.
4. Owner-labels + pruning + `smithctl diff` against live state.
5. Optional in-cluster pull-loop with drift correction.
