# RHSTOR-6440 / ODF CBT — Research Summary (workflow, 2026-08-15)

Verdict: **stay on ODF 4.21.10 with our workarounds** — dev/source images add no
CBT data-path fixes relevant to velero changeID validation today.

## Why

- The changeID data path (GetMetadataAllocated/GetMetadataDelta in ceph-csi RBD)
  merged as ceph/ceph-csi #5347 on 2025-07-08 — already in the ODF 4.21.10 driver.
  Later driver changes are hygiene only (#6203 debug logging, #6401 read-only image
  open); the one real data-path candidate (#6202 getRBDSnapID clone-image) was
  closed UNMERGED — even tip-of-main lacks it.
- Our three deployment gaps are all fixed in later ODF lines, not 4.21, and each
  fix ≈ our workaround done in code:
  - imageset cross-major fallback: ocs-client-operator #613 (df070f7e, 2026-06-19;
    4.22 backport #682) — ≈ our cloned `csi-images-v5.0` CM.
  - sidecar tls-key-volume trigger: ceph-csi-operator #513 (72b746c2, 2026-06-23;
    main/4.23) — 4.21 path: SMS CR named exactly the Driver CR name +
    `tls-key` volume (mountPath `/tmp/certificates`) in Driver CR
    `spec.controllerPlugin.volumes` + operator restart (4.21 has no SMS watch).
    See ceph-csi-operator docs/features/rbd-snapshot-metadata.md.
  - SCC secret-volume allowance: ocs-client-operator #540 (9b4a6df, 2026-03-30;
    4.22+) — ≈ our projected-volume workaround (projected allowed on 4.21 SCC).
- Dev-image route degraded: quay.io/ocs-dev catalogs stale (2025-07/2024-11);
  konflux 4.21/4.22 FBC fragments (2026-08-11) reference PRIVATE rhceph-dev
  component images (auth required) → ImagePullBackOff; freshest ocs-dev
  ocs-client-operator (main-6f2a746, 2026-06-29) predates the #635 automation
  anyway.

## RHSTOR-6440 state (reconstructed from GitHub; Jira not anonymously readable)

Epic = ODF CBT (KEP-3314) for RBD. Implemented/merged: ceph-csi #5347 #5411
#6203 #6401; ceph-csi-operator #274 (sidecar deploy, alpha) #294 #302 (CRD check;
downstream 4.20 backport red-hat-storage/ceph-csi-operator #141) #519 (--audience)
#546 (extra args); ocs-client-operator #635 (full wiring automation: Service
`openshift-storage-rbd-snapshot-metadata` + vendor ConfigMap, 2026-07-01, 4.23)
#671 (TLSProfile); downstream external-snapshot-metadata #2 (CLI cert flags);
velero #9528 design.
Open: ocs-ci #15906 (QE automation, targets ODF 4.23/OCP 4.20+), ceph-csi #6459
(upstream e2e), k8s external test enablement (blocked on kubernetes #130918).
Note: ODF 4.23 deliberately does NOT create the SnapshotMetadataService CR
(v1beta1 CRD not default-installed; vendor ConfigMap is the interface) — DFBUGS-9181.

## Cautions for our validation

- Do NOT flatten RBD images during delta testing — flattening destroys delta
  metadata for flattened snapshots; corrupts GetMetadataDelta results.
- 4.21 sidecar cert-mount is added only if the tls-key volume exists — if the
  volume disappears the sidecar crash-loops (args always pass --tls-cert/key).
- Contingency if operator fights injection: swap only cephcsi-operator image to
  quay.io/cephcsi/ceph-csi-operator:latest (has #513); do NOT touch
  ocs-client-operator; do NOT use konflux FBC fragments (private images).
- Clean re-run target later: ODF 4.23 (all three gaps fixed; matches ocs-ci #15906).
