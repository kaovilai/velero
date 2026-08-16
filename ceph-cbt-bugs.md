# Ceph × Velero Block Data Mover — Bug Log

Validation run for [velero-io/velero#9714](https://github.com/velero-io/velero/issues/9714)
(Verify ChangeId retrieve for Ceph), parent [#9556](https://github.com/velero-io/velero/issues/9556)
(Block level backup/restore support, v1.19).

Environment: OCP 5.0.0-0.nightly-multi-2026-08-09-185812 (cluster
`260814-aws-amd64`, 3× m5.2xlarge workers) + ODF 4.21.10 via v4.21 catalog.
**No feature gate set** — SnapshotMetadataService CRD installed directly, so
VolumeGroupSnapshot stays off (RHSTOR-8098 avoided by construction). Velero
upstream main (293f6f6a6) image `quay.io/tkaovila/velero:ceph-changeid-fix4`
(carries the BUG-4 exposer fix; earlier results tagged pre-fix used
`:ceph-changeid`), ns `openshift-adp`. VGS out of scope.

Each entry below is a candidate GitHub issue — collected by Tiger for posting
after review. Do not post these anywhere automatically.

## Template

```
### BUG-N: <one-line summary>
- Component: velero | ceph-csi | external-snapshot-metadata | ODF operator | docs
- Severity: blocker | major | minor
- Environment: <velero sha, ODF/ceph-csi version, OCP version>
- Steps: <exact reproduction>
- Expected:
- Actual: <exact error output, quoted>
- Evidence: <log paths, resource YAML>
- Suspected cause / code ref:
```

## Bugs

### BUG-1: Datamover pod arg `--csi-snapshot-metadata-service-sa` doesn't match CLI flag `--cbt-sa-name`
- Component: velero
- Severity: blocker (for any CBT backup using a dedicated CBT service account)
- Environment: velero upstream main @ 293f6f6a6 (code inspection; live repro pending)
- Steps: set node-agent ConfigMap `csiSnapshotMetadataServiceConfigs.saName: <sa>`
  (`pkg/types/node_agent.go:113`), run a block-datamover backup.
- Expected: backup micro-service pod receives the SA name and uses it for the
  SnapshotMetadataService token.
- Actual (predicted from code): exposer appends
  `--csi-snapshot-metadata-service-sa=<sa>` (`pkg/exposer/csi_snapshot.go:746`)
  but the datamover backup command only registers `--cbt-sa-name`
  (`pkg/cmd/cli/datamover/backup.go:104`) — cobra rejects the unknown flag, the
  data mover pod exits immediately, DataUpload fails.
- Evidence: grep across pkg/cmd: no registration of
  `csi-snapshot-metadata-service-sa` anywhere; only producer/consumer name
  mismatch. To be confirmed live in T1 (with saName set).
- Suspected cause / code ref: flag renamed on one side during #9826/#9863-era
  refactor; `pkg/exposer/csi_snapshot.go:746` vs `pkg/cmd/cli/datamover/backup.go:104`.
- Workaround: leave `saName` unset (empty) — arg not appended; sidecar token is
  then requested for the velero/node-agent SA instead.

### BUG-2: built-in CSI path still gated behind the vestigial `EnableCSI` flag; flag-off + `--snapshot-move-data` = silent no-op backup
- Component: velero (core)
- Severity: major (silent data-loss illusion)
- Environment: velero main @ 293f6f6a6, OCP 5.0 nightly, ODF 4.21.10 (live repro)

**The defect is the gate, not the missing flag.** Since v1.14 the CSI plugin is
compiled into the velero binary — there is no separate plugin to install or
omit. Yet `EnableCSI` still hard-gates every CSI BackupItemAction/RIA, defaults
to off, and when off produces a backup that reports success while moving no
data. Adding the flag is a workaround, not the fix; a user cannot be expected to
opt into a code path the product documents as built-in.

- Steps: install velero from main WITHOUT `--features=EnableCSI` (this is the
  default — `velero install` sets no features: `pkg/cmd/cli/install/install.go:80,142`);
  `velero backup create --snapshot-move-data --backup-type Full
  --resource-policies-configmap <block policy>` on CSI-backed (Ceph RBD) PVCs.
- Expected: either (a) CSI actions run unconditionally now that CSI is core, or
  (b) if the gate stays, the backup fails / PartiallyFails and surfaces a
  warning on the Backup CR — the user explicitly asked for snapshot data
  movement and got none.
- Actual: backup phase `Completed` in 2s; DataUploads: none; nothing on the
  Backup CR indicating volumes were dropped. Only an **Info** log line:
  `Skip action velero.io/csi-pvc-backupper ... because the CSI feature is not
  enabled. Feature setting is .` (`pkg/backup/item_backupper.go:427`). The
  skipped-PV summary misattributes it as `no applicable volumesnapshotter found`.
- Evidence: backup `t1-full` on cluster 260814; /tmp/t1-full.log.

**Code refs (velero main @ 293f6f6a6):**
- Gate: `pkg/util/csi/util.go:30` `ShouldSkipAction()` — `!features.IsEnabled(CSIFeatureFlag) && strings.Contains(actionName, "velero.io/csi-")`
- Callers: `pkg/backup/item_backupper.go:426` (Info-only skip, no Backup warning), `pkg/restore/restore.go:1634` (same on restore — a backup taken with the flag on restores as a no-op if the flag is off at restore time)
- Other gate sites: `pkg/backup/snapshots.go:47` (VS/VSC not persisted into the backup), `pkg/backup/item_backupper.go:616,627`, `internal/volume/volumes_information.go:566`, `pkg/controller/backup_controller.go:922,934` (metrics only)
- Flag const: `pkg/apis/velero/v1/constants.go:41`

**Doc self-contradiction:** `site/content/docs/main/csi.md` states the CSI plugin
was merged into velero core in 1.14 and there is "no need to install Velero CSI
plugin anymore", then a few lines later requires `velero install --features=EnableCSI`.

**Upstream history — removal was proposed by maintainers and never landed:**
- [#6694 "Remove the EnableCSI feature flag"](https://github.com/velero-io/velero/issues/6694) — filed by `reasonerjt` (MEMBER) 2023-08-23: *"Since v1.12 velero provides the default datamover ... we should remove this feature flag, update the code and doc."* **Closed 2024-02-11 by the stale bot, not by a decision** (`blackpiglet` had explicitly marked it "Not staled" 2023-11-27).
  - `sseago` (MEMBER) laid out the agreed plan: *"First, always enable CSI. Feature flag does nothing, and remove all of the 'if enableCSI' checks in the code. At the same time, deprecate the flag."*
  - `shawn-hurley`: *"if someone turns it off, but has the plugin installed, expos[e] some warning that this flag has no semantic meaning."* — never implemented; that warning is exactly what's missing here.
  - `reasonerjt`'s rationale for keeping it: *"If user wants to disable CSI snapshot of velero, he/she can just skip installing the CSI plugin."* **That escape hatch no longer exists** — 1.14 bundled the plugin, so the flag became the sole switch, and the thread's own premise for retaining it is void.
  - `draghuram` warned about precisely this class of silent skip: *"there is a serious problem when the feature is enabled but plug-in is not installed where Velero proper will skip some PVCs, assuming they will be handled by CSI plug-in."* Post-1.14 the mirror case (flag off, plugin always present) has the same shape.
- [#8000 "PV snapshot was skipped when use `EnableCSI` flag"](https://github.com/velero-io/velero/issues/8000) — user hit the identical silent success (`Phase: Completed, Errors: 0, Warnings: 0`), reasoning *"the CSI volume snapshot function was supported in AWS, Azure and GCP plugin, that means we don't need to install CSI plugin anymore."*
- [#6694 comment 2024-03-10](https://github.com/velero-io/velero/issues/6694#issuecomment-1987283385) — another user silently lost volume backups to the flag.
- [#5862](https://github.com/velero-io/velero/issues/5862) / PR [#6062](https://github.com/velero-io/velero/pull/6062), [#4958](https://github.com/velero-io/velero/issues/4958) (Rook/Ceph), [#3549](https://github.com/velero-io/velero/issues/3549) — prior art: the flag's failure modes keep getting softened, never removed.
- Origin: [#2142](https://github.com/velero-io/velero/issues/2142) — introduced in v1.4 "to keep the beta functionality from breaking existing code." CSI snapshots are no longer beta.

**Suggested fix (pick one):**
1. Drop the gate: delete `ShouldSkipAction` and the `IsEnabled(CSIFeatureFlag)`
   branches; accept `EnableCSI` as a deprecated no-op that logs a deprecation
   warning at server startup, remove after N releases per the deprecation policy.
   This is #6694's agreed plan, unblocked now that the plugin is core.
2. Minimum viable: fail-fast or attach a Backup **warning** when the flag is off
   and the backup contains CSI-provisioned PVCs, especially when
   `spec.snapshotMoveData: true` — never report `Completed` with zero volumes
   moved after data movement was explicitly requested.
3. Fix the misleading `no applicable volumesnapshotter found` attribution in the
   skipped-PV summary to name the real cause.

- Workaround (not a fix): add `--features=EnableCSI` to velero deploy + node-agent args.

### BUG-3: Unified repo hardcodes config dir at `/udmrepo` — breaks non-root velero server (OpenShift)
- Component: velero
- Severity: major on OpenShift (blocks ALL unified-repo/datamover use with upstream install)
- Environment: velero main @ 293f6f6a6, OCP 5.0 nightly (live repro)
- Steps: upstream `velero install --use-node-agent` on OpenShift (server pod gets
  arbitrary non-root UID under restricted-v2 SCC); create any snapshot-move-data backup.
- Expected: repo config written somewhere writable (`$HOME`, `/tmp`, or configurable),
  or documented requirement that the server must run as root on OpenShift.
- Actual: every DataUpload fails: `error to ensure backup repository
  default-cbt-test-kopia: ... unable to create config directory: mkdir /udmrepo:
  permission denied` — raised by the BackupRepository controller in the velero
  SERVER pod. Backup ends PartiallyFailed.
- Evidence: backups t1-full-v2/v3 on cluster 260814; fixed instantly by running
  server as root (`runAsUser: 0` + privileged SCC): kopia maintenance job then
  completes.
- Suspected cause / code ref: udmrepo work path (`pkg/repository/provider/unified_repo.go:124`
  `udmrepo.WithConfigFile(urp.workPath, ...)`) rooted at `/udmrepo`; docs
  (`site/content/docs/main/customize-installation.md:26-32`) cover only node-agent
  privilege (`--privileged-node-agent`, `privilegedFsBackup`) — nothing about the
  server-side repo controller needing writable `/udmrepo`.
- Workaround: grant privileged SCC to velero SA + `runAsUser: 0` on the server
  deployment; node-agent set `privileged: true` (matches `--privileged-node-agent`,
  also required later for block-mode device access).

### BUG-4: Generic changeID retrieval always empty — exposer reads status of freshly-created backupVSC (HEADLINE for #9714)
- Component: velero
- Severity: blocker (CBT incremental backups impossible for any non-vSphere driver, incl. Ceph)
- Environment: velero main @ 293f6f6a6, OCP 5.0 + ODF 4.21.10, live repro t1-full-v4/t2-incr
- Steps: two consecutive `velero-block` backups (Full then Incremental) of a Ceph
  RBD PVC.
- Expected: T1 stores kopia tag `cbt-change-id` = VSC snapshotHandle; T2 finds it
  and calls GetMetadataDelta with it.
- Actual: T1 datamover pod gets `--change-id=` (empty). T2 logs
  `Using previous snapshot k5dec...` then
  `No ChangeID tag from parent snapshot , fallback to full backup`
  (`pkg/uploader/block/snapshot.go:181`) — chain can never form; every
  "incremental" is a full.
- Evidence: t2-incr node-agent logs (both PVCs, fs+block); code walk below.
- Root cause / code refs: `pkg/exposer/csi_snapshot.go:180` `createBackupVSC`
  returns the object straight from `Create()` — its `status` is nil (the
  snapshot-controller fills `status.snapshotHandle` async); nothing re-fetches
  it before `getCBTInfo` (`csi_snapshot.go:268`), whose generic branch reads
  `vsc.Status.SnapshotHandle` (`csi_snapshot.go:333`) → "" silently (only
  volumeID emptiness errors, `csi_snapshot.go:344`). vSphere verification (#9713)
  passed only because that env uses the VS-annotation branch (`csi_snapshot.go:317`).
- Suggested fix: read `backupVSC.Spec.Source.SnapshotHandle` (set at creation
  from the ready source VSC, `csi_snapshot.go:581`) — or re-Get the VSC after
  ready-wait; plus warn/error when changeID ends up empty for velero-block.
- Interaction: even with this fixed, incremental delta on Ceph still requires the
  base RBD snapshot to exist (BUG/GAP: `retainSnapshot` Case 2 not implemented —
  see test plan T5); expected next failure mode is GetMetadataDelta
  FAILED_PRECONDITION → fallback-to-full, which IS the designed behavior.

### BUG-5: SMS API version mismatch — sidecar v0.2.0 (v1alpha1) vs SnapshotMetadataService CRD v1beta1 — **WITHDRAWN, do not file**
- **Status: not a defect. This documented a transient mispairing of our own
  making, and we then fixed it by upgrading our own sidecar.** Kept here so the
  reasoning is not rediscovered later.
- Why it is withdrawn: the CBT stack on this cluster is **entirely
  hand-assembled from upstream components**, not what ODF ships.
  - The CRD `snapshotmetadataservices.cbt.storage.k8s.io` has
    `creationTimestamp: 2026-08-15T03:38:56Z` (i.e. during this campaign), serves
    exactly one version (`v1beta1`), and carries a
    `kubectl.kubernetes.io/last-applied-configuration` annotation — we applied it.
  - The running sidecar is `registry.k8s.io/sig-storage/csi-snapshot-metadata:v1.1.0`,
    the upstream image, injected by hand.
  So the original symptom was our v0.2.0 sidecar against our own v1beta1 CRD.
  Moving to v1.1.0 made the pair internally consistent, which is *why* CBT worked
  for the rest of the campaign. There is no ODF-shipped combination being
  exercised here, hence no ODF defect observed.
- What remains true and *is* worth raising with ODF/ceph (forward-looking, not a
  bug report against a shipped feature):
  1. **Version skew in ODF's pinned image set.** ODF 4.21.10's `OperatorConfig`
     uses `imageSet: csi-images-v5.0`, whose `snapshot-metadata` entry is
     `registry.redhat.io/openshift4/ose-csi-external-snapshot-metadata-rhel9@sha256:02410491…`.
     Its image labels give `SOURCE_GIT_URL=openshift/csi-external-snapshot-metadata`
     and `SOURCE_GIT_COMMIT=693a826`, `version=v4.20.0`. Branch map of that repo:

     | Branch | HEAD | Upstream ver | API |
     |---|---|---|---|
     | `release-4.20` | `693a826` (PR #1 "Add build files", 2025-06-18) | pre-v0.2.0 | **v1alpha1** |
     | `release-4.21` | `af250fdb` (STOR-2586 rebase, 2025-11-20) | v0.2.0 | **v1alpha1** |
     | `release-4.22` | `7652318` (OCPBUGS-77411 ART bump, 2026-02-26) | v0.2.0 | **v1alpha1** |
     | `release-4.23`/`5.0`/`5.1`/`main` | `239703c` (STOR-2965 rebase, 2026-06-25) | v1.0.0 | **v1beta1** |

     Two things follow. **The first v1beta1-capable sidecar exists only from
     `release-4.23`/`5.0`** — upstream v1.0.0 (2026-03-13) removed v1alpha1 — so
     no shipped OCP/ODF ≤4.22 sidecar can serve a v1beta1 CRD. And **ODF 4.21.10
     pins the release-4.20 build, not release-4.21**, i.e. the prior release's
     sidecar, from a commit that is only build scaffolding, even though its own
     4.21 branch was rebased to v0.2.0 in November 2025. Worth asking ART/ODF
     packaging whether that pin is deliberate (CBT treated as not-shipped, image
     is a placeholder) or an image-set miss. Cross-ref RHSTOR-6440.
  2. **Enablement gap.** The sidecar is not wired up by default; it needs manual
     injection plus a projected cert volume on the Driver CR to survive operator
     reconciles.

  **Provenance: neither item is an observed failure.** Both come from image
  labels and branch history, not reproductions — ODF's own build was never run
  here. Raise them as packaging observations, not bug reports. RHSTOR
  ship-status is also unverified: the Atlassian MCP server is down on a
  garbage-collected certifi CA path and no `jira`/`acli` CLI is installed.
- Original finding, retained for context: sidecar v0.2.0 logged
  `failed to find the SnapshotMetadataService CR for driver … the server could not
  find the requested resource` because it looked the CR up via the v1alpha1 API,
  while velero's own v1beta1 client read the same CR fine; `GetMetadataAllocated`
  then returned `code=Internal` and velero fell back to a full backup.

### GAP-6: Snapshot retention (Case 2) unimplemented — every Ceph incremental silently degrades to full, and the user cannot opt out (root cause traced; **FIXED + VALIDATED LIVE**)
- Component: velero (design gap; `retainSnapshot` from #9528 design never implemented). Root cause is a hardcoded `DeletionPolicy: Delete` in the CSI snapshot exposer — see "Root cause" below.
- Severity: major (feature works but never delivers incrementals on Ceph, and no user-facing setting can change that)
- **Status: fixed in this worktree and proven end-to-end on live Ceph — see "Fix + live validation" at the end of this entry. This is the headline result for #9714: the first working Ceph incremental (20 MiB moved instead of 2 GiB), restoring byte-exact.**
- Environment: velero main + BUG-4 fix, ODF 4.21.10, sidecar v1.1.0, live t3/t4
- Steps: Full backup t3 (succeeds via GetMetadataAllocated, changeID tagged),
  write 50Mi delta, Incremental backup t4.
- Expected (with retention): GetMetadataDelta(base=t3 handle) returns ~50Mi delta.
- Actual: velero deleted t3's backup VS/VSC after upload → RBD snap + omap gone →
  `GetMetadataDelta ... rpc error: code = Internal desc = failed to get volume
  from id "0001-0011-openshift-storage-...-c38f5087..." : key not found: no snap
  source in omap for "csi.snap.c38f5087-..."` → warn + fallback to real full
  (bytesDone 2147483648 == full volume). Backup Completed; nothing in backup
  status/warnings hints the incremental degraded — only pod logs.
- Direct storage-side confirmation (T5, `rbd` via rook-ceph toolbox, pool
  `ocs-storagecluster-cephblockpool`, after 4 successful backups):
  ```
  $ rbd snap ls  <pool>/csi-vol-73580c64-...   # cbt-test block PVC
  (empty)
  $ rbd info     <pool>/csi-vol-73580c64-...
      size 2 GiB in 512 objects
      order 22 (4 MiB objects)
      snapshot_count: 0
      features: layering, exclusive-lock, object-map, fast-diff, deep-flatten
  ```
  Same for the fs PVC image `csi-vol-3447342a-...` (`snapshot_count: 0`). Pool
  holds only the 4 live PV images, `rbd trash ls` empty, and cluster-wide there
  are **0 VolumeSnapshots / 0 VolumeSnapshotContents**. So nothing survives a
  completed DataUpload for `GetMetadataDelta` to diff against — the omap error
  above is the downstream symptom, not the cause.
  Note `fast-diff` **is** enabled on the images, i.e. the Ceph-side CBT
  capability is present and cheap; the missing piece is purely velero-side
  retention.
- **T8 — the obvious user-side workaround does not work.** Hypothesis: if the
  snapshot is destroyed because its VolumeSnapshotContent says `Delete`, then a
  `VolumeSnapshotClass` with `deletionPolicy: Retain` should preserve it. Created
  class `ocs-rbd-retain` (driver `openshift-storage.rbd.csi.ceph.com`,
  `deletionPolicy: Retain`) and selected it per-PVC via the annotation
  `velero.io/csi-volumesnapshot-class=ocs-rbd-retain` on `cbt-test/data-fs` and
  `cbt-test/data-block`, then ran backup `t8-full` (`backupType: Full`).
  Result: backup Completed with no errors, **and no Ceph snapshot survived** —
  `rbd snap ls` empty on both images, no `csi-snap-*` clone images, `rbd trash ls`
  empty, and `oc get volumesnapshotcontents -A` / `volumesnapshots -A` both
  "No resources found".
  Velero *did* honor the annotation — the backup log records the selection once
  per PVC (`velero backup logs t8-full`):
  ```
  msg="VolumeSnapshotClass=ocs-rbd-retain" ... logSource="pkg/backup/actions/csi/pvc_action.go:248"
  ```
  So the class is chosen correctly and then overridden downstream.
- **Root cause (code + live logs).** The CSI snapshot exposer does not reuse the
  user's VolumeSnapshotContent. It creates **its own static VSC over the same CSI
  `snapshotHandle`** with the deletion policy hardcoded, then retires the user's
  pair. `pkg/exposer/csi_snapshot.go`, `createBackupVSC` (:565), line 588:
  ```go
  Spec: snapshotv1api.VolumeSnapshotContentSpec{
      ...
      Source: snapshotv1api.VolumeSnapshotContentSource{
          SnapshotHandle: snapshotVSC.Status.SnapshotHandle,
      },
      DeletionPolicy:          snapshotv1api.VolumeSnapshotContentDelete,   // <-- hardcoded
      Driver:                  snapshotVSC.Spec.Driver,
      VolumeSnapshotClassName: snapshotVSC.Spec.VolumeSnapshotClassName,     // class copied...
  },
  ```
  It copies `VolumeSnapshotClassName` forward but discards the policy that class
  declares. Full sequence in `Expose` (:160-206): get source VSC → `createBackupVS`
  (:167) → `createBackupVSC` (:180) → `RetainVSC(source)` (:186) →
  `EnsureDeleteVS(source)` (:194) → `EnsureDeleteVSC(source)` (:200). The user's
  pair is therefore removed *safely* (velero force-Retains it first), and the only
  remaining reference to the handle is velero's own `Delete`-policy VSC.
  Then at the end of the DataUpload, `CleanUp` (:514) does:
  ```go
  csi.DeleteVolumeSnapshotIfAny(ctx, e.csiSnapshotClient, backupVSName, ownerObject.Namespace, e.log)   // :523
  ```
  and `DeleteVolumeSnapshotIfAny` (`pkg/util/csi/volume_snapshot.go:260`) is a
  plain `VolumeSnapshots(ns).Delete(...)` with **no policy patch** — so the bound
  backup VSC's hardcoded `Delete` governs, external-snapshotter issues CSI
  `DeleteSnapshot`, and the RBD snapshot is destroyed.
- **Live confirmation of every step**, node-agent logs for both `t8-full`
  DataUploads (source pair torn down, snapshot still alive at this point):
  ```
  16:25:00 msg="Got VSC from VS in namespace cbt-test"  logSource="pkg/exposer/csi_snapshot.go:165" owner=t8-full-ngs4s vs name=velero-data-fs-4rz5z vsc name=snapcontent-afe45cc2-...
  16:25:00 msg="Backup VSC is created from snapcontent-afe45cc2-..."  logSource=".../csi_snapshot.go:185" owner=t8-full-ngs4s vsc name=t8-full-ngs4s
  16:25:00 msg="Finished to retain VSC"  logSource=".../csi_snapshot.go:192" owner=t8-full-ngs4s retained=true vsc name=snapcontent-afe45cc2-...
  16:25:04 msg="VSC is deleted"  logSource=".../csi_snapshot.go:206" owner=t8-full-ngs4s vsc name=snapcontent-afe45cc2-...
  16:25:05 ... same four lines for owner=t8-full-7xwtm / vsc snapcontent-45fa29d9-... ending 16:25:09
  ```
  and the kill itself, from the ODF csi-snapshotter sidecar
  (`openshift-storage.rbd.csi.ceph.com-ctrlplugin-...`, container `csi-snapshotter`):
  ```
  I0815 16:25:20.445953  "GRPC call" method="/csi.v1.Controller/DeleteSnapshot" request="{... \"snapshot_id\":\"0001-0011-openshift-storage-0000000000000002-58b1e078-...\"}"
  I0815 16:25:27.315896  "GRPC call" method="/csi.v1.Controller/DeleteSnapshot" request="{... \"snapshot_id\":\"0001-0011-openshift-storage-0000000000000002-0fa2e92b-...\"}"
  ```
  These land **in the same second as DataUpload completion** —
  `t8-full-ngs4s` completed `16:25:20Z`, `t8-full-7xwtm` completed `16:25:27Z` —
  i.e. they are `CleanUp`, not the earlier source-VSC teardown (which was
  Retain-protected and produced no RPC at 16:25:04/16:25:09).
- **Runtime proof of the downstream consequence (2026-08-15, node-agent logs).**
  Up to now GAP-6 was established from the *storage* side (snapshot gone). The
  uploader side now confirms it end-to-end: both `t4-incr-fix` DataUploads called
  `GetMetadataDelta` with the previous run's handle and got a Ceph-side "the base
  snapshot no longer exists" error, verbatim:
  ```
  error getting changed blocks from CBT service:
  GetMetadataDelta(openshift-adp,0001-0011-openshift-storage-0000000000000002-c38f5087-5e40-4581-8dd4-9d80a4370e60,t4-incr-fix-vn6fk).Recv:
  rpc error: code = Internal desc = failed to get volume from id
  "0001-0011-openshift-storage-0000000000000002-c38f5087-5e40-4581-8dd4-9d80a4370e60":
  key not found: no snap source in omap for "csi.snap.c38f5087-5e40-4581-8dd4-9d80a4370e60"
  ```
  (identically for `...-79580953-1c45-454b-bcde-4291b4374d74` / `t4-incr-fix-mj5m9`),
  both tagged `error.file=".../pkg/uploader/cbt/set.go:54"`,
  `error.function=...cbt.SetBitmapOrFull`,
  `logSource="pkg/uploader/block/snapshot.go:116"`. The changeIDs in those calls
  are exactly the prior run's snapshot handles — the ones velero itself destroyed
  via the hardcoded `Delete` policy above. So the loop closes: velero writes a
  changeID tag, velero deletes the snapshot that changeID names, velero then asks
  Ceph to diff against it.
- **Per-run transfer sizes** (from `pkg/uploader/provider/block.go:153`,
  `"Block backup finished, snapshot ID %s, backup size %v, incremental size %v"`;
  device size 2147483648 in every row). Note `SnapshotInfo.Size` is the raw block
  device length (`dev.Seek(0, io.SeekEnd)`), `SnapshotInfo.IncrementalSize` is
  bytes actually uploaded — they are different quantities, not duplicates:

  | DataUpload | PVC | kind | uploaded bytes | % of device |
  |---|---|---|---|---|
  | t3-full-fix-l6x88 | data-fs | full (allocated) | 329,252,864 | 15.3% |
  | t3-full-fix-rw9d7 | data-block | full (allocated) | 157,286,400 | 7.3% |
  | t8-full-ngs4s | data-fs | full (allocated) | 360,710,144 | 16.8% |
  | t8-full-7xwtm | data-block | full (allocated) | 178,257,920 | 8.3% |
  | t4-incr-fix-vn6fk | data-fs | incremental → fell back | 2,147,483,648 | **100%** |
  | t4-incr-fix-mj5m9 | data-block | incremental → fell back | 2,147,483,648 | **100%** |
  | t11-full-n8jc7 | data-fs | full (allocated), post-fix | 360,710,144 | 16.8% |
  | t11-full-5kkr9 | data-block | full (allocated), post-fix | 178,257,920 | 8.3% |
  | **t11-incr-flftm** | data-fs | **incremental (real delta)** | **32,505,856** | **1.5%** |
  | **t11-incr-4s294** | data-block | **incremental (real delta)** | **20,971,520** | **1.0%** |

  Reading: CBT genuinely works for fulls — `GetMetadataAllocated` cuts a 2 GiB
  device to 7–17%. Only the incrementals blow up, and they blow up *past* what a
  full would have cost (see BUG-9).
- Hypotheses eliminated along the way: annotation ignored (no — logged at
  `pvc_action.go:248`); snapshot promoted to an independent clone image (no —
  zero `csi-snap-*` images in the pool); snapshot soft-deleted (no — `rbd trash ls`
  empty); a stray VSC still holding it (no — zero VS/VSC cluster-wide).
- Notes: (a) confirms design doc's Case 2 requirement for Ceph RBD
  (cephcsi-cbt-e2e/velero-feedback.md); (b) ceph-csi returns `Internal` where
  `FAILED_PRECONDITION`/`NOT_FOUND` would let clients distinguish "base gone" —
  minor ceph-csi issue candidate; (c) suggest velero surface fallback-to-full as
  a Backup warning, not just a pod log line; (d) the fix has a natural shape:
  honor the source VSC's `DeletionPolicy` (or the class's) when building the
  backup VSC at `csi_snapshot.go:588` instead of hardcoding `Delete`, gated on
  whatever `retainSnapshot` knob #9528 lands — a hardcoded `Delete` makes the
  retention feature unimplementable from the outside; (e) minor log-quality nit:
  the `retained=` field at `csi_snapshot.go:192` is `(retained != nil)`, but
  `RetainVSC` (`pkg/util/csi/volume_snapshot.go:136-145`) returns non-nil on
  **both** branches — already-Retain and just-patched — so the field is always
  `true` and tells you nothing about whether a patch occurred.
- **Fix + live validation (2026-08-15, image `quay.io/tkaovila/velero:ceph-changeid-fix5`,
  version `ceph-changeid-fix5 (293f6f6a6-gap6-dirty)`).** Two source changes in
  `pkg/exposer/csi_snapshot.go`, both minimal and upstreamable:

  1. `createBackupVSC` — inherit the source VSC's deletion policy instead of
     hardcoding `Delete`. The backup VSC is statically provisioned against the
     *same* `snapshotHandle` as the source VSC, so the two objects are two API
     handles on one physical snapshot; forcing `Delete` on velero's copy destroys
     a snapshot the user explicitly asked to retain.
     ```go
     // The backup VSC is statically provisioned against the same
     // snapshot handle as the source VSC, so both objects refer to one
     // physical snapshot. Inherit the source's deletion policy instead
     // of forcing Delete, otherwise a user who configured Retain on the
     // VolumeSnapshotClass still loses the snapshot when the backup VSC
     // is cleaned up.
     DeletionPolicy:          snapshotVSC.Spec.DeletionPolicy,
     Driver:                  snapshotVSC.Spec.Driver,
     ```
  2. `CleanUp` — companion object-leak fix. Deleting the backup VS only cascades
     to the backup VSC when the policy is `Delete`; under `Retain` the VSC object
     is orphaned. Delete it explicitly. (Deleting a `Retain` VSC drops only the
     API object; the storage snapshot survives — which is exactly what CBT needs.)
     ```go
     backupVSCName := ownerObject.Name
     ...
     csi.DeleteVolumeSnapshotContentIfAny(ctx, e.csiSnapshotClient, backupVSCName, e.log)
     ```

  Unit coverage: `TestCreateBackupVSCDeletionPolicy` in `csi_snapshot_test.go`
  (table test, `Delete` + `Retain` subtests, both pass). Note the pre-existing
  assertion at `csi_snapshot_test.go:1133` was vacuous — its fixture policy was
  already `Delete`, so it passed against the hardcoded value and would have kept
  passing if inheritance broke. That it was written as
  `assert.Equal(t, vscObj.Spec.DeletionPolicy, backupVSC.Spec.DeletionPolicy)`
  suggests inheritance was the original intent all along.

  **Deployment note:** `pkg/exposer` runs in the *node-agent* (DataUpload
  controller), so both the velero Deployment and the node-agent DaemonSet must be
  rolled. Velero's Makefile `container` targets are docker-only; built with
  podman directly.

  **Live results.** `t11-full` (Full) then 25 MiB fs write + 20 MiB block write at
  `seek=600`, then `t11-incr` (`--backup-type Incremental`) — Completed, no errors:

  | DataUpload | PVC | kind | uploaded bytes | vs its own full |
  |---|---|---|---|---|
  | t11-incr-flftm | data-fs | incremental (delta) | 32,505,856 (31 MiB) | 9.0% |
  | t11-incr-4s294 | data-block | incremental (delta) | 20,971,520 (20 MiB) | 11.8% |

  `20971520` is **exactly** the 20 MiB written at `seek=600` — precision proof
  that `GetMetadataDelta` returned the exact changed extent and nothing else.
  data-fs's 31 MiB = the 25 MiB file plus filesystem metadata. Neither DataUpload
  logged `Forcing full snapshot`, neither hit the omap `no snap source` error, and
  neither took the BUG-9 whole-device fallback. Both logged the healthy path:
  `Searching for parent snapshot` → `Using previous snapshot k<...>` →
  `Using parent snapshot , start time ...` (note the empty ID slot — BUG-10 is
  present even on the success path).

  **Restore correctness (T12) — the test that matters.** Restored `t11-incr`
  (the 20 MiB incremental) into namespace `cbt-t11r`: Restore Completed,
  both DataDownloads Completed, all six checksums byte-identical to source:

  | object | source | restored |
  |---|---|---|
  | /data/file-t11.bin (26214400 B) | `47fa79d9919e8dc3c0d14115590f309f` | same |
  | /data/file1.bin (209715200 B) | `08eab60bb8f8be9393d51730f93bc868` | same |
  | /data/file2-delta.bin (104857600 B) | `7d5312d62393e8960702604a901c706d` | same |
  | /data/file3-incr.bin (31457280 B) | `2d43f92eda75556191d8e0e4a008e371` | same |
  | /dev/xvda, full 2 GiB | `95d7032e1cdc8edcb003a8bc1e2d89fa` | same |
  | /dev/xvda, 600–620 MiB region | `f872244eb797bcbdd93080a61c7d6dae` | same |

  A 20 MiB transfer reconstructs the full 2 GiB device byte-for-byte. The 9
  restore warnings are all benign OCP `already exists` collisions (auto-created
  RoleBindings `system:deployers`/`image-builders`/`image-pullers`, ConfigMaps
  `kube-root-ca.crt`/`openshift-service-ca.crt`, CRD
  `reclaimspacecronjobs.csiaddons.openshift.io`) — not velero defects.

  **Caveat for upstream review:** this fix makes retention *reachable* but it is
  not yet a complete feature. It ties CBT base-snapshot retention to the
  VolumeSnapshotClass's `deletionPolicy`, which is a coarse, cluster-scoped knob
  that also governs unrelated CSI snapshots. #9528's `retainSnapshot` is still the
  right long-term surface; this change is what makes such a knob implementable at
  all (a hardcoded `Delete` cannot be overridden from outside). And retention with
  no reclamation is its own problem — see **BUG-11**.

### GAP-7: Restored Pods carry stale CNI annotations — fix merged for #9719 but opt-in, so default restore still breaks on OVN-Kubernetes
- Component: velero (restore) / OADP defaults
- Severity: major (restored workload never gets a sandbox; failure is scheduling-luck dependent, so it looks like a flake)
- **Reproduction rate, measured (T18 → T24): 8 of 9 restores.** The one that came
  up healthy without intervention (`cbt-t23i`) is the exception that confirms the
  mechanism — the restored Pod always carries the same stale
  `k8s.ovn.org/pod-networks`, and whether it hangs depends on whether the stale IP
  collides with a live allocation on the node it lands on. It is therefore
  scheduling-dependent, not intermittent in the CNI, which is why it reads as a
  flake in CI. `oc delete pod` was needed on `cbt-t18fs`, `cbt-t18b`, `cbt-t20r`,
  `cbt-t21r`, `cbt-t22r`, `cbt-t23f` and `cbt-t23j`; `cbt-t23i` started clean.
- Environment: velero main @ 293f6f6a6 (image `ceph-changeid-fix4`), OCP 5.0 nightly, OVN-Kubernetes + Multus
- Steps: restore a namespace containing a Deployment-managed Pod into a **new**
  namespace (`--namespace-mappings cbt-test:cbt-t4b`) on the same cluster, with a
  stock velero server (no `--default-resource-modifier-configmap`).
- Expected: the ReplicaSet creates a fresh Pod; OVN allocates a new IP/MAC on the
  node the Pod lands on.
- Actual: velero restores the **Pod object verbatim**, including
  `k8s.ovn.org/pod-networks`. Two restores of the same backup into two different
  namespaces produced pods with the **same name** (`writer-5d997c6c7d-sg224`) and
  **byte-identical** network annotations — `"mac_address":"0a:58:0a:83:00:39"`,
  `"ip_addresses":["10.131.0.57/23"]`. Whether the pod starts is pure scheduling
  luck: `cbt-t4a` landed on `ip-10-0-94-134` (owns that subnet) and ran; `cbt-t4b`
  landed on `ip-10-0-38-116` (different subnet) and hung indefinitely:
  ```
  addLogicalPort failed for cbt-t4b/writer-5d997c6c7d-sg224: unable to ensure IPs
  allocated for already annotated pod: default/cbt-t4b/writer-5d997c6c7d-sg224,
  IPs: 10.131.0.57, error: failed to allocate IP 10.131.0.57 for
  ip-10-0-38-116.ec2.internal: not contained in any known subnet

  FailedCreatePodSandBox (x6): ... multus-shim ... timed out waiting for OVS port
  binding (ovn-installed) for 0a:58:0a:83:00:39 [10.131.0.57/23]
  ```
  `oc get co network` was `Available=True, Degraded=False` throughout — not an
  infra outage. Both PVCs attached and mapped fine in both namespaces, so the
  data-mover path is unaffected; this is purely the pod-network annotation.
  Deleting the stuck pod let the ReplicaSet recreate it cleanly
  (`writer-5d997c6c7d-zclws`) and it started immediately.
- **Third reproduction, deterministic**: a later restore of the same backup into a
  third namespace (`cbt-t4c`) produced **the same pod name again**
  (`writer-5d997c6c7d-sg224`) Pending on **the same bad node**
  (`ip-10-0-38-116.ec2.internal`). Three restores, three identical pod
  names/annotations — this is not racy; the annotation is a fixed property of the
  backed-up object, and only node placement decides whether it happens to work.
  Same remedy each time: delete the pod, RS recreates it (`…-rp9xs`) and it starts.
  So the "flake" framing is wrong — it's a deterministic bug with a probabilistic
  trigger, and on a cluster whose nodes all differ from the original it would fail
  100% of the time.
- **Fourth and fifth reproduction, cross-backup (T18)**: restoring **two different
  backups** of the same source namespace — `t18-fs` → `cbt-t18fs` and `t18-blk` →
  `cbt-t18b` — produced two pods that were *both* named
  `writer-5d997c6c7d-sg224` and both claimed `10.131.0.57` /
  `0a:58:0a:83:00:39`, i.e. the same identity as each other *and* as the still-live
  source pod in `cbt-test`. Both hung in `ContainerCreating` with the now-familiar
  pair of events:
  ```
  ErrorUpdatingResource ... ovnk-controlplane: addLogicalPort failed for
  cbt-t18fs/writer-5d997c6c7d-sg224: unable to ensure IPs allocated for already
  annotated pod: default/cbt-t18fs/writer-5d997c6c7d-sg224, IPs: 10.131.0.57,
  error: failed to allocate IP 10.131.0.57 for ip-10-0-21-207.ec2.internal: not
  contained in any known subnet

  FailedCreatePodSandBox (every ~2m): ... multus-shim ... failed to configure pod
  interface: timed out waiting for OVS port binding (ovn-installed) for
  0a:58:0a:83:00:39 [10.131.0.57/23]
  ```
  Storage was fine on both — `SuccessfulAttachVolume` and
  `MapVolume.MapPodDevice succeeded` in both namespaces — reconfirming the failure
  is purely CNI. Two extra facts this round adds: (a) the collision is not
  per-backup, it is per-*source-pod*, so any number of backups of one workload all
  restore into the same contested IP/MAC; and (b) it is orthogonal to uploader mode
  — the fs-uploaded and block-uploaded backups behaved identically.
- **Workaround, confirmed reliable across all five reproductions**:
  `oc delete pod <restored-pod> -n <ns>`. The ReplicaSet immediately recreates a
  pod with a fresh generated name and no inherited annotation, OVN allocates
  normally, and it reaches `1/1 Running` within seconds — T18 gave
  `writer-5d997c6c7d-7f6xl` on `10.131.0.236` and `writer-5d997c6c7d-cwdfn` on
  `10.128.2.65`. This only works for Pods owned by a controller; a bare restored
  Pod has nothing to recreate it, so for those the annotation must be stripped at
  restore time (the #10098 modifier) or edited out by hand.
- Status upstream: [#9719](https://github.com/velero-io/velero/issues/9719)
  (filed by kaovilai, milestone v1.19) closed **completed** 2026-08-04 by
  [PR #10098](https://github.com/velero-io/velero/pull/10098) (design
  [#9921](https://github.com/velero-io/velero/pull/9921)). The code **is** in this
  tree — `SkipDefaultResourceModifier` (`pkg/apis/velero/v1/restore_types.go:138-144`),
  `--default-resource-modifier-configmap` (`pkg/cmd/server/config/config.go:287-289`),
  applied at `pkg/controller/restore_controller.go:447-451`.
- Why it still reproduces: the feature is **opt-in by design** — "no auto-creation
  during install; admins create and configure the ConfigMap" (#9921). Absent the
  flag, nothing strips the annotations. This deployment ran with
  `["server","--features=","--uploader-type=kopia","--features=EnableCSI"]`, i.e.
  no default modifier configured.
- Open question for upstream: on OVN-Kubernetes (all of OpenShift) the
  out-of-the-box restore experience is broken until an admin discovers and wires
  up a ConfigMap, and the failure is intermittent (scheduling-dependent), which
  makes it read as a flake rather than a config gap. Velero already strips
  `volume.kubernetes.io/selected-node` from PVCs unconditionally — precedent for
  making the CNI-annotation strip a built-in default rather than opt-in. At
  minimum, ship the curated ConfigMap pre-created (still overridable/skippable).
- Related: openshift/openshift-velero-plugin#389 (OADP interim fix).

### GAP-8: `maintenanceFrequency` cannot trigger kopia blob GC — full-maintenance mode is unreachable (`GenOptionMaintainMode` is dead code)
- Component: velero repository maintenance (kopia)
- Severity: moderate (storage never reclaimed on the admin's schedule; looks like
  a "delete didn't free space" bug to users)
- Environment: velero main @ 293f6f6a6, BackupRepository `cbt-test-default-kopia`
- What `maintenanceFrequency` actually controls: only how often velero *starts* a
  maintenance Job (`dueForMaintenance`,
  `pkg/controller/backup_repository_controller.go:549-550`). It does not choose
  what that job does.
- What the job does: `pkg/repository/udmrepo/kopialib/lib_repo.go:269-284` hardcodes
  `mode: maintenance.ModeAuto` and only overrides it from
  `repoOption.GeneralOptions[udmrepo.GenOptionMaintainMode]`. **No caller anywhere
  in the tree ever sets that key** — grep for `GenOptionMaintainMode` /
  `GenOptionMaintainFull` / `GenOptionMaintainQuick` returns only the constant
  definitions (`pkg/repository/udmrepo/repo_options.go:33-35`) and this one
  consumer. So `ModeFull` and `ModeQuick` are unreachable; it is always
  `ModeAuto`, and kopia's own cycle timers decide.
- Consequence (live-proven): kopia's ModeAuto runs **full** maintenance (the one
  that does `snapshotgc` blob reclaim) only once per full-cycle interval —
  default 24h. Observed on this repo:
  - job `…-1786802622101` @ 14:03 (repo creation): `Running full maintenance...`,
    `GC found 0 unused contents (0 B)`, `Finished full maintenance.`
  - job `…-1786806263644` @ 15:04: quick
  - job `…-1786810016486` @ 16:06: `Running quick maintenance...` →
    `Compacting an eligible uncompacted epoch` / `Advancing epoch markers` /
    `Finished quick maintenance.` — **no `snapshotgc` lines at all**
  Patching `spec.maintenanceFrequency` from `1h0m0s` down to `1m` successfully
  forces a job to start, but the job is still quick. S3 went 64 objects /
  545,549,651 B → **65 objects / 545,550,956 B** across that maintenance — up
  again. The blobs freed by deleting `t3-full-fix` stay on the wire until
  ~2026-08-16T14:03Z.
- Impact: an admin who deletes a large backup to reclaim object-storage spend has
  no supported lever to make that happen; tuning the one knob that sounds like it
  should work (`maintenanceFrequency`) provably does not.
- Suggested fixes (pick one):
  1. Plumb `GenOptionMaintainMode` from BackupRepository spec (e.g.
     `spec.maintenanceMode: auto|quick|full`) so the dead constants become
     reachable — smallest change, uses existing plumbing.
  2. Have velero write kopia's maintenance params (full/quick cycle intervals)
     at repo init, derived from `maintenanceFrequency`, so ModeAuto lines up with
     the admin's stated cadence.
  3. At minimum, document that `maintenanceFrequency` does not control blob GC
     and that reclaim is on kopia's 24h full cycle.
- Related: [#9835](https://github.com/velero-io/velero/issues/9835) (why does
  delete not shrink the repo).

### BUG-9: CBT failure falls back to whole-device, not allocated-blocks — a failed incremental transfers ~6x more than a plain full
- Component: velero (`pkg/uploader/cbt/set.go`)
- Severity: major (turns a recoverable degradation into a pathological one; on
  Ceph today it fires on *every* incremental because of GAP-6)
- Environment: velero main @ 293f6f6a6 (image `ceph-changeid-fix4`), ODF 4.21,
  SMS sidecar v1.1.0
- Root cause — `SetBitmapOrFull` has three tiers, and the error tier is the worst
  one:
  ```go
  func SetBitmapOrFull(ctx context.Context, service cbtservice.Service, bitmap types.Bitmap) (err error) {
      defer func() {
          if err != nil {
              bitmap.SetFull()          // <-- any error => whole device
          }
      }()
      ...
      if bitmap.ChangeID() == "" {
          return errors.Wrapf(service.GetAllocatedBlocks(...), "error getting allocated blocks from CBT service")
      }
      return errors.Wrapf(service.GetChangedBlocks(...), "error getting changed blocks from CBT service")   // :54
  }
  ```
  When `GetChangedBlocks` fails there is **no degradation to
  `GetAllocatedBlocks`**, even though clearing `ChangeID` and retrying the
  allocated path is exactly the information the service can still provide. The
  deferred `SetFull()` marks every block dirty, including blocks the RBD image
  never allocated.
- Observed cost (same cluster, same 2 GiB volumes, sizes from
  `pkg/uploader/provider/block.go:153`):
  - full via `GetMetadataAllocated`: **360,710,144 B** (data-fs), **178,257,920 B** (data-block)
  - "incremental" after delta failure: **2,147,483,648 B** on both
  - i.e. the failed incremental moves **~6x more bytes than an explicit full
    backup of the same volume would have**, and ~12x more on the block PVC.
- Why it matters beyond the byte count: the log says "fallback to real full
  backup" (`pkg/uploader/block/snapshot.go:116`), which reads as "you got a full"
  — but a real full is allocated-only and much cheaper. The fallback is strictly
  worse than the thing it claims to fall back to. Anyone sizing a backup window
  from full-backup measurements will be off by an order of magnitude the first
  time a delta fails.
- **Live reproduction in isolation (T20, 2026-08-15/16, backup `t20-stale`).**
  The original observation above was made while GAP-6 was still broken, so it
  could be argued the two were entangled. T20 settles that: GAP-6 is fixed, the
  parent snapshot resolves correctly, and BUG-9 was triggered on its own by
  making the CBT service unreachable — `SnapshotMetadataService.spec.address`
  repointed from `csi-snapshot-metadata.openshift-storage.svc:6443` to a name
  that does not resolve. Snapshot *creation* is unaffected by this (it goes
  through the normal CSI provisioner), so only `GetMetadataDelta` fails, which is
  precisely the `SetBitmapOrFull` error tier.
  - Setup: 8 MiB written at offset 1500 MiB on the 3 GiB Block PVC. **Nothing at
    all** was written to the 2 GiB fs PVC.
  - Result:
    ```
    NAME              PHASE       TOTAL        DONE         INCR
    t20-stale-fgnkb   Completed   3221225472   3221225472   3221225472   # block
    t20-stale-mlpd4   Completed   2147483648   2147483648   2147483648   # fs
    ```
    Block moved **3,221,225,472 B for an 8,388,608 B delta — 384x
    amplification**. The fs volume moved **2,147,483,648 B for a delta of
    exactly zero**. On the identical volumes one run earlier, `t19-zero`
    reported `incremental size 0` on both. So a single unreachable-CBT event
    turned 8 MiB of real change into 5.1 GiB of transfer.
  - The error is explicit and correctly attributed in the node-agent log:
    ```
    level=warning msg="Failed to create CBT with source {t20-stale-fgnkb 0001-0011-…-be531b68-… 0001-0011-…-73580c64-…}, fallback to real full backup"
      error="error getting changed blocks from CBT service: GetMetadataDelta(openshift-adp,0001-0011-…-2ae6cf74-…,t20-stale-fgnkb): rpc error: code = Unavailable desc = name resolver error: produced zero addresses"
      error.file="…/pkg/uploader/cbt/set.go:54" error.function=…cbt.SetBitmapOrFull
      logSource="pkg/uploader/block/snapshot.go:116"
    ```
  - **But nothing surfaces above the node agent.** The Backup object is clean:
    ```
    NAME        PHASE       TYPE          ERR      WARN
    t20-stale   Completed   Incremental   <none>   <none>
    ```
    A 384x cost blow-up is invisible to `velero backup describe`, to
    `.status.warnings`, and to any alerting built on them. Combined with BUG-13
    (which suppresses the incremental line entirely) an operator has **no
    API-level signal at all** that CBT stopped working — only a grep of
    node-agent logs distinguishes it.
- **Second amplifier found during T20 — the kopia parent is dropped too.**
  `pkg/uploader/block/snapshot.go:114-117` does more than log:
  ```go
  err := cbt.SetBitmapOrFull(ctx, cbtService, bitmap)
  if err != nil {
      parentBackup.parentObject = ""     // <-- kopia parent discarded as well
      log.WithError(err).Warnf("Failed to create CBT with source %v, fallback to real full backup", cbtSource)
  }
  ```
  So a CBT error costs the run *both* the dirty-block bitmap *and* the kopia
  incremental base: every block is read, and every block is re-hashed with no
  parent to diff against. Note the ordering — `getParentBackupInfo` at `:109`
  had already resolved a perfectly good parent (`Using previous snapshot
  kf90616578ef4a5fc82ad021b25b5703e`) and it is thrown away purely because the
  *bitmap* source failed. These are two independent inputs and there is no
  reason a bitmap failure must invalidate the parent object.
- Mitigating nuance (worth stating in any upstream issue so severity is not
  overstated): kopia's **content-addressed dedup still fires**, so the *stored*
  footprint does not grow proportionally. In T20 the fs volume produced root
  object `kb52a1f2342c58c0b3d6a2b8312a79435` — byte-identical to `t19-zero`'s —
  despite `parentObject` having been cleared. The block volume's root changed
  (`ke6b86ac4ed7418405802885ccef9b7b3`) because its content genuinely changed.
  So the damage is **read I/O, hashing CPU, backup-window wall time, and
  reported figures**, not unbounded object-storage growth. That is still severe
  on large volumes and on metered/throttled storage, and it is the difference
  between a 2 s and a multi-hour backup at TB scale.
- Data correctness under fallback: **not affected** — see the T20 row in the
  validation table. The whole device is read, so the restored image matches the
  source byte for byte.
- **Fix implemented locally** (in this worktree, for upstreaming). Two halves,
  each independently useful:
  1. `pkg/uploader/cbt/set.go` — `SetBitmapOrFull` now walks a ladder of
     **changed → allocated → full** instead of jumping straight to full. It
     returns a `Tier` (`TierChanged` / `TierAllocated` / `TierFull`) as the
     caller's control signal, with the `error` demoted to a diagnostic that
     explains why the result is not `TierChanged`. Note the tiers are additive
     and require no new `Bitmap` method: the interface has `Set` and `SetFull`
     but no `Clear`, and a partial delta unioned with the allocated set is still
     a safe superset of the true delta.
  2. `pkg/uploader/block/snapshot.go` — the CBT error path no longer discards the
     kopia parent **unconditionally**. Parent handling is now per tier, and the
     distinction is a correctness one, not an optimisation:
     - `TierChanged` — keep the parent. Normal incremental.
     - `TierFull` — **keep** the parent. Every block is dirty, so every block is
       written and nothing can be inherited; the parent is pure dedup benefit.
       The old code cleared it here, costing the run its dedup base on top of
       forcing a whole-device read — one failure turned into two. The parent was
       resolved from the repository's own snapshot manifests and validated by
       `getParentBackupInfo` (`:180-192`) independently of the CBT service, so a
       bitmap failure is no reason to distrust it.
     - `TierAllocated` — **drop** the parent. This one is subtle and was caught
       only by tracing `backupData`: `writer.WriteAt(buffer, offset)`
       (`pkg/uploader/block/uploader.go:356`) writes *only* bitmap blocks, and in
       `ObjectDataBackupModeInc` every unwritten offset resolves to the parent
       object. An allocated-blocks bitmap describes what is allocated **now**, not
       what changed — so a block that existed in the parent and has since been
       discarded/TRIMmed would be *inherited back from the parent* instead of
       reading as a hole. Clearing the parent selects
       `ObjectDataBackupModeFull` (`uploader.go:90-92`), where unwritten ranges are
       holes. Still far cheaper than a whole-device transfer. (In the *planned*
       full-backup case this is a no-op: `ChangeID == ""` and
       `parentObject == ""` are set together at `snapshot.go:196-198`, so there is
       never a parent to drop.)
     Distinct log lines per tier replace the single misleading "fallback to real
     full backup" message.
  Tests: `pkg/uploader/cbt/set_test.go` gains
  `changed blocks error degrades to allocated blocks` (asserts `SetFull` is
  **not** called) and `changed and allocated blocks both fail`;
  `pkg/uploader/block/snapshot_test.go` gains
  `TestSnapshotSourceKeepsParentOnCBTFailure` (asserts `Backup` gets the resolved
  parent when both CBT tiers fail) and
  `TestSnapshotSourceDropsParentOnAllocatedTier` (asserts `Backup` gets `""` when
  only the delta failed). `go test ./pkg/uploader/...` passes.
- **Validation status of the fix: all three rungs exercised live.** See the T21
  and T22 rows in the validation table.
  - `TierChanged` (T21) — unchanged behaviour, delta reported to the byte.
  - `TierFull` (T20, with the SMS address repointed at an unresolvable name) —
    whole-device fallback, still byte-correct on restore.
  - `TierAllocated` (T22) — reached **without** any manual `rbd` surgery, by
    taking the base backup with the default **Delete**-policy snapshot class so
    velero's own cleanup removes the physical snapshot. That reproduces the
    pre-GAP-6 condition exactly: the recorded `cbt-change-id` names a snapshot
    that no longer exists while the SnapshotMetadataService stays healthy, which
    is the precise input the middle rung exists for. ceph-csi returns
    `rpc error: code = Internal desc = failed to get volume from id "...":
    key not found: no snap source in omap for "csi.snap.6fb78561-..."` — a
    well-formed service error, not a transport failure, confirming the service
    itself was up. Result: **273,678,336 B moved on the 3 GiB block volume
    (8.5%) and 422,576,128 B on the 2 GiB fs volume (19.7%)**, where the
    unfixed code moved the entire device. That is an **11.8x** and **5.1x**
    reduction on the exact scenario BUG-9 describes.
  - **Cross-check that the fix meets this bug's own bar (T24).** BUG-9's
    headline complaint was that a failed incremental transferred *more than a
    plain full backup of the same volume would have*. An explicit
    `--backup-type Full` taken immediately after T22, with no writes in between,
    moved **273,678,336 B (block) and 422,576,128 B (fs)** — byte-for-byte the
    same as T22's degraded run. The middle rung therefore costs exactly what a
    real full costs, not more. That is the property the bug asked for, measured
    directly rather than inferred.
- Original suggestion (retained for the issue write-up): in the error path,
  before `SetFull()`, retry with an empty
  `ChangeID` (`GetAllocatedBlocks`) and only `SetFull()` if that *also* fails.
  Three tiers become delta → allocated → full, which is the intuitive ladder.
  Emit distinct log lines for the two degradations so operators can tell "lost
  the base snapshot" (GAP-6, recoverable) from "CBT service unusable" (real
  fallback).
- Independence: this is a separate defect from GAP-6. GAP-6 causes the delta to
  fail; BUG-9 decides how expensive that failure is. Fixing GAP-6 hides BUG-9 on
  Ceph but leaves it live for any driver whose `GetMetadataDelta` can fail for
  other reasons (transient SMS outage, expired base on other backends).

### BUG-10: parent-snapshot log lines print an empty ID — every "fallback to full" diagnostic omits the one identifier needed to debug it
- Component: velero (`pkg/uploader/block/snapshot.go`)
- Severity: minor (cosmetic) / high debugging cost
- Observed, verbatim from node-agent:
  ```
  msg="Using parent snapshot , start time 2026-08-15 14:35:07.865360327 +0000 UTC, end time ..., description ..."
  ```
  Note the empty slot after "snapshot".
- Root cause: `getParentBackupInfo` (`pkg/uploader/block/snapshot.go:150-196`)
  takes `parentSnapshot string` as a parameter and has two paths — an explicit
  parent passed by the caller, and one discovered by `findPreviousSnapshot`. On
  the discovery path `parentSnapshot` is `""`, but lines **179, 181, 183, 185,
  187, 193** all interpolate the *parameter* rather than the discovered
  snapshot's ID. Line 193 is representative:
  ```go
  log.Infof("Using parent snapshot %s, start time %v, end time %v, description %s", parentSnapshot, previous.StartTime, previous.EndTime, previous.Description)
  ```
  Only :159 (`"Using provided parent snapshot %s"`, the explicit-parent branch)
  is correct.
- Impact: the discovery path is the normal path for scheduled/incremental
  backups. Every message about which parent was chosen, why a changeID tag was
  missing, and why the run fell back to full is emitted without the parent ID —
  so the logs describe a decision without naming the object the decision was
  about. This directly slowed down diagnosing GAP-6.
- **Fix implemented locally** (in this worktree, for upstreaming): bind a
  `parentID` local in `getParentBackupInfo`, initialised to `parentSnapshot` and
  reassigned to `string(snap.RootObject.ID)` on the discovery branch, then log
  `parentID` from all six sites. One local, six substitutions, no signature
  change. Test: `pkg/uploader/block/snapshot_test.go` gains
  `TestGetParentBackupInfoLogsDiscoveredParentID`, which drives the discovery
  branch through a `logrus` test hook and asserts the `Using parent snapshot`
  message contains the discovered root object ID. Confirmed non-vacuous —
  reverting just that one log line makes it fail with exactly the reported
  symptom: `"Using parent snapshot , start time 0001-01-01 …" does not contain
  "root-obj-42"`. `go test ./pkg/uploader/...` passes with the fix in place.
  **Confirmed live** on cluster 260814 with `ceph-changeid-fix8` (T22): the same
  message now reads `Using parent snapshot kdd9e660735cf19dc34656fe17d4d5397,
  start time 2026-08-16 01:54:19.562742569 +0000 UTC, …`, where every run before
  the fix printed an empty slot.
- Note on line numbers: the citations above are against upstream
  `293f6f6a6`. The BUG-9 fix in this same worktree shifts them (`:169` → `:176`,
  `:193` → `:200`); use the upstream numbers when filing.
- Suggested fix: use `previous.RootObject.ID` in those six calls — the same
  identifier the discovery branch already logs at :169
  (`log.Infof("Using previous snapshot %s", snap.RootObject.ID)`). Note
  `udmrepo.Snapshot` has no bare `ID` field (`pkg/repository/udmrepo/repo.go:100-108`:
  `Source`, `Description`, `StartTime`, `EndTime`, `Tags`, `TotalSize`,
  `RootObject`), so `RootObject.ID` is the identifier to use. Cleanest shape is a
  local `parentID` bound in each branch (`parentSnapshot` in the explicit branch,
  `snap.RootObject.ID` in the discovery branch) and logged from :177 onward.
- Live re-confirmation on the *success* path (T11, 2026-08-15): even when parent
  selection works and a real delta is produced, the line still reads
  `"Using parent snapshot , start time 2026-08-15 17:35:46.090833577 +0000 UTC, end time ..."`
  — empty ID slot. This is not a failure-only cosmetic issue; the healthy path
  logs it too, so operators can never confirm from logs which base a given
  incremental diffed against.

### BUG-11: Retained CBT base snapshots have no lifecycle owner — unbounded orphan growth, one per volume per backup

- Component: velero (CBT / block data mover, snapshot lifecycle). Surfaced by the
  GAP-6 fix; latent in the design regardless of how retention is implemented.
- Severity: major (storage leak with no bound and no supported reclamation path)
- Once a base snapshot is retained so the next incremental can diff against it,
  nothing in velero ever reclaims it. Observed live after the GAP-6 fix:
  ```
  $ rbd ls ocs-storagecluster-cephblockpool | grep csi-snap
  csi-snap-3363fd44-a9ef-43d2-b307-ee60cb0a8522
  csi-snap-731d9613-8f7f-42d8-9539-60389d77613d
  csi-snap-be4ae34a-13d4-414b-9949-1ca0b8b65032
  csi-snap-dc693a86-f62f-4abe-a22a-69e7034e4705

  $ oc get volumesnapshotcontents,volumesnapshots -A
  No resources found
  ```
  Four retained snapshot clone images (two backup runs × two volumes), and **zero**
  Kubernetes objects referencing any of them. They are not owned by a VS, not
  owned by a VSC, not referenced by a Backup or DataUpload, and not tracked in any
  velero status field.
- Consequences:
  - Growth is one snapshot per volume per backup, forever. A daily schedule over
    10 volumes leaves 3,650 orphan RBD images after a year.
  - Each is a CoW clone image, so consumption scales with post-snapshot writes,
    not with a fixed per-snapshot cost.
  - Deleting the Backup does not help — **verified live (T13)**, not inferred.
    Deleted both backups in the chain:
    ```
    $ velero backup delete t11-full  --confirm     # Backup CR gone in ~20s
    $ velero backup delete t11-incr  --confirm     # Backup CR gone in ~20s
    $ rbd ls ocs-storagecluster-cephblockpool | grep -c csi-snap
    4                       # same four UUIDs, before and after both deletions
    $ rbd trash ls ocs-storagecluster-cephblockpool
                            # empty — not even queued for deferred deletion
    ```
    Deletion cleans the Backup CR, the S3 prefix, and the kopia snapshots
    (verified in T9), but it has no code path that touches the retained CSI
    snapshot: velero deleted its own backup VSC at DataUpload completion and kept
    no handle to the storage object. With no backup left referencing them, the
    four snapshots are unreachable through any velero or Kubernetes API.
  - **Each orphan also pins a hidden snapshot on the *live* source volume**, which
    is the more serious half of the leak. The retained `csi-snap-*` image is an
    RBD clone whose parent is a snapshot of the in-use `csi-vol-*` image:
    ```
    $ rbd info ocs-storagecluster-cephblockpool/csi-snap-3363fd44-…
      op_features: clone-child
      parent: ocs-storagecluster-cephblockpool/csi-vol-73580c64-…@4c99b3d5-…
      overlap: 2 GiB

    $ rbd children ocs-storagecluster-cephblockpool/csi-vol-73580c64-…
    ocs-storagecluster-cephblockpool/csi-snap-3363fd44-…
    ocs-storagecluster-cephblockpool/csi-snap-be4ae34a-…
    ```
    The pinning snapshots are invisible to normal tooling — ceph-csi moves them
    into the snapshot *trash namespace*, so `rbd snap ls` on the live volume
    prints nothing and only `rbd snap ls --all` reveals them:
    ```
    $ rbd snap ls  ocs-storagecluster-cephblockpool/csi-vol-73580c64-…
                            # (no output)
    $ rbd snap ls --all ocs-storagecluster-cephblockpool/csi-vol-73580c64-…
    SNAPID  NAME          SIZE   TIMESTAMP                 NAMESPACE
        33  37c8c85a-…    2 GiB  Sat Aug 15 17:35:31 2026  trash (user csi-snap-be4ae34a-…)
        37  4c99b3d5-…    2 GiB  Sat Aug 15 17:37:46 2026  trash (user csi-snap-3363fd44-…)
    ```
    So the PVCs `data-block` and `data-fs` are still Bound and in use, each now
    carrying two invisible pinned snapshots. This extends the leak past the
    volume's own lifetime: deleting the PVC cannot fully reclaim the image while
    children exist, and an operator auditing `rbd snap ls` on their volumes sees a
    clean result.
  - `rbd du` currently reports `USED 0 B` per orphan, which understates the cost.
    The clones are thin at creation; the pinned parent snapshots retain the
    pre-snapshot blocks as the live volume is written, so real consumption accrues
    with post-snapshot write volume, indefinitely.
  - There is no user-facing knob to cap or expire them, and no way to distinguish
    "retained as a CBT base" from an unrelated user snapshot once the API objects
    are gone.
- Root cause is structural: retention is expressed only as a `deletionPolicy` on
  a VolumeSnapshotClass, which is a *storage-side* instruction with no feedback to
  velero. Velero drops all references at cleanup, so it cannot later distinguish,
  find, or reclaim what it asked to keep.
- Suggested direction (design-level, for #9528 / #9714 discussion):
  1. Keep a durable reference to the retained base — either leave the backup VSC
     in place under `Retain` and label it (`velero.io/cbt-base-for: <pvc-uid>`), or
     record the snapshot handle in a velero-owned CR. Something must own it.
  2. Reclaim the previous base once a newer incremental has successfully chained
     past it: at most `N` bases per volume (`N=1` for a simple chain), deleted
     after the successor's DataUpload reaches Completed.
  3. Reclaim on Backup deletion — if a deleted backup's base is not the current
     chain head for its volume, delete the snapshot too. T13 shows deletion
     currently reclaims nothing, so this is the minimum bar for "delete means
     delete".
  4. Until (1)–(3) exist, document the leak: enabling CBT retention on Ceph today
     is a permanent, unbounded commitment of pool capacity, and it leaves hidden
     trash-namespace snapshots pinned on volumes that are still in use.
- Reproduce: run two `--snapshot-move-data` backups with a `Retain` snapshot class
  and the GAP-6 fix applied, delete both Backups, then `rbd ls <pool>`. The
  `csi-snap-*` images remain. Two tooling traps make this leak easy to miss:
  ceph-csi materializes a CSI snapshot as a *separate* clone image
  `csi-snap-<uuid>`, so inspecting the parent `csi-vol-<uuid>` suggests nothing
  leaked; and the snapshot that pins the parent lives in the trash namespace, so
  it needs `rbd snap ls --all` rather than `rbd snap ls`. Use
  `rbd children <pool>/csi-vol-<uuid>` for the authoritative answer.

### BUG-12: block data mover silently falls back to the filesystem uploader when no volume policy is supplied — a Block-mode PVC is backed up as a file, and the backup reports success

- Severity: medium (cost + observability; **not** a correctness bug — the T18
  measurements below rule out chain corruption *and* show the fs-mode backup
  of a Block PVC restores byte-identically, whole-device md5 included)
- Component: `pkg/uploader/provider/*`, volume policy plumbing
- Symptom: `velero backup create ... --snapshot-move-data --backup-type Incremental`
  **without** `--resource-policies-configmap <policy>` routes a `volumeMode: Block`
  PVC down the *filesystem* kopia uploader. The Backup and both DataUploads
  reach `Completed` with `errors: <none>` and `warnings: <none>`. Nothing in
  any CR status, in `velero backup describe`, or in the CLI output indicates
  that the block data mover was not used. The only evidence is in node-agent
  logs, and it is indirect — the giveaway is *which* log lines appear
  (`provider/kopia.go:144 "Starting backup"` and `kopia/snapshot.go:243/258/302`)
  rather than the block equivalents (`provider/block.go:121 "Run block backup,
  CBT source info"` and `block/snapshot.go:106/162/169`).
- Why it matters:
  1. **CBT is silently skipped.** The fs uploader never calls
     `GetMetadataAllocated`/`GetMetadataDelta`. On a Block PVC that means kopia
     walks the raw device as a single file — the entire device is read every
     run, including unallocated extents, and the whole point of #9714 is lost
     without any signal to the operator.
  2. **The failure mode is invisible in exactly the situation it is most
     likely to occur.** The policy ConfigMap is a per-backup CLI flag, not a
     property of the Backup schedule target or the PVC, so it must be repeated
     on every single `velero backup create`. One omission in a scripted or
     scheduled workflow silently degrades that run, and the run still reports
     success. Every scripted invocation in this validation effort that omitted
     the flag produced a "successful" backup that was not a block-mode backup
     (`t17-i1-svghb`, `t18-fs-jgxwd` — both logged `realSource=cbt-test/data-block`
     under the *fs* code path).
  3. Note the logged `realSource` is misleading here: both providers namespace
     the source path by uploader type (`provider/block.go:132`,
     `provider/kopia.go:158`), but the logrus `WithFields` binding happens
     *before* that reassignment, so the log always prints the unprefixed path.
     The prefix is what actually lands in the repo. An operator grepping for
     `realSource` sees an identical string in both modes.
- What is **not** broken, part 1 — **chain safety** (measured, T18): a stale
  fs-mode snapshot cannot poison a later block-mode incremental, in either
  direction. Two independent mechanisms prevent it:
  1. **Source-path namespacing (primary).** `provider/block.go:129-133` and
     `provider/kopia.go:154-159` prefix `realSource` with the uploader type, so
     block snapshots live under `<requestor>/velero-block/<ns>/<pvc>` and fs
     snapshots under `<requestor>/kopia/<ns>/<pvc>`. Both parent searches are
     path-scoped (`rep.ListSnapshot(ctx, path)` in block,
     `listSnapshotsFunc(ctx, rep, sourceInfo)` in fs), so the other mode's
     snapshots are never even candidates.
  2. **Uploader-type tag filter (redundant second defense).**
     `block/snapshot.go:271-273` rejects any candidate whose
     `snapshot-uploader` tag `!= uploader.BlockType`;
     `kopia/snapshot.go:359-361` rejects any whose tag differs from the current
     run's. The constants are disjoint (`"velero-block"` vs `"kopia"`,
     `pkg/uploader/types.go:25-26`).
- What is **not** broken, part 2 — **restore integrity** (measured, T18): the
  silently-fs-uploaded backup of a Block PVC restores byte-for-byte. Both
  `t18-fs` (no policy flag, fs uploader) and `t18-blk` (policy flag, block
  uploader) were taken over the same source, then restored side by side into
  `cbt-t18fs` / `cbt-t18b` and compared against the live source at three
  granularities:
  - all 9 files on the Filesystem PVC — identical md5s;
  - 5 raw-device region probes at 600-620, 700-710, 800-810, 1000-1010 and
    2500-2520 MiB (the last two straddling the T18 write at 1000 MiB and the
    region past the pre-T17 2 GiB end) — identical md5s;
  - the whole 3 GiB `/dev/xvda` — `b0040ae8216de05c5123a3bed69cf0a8` in all
    three of source, fs-mode restore and block-mode restore. A whole-device
    md5 also pins the device length, so size is covered too.

  So the cost of forgetting the flag is transfer volume and lost CBT, not
  data. That is what keeps this at medium rather than critical — and it is
  also precisely why it needs a warning: nothing downstream ever fails, so
  the misconfiguration can persist indefinitely.
- Suggested fix (either, or both):
  1. Emit a Backup **warning** when a PVC with `volumeMode: Block` is handled by
     the filesystem uploader — this is almost always a misconfiguration, and a
     warning is visible in `velero backup describe` without changing behavior.
  2. Better: make the data mover selectable without a per-invocation CLI flag —
     honor a PVC/StorageClass annotation, or let the DataUpload inherit a
     server-level default — so the block path cannot be lost by forgetting one
     argument on one `backup create`.
- Reproduce: create a `volumeMode: Block` PVC on a CBT-capable CSI driver, run
  `velero backup create <name> --include-namespaces <ns> --snapshot-move-data
  --backup-type Incremental` with **no** `--resource-policies-configmap`, then
  grep node-agent logs for that DataUpload name. The presence of
  `"Starting backup"` (fs) instead of `"Run block backup, CBT source info"`
  (block) is the tell; the Backup CR itself shows nothing.

### BUG-13: a perfect (zero-delta) incremental is unrepresentable — it renders as a full-size transfer

- Severity: medium (reporting/observability; no data loss). Not Ceph-specific —
  affects any CBT-capable driver, and the same gate exists on the filesystem
  uploader path.
- Symptom: when a CBT incremental has an **exactly zero** delta — nothing
  changed since the parent snapshot — velero reports it identically to a backup
  that moved the entire device. `velero backup describe --details` prints
  `Moved data Size (bytes): 3221225472` with `Result: succeeded` and **no
  incremental line at all**, for a run that moved **zero bytes**. The
  DataUpload's `status.incrementalBytes` is likewise absent rather than `0`.
  The best possible CBT outcome is displayed as the worst possible one.
- Why it matters: this is exactly backwards for the feature's headline value.
  A *working but imperfect* incremental (10 MiB delta) renders an
  `Incremental data Size (bytes): 10485760` line, so the operator sees the CBT
  benefit; a *perfect* incremental renders nothing, so the operator sees only
  a full-device figure. It is also indistinguishable from three other states:
  (a) a genuine full transfer, (b) a BUG-9 whole-device fallback, and (c) an
  older backup taken before incremental accounting existed.
- Chain (each link verified in-tree):
  1. `pkg/uploader/block/snapshot.go:86` — `IncrementalSize: backupSize`,
     where `backupSize` is bytes actually written this run. A true zero delta
     legitimately produces `0`. Nothing wrong here.
  2. `pkg/controller/data_upload_controller.go:515` —
     `du.Status.IncrementalBytes = result.Backup.IncrementalBytes` writes the
     `0`. (Mirror for pod volume backups:
     `pkg/controller/pod_volume_backup_controller.go:553`.)
  3. `pkg/apis/velero/v2alpha1/data_upload_types.go:168-170` and
     `pkg/apis/velero/v1/pod_volume_backup_types.go:121-123` — the field is
     `json:"incrementalBytes,omitempty"`, so `0` is erased from the serialized
     object. `oc get ... -o custom-columns=INCR:.status.incrementalBytes`
     prints `<none>`, not `0`. **A measured zero and a never-measured zero are
     now the same bytes on the wire.**
  4. `pkg/backup/backup.go:1336` — copies the `0` into
     `SnapshotDataMovementInfo.IncrementalSize`.
  5. Display gates suppress the line on `0`:
     `pkg/cmd/util/output/backup_describer.go:742`
     (`if info.SnapshotDataMovementInfo.IncrementalSize > 0`),
     `pkg/cmd/util/output/backup_describer.go:918-933` (`volumesByPod.Add`,
     same `> 0` condition), and
     `pkg/cmd/util/output/backup_structured_describer.go:470-472`.
  6. The only surviving output is
     `pkg/cmd/util/output/backup_describer.go:741` —
     `Moved data Size (bytes): %d` — sourced from `TotalBytes`, which for the
     block uploader is the **device length**, not the transferred volume.
- Live evidence (T19, backup `t19-zero`, zero writes since `t18-blk`):
  ```
  NAME               PHASE       TOTAL        DONE         INCR
  t19-zero-sxctg     Completed   3221225472   3221225472   <none>   # block, 3 GiB
  t19-zero-hrmfz     Completed   2147483648   2147483648   <none>   # fs,    2 GiB
  ```
  Node-agent log for the same runs (`pkg/uploader/provider/block.go:153`)
  states the truth plainly:
  `Block backup finished, snapshot ID 3e3b81d8b72f53404662d996203632c4, backup size 3221225472, incremental size 0`.
  And `velero backup describe t19-zero --details`:
  ```
      CSI Snapshots:
        cbt-test/data-block:
          Data Movement:
            Data Mover: velero-block
            Uploader Type: kopia
            Moved data Size (bytes): 3221225472
            Result: succeeded
  ```
  Contrast with `t18-blk-5tmz6` (10 MiB delta), which *does* render
  `Incremental data Size (bytes): 10485760`.
- Corroboration that the accounting itself is sound (so this is purely a
  representation bug): `t14-full` 199229440 / 387973120, `t14-i2` 14680064 /
  10485760, `t17-i2` 9437184 / 20971520, `t18-blk` 10485760 / 8388608 — all
  non-zero, all rendered correctly.
- **Fix implemented locally and validated live** (in this worktree, for
  upstreaming). The requirement driving it: operators must be able to see that
  CBT is saving them transfers, and today the best possible result is the one
  that shows nothing.
  - **`omitempty` had to go from two places, not one.** Besides the API status
    fields, `pkg/datapath/types.go` `BackupResult.IncrementalBytes` also carried
    `omitempty`, and that struct crosses a JSON boundary from the data mover pod
    to the controller (`pkg/datapath/micro_service_watcher.go:366`
    `json.Unmarshal(...&backupResult)`). So a measured zero was being destroyed
    **twice**, and the first destruction happened before the controller ever saw
    it. Dropping `omitempty` there is sufficient and correct — every uploader
    always reports a figure, so 0 internally always means "transferred nothing".
  - **API fields moved to `*int64`** on `DataUploadStatus` and
    `PodVolumeBackupStatus` (both already carried `+optional`, so the generated
    CRD schema is unchanged — `*int64` and `int64` both render as
    `type: integer, format: int64`, and no CRD regeneration is required).
    `nil` = not measured, `&0` = measured zero. Deepcopy updated by hand in both
    `zz_generated.deepcopy.go` files.
  - Why a pointer rather than just dropping `omitempty` on the API fields:
    the field **shipped in v1.18.0–v1.18.2** (`git tag --contains 5fc76db8c`), so
    real backups exist in the wild whose stored volume info has no
    `incrementalSize` at all. With a plain `int64` those unmarshal to `0` and
    would render `Incremental data Size (bytes): 0` — a *false* claim of a
    perfect incremental on a backup that never measured one. The pointer keeps
    the two apart. **This was verified live, and it is the concrete
    justification for the API change** — see T25.
  - Propagation: `internal/volume` `SnapshotDataMovementInfo.IncrementalSize` and
    `PodVolumeInfo.IncrementalSize` also become `*int64`; controllers wrap with
    `ptr.To(...)` at the API boundary
    (`data_upload_controller.go:515`, `pod_volume_backup_controller.go:553`);
    `pkg/backup/backup.go:1336` and `internal/volume/volumes_information.go:255`
    now pass the pointer through unchanged.
  - Display gates relaxed from `> 0` to `!= nil` in all three places:
    `backup_describer.go:742` (`Incremental data Size (bytes)`),
    `backup_describer.go:918-933` (`volumesByPod.Add`, whose signature takes
    `*int64` now — the restore describer passes `nil`, which is correct since
    restores measure no incremental), and
    `backup_structured_describer.go:470-472`, where `size` and `incrementalSize`
    were previously emitted under one shared gate and are now gated separately.
  - Tests: `go build ./...` clean; `pkg/cmd/util/output`, `internal/volume`,
    `pkg/backup`, `pkg/builder`, `pkg/datapath`, `pkg/uploader/...`,
    `pkg/exposer`, `pkg/restore/...` all pass. (`pkg/controller`'s ginkgo
    `TestAPIs` suite fails locally for an unrelated reason — envtest control
    plane binaries are not installed; its non-envtest unit tests pass.
    `pkg/podvolume` has pre-existing `go vet` printf complaints on files this
    change does not touch, and passes with `-vet=off`.)
- **Live proof (T25, cluster 260814, image `ceph-changeid-fix9`).** Ran
  `t25-zero` with no writes at all since `t24-incr`, so the true delta is zero on
  both volumes. Side by side with the unfixed run on the same volumes:
  ```
  t19-zero-hrmfz   Completed   2147483648   <none>   # before
  t19-zero-sxctg   Completed   3221225472   <none>   # before
  t25-zero-qx24q   Completed   2147483648        0   # after
  t25-zero-tzzvt   Completed   3221225472        0   # after
  ```
  and `velero backup describe t25-zero --details` now renders the line that was
  previously absent:
  ```
      cbt-test/data-fs:
        Data Movement:
          Data Mover: velero-block
          Uploader Type: kopia
          Moved data Size (bytes): 2147483648
          Incremental data Size (bytes): 0
          Result: succeeded
  ```
  `-o json` agrees (`"incrementalSize": 0` for both volumes).
  **Backward compatibility verified in the same session**: describing the *old*
  `t19-zero` with the *new* CLI still prints **no** incremental line, and its
  JSON contains zero occurrences of `incrementalSize` — correctly reporting
  "not measured" rather than falsely reporting `0`. That is exactly the case the
  pointer exists for.
  - Operational note worth carrying into the issue: `velero backup describe`
    renders **client-side** from the backup's stored volume info, so seeing the
    new line requires both a server that wrote the field at backup time *and* a
    CLI new enough to render it. An old CLI against new backups silently shows
    the old output.
- Original suggestion (retained for the issue write-up): distinguish "not measured" from
  "measured zero". Two independent halves, either useful alone:
  1. Drop `omitempty` from `IncrementalBytes` on both
     `DataUploadStatus` and `PodVolumeBackupStatus`, or move to a pointer, so a
     measured `0` survives serialization.
  2. Relax the display gates from `IncrementalSize > 0` to a
     "was this run incremental at all" predicate (e.g. gate on the backup being
     type `Incremental`, or on a separate `IncrementalMeasured` bool), so
     `Incremental data Size (bytes): 0` prints. Note this touches the golden
     strings in `pkg/cmd/util/output/backup_describer_test.go:614` and `:644`.
  A narrower alternative that fixes the worst of it without an API change:
  when the run is an incremental, label the existing figure as the source
  device size rather than "Moved data Size", since for the block uploader
  `TotalBytes` never means moved bytes. `TotalBytes` cannot simply be
  repurposed — it is consumed as "volume/snapshot size" by
  `pkg/backup/backup.go:1335`,
  `pkg/restore/actions/dataupload_retrieve_action.go:79`,
  `internal/volume/volumes_information.go:254`/`:268`,
  `pkg/backup/actions/csi/pvc_action.go:520`,
  `pkg/restore/actions/csi/pvc_action.go:254`, and `pkg/podvolume/util.go:98`.
  **Still open after the fix above**: `Moved data Size (bytes)` remains bound to
  `Size`, which for the block uploader is the *device length*, not the moved
  volume. With the incremental line now always present the savings are legible,
  but that label is still inaccurate and is worth a follow-up decision.
- Sub-finding (same describe output, separate one-line fix): the Backup header
  prints `Data Mover: velero-fs` — derived from an unset `backup.spec.dataMover`
  — while every per-volume block underneath prints `Data Mover: velero-block`.
  The header is the field the operator reads first and it contradicts reality
  whenever the mover is chosen by volume policy rather than by the Backup spec.
- Reproduce: run any successful CBT incremental, then immediately run a second
  incremental with **no** intervening writes. The second one moves nothing;
  `velero backup describe <name> --details` will report the full device size
  with no incremental line, and `.status.incrementalBytes` will be absent.

### BUG-14: cancelling a block data mover backup reports it as a failure — the cancel sentinel is compared with `==` against an error that is always wrapped

- Component: velero (`pkg/uploader/provider/block.go`)
- Severity: major (user-initiated cancellation is indistinguishable from a real
  failure; it produces a `PartiallyFailed` Backup and a `Failed` DataUpload with
  an error message, so it will page people and may drive retry logic)
- Scope: **not Ceph-specific.** Pure velero block-data-mover logic; every driver
  using the block uploader is affected, vSphere included. The filesystem/kopia
  provider does **not** have this bug.
- Symptom: cancel a DataUpload the supported way (`.spec.cancel = true`) and the
  uploader does stop correctly, but the outcome is reported as an error:
  ```
  NAME               PHASE    CANCEL   MSG
  t27-cancel-bvwf2   Failed   true     data path backup failed: … Failed to run block backup:
    Failed to run uploader backup for si {…}: error backing up bdev
    snapshot-data-upload-download/velero-block/cbt-vid/data-vid: error writing data: uploader is canceled

  NAME         PHASE             ERR   WARN
  t27-cancel   PartiallyFailed   1     <none>
  ```
  Expected: DataUpload `Canceled`, Backup not counted as failed.
- Root cause — sentinel equality against a wrapped error:
  ```go
  // pkg/uploader/provider/block.go:137 (and :183 for restore)
  if err == block.ErrCanceled {              // never true
      log.Warn("Block backup is canceled")
      return …, ErrorCanceled
  }
  if err != nil {
      return …, errors.Wrapf(err, "Failed to run block backup")   // always taken
  }
  ```
  `block.ErrCanceled` (`pkg/uploader/block/uploader.go:40`) is raised deep in the
  write loop (`uploader.go:332/336/517/521`) and then wrapped **twice** before it
  reaches the provider:
  1. `pkg/uploader/block/uploader.go:110` — `errors.Wrapf(err, "error backing up bdev %s", …)`
  2. `pkg/uploader/block/snapshot.go:121` — `errors.Wrapf(err, "Failed to run uploader backup for si %v", …)`

  So the `==` comparison cannot match, the `ErrorCanceled` returns at `:139` and
  `:185` are **unreachable dead code**, and every cancellation falls through to
  the generic error path.
- Contrast with the filesystem provider, which gets this right by asking the
  uploader for its **state** instead of inspecting the error
  (`pkg/uploader/provider/kopia.go:179`):
  ```go
  if kpUploader.IsCanceled() {
      log.Warn("Kopia backup is canceled")
      return snapshotID, false, 0, 0, ErrorCanceled
  }
  ```
  A state query is immune to wrapping. The block provider is the odd one out.
- Why the existing tests missed it — two compounding reasons, both worth fixing
  upstream:
  1. `pkg/uploader/provider/block_test.go:260` injects the **bare** sentinel
     (`mockBackupErr: block.ErrCanceled`), which is a shape production never
     produces, so `==` matched in the test and only in the test.
  2. The assertion was `require.ErrorContains(t, err, "uploader is canceled")`,
     and **`provider.ErrorCanceled` and `block.ErrCanceled` have identical
     message text** (`provider.go:39` vs `uploader.go:40`). A substring assertion
     therefore passes whether or not the sentinel was recognised. Any test here
     has to assert identity (`require.ErrorIs`), not message.
- Fix implemented locally: `errors.Is(err, block.ErrCanceled)` at both
  `block.go:137` (backup) and `:183` (restore). One word each; `cockroachdb/errors`
  wrapping preserves the chain, so `errors.Is` traverses it.
  Test added: `TestBlockProviderCancelThroughWrappedError`, which injects the
  doubly-wrapped sentinel exactly as production builds it and asserts
  `require.ErrorIs(err, ErrorCanceled)`. Verified non-vacuous — reverting either
  site to `==` fails it, and the failure output reproduces the live error chain
  character for character.
- **Fix validated live (T29, image `ceph-changeid-fix10`)**, same cancellation
  procedure as the T27 reproduction:
  ```
  t27-cancel-bvwf2    Failed     true   data path backup failed: … uploader is canceled   # before
  t29-cancel2-s99vb   Canceled   true   <none>                                            # after
  ```
  The DataUpload now reports `Canceled` with **no** error message.
- **Scope of the fix, stated precisely**: the *Backup* still ends
  `PartiallyFailed` with 1 error in both runs. That is a separate, higher-level
  rollup and is arguably correct — a single DataUpload was cancelled out from
  under a still-running Backup, so that volume genuinely was not captured, and
  velero offers no "cancel the whole backup" verb (the granular
  `.spec.cancel` on the DataUpload is the operation being performed). This fix
  corrects the layer it should correct — the uploader's own reporting — and does
  not claim to change the Backup rollup. Whether a fully-cancelled Backup should
  roll up to `Cancelled` rather than `PartiallyFailed` is a maintainer question
  worth raising in the same issue, not something to change unilaterally.
- Related latent hazard (**not** currently a bug, worth flagging in the same PR):
  `pkg/datapath/data_path.go:214` and `:247` use the same fragile pattern,
  `err == provider.ErrorCanceled`. That works today only because both providers
  return `ErrorCanceled` bare. If anyone ever wraps it, cancellation breaks again
  in the same silent way. `errors.Is` there too would make the whole chain
  wrapping-proof.
- Reproduce: fill a volume with enough data that the transfer is not
  instantaneous (a mostly-unallocated device finishes in ~2 s and is hard to
  catch), start a backup, wait for its DataUpload to reach `InProgress`, then
  `oc patch datauploads.velero.io <du> --type=merge -p '{"spec":{"cancel":true}}'`.
  Observe `Failed` rather than `Canceled`.

### GAP-15: losing the data mover pod wedges the DataUpload in `InProgress` indefinitely — nothing detects it until a node-agent restart

- Component: velero (`pkg/controller/data_upload_controller.go`)
- Severity: major (a backup hangs with no error and no timeout; the Backup sits
  in `WaitingForPluginOperations` and never resolves on its own)
- Scope: **not Ceph-specific.** Data mover pod lifecycle, common to every driver.
- Symptom: delete the data mover pod while its DataUpload is `InProgress` and
  nothing happens — for as long as the owning node-agent keeps running.
  Observed live for **11+ minutes** with no change:
  ```
  NAME               PHASE        TOTAL    INCR     MSG
  t30-dmkill-p42qb   InProgress   <none>   <none>   <none>

  NAME         PHASE
  t30-dmkill   WaitingForPluginOperations
  ```
  The pod is gone and is **not** recreated (`pods "t30-dmkill-p42qb" not found`).
- Root cause: the `InProgress` branch of the reconciler
  (`data_upload_controller.go:416`) only handles `du.Spec.Cancel`. There is no
  liveness check on the exposed data mover pod in that path. The preparing
  timeout does not apply either — `:313-315` gates `onPrepareTimeout` on
  `du.Status.AcceptedTimestamp` and only fires while the DataUpload is still in
  the *Accepted* phase, which this one is long past.
- Recovery exists but is only reachable by accident:
  `AttemptDataUploadResume` (`:1100`) walks `InProgress` DataUploads owned by the
  current node, tries to resume each, and cancels the ones that cannot be
  resumed — but it runs **only at node-agent startup**. Confirmed live: deleting
  the node-agent on `du.status.node` resolved the wedge within 5 seconds:
  ```
  t30-dmkill-p42qb   Canceled   Resume InProgress dataupload failed with error
                                expose info missed for du t30-dmkill-p42qb, mark it as cancel
  t30-dmkill         PartiallyFailed   1
  ```
- **Workaround** for operators: restart the node-agent named by
  `du.status.node`. Nothing else clears it.
- This also explains why losing the *node-agent* is survivable (T28) while losing
  the *data mover pod* is not. In T28 the node-agent was force-deleted mid
  transfer and the backup still completed, moving exactly 943,718,400 B — because
  the transfer runs in the data mover pod, and the restarting node-agent ran
  `AttemptDataUploadResume`, which successfully re-attached to a pod that was
  still alive. The asymmetry is: node-agent loss triggers the recovery path, data
  mover loss is the thing that needs recovering and triggers nothing.
- Suggested fix (design decision, not a one-liner — raise before coding): watch
  the exposed pod for the `InProgress` phase and treat its disappearance as a
  terminal condition, or apply a data-movement timeout independent of
  `preparingTimeout`. The cancel-on-failed-resume logic already exists and does
  the right thing; it just needs a trigger that is not "an operator happened to
  restart the node-agent".
- Reproduce: start a backup with enough data to stay `InProgress` for a while,
  wait for that phase, then
  `oc delete pod -n <velero-ns> <dataupload-name> --grace-period=0 --force`
  (the exposer names the data mover pod after the DataUpload). Watch it hang.

## Validation results (live, cluster 260814, 2026-08-15)

| Test | Result |
|---|---|
| T1 volume policy `dataMover: velero-block` routing | PASS (DataUpload.spec.datamover=velero-block, fs+block PVCs) |
| T1 full via GetMetadataAllocated (post BUG-4 fix + sidecar v1.1.0) | PASS — t3-full-fix: allocated-only upload, fs 329,252,864B, block 157,286,400B (=exactly 150Mi written) of 2Gi volumes |
| ChangeID retrieval for Ceph (#9714 core) | PASS with BUG-4 fix: changeID = VSC snapshotHandle (`0001-0011-openshift-storage-...`), volumeID = PV volumeHandle; kopia tag round-trip verified (t4 called GetMetadataDelta with t3's handle). Re-confirmed on the T11 success path — `CBT source info` carried a non-empty changeID on every run, and the incremental consumed the *prior* run's `cbt-change-id` tag as its delta base (the struct's own `changeID` field is the current run's new handle, not the base) |
| Incremental delta (T2/T4, pre-fix) | BLOCKED by GAP-6 (retention) — graceful fallback-to-full works as designed, but unreachable on Ceph in any stock configuration: no base snapshot handle survives a DataUpload, so `GetMetadataDelta` is never exercised and every "Incremental" is a full transfer |
| **Incremental delta (T11, post GAP-6 fix)** | **PASS** — first working Ceph incremental. `t11-incr` moved 32,505,856 B (data-fs) and 20,971,520 B (data-block) against 2 GiB devices. The block figure is **exactly** the 20 MiB written at `seek=600` — `GetMetadataDelta(base=<prior cbt-change-id tag>, target=<current changeID>)` returned the precise changed extent. No `Forcing full snapshot`, no omap `no snap source` error, no BUG-9 whole-device fallback |
| Fallback on missing changeID tag (T4 pre-fix behavior) | PASS — "No ChangeID tag from parent snapshot, fallback to full backup" |
| Snapshot retention (T5, stock velero) | FAIL (GAP-6) — `snapshot_count: 0` on both source RBD images after 4 backups, 0 VS/VSC cluster-wide, `rbd trash` empty; `fast-diff` enabled so storage-side CBT is available. Fixed by the GAP-6 patch below |
| Snapshot retention via `deletionPolicy: Retain` class (T8, stock velero) | FAIL (GAP-6) — class `ocs-rbd-retain` selected correctly per PVC annotation (`pvc_action.go:248` logs it) yet snapshot still destroyed; root cause is hardcoded `DeletionPolicy: Delete` on velero's own backup VSC (`pkg/exposer/csi_snapshot.go:588`), killed by `CleanUp` (:523) at DataUpload completion — CSI `DeleteSnapshot` RPC observed in the same second as each completion |
| **Snapshot retention via `deletionPolicy: Retain` class (T11, post GAP-6 fix)** | **PASS** — with the backup VSC inheriting the source policy, the retained base snapshot survives DataUpload cleanup. `rbd ls ocs-storagecluster-cephblockpool` shows the `csi-snap-<uuid>` clone images persisting after completion, and the next incremental successfully diffs against them. Selected purely via volume policy `snapshotClass: ocs-storagecluster-rbdplugin-snapclass-retain` (no PVC annotation, no class label) |
| Restore integrity (T6) | PASS — t6-restore from t3-full-fix into cbt-restored: fs file1 `46e6370e…` + file2 `4c54f934…` byte-identical, post-backup file3 correctly absent, raw block 512MiB hash `585b660b…` exact; kopia tags (`cbt-change-id`, `cbt-volume-id`) verified present in restore logs |
| Delete-base-full, restore dependent incremental (T7 A→C-early) | PASS — baseline restore of `t4-incr-fix` into cbt-t4a matched all 4 hashes; deleted base full `t3-full-fix` (CR + S3 prefix + its 2 DataUploads gone in ~10s, DeleteBackupRequest self-reaped); re-restored `t4-incr-fix` into cbt-t4b → **identical** `46e6370e…` / `4c54f934…` / `92c0af9f…` / block `26c7b274…`. Deleting the base did not corrupt or orphan the dependent incremental. See note below on kopia size growth |
| Restored Pod network annotations | FAIL (GAP-7) — restored Pod carries verbatim `k8s.ovn.org/pod-networks`; cbt-t4a happened to land on the node owning `10.131.0.57/23` and ran, cbt-t4b landed elsewhere and hung in `FailedCreatePodSandBox` forever. Recurred in T12: `writer-5d997c6c7d-sg224` came up Running 1/1 at the same stale IP `10.131.0.57` on `ip-10-0-94-134.ec2.internal` — it won the placement lottery again, which is luck, not a fix. Reproduced twice more in T18, this time from two *different* backups of the same source restored side by side — both restored pods claimed `10.131.0.57` / `0a:58:0a:83:00:39` and hung in `ContainerCreating` while their PVCs attached and mapped fine, so the collision is per-source-pod and independent of uploader mode. Five reproductions, five identical annotations. Fix exists in-tree (#10098) but is opt-in and unconfigured here; `oc delete pod` (RS recreates) is the reliable workaround for controller-owned pods |
| Restore after index compaction / epoch advance (T7 C-late) | PASS — forced repo maintenance (job `…-1786810016486`) rewrote index blobs and advanced epoch markers, then restored `t4-incr-fix` into cbt-t4c → all 4 hashes still byte-exact (`46e6370e…` / `4c54f934…` / `92c0af9f…` / block `26c7b274…`, sizes 209715200 / 104857600 / 31457280). Kopia index maintenance does not disturb the CBT-backed snapshot chain |
| Force blob reclamation after backup delete | FAIL (GAP-8) — `maintenanceFrequency` patched `1h`→`1m` starts jobs on demand but they are always **quick**; only kopia's own 24h full cycle runs `snapshotgc`. No supported knob reaches `ModeFull` |
| Backup deletion cleanup, all three layers (T9) | PASS — deleted `t8-full`: (1) K8s: Backup CR + both DataUploads gone; (2) S3: `s3://tkaovila-oadp/ceph-changeid/backups/t8-full/` (9 objects) removed; (3) repo: kopia `forget` fired for snapshots `0de3c289731c130c79030813592ce3f4` and `c7d24e62460d9a41c84dc0d2699c6516` at 16:41:25 (`backup_deletion_controller.go:600`). Repo-side removal is inline in the velero pod (`deleteMovedSnapshots`), synchronous, and its errors gate `backupStore.DeleteBackup` — that gate succeeding proves `len(errs) == 0`, i.e. the forgets were error-free. No `datadelete` CRD is involved |
| Bytes actually uploaded per run (CBT benefit, measured) | **PASS both paths, post GAP-6 fix.** Fulls: allocated-blocks path moves 157–360 MB of a 2 GiB device (7–17%). Incrementals: 20,971,520 B (1.0%) and 32,505,856 B (1.5%) — a 50–100x reduction vs a full. Pre-fix incrementals fell back and moved the whole 2,147,483,648 B, *worse than a full* (BUG-9); that regression is now only reachable on a genuine CBT error. Sources: the node-agent log line at `pkg/uploader/provider/block.go:153` (`backup size <device> , incremental size <transferred>`), and — machine-readable — DataUpload `.status.incrementalBytes`, written at `pkg/controller/data_upload_controller.go:515`. Both agree on every run measured here. Do **not** use `status.progress.bytesDone` / `totalBytes` for this: both report the device size, not uploaded bytes (`block.go:146-151` deliberately sets `BytesDone = TotalBytes = <device size>` on completion, mirroring `kopia.go:184-189`). One caveat on `.status.incrementalBytes`: the field is `omitempty`, so an exactly-zero delta serializes as absent and renders `<none>` rather than `0` — see BUG-13. In that one case the log line is the only surviving evidence |
| **Restore from an incremental (T12)** | **PASS** — restored backup `t11-incr` (the 20 MiB transfer) into `cbt-t11r`; Restore Completed, both DataDownloads Completed. All six md5s byte-identical to source: `/data/file-t11.bin` `47fa79d9…`, `file1.bin` `08eab60b…`, `file2-delta.bin` `7d5312d6…`, `file3-incr.bin` `2d43f92e…`, full 2 GiB `/dev/xvda` `95d7032e…`, and the 600–620 MiB written region `f872244e…`. A 20 MiB incremental reconstructs the full 2 GiB device exactly. 9 warnings, all benign OCP `already exists` collisions (auto-created RoleBindings, `kube-root-ca.crt`/`openshift-service-ca.crt`, `reclaimspacecronjobs` CRD) |
| CBT base-snapshot lifecycle after retention | FAIL (BUG-11) — retained `csi-snap-*` clone images have no owning K8s object and no velero GC path: 4 orphans in the pool vs 0 VS/VSC cluster-wide. Grows one per volume per backup, unbounded |
| Backup deletion reclaims retained base (T13) | FAIL (BUG-11) — deleted both `t11-full` and `t11-incr`; each Backup CR gone in ~20 s, but `rbd ls \| grep -c csi-snap` returned `4` before and after, same four UUIDs, and `rbd trash ls` was empty. Nothing on the storage side is reclaimed, and no backup references them any more |
| Retained bases do not affect live volumes (T13) | FAIL (BUG-11) — each orphan is a `clone-child` with `overlap: 2 GiB` whose parent is a snapshot of the **in-use** `csi-vol-*`. `rbd children` shows 2 per source volume. The pinning snapshots sit in the trash namespace, so `rbd snap ls` on the live volume prints nothing and only `rbd snap ls --all` reveals them |
| **Incremental chain depth ≥ 2 (T14)** | **PASS** — a second incremental diffs against the *previous incremental*, not against the base full. Fresh chain `t14-full` → 10 MiB at `seek=700` → `t14-i1` → 10 MiB at `seek=800` → `t14-i2`. Block volume transfers (`block.go:153`): `t14-i1` **10,485,760 B** and `t14-i2` **10,485,760 B** — exactly 10 MiB each. Had `t14-i2` diffed against the full it would have moved ~20 MiB. Log proof of the parent link: `t14-i2-j8cfj` logs `Using previous snapshot kddf4c2e6aabc8ae7c49999c09e0087c1` (`snapshot.go:169`) and `Using parent snapshot , start time 2026-08-15 18:07:45.031290256` (`snapshot.go:193`) — that start time is `t14-i1-nqsd9`'s completion instant. `lib_repo.go:500` confirms `in block mode with parent IIx8283656969f96ed3decb6f66f1021338`. Neither incremental logged `Forcing full snapshot`, so BUG-9's fallback never fired. The fs volume behaved consistently (14,680,064 B on both, = 10 MiB payload + fs metadata) |
| Full path uses allocated blocks, not device size (T14) | **PASS** — `t14-full` moved **199,229,440 B** (block, 190 MiB) and **387,973,120 B** (fs) against 2,147,483,648 B devices. Both runs logged `Forcing full snapshot` (expected: no parent in a fresh chain) yet still went through `GetMetadataAllocated` rather than BUG-9's whole-device `SetFull()` — the two paths are distinguishable precisely because the transferred size is far below the device size |
| **Restore from a 2-deep incremental chain (T15)** | **PASS** — restored `t14-i2` (chain head, 2 incrementals past the full) into `cbt-t14r`; Restore Completed, both DataDownloads Completed, 9 warnings (same benign OCP `already exists` collisions), 0 errors. All 11 md5s byte-identical to source: 7 files under `/data` (`checksums.txt` `962beadb…`, `d1.bin` `4d94a74e…`, `d2.bin` `531eab88…`, `file-t11.bin` `47fa79d9…`, `file1.bin` `08eab60b…`, `file2-delta.bin` `7d5312d6…`, `file3-incr.bin` `2d43f92e…`), the whole 2 GiB `/dev/xvda` `d8c1b195…`, and all three written regions — 600–620 MiB `f872244e…`, 700–710 MiB `eceac904…`, 800–810 MiB `2fa070a2…`. Cross-check: the 600–620 MiB hash and the four older file hashes are unchanged from T12, so data written before the (since-deleted) t11 backups survived two further incrementals intact |
| **Delete a mid-chain backup, restore the head (T16)** | **PASS** — chain `t14-full → t14-i1 → t14-i2`, deleted the *middle* backup `t14-i1`, then restored the head `t14-i2` into `cbt-t16r`. Restore Completed, 9 benign warnings, 0 errors; all 11 md5s byte-identical (`checksums.txt` `962beadb…`, `d1.bin` `4d94a74e…`, `d2.bin` `531eab88…`, `file-t11.bin` `47fa79d9…`, `file1.bin` `08eab60b…`, `file2-delta.bin` `7d5312d6…`, `file3-incr.bin` `2d43f92e…`, whole device `d8c1b195…`, regions `f872244e…` / `eceac904…` / `2fa070a2…`). The full deletion path fired for `t14-i1` at 18:15:44–46Z (`backup_deletion_controller.go:497 → :305 → :330 → :338 → :345 → :365 → :373 → :460`), so this is a real delete, not a no-op. Kopia's content-addressed store keeps the head self-sufficient: deleting an intermediate *backup* does not break the chain, because the parent linkage is by kopia snapshot content, not by Backup CR. Note this is orthogonal to BUG-11 — the RBD base snapshots were still not reclaimed |
| **Volume expansion mid-chain (T17)** | **PASS** — grew `data-block` 2Gi→3Gi *online* between incrementals (`oc patch pvc`), `status.capacity` reached `3Gi` in ~20 s with no pod restart and no leftover `FileSystemResizePending` condition; in-pod device grew live (`dd skip=3060` reads OK, sysfs `rbd1` = 6291456 sectors = 3 GiB). Wrote 20 MiB at `seek=2500` — i.e. **entirely past the old 2 GiB end** — plus a 5 MiB `/data/file-t17.bin`, then took `t17-i2` against a base snapshot captured while the device was still 2 GiB. Block volume: `backup size 3221225472, incremental size 20971520` (`block.go:153`) — the new device length is picked up by `dev.Seek(0, SeekEnd)` (`block/snapshot.go:72`) and the transfer is **exactly** the 20 MiB written, so `GetMetadataDelta` handled a target larger than its base correctly. No `Forcing full snapshot`, so BUG-9's whole-device fallback never fired across the size change. Parent linkage intact (`Using previous snapshot k4b8eed4303d5537f5a7dbfb123a1021a`, parent start `18:09:06.58` = `t14-i2`'s completion). Unexpanded fs volume in the same backup stayed at `backup size 2147483648, incremental size 9437184`. Restore of `t17-i2` into `cbt-t17r` Completed (9 benign warnings, 0 errors): restored PVC comes back at **3Gi** (not the original 2Gi), and all 12 checksums are byte-identical to source including `file-t17.bin` `1456b992…` and the new-region 2500–2520 MiB `572f9032…`; whole 3 GiB device `7ce31796…` matches exactly |
| **Mixed-mode chain safety (T18)** | **PASS** — an fs-mode snapshot sitting *newer* in the repo than the last block-mode one does not get picked up as a block parent, and vice versa. Sequence: `t17-i2` (block) → `t18-fs` (no policy flag, so silently fs — BUG-12) → `t18-blk` (policy flag, block), deleting nothing in between so both candidates were live. `t18-blk-5tmz6` logged `Using previous snapshot kdf7c2a8d9522c91efe37d7c8d6045bc3` with parent start `18:28:24.615407443` = `t17-i2`'s block snapshot, **skipping** the newer 18:44:07 fs snapshot `8a6caa3df21ee02d1e6b955495c4d5d2`, and reported `backup size 3221225472, incremental size 10485760` — exactly the 10 MiB written at `seek=1000`. No `Forcing full snapshot`, no fallback warning. The fs volume in the same backup behaved identically (parent `k7c9c609672e24d7ce869e9ec8b506827`, start `18:28:25.427`, `incremental size 8388608`). Two independent code-level mechanisms explain it — source-path namespacing by uploader type (`provider/block.go:132`, `provider/kopia.go:158`, both parent searches path-scoped) and the uploader-type tag filter (`block/snapshot.go:271-273`, `kopia/snapshot.go:359-361`). BUG-10 reproduced on both volumes (`snapshot.go:193` prints an empty parent ID) |
| **fs-mode backup of a Block PVC still restores correctly (T18)** | **PASS** — settles BUG-12 as cost/observability rather than correctness. Restored both `t18-fs` (silently fs-uploaded) and `t18-blk` into `cbt-t18fs` / `cbt-t18b`, then compared all three against the live source: 9 `/data` files, 5 raw-device regions (600-620, 700-710, 800-810, 1000-1010, 2500-2520 MiB) and the whole 3 GiB `/dev/xvda` — every md5 identical, whole device `b0040ae8216de05c5123a3bed69cf0a8` in all three. Whole-device md5 pins length, so size matches too. GAP-7 reproduced again on both restores (stale OVN annotations; both restored pods claimed the source pod's `10.131.0.57` / `0a:58:0a:83:00:39` and hung in `ContainerCreating` — storage attached and mapped fine). Workaround confirmed: `oc delete pod` lets the ReplicaSet recreate with a fresh name and no stale annotation; both came up Running (`10.131.0.236`, `10.128.2.65`) |
| **Zero-delta incremental (T19)** | **PASS** (with a reporting defect — BUG-13). Took `t19-zero` immediately after `t18-blk` with **no intervening writes**, so the true delta is exactly zero. Both volumes reported `incremental size 0` (`block.go:153`) — `backup size 3221225472, incremental size 0` (block) and `backup size 2147483648, incremental size 0` (fs). Parent resolution correct: block picked `kf90616578ef4a5fc82ad021b25b5703e` (parent start `18:44:52.011066734`) and fs picked `kb52a1f2342c58c0b3d6a2b8312a79435` (parent start `18:44:45.275377707`) — both `t18-blk`'s snapshots. No `Forcing full snapshot`, no fallback warning, so BUG-9's `SetFull()` whole-device path is **not** reachable on a healthy zero delta — the failure mode is genuinely limited to CBT errors. Content-addressed dedup confirmed end to end: the new snapshot root object ID is **byte-identical to the parent's** on both volumes (`kf9061657…` and `kb52a1f23…` again), i.e. a zero-delta incremental writes no new root. Two defects reproduced: BUG-10 on both volumes (`Using parent snapshot , start time …` — empty ID), and BUG-13, in that `.status.incrementalBytes` renders `<none>` rather than `0` (`omitempty` erases a measured zero) and `velero backup describe --details` prints only `Moved data Size (bytes): 3221225472` with no incremental line — the best possible CBT result displayed as the worst |
| **CBT-error fallback: cost and correctness (T20)** | **BUG-9 reproduced in isolation; data correctness PASS.** With GAP-6 fixed and parent resolution healthy, BUG-9 was triggered on its own by repointing `SnapshotMetadataService.spec.address` at a name that does not resolve (reversible; restored afterwards). Snapshot creation is unaffected, so only `GetMetadataDelta` fails. Wrote 8 MiB at offset 1500 MiB on the 3 GiB block PVC and **nothing** on the 2 GiB fs PVC, then ran `t20-stale --backup-type Incremental`. Both DataUploads report `INCR == TOTAL`: `t20-stale-fgnkb 3221225472` (block, real delta 8 MiB → **384x amplification**) and `t20-stale-mlpd4 2147483648` (fs, real delta **zero**). One unreachable-CBT event turned 8 MiB of change into 5.1 GiB of transfer; the same volumes reported `incremental size 0` one run earlier in T19. Node-agent logs the cause exactly (`GetMetadataDelta(...): rpc error: code = Unavailable desc = name resolver error: produced zero addresses`, `set.go:54` → `snapshot.go:116`), but the Backup object is clean — `PHASE Completed / ERR <none> / WARN <none>` — so nothing above the node agent signals it; with BUG-13 also suppressing the incremental line, there is no API-level indication CBT stopped working. Second amplifier found: `snapshot.go:114-117` also clears `parentBackup.parentObject`, discarding a parent (`kf90616578ef4a5fc82ad021b25b5703e`) that `getParentBackupInfo` had already resolved successfully, so the run loses the kopia incremental base as well as the bitmap. Mitigating: kopia content-addressed dedup still fired — the fs volume produced root `kb52a1f2342c58c0b3d6a2b8312a79435`, byte-identical to T19's, so stored footprint does not grow proportionally; the cost is read I/O, hashing CPU and wall time. **Restore verified byte-identical**: restored `t20-stale` into `cbt-t20r` and compared against the live source — 9 `/data` files, the 8 MiB delta region 1500-1508 MiB (`49ce5c0c3347238bdb0817a554da34c8`) and the whole 3 GiB `/dev/xvda` (`7157f71b8655ed54a0e199ef44290dc6`) all match. So BUG-9 is cost/observability, not correctness. GAP-7 reproduced a sixth time (restored pod `Pending`; `oc delete pod` → `writer-5d997c6c7d-nk655` Ready) |
| **BUG-9 fix: no regression on the healthy path (T21)** | **PASS.** Built the ladder fix into `quay.io/tkaovila/velero:ceph-changeid-fix6` and rolled both the velero Deployment and the node-agent DaemonSet (`pkg/uploader` runs in the node agent). Wrote a 6 MiB delta at offset 2000 MiB on the block PVC, nothing on the fs PVC, then ran `t21-fix6 --backup-type Incremental`. The block volume moved **exactly 6,291,456 B — byte-for-byte the delta written** — and the fs volume moved 0. No `degraded to allocated-blocks` and no `fallback to whole-device` warning appeared, i.e. the healthy `TierChanged` path is unchanged by the patch. Parent chain continued correctly across the preceding BUG-9 fallback: `t21-fix6` picked `ke6b86ac4ed7418405802885ccef9b7b3`, which is `t20-stale`'s root. **Restore verified byte-identical**: `t21r` into `cbt-t21r` matched the source on all 9 `/data` files, both delta regions (1500-1508 MiB `49ce5c0c3347238bdb0817a554da34c8`, 2000-2006 MiB `b0e01c5bd161912ca99691d1ad595015`) and the whole 3 GiB `/dev/xvda` (`6472cc50b0100dd8e76672ca84c26a4f`). GAP-7 reproduced a seventh time. The `TierAllocated` rung is covered separately by T22 |
| **BUG-9 fix: the allocated-blocks rung, live (T22)** | **PASS — the headline result.** Reached the middle rung with no manual `rbd` surgery: took `t22-del` using the default **Delete**-policy snapshot class (`ocs-storagecluster-rbdplugin-snapclass`) via a second policy ConfigMap `block-mover-policy-delete`, so velero's own cleanup removed the physical snapshot. Confirmed gone (`csi-snap-6fb78561-…` absent from `rbd ls`), while the SnapshotMetadataService stayed healthy — this is the pre-GAP-6 condition reproduced through supported paths only. Then wrote a 3 MiB delta and ran `t22-incr --backup-type Incremental` on `ceph-changeid-fix8`. ceph-csi returned a well-formed service error, not a transport one: `rpc error: code = Internal desc = failed to get volume from id "…6fb78561…": key not found: no snap source in omap for "csi.snap.6fb78561-…"`. velero logged the new middle-rung line `CBT delta unavailable for source {…}, degraded to allocated-blocks backup` (`set.go:104` → `snapshot.go:123`) and, critically, `Write object … in block mode without parent` — the parent-drop correctness fix behaving as designed. Transfer: **273,678,336 B on the 3 GiB block volume (8.5%)** and **422,576,128 B on the 2 GiB fs volume (19.7%)**, versus the whole device unfixed — an **11.8x** and **5.1x** reduction on precisely the scenario BUG-9 describes. **Restore verified byte-identical** (`t22r` → `cbt-t22r`): all 9 `/data` files, all four delta regions (1500-1508 `49ce5c0c…`, 2000-2006 `b0e01c5b…`, 2400-2404 `49aa66a4…`, 2600-2603 `dc9be506…`) and the whole 3 GiB `/dev/xvda` (`25c96f72770e20be467cb83a2c80f859`) match the source. This is the load-bearing correctness check for the parent-drop decision: had the parent been retained on this tier, blocks discarded since the parent would have been inherited back. **BUG-10 fix also confirmed live in the same run** — `Using parent snapshot kdd9e660735cf19dc34656fe17d4d5397, start time …` and `kb52a1f2342c58c0b3d6a2b8312a79435` now carry the ID where every prior run printed an empty slot. GAP-7 reproduced an eighth time |
| **Durability of old backups after a long chain + maintenance (T23)** | **PASS.** Question: does an older backup still restore to the same bytes after many newer backups, two BUG-9 fallback events and kopia maintenance? Restored two backups a *second* time and compared against the restore taken when each was fresh: `t14-i2` (originally `cbt-t14r`, 2026-08-15 18:08) → `cbt-t23j`, and `t17-i2` (originally `cbt-t17r`, 18:28) → `cbt-t23i`. Both **byte-identical** across every `/data` file and the whole `/dev/xvda` — `d8c1b195c985866318eead772a808c2e` for the `t14-i2` pair, `7ce317962358a14d66eb155313e22f71` for the `t17-i2` pair — despite 12 intervening backups, a whole-device `TierFull` fallback (T20), two `TierAllocated` degradations (T22) and 3 completed kopia maintenance jobs. Incrementals are the fragile case here since they depend on parent chains that later runs could disturb; they held. Method note: an initial comparison of `t14-full` against `cbt-t14r` showed a difference, but that was a mis-paired test on my part, not a defect — `cbt-t14r` was restored from `t14-i2`, not `t14-full`, and `d1.bin`/`d2.bin` were written between the two backups. Both restores were individually correct. Valid comparisons require the *same* backup restored twice, which is what the two pairs above do |
| **Explicit Full after an incremental chain, and re-parenting (T24)** | **PASS, with a strong cross-check on the BUG-9 fix.** Ran `t24-full --backup-type Full` after the whole chain: logged `Forcing full snapshot` (`snapshot.go:193`), took the allocated-blocks path, and wrote `in block mode without parent`. Transfer: **273,678,336 B (block) and 422,576,128 B (fs)** — **byte-for-byte identical to what T22's degraded `TierAllocated` run moved**. No writes occurred between the two backups, so the allocated set was unchanged, and the match proves the ladder's middle rung transfers *exactly* what an explicit Full transfers. That is precisely the property BUG-9's headline demanded — the original defect was that a failed incremental cost ~6x *more* than a plain full; post-fix it costs exactly the same as one. Then wrote a 5 MiB delta and ran `t24-incr --backup-type Incremental`: it parented onto the Full (`Using parent snapshot k37943fa8c6575e6087ba1baeacd9880f`, start time `03:34:50` = `t24-full`'s snapshot), wrote `in block mode with parent IIx5178f712…`, and moved **exactly 5,242,880 B**. So `--backup-type Full` correctly resets the chain and subsequent incrementals re-parent onto it |
| **BUG-13 fix: a measured zero is now reportable (T25)** | **PASS.** Ran `t25-zero` on `ceph-changeid-fix9` with no writes at all since `t24-incr`, so the true delta is zero on both volumes. `.status.incrementalBytes` now reads **`0`** where the identical scenario reported `<none>` before (`t19-zero`), and `velero backup describe --details` renders `Incremental data Size (bytes): 0` alongside `Moved data Size (bytes): 2147483648`; `-o json` carries `"incrementalSize": 0` for both volumes. **Backward compatibility proven in the same session**: describing the *old* `t19-zero` with the *new* CLI still prints no incremental line and its JSON has zero occurrences of `incrementalSize` — correctly "not measured" rather than a false `0`. That is the case the `*int64` exists for and the reason a plain `omitempty` drop would have been wrong, since the field shipped in v1.18.0–v1.18.2 and real backups predate it. Found while fixing: `omitempty` was erasing the zero **twice** — `pkg/datapath/types.go` `BackupResult` also carried it, and that struct crosses a JSON boundary from the data mover pod to the controller (`micro_service_watcher.go:366`), so the value was already destroyed before the controller could persist it. Operational note: `backup describe` renders client-side, so the new line needs both a server that wrote the field and a CLI new enough to render it — the stale `/tmp/velero-cli` initially showed the old output against a correctly-written backup |
| **Cross-volume parent rejection (T26)** | **PASS — the highest-consequence guard in the feature, and it was unit-only until now.** The kopia repository path is `namespace/pvc`, so recreating a PVC under the same name yields a *different* RBD image on an *unchanged* path: the previous snapshot is still found as a parent while belonging to another volume. Computing a delta there would be silent data corruption. Built the case in a fresh `cbt-vid` namespace: baseline `t26-base` on volume `…-b5f4ae24-…` (1 GiB device, 32 MiB written at offset 100 MiB, moved exactly 33,554,432 B), then deleted and recreated the PVC → volume `…-8937e931-…`, wrote a *different* 16 MiB at offset 300 MiB, and ran `t26-mismatch --backup-type Incremental`. The guard fired: `VolumeID …-b5f4ae24-… from parent snapshot k8e29aa42810acd38619e5a695519cc94 is not expected as …-8937e931-…, fallback to full backup` (`snapshot.go:205`), followed by `in block mode without parent` and a transfer of exactly **16,777,216 B** — the new volume's allocated blocks only. **Verified by content, not just by log**: in the restore, the region 100-132 MiB where the *old* volume held 32 MiB of random data reads as `58f06dd588d8ffb3beb46ada6309436b`, which is the md5 of 32 MiB of zeros; region 300-316 MiB matches the source; whole `/dev/xvda` identical at `4db7787cac2e653d9a1844ab65b401db`. No cross-volume contamination. Bonus: the BUG-10 fix earns its keep here — the rejection warning now *names* the rejected parent snapshot, where before it printed an empty slot on precisely the diagnostic an operator would need |
| **Cancellation mid-flight (T27)** | **FAIL → BUG-14 (new), now fixed.** Filled `cbt-vid` with 900 MiB so the transfer was not instantaneous, started a Full backup, polled until the DataUpload reached `InProgress`, then requested cancellation the supported way (`oc patch … --type=merge -p '{"spec":{"cancel":true}}'`). The uploader stopped correctly, but the outcome was reported as a failure: DataUpload `Failed` with `… error writing data: uploader is canceled`, and Backup `PartiallyFailed` with 1 error. Root cause: `block.go:137` compares `err == block.ErrCanceled` against an error wrapped twice en route (`uploader.go:110`, `snapshot.go:121`), so the cancel branch is unreachable dead code; the restore path at `:183` has the same defect. The filesystem provider avoids it by querying uploader *state* (`kopia.go:179 kpUploader.IsCanceled()`) rather than inspecting the error. Fixed locally with `errors.Is` at both sites, plus `TestBlockProviderCancelThroughWrappedError` which injects the doubly-wrapped sentinel and asserts identity rather than message — necessary because `provider.ErrorCanceled` and `block.ErrCanceled` share the exact same message text, so the pre-existing `ErrorContains` assertion passed either way. Verified non-vacuous: reverting either site to `==` fails the test with the live error chain reproduced character for character |
| **Concurrent backups of one namespace (T31)** | **PASS — no race; velero serialises.** Submitted `t31-a` and `t31-b` together against `cbt-test` after a 7 MiB write. They did not run in parallel: `t31-a InProgress` / `t31-b Queued`. Chaining was then correct — `t31-a` parented on `t25-zero`'s snapshot and moved exactly **7,340,032 B** (the 7 MiB written), and `t31-b` parented on **`t31-a`'s own** snapshot (parent start time `14:37:02.987` matches `t31-a`'s completion) and moved **0**, since nothing was written in between. The fs volume behaved the same way. So the obvious corruption route — two backups racing on one repository path and picking the same or an in-flight parent — is not reachable through the normal backup API, because the Backup controller queues rather than parallelises |
| **Empty / never-written volume (T32)** | **PASS.** Fresh 1 GiB Block PVC in `cbt-empty`, never written (device md5 `cd573cfaace07e7949bc0c46028904ff` = 1 GiB of zeros). A `--backup-type Full` moved **0 bytes** — `Forcing full snapshot`, `in block mode without parent`, `backup size 1073741824, incremental size 0` — i.e. the allocated bitmap was correctly empty rather than defaulting to the whole device. `.status.incrementalBytes` reported `0` (not `<none>`), confirming the BUG-13 fix applies to full backups too. **Restore verified**: `t32r` into `cbt-emptyr` reproduced a full-size device whose md5 matches the source exactly, so a zero-byte transfer still reconstructs the complete 1 GiB zeroed image. GAP-7 reproduced a ninth time |

_(more as testing proceeds)_

### Consolidated transfer measurements

Every CBT run on cluster 260814, block PVC = 3 GiB `data-block`, fs PVC = 2 GiB
`data-fs`. `TOTAL` is `status.progress.totalBytes` (the **device size** — never
the moved volume, see BUG-13); `INCR` is `status.incrementalBytes`, which is the
bytes actually transferred. `<none>` is a measured **zero** erased by `omitempty`
(BUG-13), not a missing measurement.

```
DATAUPLOAD          TOTAL        INCR         READING
t14-full-52pgk      2147483648    199229440   full, allocated-blocks (9.3%)
t14-full-vjpwv      2147483648    387973120   full, allocated-blocks (18.1%)
t14-i2-6zcjp        2147483648     14680064   healthy delta
t14-i2-j8cfj        2147483648     10485760   healthy delta
t17-i2-kb272        2147483648      9437184   healthy delta
t17-i2-wz4zk        3221225472     20971520   healthy delta
t18-blk-5tmz6       3221225472     10485760   healthy delta
t18-blk-zqktc       2147483648      8388608   healthy delta
t18-fs-8cqkg         402653441    402653441   BUG-12 fs mis-route
t18-fs-jgxwd        3221225472       <none>   BUG-12 fs mis-route, file-granular (Anomaly B)
t19-zero-hrmfz      2147483648       <none>   TRUE ZERO delta -> BUG-13
t19-zero-sxctg      3221225472       <none>   TRUE ZERO delta -> BUG-13
t20-stale-fgnkb     3221225472   3221225472   BUG-9 TierFull: 8 MiB delta -> whole device (384x)
t20-stale-mlpd4     2147483648   2147483648   BUG-9 TierFull: zero delta -> whole device
t21-fix6-266cf      3221225472      6291456   post-fix TierChanged, == the 6 MiB written
t21-fix6-lp9tj      2147483648       <none>   post-fix, zero delta
t22-del-dnmhn       3221225472      4194304   post-fix TierChanged, == the 4 MiB written
t22-del-pkw57       2147483648       <none>   post-fix, zero delta
t22-incr-mxbxm      3221225472    273678336   post-fix TierAllocated (8.5%) <-- was whole device
t22-incr-8r8w5      2147483648    422576128   post-fix TierAllocated (19.7%) <-- was whole device
t24-full-9vnlw      3221225472    273678336   explicit Full == T22's TierAllocated, exactly
t24-full-p55vq      2147483648    422576128   explicit Full == T22's TierAllocated, exactly
t24-incr-2rxh2      3221225472      5242880   re-parented onto the Full, == the 5 MiB written
t24-incr-lpswz      2147483648       <none>   zero delta
```

Three things this table shows at a glance:
1. **The delta path is exact.** Post-fix runs report the written delta to the
   byte: 6,291,456 / 4,194,304 / 5,242,880 for 6 / 4 / 5 MiB writes.
2. **BUG-9's cost is the two `INCR == TOTAL` rows**, and the pre-GAP-6 rows
   `t1-full-v4`, `t2-incr`, `t4-incr-fix` (all `2147483648`) are the same
   signature seen before the fixes.
3. **The fix lands the degraded case exactly on the full-backup cost** — the
   `t22-incr` and `t24-full` pairs are byte-identical.


## Non-bug observations

- OCP `5.0.0-0.nightly-multi-2026-08-09-185812` ships marketplace catalogs
  (redhat-operators et al.) with **zero** odf/ocs/ceph packages — ODF not yet
  published for OCP 5.0. Workaround: extra CatalogSource
  `registry.redhat.io/redhat/redhat-operator-index:v4.21` → `odf-operator`
  channel `stable-4.21`. (Environment quirk, not a velero bug.)
- OCP 5.0 nightly: SnapshotMetadataService CRD not present by default;
  installed directly from openshift/csi-external-snapshot-metadata main —
  avoided any featuregate change entirely (no CustomNoUpgrade needed, cluster
  stays upgradeable, VolumeGroupSnapshot gate untouched → RHSTOR-8098 moot).
- ODF 4.21 on OCP 5.0 nightly: `ocs-client-operator` refuses to deploy CSI —
  `failed to find configmap containing images suitable for 5.0.0 platform version`
  (imageset ConfigMaps ship only `csi-images-v4.16`…`v4.21`; note its
  `disableVersionChecks: "true"` config does NOT cover this lookup). Result:
  `ceph-csi-controller-manager` pinned 0/0, no Driver CRs, no RBD provisioner.
  Workaround: clone `csi-images-v4.21` CM as `csi-images-v5.0` **and** set its
  `ocs.openshift.io/csi-images-version: v5.0` label — the operator parses the
  version from that label (requires same major + minor ≤ platform), not the CM
  name (`internal/controller/operatorconfigmap_controller.go`
  `getImageSetConfigMapName`). (ODF bug candidate — for Red Hat Jira, not
  velero GitHub.)
- Upstream `velero install` (main @ 293f6f6a6) defaulted install namespace to
  `openshift-adp` instead of `velero` — likely picked up from kubeconfig
  context namespace; verify before calling it a bug.
- `velero backup delete` makes the kopia repo **larger**, not smaller. Deleting
  `t3-full-fix` took the S3 kopia prefix from 61 objects / 545,544,526 B to
  64 objects / 545,549,651 B — the delete path writes new manifest/tombstone
  blobs and defers actual data-blob reclaim to scheduled repository
  maintenance (`dueForMaintenance`,
  `pkg/controller/backup_repository_controller.go:549-550`). The Backup CR, its
  `backups/<name>/` prefix and its DataUploads *do* disappear within ~10s. This
  is expected kopia behavior and answers the confusion in
  [#9835](https://github.com/velero-io/velero/issues/9835) — worth documenting
  rather than fixing.
- Second deletion (T9, `t8-full`) reproduced the growth exactly: the kopia prefix
  went 72 → 75 objects and 547,675,551 → 547,680,687 B (**+3 objects, +5,136 B**)
  while the backup's own S3 prefix and both DataUploads disappeared. Same
  mechanism as above; cross-reference GAP-8 for why the reclaim never arrives on
  demand.
- Leftover maintenance-job pods after a delete are **not** a leak. Velero keeps
  the most recent N by design: `DefaultKeepLatestMaintenanceJobs = 3`
  (`pkg/repository/maintenance/maintenance.go:56`). Three lingering pods is the
  expected steady state, not evidence of a stuck controller.
- The two size fields on `uploader.SnapshotInfo` are different quantities, not
  duplicates — worth knowing before reading any of the numbers above.
  `Size` is the raw block-device length from `dev.Seek(0, io.SeekEnd)`
  (`pkg/uploader/block/snapshot.go:72`, always 2147483648 here);
  `IncrementalSize` is the bytes actually uploaded — `backupSize` returned by
  `snapshotSource` and assigned at `snapshot.go:86`. Equality between them means
  the bitmap covered the whole device. Also note the uploader-layer field is
  `IncrementalSize`; `IncrementalBytes` exists only on the DataUpload/PVB CR
  status — grepping the wrong one returns nothing and looks like the computation
  is missing.
