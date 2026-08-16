# Ceph × Velero Block Data Mover — Test Plan (velero #9714)

Goal (today): verify Ceph RBD works with velero block data mover (CBT/changeID
path) end-to-end on OpenShift + ODF; log bugs to `ceph-cbt-bugs.md`.

## Environment (target)

- OCP on AWS, 3× workers ≥8 vCPU/24Gi (cluster-manager provisioning; kubeconfig TBD)
- ODF stable via `create-ocp-ceph` skill: `install-ceph.sh --route=odf --version=stable`
  - Feature gate: `CustomNoUpgrade` + only `ExternalSnapshotMetadata` (RHSTOR-8098 avoidance; VGS off, not tested)
  - CBT sidecar `registry.k8s.io/sig-storage/csi-snapshot-metadata:v0.2.0`, `ceph-csi-controller-manager` scaled to 0
- Velero upstream main (293f6f6a6) image: `quay.io/tkaovila/velero:ceph-changeid` (**pushed**, amd64)
- BSL: AWS S3 (creds per oadp-e2e-local-creds skill)
- Driver: `openshift-storage.rbd.csi.ceph.com`; VSC `snapshotHandle` = changeID (generic path, `pkg/exposer/csi_snapshot.go:333`); Ceph handle format `<clusterID>-<pool>-<imageID>-<snapID>` — handle-based delta (`GetChangedBlocksByID`) proven to work even after the VS k8s object is deleted, provided the underlying RBD snap is retained (cephcsi-cbt-e2e velero_compliance tests)

---

# OpenShift + ODF requirements — how to replicate these results

The "Environment (target)" section above is the **pre-campaign plan** and has
drifted (it still names sidecar v0.2.0 and an older velero image). This section
is what was **actually built**, and is the one to copy.

Read the "Scope limitations" subsection in the coverage matrix before citing any
of this as an ODF result — the CBT stack here is hand-assembled from upstream
components, not ODF's shipped ones.

## 1. Cluster

| Item | Value used | Requirement |
|---|---|---|
| OCP | 5.0 (k8s `v1.36.2`) | ODF stream must match the OCP release |
| Platform | AWS | any ODF-supported platform |
| Nodes | 3 control-plane + 3 workers | ≥3 workers, each ≥8 vCPU / 24Gi allocatable (ODF lean profile ≈ 24 vCPU / 72Gi total). On AWS: 3× m5.2xlarge or m6a.2xlarge |
| Backing SC | `gp3-csi` | any dynamic StorageClass for OSD PVCs |

## 2. Feature gate — enable exactly one

```bash
oc patch featuregate cluster --type=merge -p \
  '{"spec":{"customNoUpgrade":{"enabled":["ExternalSnapshotMetadata"]},"featureSet":"CustomNoUpgrade"}}'
```

**Do not** use `TechPreviewNoUpgrade` / `DevPreviewNoUpgrade`. They also enable
VolumeGroupSnapshot, which trips the v1beta1/v1beta2 VGS API mismatch
(RHSTOR-8098). Gate list: <https://github.com/openshift/api/blob/master/features.md>.

Caveats: any custom featureSet makes the cluster **non-upgradeable**, and the
first set triggers a ~30 min rolling node restart.

## 3. ODF

| Item | Value used |
|---|---|
| Operator | `odf-operator.v4.21.10-rhodf`, channel `stable-4.21`, catalog `redhat-operators-v421` |
| Namespace | `openshift-storage` |
| RBD driver | `openshift-storage.rbd.csi.ceph.com` |
| StorageClass | `ocs-storagecluster-ceph-rbd` (`allowVolumeExpansion: true`) |
| Pool | `ocs-storagecluster-cephblockpool` |
| Toolbox | `rook-ceph-tools` (enable it — `rbd` introspection is how snapshot retention is verified) |

**Version caveat that matters more than the ODF version itself:** ODF ≤4.22
ships a `snapshot-metadata` sidecar built from a v1alpha1-generation branch,
which cannot serve a v1beta1 `SnapshotMetadataService` CRD. See
`/create-ocp:odf-prerelease` for how to check what your cluster actually has, and
for installing a stream whose shipped sidecar is v1+.

## 4. CBT stack — both halves were applied by hand here

This is the part that makes the results "upstream components on ODF storage"
rather than "ODF".

| Item | Value used |
|---|---|
| CRD | `snapshotmetadataservices.cbt.storage.k8s.io`, **v1beta1 only**, applied manually |
| Sidecar | `registry.k8s.io/sig-storage/csi-snapshot-metadata:v1.1.0`, injected into the RBD ctrlplugin pod |
| SMS CR | name `openshift-storage.rbd.csi.ceph.com`, `address: csi-snapshot-metadata.openshift-storage.svc:6443`, audience = driver name, `caCert` from the serving cert |
| Service | `csi-snapshot-metadata` :6443 → selector `app: openshift-storage.rbd.csi.ceph.com-ctrlplugin` |

The sidecar needs a projected cert volume on the Driver CR to survive operator
reconciles, otherwise ceph-csi-operator reverts the injection.

## 5. Velero / OADP

| Item | Value used |
|---|---|
| Namespace | `openshift-adp` |
| Image | `quay.io/tkaovila/velero:ceph-changeid-fix10` (upstream main `293f6f6a6` + the fixes in `ceph-cbt-bugs.md`) |
| Server args | `--features=EnableCSI --uploader-type=kopia` |
| node-agent | DaemonSet, privileged (OpenShift needs the SCC — see velero's `customize-installation` docs) |
| BSL | AWS S3, bucket `tkaovila-oadp`, prefix `ceph-changeid`, region `us-east-2` |

**Both the Deployment and the DaemonSet must be rolled when changing the image**
— `pkg/exposer` and `pkg/uploader` run in the node agent.

Stock upstream velero will *not* reproduce these results: BUG-4 and GAP-6 alone
make every incremental silently degrade to a full. Build from the worktree, or
wait for the PRs in `velero-upstreaming-plan.md` to land.

## 6. Snapshot class and volume policy

Ceph is **Case-2 storage** — `rbd snap diff` needs base and target in the same
clone chain, so the base snapshot must be retained. Create a Retain-policy class:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: ocs-storagecluster-rbdplugin-snapclass-retain
driver: openshift-storage.rbd.csi.ceph.com
deletionPolicy: Retain
parameters:
  clusterID: openshift-storage
  csi.storage.k8s.io/snapshotter-secret-name: rook-csi-rbd-provisioner
  csi.storage.k8s.io/snapshotter-secret-namespace: openshift-storage
```

Then a volume policy ConfigMap in the velero namespace selecting the block mover.
**This ConfigMap must be passed on every backup** (`--resource-policies-configmap`);
omitting it silently routes the volume down the filesystem uploader (BUG-12).

```yaml
version: v1
volumePolicies:
- conditions:
    storageClass: [ocs-storagecluster-ceph-rbd]
  action:
    type: snapshot
    parameters:
      dataMover: velero-block
      snapshotClass: ocs-storagecluster-rbdplugin-snapclass-retain
```

Note the policy keys on **storageClass, not volumeMode** — a Filesystem PVC on
the same class also routes to the block uploader.

A second ConfigMap using the stock **Delete**-policy class
(`ocs-storagecluster-rbdplugin-snapclass`) is useful for deliberately reproducing
the stale-changeID path (T22).

## 7. Test workload

One Deployment with two PVCs on `ocs-storagecluster-ceph-rbd`:

- `data-block` — `volumeMode: Block`, 3Gi, exposed at `/dev/xvda`
- `data-fs` — `volumeMode: Filesystem`, 2Gi, mounted at `/data`

Deltas are written with `dd if=/dev/urandom of=/dev/xvda bs=1M seek=<off> count=<n> conv=notrunc; sync`,
and verification is `md5sum` of whole `/dev/xvda` plus per-region `dd | md5sum`.
Whole-device md5 also pins length, so size equality comes free.

## 8. Running a backup

```bash
velero backup create <name> \
  --include-namespaces cbt-test \
  --snapshot-move-data \
  --backup-type Incremental \
  --resource-policies-configmap block-mover-policy \
  -n openshift-adp --wait
```

Measure transfer with **both** of:

```bash
oc get datauploads.velero.io -n openshift-adp \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,TOTAL:.status.progress.totalBytes,INCR:.status.incrementalBytes'

oc logs -n openshift-adp -l name=node-agent --tail=-1 --prefix=false \
  | grep -E '<dataupload>' | grep -E 'CBT source info|Using parent|Block backup finished|degraded to allocated|fallback to whole-device'
```

`TOTAL` is the **device size**, never the moved bytes. `INCR` is the transferred
volume. Do not read `bytesDone` as transfer.

## 9. Known environment gotchas

- **GAP-7**: restored pods carry stale OVN annotations and hang `Pending` in ~8
  of 9 restores. Workaround: `oc delete pod <restored-pod> -n <ns>` and let the
  ReplicaSet recreate it.
- Restored pods keep their **original names**, so source and restore namespaces
  collide by name — always pass `-n <ns>` to `oc exec`.
- `rbd snap ls` hides trash-namespace snapshots; use `--all`. `rbd children` is
  authoritative for clone chains.
- `blockdev` is absent from ubi-minimal; use `/sys/class/block/*/size` or `dd`.

---

## Velero setup

1. `velero install` with `--use-node-agent`, image override `quay.io/tkaovila/velero:ceph-changeid`, AWS plugin, S3 BSL.
2. VolumeSnapshotClass: ODF RBD VSC labeled `velero.io/csi-volumesnapshot-class=true`, `deletionPolicy: Retain`? (start with ODF default; observe)
3. Volume policy ConfigMap to select block mover:
   ```yaml
   version: v1
   volumePolicies:
   - conditions:
       storageClass: [ocs-storagecluster-ceph-rbd]
     action:
       type: snapshot
       parameters:
         dataMover: velero-block
   ```
   (`internal/resourcepolicies`: valid dataMover values `velero`, `velero-fs`, `velero-block`)
4. Backup spec: `snapshotMoveData: true`, `backupType: Full|Incremental` (`pkg/apis/velero/v1/backup_types.go:191`).

## Tests

| # | Case | Steps | Verify |
|---|------|-------|--------|
| T1 | Full backup, FS-mode RBD PVC | app pod writes known data (sha256), backup `backupType: Full` | Backup/DataUpload Completed; kopia snapshot tag `cbt-change-id` == VSC snapshotHandle; restore to new ns, hash matches |
| T2 | Incremental after delta | write delta (~100Mi) to same PVC, backup `backupType: Incremental` | DataUpload uses parent changeID (`--change-id` arg on datamover pod); GetMetadataDelta called (sidecar logs); incremental size ≈ delta not full; restore, hash matches |
| T3 | Block-mode PVC (`volumeMode: Block`) | same full→incremental cycle via dd | same as T1/T2 |
| T4 | Missing parent changeID fallback | delete parent kopia tag scenario or first Incremental with no parent | warn "No ChangeID tag from parent snapshot ... fallback to full" (`pkg/uploader/block/snapshot.go:181`); backup still Completed |
| T5 | **Snapshot retention semantics (KEY RISK — pre-confirmed gap)** | after T1 full backup, toolbox `rbd snap ls` on the image | **Ceph = Case 2 storage**: `rbd snap diff` needs base+target snaps in same clone chain (cephcsi-cbt-e2e/velero-feedback.md). Design's `retainSnapshot` volume-policy param is **NOT implemented in velero main** (grep: only expose-time `RetainVSC` at `pkg/exposer/csi_snapshot.go:187`). Expected today: VS/VSC deleted post-backup → RBD snap gone → every "incremental" silently falls back to full (`SetBitmapOrFull` error → warn + full, `pkg/uploader/block/snapshot.go`). Measure T2 upload size to prove; log as bug/gap for #9714 |
| T6 | Restore of incremental chain | restore from T2 incremental backup into fresh ns | full+delta data intact |
| T7 | Backup deletion | delete full backup while incremental exists | behavior per #9835 (open issue — expect gaps; record, don't file as new if matches known) |

## Debug aids

- `~/git/cephcsi-cbt-e2e/debug-cbt.sh`, `cmd/cbt-check` (GetMetadataAllocated against existing VS)
- rbd introspection via toolbox (`pkg/rbd` patterns from cephcsi-cbt-e2e)
- velero-pod-log-watch skill for live ERROR/FATAL tail
- Known-issue cross-check before filing: open sub-issues #9831, #9833–#9839 (describe, mixed movers, deletion, repo mgmt, CBT detection are known-open — not bugs)

## Bug logging

Every defect → `ceph-cbt-bugs.md` per template. No GitHub issues posted — Tiger reviews and posts.

---

# Validation coverage matrix

Purpose: establish what "ceph-csi Kubernetes CBT is validated against velero's
block data mover" actually requires, by enumerating the **branch points in the
implementation** rather than a list of user scenarios, and recording which are
covered live, which only by unit tests, and which remain open.

Status: `LIVE` (exercised on cluster 260814) · `UNIT` (Go tests only) ·
`OPEN` (neither) · `N/A` (not reachable on Ceph)

Live tests are T-numbers; full detail and measurements are in
`ceph-cbt-bugs.md` → *Validation results*.

## A. Exposer — changeID acquisition (`pkg/exposer/csi_snapshot.go:308-352`)

| ID | Path | Status | Evidence |
|----|------|--------|----------|
| A1 | Generic branch, `vsc.Status.SnapshotHandle` populated | LIVE | Every run from T14 onward |
| A2 | Generic branch, status empty → `Spec.Source.SnapshotHandle` fallback | LIVE | BUG-4 fix; T1-full-v4 onward. This is the race that made every backup a full |
| A3 | vSphere/VKS annotation branch (`:316-326`) | N/A | Structurally unreachable on Ceph — requires `VSphereCNSChangeIDAnno`. Must be validated by someone on vSphere; note it in any "validated" claim rather than implying coverage |
| A4 | `volumeID == ""` → hard error (`:349-351`) | OPEN | Negative path; would need a PV without `spec.csi.volumeHandle` |

## B. Exposer — snapshot retention (`:187`, `:602`)

| ID | Path | Status | Evidence |
|----|------|--------|----------|
| B1 | Retain-policy snapclass → physical RBD snapshot survives cleanup | LIVE | GAP-6 fix; the precondition for every working incremental (T14+) |
| B2 | Delete-policy snapclass → physical snapshot removed at cleanup | LIVE | T22 — confirmed absent from `rbd ls` afterwards |
| B3 | Backup VSC API object deleted, physical snapshot preserved | LIVE | GAP-6 fix; `rbd ls` shows orphans with no k8s owner (→ BUG-11) |

## C. Parent selection (`pkg/uploader/block/snapshot.go:157-210`)

| ID | Path | Status | Evidence |
|----|------|--------|----------|
| C1 | `forceFull` → skip parent lookup entirely | LIVE | T24 (`Forcing full snapshot`) |
| C2 | Explicit `parentSnapshot` supplied by caller | UNIT | velero always uses the discovery branch in practice |
| C3 | Discovery finds a matching snapshot | LIVE | T21, T22, T24 |
| C4 | Discovery finds nothing (first backup for the path) | LIVE | T1, T26-base |
| C5 | Parent has nil tags | UNIT | — |
| C6 | Parent missing `cbt-change-id` tag | UNIT | — |
| C7 | Parent missing `cbt-volume-id` tag | UNIT | — |
| C8 | **Parent `cbt-volume-id` ≠ current volume** | **LIVE** | **T26** — the guard that prevents cross-volume corruption |
| C9 | `loadObjectFromSnapshot` fails | UNIT | Hard to induce without corrupting the repo |

**C8 is the highest-consequence guard in the whole feature** and was unit-only
until T26. Recreating a PVC under the same name gives a new RBD image on an
unchanged repository path (`cbt-vid/data-vid`), so the previous snapshot is found
as a parent while belonging to a different volume. Computing a delta there would
be silent data corruption. Verified by content, not just logs: the region where
the *old* volume held 32 MiB of data reads as zeros in the restore.

## D. CBT bitmap tiers (`pkg/uploader/cbt/set.go`)

| ID | Path | Status | Evidence |
|----|------|--------|----------|
| D1 | `TierChanged` — delta query succeeds | LIVE | T21 (6 MiB), T22-del (4 MiB), T24-incr (5 MiB) — all byte-exact |
| D2 | `TierAllocated` — delta fails, allocated succeeds | LIVE | T22 — 8.5% / 19.7% of device, == an explicit Full (T24) |
| D3 | `TierFull` — both fail | LIVE | T20 — SMS address repointed at an unresolvable name |
| D4 | `ChangeID == ""` → allocated by design (planned full) | LIVE | T24-full, T26-base, T26-mismatch |
| D5 | `service == nil` | UNIT | — |
| D6 | `bitmap.Snapshot() == ""` | UNIT | — |

## E. Uploader write modes (`pkg/uploader/block/uploader.go`, `provider/block.go`)

| ID | Path | Status | Evidence |
|----|------|--------|----------|
| E1 | Incremental mode with parent object | LIVE | `in block mode with parent IIx…` |
| E2 | Full mode, no parent | LIVE | T24-full, T22-incr, T26-mismatch (`without parent`) |
| E3 | Backup cancelled mid-flight (`block.go:137-139`, `ErrCanceled`) | **LIVE** | **T27 — found BUG-14.** The sentinel is compared with `==` against an error wrapped twice, so the cancel branch is dead code and a cancellation reports as `Failed`/`PartiallyFailed`. Fixed with `errors.Is` |
| E4 | Restore cancelled mid-flight (`block.go:183-185`) | UNIT | Same defect, same fix; covered by `TestBlockProviderCancelThroughWrappedError/restore`. Not yet exercised on cluster |

## F. Restore

| ID | Path | Status | Evidence |
|----|------|--------|----------|
| F1 | Block restore into a new namespace | LIVE | T12, T14r, T16r, T17r, T18, T20r, T21r, T22r, T23, T26r |
| F2 | Restore in place (same namespace) | OPEN | — |
| F3 | Restore into a larger PVC | OPEN | — |
| F4 | Restore an old backup after many newer ones + maintenance | LIVE | T23 — two same-backup pairs, byte-identical |

## G. Lifecycle and operations

| ID | Path | Status | Evidence |
|----|------|--------|----------|
| G1 | Delete a backup mid-chain, restore from a later one | LIVE | T16 |
| G2 | Backup expiration (TTL) removing a CBT parent | OPEN | All current backups expire 2026-09-14+ |
| G3 | Kopia maintenance / blob GC | PARTIAL | 3 maintenance jobs ran; blob GC unconfirmed (→ GAP-8) |
| G4 | node-agent restart mid-backup (resume) | **LIVE** | **T28 — survivable.** Force-deleted the node-agent mid-transfer; the backup still completed and moved exactly 943,718,400 B. The transfer runs in the data mover pod, and the restarting agent's `AttemptDataUploadResume` re-attached |
| G4b | **Data mover pod lost mid-transfer** | **LIVE** | **T30 — found GAP-15.** Wedges `InProgress` indefinitely (11+ min observed); no liveness check in the `InProgress` branch and `preparingTimeout` does not apply. Only a node-agent restart clears it, via `AttemptDataUploadResume` → cancel |
| G5 | Concurrent backups of the same namespace | **LIVE** | **T31 — no race; velero queues them.** Two `backup create` calls submitted together ran as `InProgress` + `Queued`, not in parallel. The second then chained correctly onto the first (parent start time == the first's snapshot) and moved 0, while the first moved exactly the 7 MiB written |
| G6 | Volume expansion between backups | LIVE (incidental) | `data-block` was expanded 2Gi→3Gi mid-campaign; subsequent incrementals (t17-i2 onward) tracked the new size correctly |

## H. Volume shapes

| ID | Path | Status | Evidence |
|----|------|--------|----------|
| H1 | `volumeMode: Block` | LIVE | `data-block`, `data-vid` |
| H2 | `volumeMode: Filesystem` routed through the block mover | LIVE | `data-fs` — policy keys on storageClass, not volumeMode |
| H3 | Multiple volumes in one backup | LIVE | Every `cbt-test` backup covers 2 PVCs |
| H4 | Empty / never-written volume | **LIVE** | **T32 — 0 bytes moved, restore still correct.** A Full backup of an untouched 1 GiB Block PVC transferred **0 B** (empty allocated bitmap) and restored to a full-size device whose md5 matches 1 GiB of zeros exactly |
| H5 | Filesystem PVC via the *fs* uploader (no volume policy) | LIVE | T18 — BUG-12 mis-route, restored byte-identical |

---

## What is still required before claiming "validated"

### Scope limitations that must be stated in any validation claim

1. **The CBT stack under test is hand-assembled upstream, not what ODF ships.**
   Both halves are ours:
   - CRD `snapshotmetadataservices.cbt.storage.k8s.io` —
     `creationTimestamp: 2026-08-15`, single version `v1beta1`, carries
     `kubectl.kubernetes.io/last-applied-configuration`. We applied it.
   - Sidecar `registry.k8s.io/sig-storage/csi-snapshot-metadata:v1.1.0` —
     upstream image, injected by hand.

   ODF 5.0 pins its own build by digest in `csi-images-v5.0`
   (`ose-csi-external-snapshot-metadata-rhel9@sha256:02410491…`), whose labels
   give `SOURCE_GIT_COMMIT=693a826`, `version=v4.20.0` — a **June-2025,
   pre-v0.2.0, v1alpha1-generation** source commit. In
   `openshift/csi-external-snapshot-metadata`, the first **v1beta1-capable**
   branch is `release-4.23`/`5.0` (`239703c`, rebase to upstream v1.0.0,
   2026-06-25); everything at or below `release-4.22` is v1alpha1, and upstream
   v1.0.0 removed that API.

   Consequence: **CBT worked here because our two hand-picked components are
   internally consistent. ODF's own shipped combination has never been
   exercised**, and on ≤4.22 it could not have worked against a v1beta1 CRD at
   all. Any claim must say "validated against upstream ceph-csi CBT components on
   ODF-provided Ceph storage", not "validated on ODF". Running one full CBT cycle
   against ODF's sidecar would close this.
2. **The vSphere changeID branch is unreachable here** (see A3). Scope the claim
   to non-vSphere CSI drivers, or pair it with a vSphere run.
3. **BUG-5 is withdrawn, not pending.** It documented our own transient
   v0.2.0-vs-v1beta1 mispairing, which we resolved by moving to v1.1.0. See the
   BUG-5 entry in `ceph-cbt-bugs.md` for what survives as a forward-looking
   ODF/ceph packaging item (their pinned sidecar is a v1alpha1-generation build
   while their operator default already says v1.0.0).

### Remaining untested paths

Ordered by how much a reviewer would care:

1. **G2 — expiration removing a CBT parent.** BUG-11 says retained snapshots have
   no lifecycle owner; expiry is where that bites. Marginal over T16 (mid-chain
   *deletion*, already PASS) and slow to exercise — velero's GC controller
   defaults to hourly — so it needs either a patched frequency or patience.
3. **F2 / F3 — restore in place, and into a larger PVC.** F3 needs a resource
   modifier ConfigMap to rewrite `spec.resources.requests.storage`, so it is a
   setup step rather than a one-liner.
4. **E4 — restore cancellation live.** Fixed and unit-covered by
   `TestBlockProviderCancelThroughWrappedError/restore`; the backup half was
   confirmed live in T29. Lower value now that the shared root cause is proven.
5. **G3 — kopia blob GC** (GAP-8), needs the next full-maintenance cycle.
6. **A3 — the vSphere changeID branch** cannot be covered here at all. Scope the
   claim to non-vSphere CSI drivers, or pair it with a vSphere run.
7. **ODF's own sidecar** — blocked: the pre-release catalog resolves but its
   component images are not pullable (`manifest unknown`). Needs the correct
   mirror mapping and credentials. See `/create-ocp:odf-prerelease`.

Unit-only and hard to induce live, listed for completeness rather than as gaps
worth chasing: C2, C5–C7, C9 (parent-guard variants — C8, the one that matters,
is covered live by T26), D5/D6, A4.

Everything in A1/A2, B, C (except C2/C5/C6/C7/C9), D1–D4, E1/E2/E3, F1/F4,
G1/G4/G4b/G5/G6 and H1–H5 is covered live with byte-level verification.

