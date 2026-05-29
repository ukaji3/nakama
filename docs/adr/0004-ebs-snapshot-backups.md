# ADR 0004: EBS Snapshot Backups with In-Place Restore

- Status: Accepted
- Date: 2026-05-29
- Context: nakama v1 design

## Context

A nakama instance's only irreplaceable state lives on its **root EBS
volume** (the user's code, code-server settings, and `/home/ubuntu`).
Every other resource is reconstructable: the code-server password is
stored as an SSM SecureString, and the ENI / SG / CloudFront /
metadata are rebuilt deterministically by `nakama aws create` and the
`/nakama/<name>/meta/*` markers.

`requirements.md` §7 originally listed "backup / snapshot" as out of
scope for v1. We now want a minimal, opt-in backup capability without
adding a control plane or a daemon.

nakama is a stateless CLI: it holds no local database and discovers
state from AWS resource tags (`nakama:managed=true`,
`nakama:name=<name>`). Any backup mechanism must fit this model.

## Decision

Back up the **root EBS volume only**, via manual, CLI-driven
**EBS snapshots** (`ec2:CreateSnapshot`).

Command surface:

- `nakama aws backup create <name> [--label <label>] [--note <text>] [--keep N]` —
  snapshot the root volume. `--label` is an optional human-readable
  name (auto-generated from the timestamp when omitted). `--note`
  attaches a free-text memo (e.g. "X installed").
- `nakama aws backup list <name>` — list backups by label / time /
  size / state / note.
- `nakama aws backup note <name> --label <label> [--note <text>]` —
  set or update the memo after the fact (empty value clears it).
- `nakama aws backup delete <name> --label <label>` — delete the
  backup selected by label.
- `nakama aws restore <name> [--label <label>] [--yes]` — **in-place**
  restore: stop the instance, create a volume from the selected
  snapshot (latest when `--label` is omitted), swap it in as the root
  device, and delete the previous root volume.

**The AWS-native `snap-...` ID is never required in the UI.** nakama
is designed for users with little AWS knowledge and already refers to
instances by `<name>` rather than `i-...`. Backups are likewise
addressed by a human label, with `restore` defaulting to the latest
backup. The raw snapshot-id is surfaced only under `-o json` for
debugging.

Snapshots are tagged `nakama:managed=true`, `nakama:name=<name>`,
`nakama:component=backup`, `nakama:backup-time=<RFC3339>`,
`nakama:backup-label=<label>`, and (optionally) `nakama:note=<text>`,
so they are discoverable and selectable by tag in the same way as
every other nakama resource — no new SSM markers are introduced.

**Generation management is count-based and pruned at CLI execution
time.** When `--keep N` is supplied to `backup create`, nakama deletes
the oldest snapshots beyond the N most recent (by `nakama:backup-time`)
after the new snapshot succeeds. Without `--keep`, snapshots are
retained until deleted manually. nakama runs no scheduler, so there is
no time-based or background pruning in v1.

## Rationale

- EBS snapshots are incremental, cheap, and natively taggable, which
  matches nakama's tag-based, database-free state model.
- Restoring in place keeps the existing CloudFront distribution, ENI,
  fixed private IP, and URL intact — the user does not have to
  re-`acl add` or bookmark a new URL.
- Count-based prune at execution time needs no daemon, no EventBridge,
  and no DLM policy lifecycle to manage. It is the smallest mechanism
  that bounds snapshot cost.

## Consequences

- **Crash consistency.** A snapshot of a *running* instance is
  crash-consistent (in-flight writes may be lost). For a consistent
  backup the user should `nakama aws stop <name>` first. This is
  documented; nakama does not auto-stop on `backup create` in v1.
- **Restore is destructive.** In-place restore deletes the prior root
  volume after a successful swap, so it requires confirmation
  (`--yes`) and stops the instance during the swap. The instance is
  left stopped; the user starts it with `nakama aws start`.
- **No automatic pruning without invocation.** Because nakama is not a
  daemon, retention is only enforced when `backup create --keep` runs.
  Snapshots created without `--keep` accrue cost until deleted.
- **Encryption follows the source volume.** Snapshots inherit the root
  volume's encryption state; nakama does not introduce a separate KMS
  key.
- **Labels must be unique per instance.** `backup create` rejects a
  `--label` that already exists for the same `<name>` so that
  `restore`/`backup delete` resolve to exactly one snapshot.
- **The note is editable; the snapshot Description is not.** The memo
  is stored in the `nakama:note` tag (updatable via `CreateTags`),
  which is the source of truth. The same text is also written to the
  snapshot `Description` at create time for AWS console visibility,
  but that field cannot be changed afterward, so `backup note` only
  updates the tag.

## Alternatives Considered

- **AMI (`CreateImage`)**: captures volume + instance config, but
  nakama rebuilds instance config from userdata/ENI/CloudFront in
  `create`. A snapshot + in-place swap is a closer fit and avoids
  managing deregistration of AMIs alongside their snapshots.
- **AWS Backup**: vault/policy-based managed backups. Richer lifecycle
  and cross-account/region support, but disproportionate for a
  single-account, single-developer tool.
- **Data Lifecycle Manager (DLM)**: tag-targeted automated snapshots
  with built-in retention — a good fit for *scheduled* backups. Out of
  v1 scope; deferred until automated/scheduled backups are required.
  Can be layered on later because DLM targets the same nakama tags.
- **Restore as a new named instance**: safer (non-destructive) but
  requires re-running the full `create` flow from a snapshot origin.
  Heavier; deferred. In-place restore covers the primary recovery use
  case.
- **Exposing the raw `snap-...` ID** (or a positional generation
  index) as the backup selector: rejected. The snapshot-id leaks AWS
  internals into a tool meant to hide them, and a generation index is
  unstable as backups are added/pruned. A stable human label, resolved
  to a snapshot via the `nakama:backup-label` tag, is used instead.

## References

- <https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ebs-creating-snapshot.html>
- <https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/snapshot-lifecycle.html>
- <https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ebs-modifying-volume.html>
