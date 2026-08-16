# Upstreaming plan — velero CBT / Ceph block data mover findings

Companion to `ceph-cbt-bugs.md` (the findings) and `ceph-cbt-test-plan.md` (how
they were produced). This file is the **order of operations**: what gets filed,
in what order, as what, and with which evidence attached.

**Hard rule: nothing is posted automatically.** Every issue body, PR description
and comment in this plan is drafted here first and posted by Tiger. Claude never
opens issues, PRs or comments in `velero-io/velero` on his behalf.

Status legend: `TODO` · `DRAFTED` · `POSTED` · `MERGED` · `DROPPED`

---

## 0. Pre-flight (do once, before anything is filed)

1. **Rebase onto current upstream main.** All findings were produced against
   `293f6f6a6`. Re-verify each still reproduces before filing — a maintainer
   closing a fixed-in-main issue costs more credibility than not filing it.
   ```
   git fetch upstream && git rebase upstream/main
   go build ./... && go test ./pkg/uploader/... ./pkg/exposer/...
   ```
2. **Search for duplicates.** For each finding, search issues *and* open PRs:
   ```
   repo:velero-io/velero <keyword> in:title,body
   ```
   Three are already known to touch existing work — see §1.
3. **Confirm the linked issue states** for #9714, #10098, #9835 before
   referencing them; they may have moved.
4. **Decide the identity question up front**: these are being filed as a velero
   maintainer, so PRs can go straight to `velero-io/velero`. Note in each issue
   that the work came from Ceph/ODF validation, so reviewers understand the
   environment without having to ask.

---

## 0.5 Delegation brief — where the work already is

The fixes are done, validated live, and pushed. Filing is what remains.

```
git fetch origin && git log --oneline origin/ceph-changeid    # fork: kaovilai/velero
```

| Commit | Finding | Files |
|---|---|---|
| `f4867d048` | BUG-4 + GAP-6 | `pkg/exposer/csi_snapshot.go` (+test) |
| `9d6c5da7a` | BUG-14 | `pkg/uploader/provider/block.go` (+test) |
| `bc24d763a` | BUG-9 **+ BUG-10** | `pkg/uploader/cbt/set.go`, `pkg/uploader/block/snapshot.go` (+tests) |
| `6c7aa9d58` | BUG-13 | API types, deepcopy, controllers, describers (+tests) |
| `ed998fefe` | working docs | **never include in a PR** |

**`bc24d763a` must be split.** BUG-9 and BUG-10 share
`pkg/uploader/block/snapshot.go`, so they are committed together. BUG-10 is just
the `parentID` local in `getParentBackupInfo`, the six log call sites using it,
and `TestGetParentBackupInfoLogsDiscoveredParentID`. Everything else in that
commit is BUG-9.

### Issue linkage

- **Ceph-specific findings (GAP-6, BUG-11) are filed as sub-issues of
  [#9714](https://github.com/velero-io/velero/issues/9714)**, which is the parent
  tracking issue for Ceph changeID/CBT. Use GitHub's sub-issue relationship, not
  just a text cross-reference, so they roll up under it.
- **Driver-agnostic findings get their own standalone issues** and merely
  *mention* #9714 as where they were found. Filing them under the Ceph parent
  would get them triaged as a vendor problem — see §1.5. This applies to BUG-4
  (a non-vSphere bug), BUG-9, BUG-10, BUG-12, BUG-13, BUG-14 and GAP-15.

### Wording every issue needs

State the environment honestly: *validated against upstream ceph-csi CBT
components (hand-applied v1beta1 CRD + upstream sidecar v1.1.0) on ODF-provided
Ceph storage*. Not "validated on ODF" — ODF ships a v1alpha1-generation sidecar
and its own build was never exercised (§1.6).

---

## 1. Finding inventory, scope and disposition

Fixed locally = a patch exists in this worktree, validated live on cluster
260814.

**Scope** is the axis that decides framing (see §1.5 for the evidence):

- `Ceph/Case-2` — only bites storage that requires the base snapshot to persist.
  Genuinely belongs under #9714.
- `non-vSphere` — every CSI driver on the generic exposer path. vSphere is
  exempt **by code**, not by luck.
- `all drivers` — same code path for everyone, vSphere included.
- `not storage` — velero-general or platform, nothing to do with CBT.

| # | Finding | Scope | Fixed | Disposition |
|---|---------|-------|-------|-------------|
| BUG-4 | Generic changeID retrieval always empty | **non-vSphere** | ✅ | Raise in #9714 (found there) but **file/frame as a general non-vSphere CSI bug**, not Ceph-specific + PR |
| GAP-6 | Snapshot retention (Case 2) unimplemented | **Ceph/Case-2** | ✅ | **Sub-issue of #9714** (genuinely Ceph territory). Same PR as BUG-4 (same file) |
| BUG-9 | CBT failure falls back to whole-device | **all drivers** | ✅ | New issue + PR. Explicitly *not* a Ceph bug |
| BUG-10 | Parent-snapshot log lines print an empty ID | **all drivers** | ✅ | New issue (or PR-only) + PR |
| BUG-13 | Measured zero-delta unrepresentable | **all drivers**, wider than block | ✅ | New issue first, get ack, then PR (API change) |
| BUG-14 | Cancelling a block backup reports it as a failure | **all drivers** (block uploader) | ✅ | New issue + PR. Independent file, no conflicts |
| BUG-1 | Datamover pod arg vs CLI flag mismatch | all drivers | ❌ | New issue |
| BUG-2 | Vestigial `EnableCSI` gate | not storage | ❌ | New issue (reframed — CSI built in since 1.14) |
| BUG-3 | `/udmrepo` hardcoded config dir breaks non-root | not storage (OpenShift) | ❌ | New issue |
| BUG-5 | SMS API version mismatch (sidecar v1alpha1 vs CRD v1beta1) | Ceph packaging | ❌ | **DROPPED — do not file.** Documented our own transient mispairing, since resolved by moving to sidecar v1.1.0. What survives is a forward-looking ODF packaging item (see §1.6), not a defect |
| GAP-15 | Data mover pod loss wedges the DataUpload in `InProgress` | **all drivers** | ❌ | New issue. Design question (pod liveness watch vs data-movement timeout) — raise before coding |
| GAP-7 | Restored pods carry stale CNI annotations | not storage (OVN) | ❌ | **Comment on existing fix #10098** — do not file new |
| GAP-8 | `maintenanceFrequency` cannot trigger kopia blob GC | not storage | ❌ | New issue |
| BUG-11 | Retained CBT base snapshots have no lifecycle owner | **Ceph/Case-2** | ❌ | **Sub-issue of #9714**; check overlap with #9835. Consequence of the GAP-6 fix |
| BUG-12 | Block data mover silently uses the fs uploader | all drivers | ❌ | New issue (design/docs question as much as a bug) |

**Only two findings are genuinely Ceph-specific** (GAP-6, BUG-11) — plus BUG-5,
which is not a velero bug at all. Everything else is a general block-data-mover
or velero defect that happens to have been found on Ceph. Filing them all under
the Ceph banner would get them triaged as a vendor problem and under-prioritised.

---

## 1.5 Scope evidence — what actually makes something Ceph-specific

The design doc (`design/block-data-mover/block-data-mover.md`) defines the split
that matters, at lines 362-374:

> Storages/CSI drivers may support the changeId differently based on the
> storage's capabilities:
> 1. […] some storages require the parent snapshot mapping to the changeId
>    always exists at the time of `GetMetadataDelta` is called, then the parent
>    snapshot can NOT be deleted as long as there are incremental backups based
>    on it.
>
> The existing exposer works perfectly with **Case 1**, that is, the snapshot is
> always deleted when the backup completes. However, for **Case 2**, since the
> snapshot must be retained, the exposer needs changes […]

and specifies a `RetainSnapshot` volume-policy parameter, defaulting to Way 1
(snapshot never retained). **That parameter is not implemented** — which is
exactly GAP-6.

- **vSphere/CNS is Case 1**: CBT is held independently of the snapshot, so
  deleting the snapshot after backup is harmless.
- **Ceph RBD is Case 2**: `rbd snap diff` needs base and target in the same
  clone chain, so deleting the base breaks the next delta.

So GAP-6 and its downstream consequence BUG-11 are Case-2 (Ceph today) issues.
They will, however, hit **any** future Case-2 driver — worth saying in the issue
so it is not read as a one-vendor problem.

### Why BUG-4 is a non-vSphere bug, not a Ceph one

`pkg/exposer/csi_snapshot.go:316-347` branches on the source of the changeID:

```go
if vs.Annotations != nil &&
    (vs.Annotations[util.VSphereCNSChangeIDAnno] != "" ||
        vs.Annotations[util.VSphereCNSSnapshotAnno] != "") {
    cbtInfo.changeID = vs.Annotations[util.VSphereCNSChangeIDAnno]   // vSphere/VKS
    ...
} else {
    if vsc.Status != nil && vsc.Status.SnapshotHandle != nil {       // everyone else
        cbtInfo.changeID = *vsc.Status.SnapshotHandle
    }
    ...
}
```

vSphere takes the annotation branch and **never reads the VSC handle at all**,
so the empty-status race that produced BUG-4 cannot occur there. Every other CSI
driver takes the `else` branch and is exposed. Ceph is simply where it was
found. Frame the issue as *"generic CSI changeID retrieval returns empty; all
non-vSphere drivers silently fall back to full backups"* — that is both accurate
and far more likely to get prioritised than a Ceph-titled issue.

### Which findings vSphere would also hit

| Finding | vSphere affected? | Note |
|---------|-------------------|------|
| BUG-9 | **Yes** | `pkg/uploader/cbt/set.go` is common to all drivers. vSphere reaches it less often — being Case 1, it does not suffer the GAP-6 trigger — but any transient `GetMetadataDelta` failure lands in the same whole-device fallback |
| BUG-10 | **Yes** | Log-only, common path |
| BUG-13 | **Yes**, and beyond block | The `omitempty` erasure is in `DataUploadStatus`, `PodVolumeBackupStatus` and `pkg/datapath` — it affects the filesystem/kopia path too, not just the block data mover |
| BUG-12 | **Yes** | Volume-policy selection is driver-independent |
| BUG-1 | **Yes** | CBT plumbing, driver-independent |
| BUG-4 | **No** | Structurally exempt — annotation branch above |
| GAP-6 | **No** | Case 1; the exposer already "works perfectly" per the design |
| BUG-11 | **No** | Only arises once snapshots are retained, i.e. Case 2 |

**Practical consequence for filing:** BUG-9, BUG-10, BUG-13 and BUG-12 should
each carry a line stating they are reproducible on any CBT-capable driver and
were merely observed on Ceph. Attach the Ceph numbers as evidence, not as scope.

---

## 1.6 Not velero — items for ODF / ceph-csi

Neither is a defect in a shipped feature, so neither should be filed as a bug
report. Both are worth raising with the ODF/ceph team, cross-referencing
RHSTOR-6440.

1. **Version skew in ODF's pinned sidecar.** Branch map of
   `openshift/csi-external-snapshot-metadata`, the repo ODF's sidecar image is
   built from (per `SOURCE_GIT_URL` on the digest):

   | Branch | HEAD | What it is | Upstream ver | API |
   |---|---|---|---|---|
   | `release-4.20` | `693a826` | PR #1 "Add build files" (2025-06-18) | pre-v0.2.0 | **v1alpha1** |
   | `release-4.21` | `af250fdb` | STOR-2586 rebase to v0.2.0 (2025-11-20) | v0.2.0 | **v1alpha1** |
   | `release-4.22` | `7652318` | OCPBUGS-77411 ART bump (2026-02-26), not a rebase | v0.2.0 | **v1alpha1** |
   | `release-4.23` / `5.0` / `5.1` / `main` | `239703c` | STOR-2965 rebase to v1.0.0 for OCP 5.0 (2026-06-25) | v1.0.0 | **v1beta1** |

   So **the first v1beta1-capable sidecar exists only from `release-4.23`/`5.0`
   onward**; everything at or below 4.22 is v1alpha1, and upstream v1.0.0 deleted
   that API. No shipped OCP/ODF ≤4.22 sidecar can serve a v1beta1 CRD.

2. **ODF 4.21.10 pins the `release-4.20` build, not `release-4.21`.** The digest
   in `csi-images-v5.0` carries `SOURCE_GIT_COMMIT=693a826` and
   `version=v4.20.0` — release-4.20 HEAD, a commit that is only build
   scaffolding — while its own `release-4.21` branch was rebased to v0.2.0 in
   November 2025. Worth asking ART/ODF packaging whether that pin is deliberate
   (CBT treated as not-shipped, image is a placeholder) or an image-set miss.
   This is independent of the v1alpha1 question.

3. **Enablement gap.** The sidecar is not wired up by default; it needs manual
   injection plus a projected cert volume on the Driver CR to survive operator
   reconciles.

**Provenance caveat to attach to all three: none is an observed failure.** They
come from image labels and branch history, not reproductions — we never ran
ODF's build, because our cluster had a hand-applied v1beta1 CRD plus hand-injected
upstream v1.1.0, which is precisely why CBT worked. Present them as packaging
observations, not bug reports.

Caveat on Jira: RHSTOR ship-status could not be verified — the Atlassian MCP
server fails on a certifi CA bundle that `uv` garbage-collected:
`~/.cache/uv/archive-v0/CFtgi4qFEXPJMs5I/lib/python3.10/site-packages/certifi/cacert.pem`
(confirmed absent). Four other certifi bundles survive under `archive-v0/`, so
the fix is one of:

- `uv cache clean` and let the MCP server reinstall — cleanest, regenerates a
  valid archive, but forces reinstalls for anything else sharing that cache; or
- point the server at a surviving bundle, e.g.
  `REQUESTS_CA_BUNDLE=~/.cache/uv/archive-v0/71ML8ygj8NINS7EU/lib/python3.10/site-packages/certifi/cacert.pem`
  — quicker, but pins a path that can itself be GC'd later.

Confirm ship-status against Jira before raising any of the three items above.

---

## 2. Issues before PRs — and why

velero's PR template has a `Fixes #(issue)` field. File the issue first so the
PR can reference it, the changelog line has a home, and a maintainer can push
back on approach *before* reviewing code. Exception: BUG-10 is a pure log-string
fix and can reasonably go PR-only.

### Filing order

Order by *reviewer cost*, cheapest first, so a reviewer picking up the batch
builds context progressively rather than opening with the API change.

1. **BUG-10** — trivial, no behavior change.
2. **BUG-4** — **file as its own issue**, titled for the general defect
   ("generic CSI changeID retrieval returns empty — all non-vSphere drivers
   silently fall back to full backups"), and cross-link #9714 as where it was
   found. Do not bury it as a #9714 comment: #9714 reads as Ceph-scoped, and
   this is the one finding whose blast radius is *every* non-vSphere CSI driver.
3. **GAP-6** — this one *is* #9714 territory. Comment there, referencing the
   design's unimplemented `RetainSnapshot` parameter (design lines 362-374) so it
   is clearly "the design specified this and it was not built", not a new ask.
4. **BUG-9** — new issue. Lead with "not Ceph-specific"; the Ceph numbers are
   evidence, not scope. File after (2)/(3) so the thread explaining *when* CBT
   fails already exists to link to.
5. **BUG-13** — file, then **wait for a maintainer response before opening the
   PR**. It changes two API status fields to pointers; that is the one decision
   in this batch a maintainer may want to make differently. Note in the issue
   that it affects the filesystem/kopia path too, so it is not block-only.
6. Remaining no-fix issues (BUG-1, BUG-2, BUG-3, GAP-8, BUG-11, BUG-12,
   **GAP-15**) — file in any order, they are independent. GAP-15 (data mover pod
   loss wedges the DataUpload) should be framed as a design question, since the
   fix shape — watch the exposed pod for liveness, versus apply a data-movement
   timeout separate from `preparingTimeout` — is a maintainer call. The
   cancel-on-failed-resume logic already exists and behaves correctly; it just
   needs a trigger other than "someone restarted the node-agent".
7. **BUG-5 — dropped.** See §1.6 for what survives, and file that with ODF/ceph,
   not velero.

### What each issue body must contain

Use `.github/ISSUE_TEMPLATE/bug_report.md`. The template asks for a support
bundle; for these, the more useful evidence is:

- The exact reproduction (the `Reproduce:` line already written in
  `ceph-cbt-bugs.md` for each finding).
- The **measured** numbers, not adjectives. The consolidated table at the end of
  `ceph-cbt-bugs.md` is the single best artifact — quote the relevant rows.
- The in-tree code chain with file:line, which each finding already carries.
- Environment: velero main @ `<sha>`, ODF 4.21, ceph-csi RBD, OCP 5.0,
  `SnapshotMetadataService` v1beta1.
- Explicitly state whether it is **Ceph-specific or driver-agnostic**, using the
  §1.5 classification, and for driver-agnostic ones say so in the *first* line.
  A reviewer who reads "found on Ceph/ODF" in the opening sentence will triage it
  as a vendor problem; one who reads "affects all non-vSphere CSI drivers;
  reproduced on Ceph/ODF" will not.

---

## 3. PR ordering

### Dependency and conflict graph

```
PR-A  BUG-10   pkg/uploader/block/snapshot.go        ─┐ same file:
PR-B  BUG-9    pkg/uploader/cbt/set.go               ─┘ MUST be sequenced
                pkg/uploader/block/snapshot.go

PR-C  BUG-4+GAP-6  pkg/exposer/csi_snapshot.go        independent
PR-D  BUG-13       apis/ + controllers/ + output/     independent
PR-E  BUG-14       pkg/uploader/provider/block.go     independent
```

**PR-A and PR-B touch the same two files** (`pkg/uploader/block/snapshot.go`,
`snapshot_test.go`). Do not open them in parallel expecting clean merges —
land A, rebase B. PR-C, PR-D and PR-E share no files with anything and can be
open concurrently.

### Recommended sequence

| Order | PR | Contents | Size | Why here |
|-------|----|----------|------|----------|
| 1 | **PR-A** | BUG-10 log fix + `TestGetParentBackupInfoLogsDiscoveredParentID` | ~15 lines | Zero behavior change, trivially reviewable, makes every later log excerpt legible |
| 2 | **PR-C** | BUG-4 + GAP-6 exposer fixes + `TestCreateBackupVSCDeletionPolicy` | ~80 lines | The headline for #9714; independent, so it can open alongside PR-A |
| 3 | **PR-E** | BUG-14 `errors.Is` + `TestBlockProviderCancelThroughWrappedError` | ~10 lines | Two-word fix with a strong regression test. Cheap to review, and cancellation being reported as failure is easy for a maintainer to confirm |
| 4 | **PR-B** | BUG-9 tier ladder + per-tier parent handling | ~120 lines | Rebase on PR-A. Behavior change, but the evidence is strong |
| 5 | **PR-D** | BUG-13 pointer + display gates | ~90 lines across 14 files | Largest blast radius and the only API change — land last, after the ack from §2 |

### Why not one big PR

All four are separable, touch different subsystems, and have independent
evidence. A single PR would force one reviewer to hold the exposer, the
uploader, the API and the CLI describers in their head at once, and one
contested decision (BUG-13's pointer) would block three uncontested fixes.

### Why not four PRs opened at once

PR-A/PR-B conflict. And a reviewer receiving four simultaneous PRs from one
contributor against one subsystem will batch them anyway — sequencing gets
faster feedback on the cheap ones.

---

## 4. Per-PR checklist

Run for every PR, in this order. The changelog step genuinely must come after
the PR exists.

1. **Branch off current upstream main**, one logical change per branch.
2. **Commit with DCO**: `git commit -s`. Commits without it delay acceptance.
3. **Open the PR** (draft is fine) so a PR number exists.
4. **Changelog**: `make new-changelog` — it derives the filename from
   `gh pr view` (`changelogs/unreleased/<PR#>-<gh-login>`), so it cannot run
   before step 3. Alternative for pure-test or pure-log PRs: comment
   `/kind changelog-not-required`.
   ```
   make new-changelog CHANGELOG_BODY="Degrade CBT bitmap failures to allocated blocks instead of a whole-device backup"
   ```
5. **Docs**: update `site/content/docs/main` if behavior users can observe
   changes. Applies to **PR-D** (describe output gains a line) and arguably
   **PR-B** (fallback behavior). PR-A and PR-C need none.
6. **Tests**: every PR here already carries them. Keep the pattern of asserting
   the *defect*, not just the happy path — e.g. `set_test.go` asserts `SetFull`
   is **not** called on the degraded tier, and the BUG-10 test was verified to
   fail before the fix.
7. **Fill the template**: `Fixes #<issue>`, summary, and the checklist boxes.

### PR description contents

Keep it short and evidential. For each:

- One-paragraph problem statement, pulled from the issue.
- The measured before/after. These are the strongest lines available:
  - PR-B: `273,678,336 B moved instead of 3,221,225,472 B` (11.8x) on the block
    volume; and an explicit `--backup-type Full` taken immediately after moved
    **byte-identical** bytes, proving the degraded path now costs exactly what a
    full costs — which was the bug's own bar.
  - PR-D: `<none>` → `0` on `.status.incrementalBytes`, plus
    `Incremental data Size (bytes): 0` in describe output, plus the
    backward-compat check showing an old backup still correctly renders nothing.
- What was verified live and on what (cluster, ODF version, restore byte-equality).
- What was deliberately **not** changed and why — this preempts review rounds.
  For PR-B: `TotalBytes` was not repurposed, and the list of its six consumers.
  For PR-D: `Moved data Size` was left bound to `Size` (see §6).

---

## 5. Evidence hygiene

- **Never claim a fix is validated if only unit tests ran.** Each PR here should
  say exactly which live test (T-number) exercised it, and which rung/path is
  covered only by unit tests.
- Quote log lines verbatim, including the `logSource=` field — it gives the
  reviewer the file:line for free.
- Prefer the consolidated measurement table over prose. Numbers that are
  byte-exact (`6,291,456 B` for a 6 MiB write) are more persuasive than
  percentages.
- Where a result has a caveat, state it in the PR rather than letting a reviewer
  find it. Example: BUG-9's mitigating nuance is that kopia content-addressed
  dedup limits the *stored* damage, so the cost is read I/O, hashing CPU and
  wall time rather than unbounded object-storage growth. Saying this makes the
  rest of the claim more credible, not less.

---

## 6. Known open decisions to surface, not to resolve unilaterally

These are maintainer calls. Raise them in the issue or PR; do not just pick one.

1. **BUG-13 / `Moved data Size (bytes)`** — still bound to `Size`, which for the
   block uploader is the *device length*, not moved bytes. The incremental line
   now always renders so savings are legible, but the label remains inaccurate.
   Renaming it changes a long-standing string that scripts may parse.
2. **BUG-13 header `Data Mover: velero-fs`** — the Backup header derives from an
   unset `backup.spec.dataMover` and contradicts the per-volume `velero-block`
   underneath whenever the mover is chosen by volume policy. One-line fix,
   separate from the rest.
3. **BUG-12** — whether routing a Block-mode PVC to the fs uploader without a
   volume policy should warn. It is arguably correct default behavior, so this
   is a design/docs question, not clearly a bug. Frame it that way.
4. **BUG-11** — retained CBT base snapshots have no lifecycle owner. The fix
   shape (owner refs? a reaper? backup-scoped GC?) is a design decision and may
   warrant a `design/` proposal rather than a PR.

---

## 7. Things that are not bugs — say so explicitly

Filing these as bugs would burn credibility. If they come up in review, they are
already answered:

- **`backup describe` renders client-side**, so a new output line requires a CLI
  new enough to render it. Expected and fine; not a defect.
- **Anomaly B** (`t18-fs-jgxwd` reporting no incremental on a 3 GiB upload) is
  explained by kopia's file-granular `GetIncrementalSize()` — a whole device read
  as a single file is either wholly cached or wholly hashed. Pre-existing
  upstream behavior, only reachable via the BUG-12 mis-route.
- **Restoring an old backup after many newer ones** was verified byte-identical
  twice (T23); no chain-integrity defect exists.

---

## 8. Maintaining this document

Update the status column in §1 as each item moves. When a PR merges, record the
PR number next to the finding in `ceph-cbt-bugs.md` too, so the findings file
stops being a to-do list and becomes a record. If a maintainer rejects an
approach, write the reason here — the rejected approach is as useful to future
work as the accepted one.

| Date | Change |
|------|--------|
| 2026-08-16 | Created. All findings `TODO`; four fixes exist locally and are validated live. |
| 2026-08-16 | Added scope classification (§1.5) after finding vSphere is structurally exempt from BUG-4. Added BUG-14 (cancel reported as failure) and GAP-15 (data mover pod loss wedges the DataUpload), both found by closing coverage gaps. Dropped BUG-5 — it documented our own transient mispairing; what survives is an ODF packaging item (§1.6). |
