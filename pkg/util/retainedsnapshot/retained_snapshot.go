/*
Copyright the Velero contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package retainedsnapshot holds the working record of a backup's retained
// snapshots during backup deletion.
//
// The DataUploads are deleted in an earlier step of the deletion, so the
// working record is a sharded ConfigMap family, retained-snapshots-<backup-UID>-<n>,
// in the Velero namespace, keyed by the DataUpload name. The family is written
// create-or-update, so a retry of `velero backup delete` merges into the map
// an earlier, failed attempt left behind instead of failing on AlreadyExists.
// ConfigMap data is limited to 1 MiB and a record is a few hundred bytes, so
// the records are sharded into entry shards -1 through -n, split before the
// limit and labeled by backup for discovery. Shard -0 is the manifest: written
// last, with shardCount n in its data and the family label, and that single
// write activates the family. Readers load shard 0, then exactly shards 1
// through n by name; a family without a shard 0 is inert -- an entry parked by
// the DataUpload finalizer hold, or a crash leftover -- until the next deletion
// merges into it and writes shard 0. A surviving record means the snapshot was
// not released; even a stale record is harmless, because a deletion that finds
// the snapshot already gone is a success.
package retainedsnapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/label"
	repotypes "github.com/velero-io/velero/pkg/repository/types"
)

const (
	// FamilyLabel marks shard 0, the manifest, of a backup's retained snapshot
	// record family. The retained-snapshot controller is keyed by it: it
	// reads shard 0, then exactly the entry shards the manifest counts, and
	// never lists entry shards on its own. The entry shards carry the backup
	// labels but not this one, so a family without a shard 0 is inert.
	FamilyLabel = "velero.io/retained-snapshots"

	// OrphanLabel marks the ConfigMaps that hold snapshots the deletion
	// escalated or skipped. They are not labeled by backup, so they are neither
	// deleted by the family's last-entry rule nor garbage-collected when the
	// Backup is gone; the orphan record is the durable home for manual cleanup
	// after the Backup object is gone.
	OrphanLabel = "velero.io/orphan-snapshots"

	// ShardCountKey is the data key of shard 0, the manifest, carrying the
	// number n of entry shards -1 through -n the family has. Shard 0 is the
	// persisted activation: the gather writes the entry shards without it,
	// and the DeleteBackupRequest writes it only after every prerequisite
	// step -- repository data and DataUploads -- has succeeded. The
	// retained-snapshot controller ignores a family without shard 0, so a
	// deletion whose repository step failed gathers records but never creates
	// a temporary content.
	ShardCountKey = "shardCount"

	// CompletedKey holds the JSON array of DataUpload names whose snapshots
	// were confirmed deleted. An entry shard with no pending entries keeps its
	// completed set -- the evidence that suppresses replay -- until the
	// deletion finishes, so a later regather from the unchanged tarball does
	// not recreate an entry.
	CompletedKey = "completed"

	// manifestIndex is the index of the manifest shard; entry shards start at
	// firstEntryShardIndex.
	manifestIndex        = 0
	firstEntryShardIndex = 1

	// dataBudget is the ConfigMap data size limit; the records are split into
	// shards before the limit is reached.
	dataBudget = 1 << 20

	// MaxAttempts is the bounded number of persisted DeleteSnapshot failures,
	// for one record, that come from the DeleteSnapshot call itself, after
	// which the record is escalated to the orphan ConfigMap and the backup
	// deletion proceeds. A driver that fails slowly still reaches the bound
	// across retries of `velero backup delete`, because the counter is
	// persisted in the entry.
	MaxAttempts = 5
)

// FamilyNamePrefix returns the prefix of the shard names of a backup's record
// family. The backup UID, not the name, keys the family: names can repeat.
func FamilyNamePrefix(backupUID string) string {
	return "retained-snapshots-" + backupUID + "-"
}

// OrphanNamePrefix returns the prefix of the orphan-snapshot ConfigMap names.
func OrphanNamePrefix() string {
	return "orphan-snapshots-"
}

// ManifestName returns the name of shard 0, the manifest, of a backup's
// record family.
func ManifestName(backupUID string) string {
	return fmt.Sprintf("%s%d", FamilyNamePrefix(backupUID), manifestIndex)
}

// IsManifest reports whether the ConfigMap is a family's shard 0: a family
// member -- prefixed and labeled with its backup UID -- that carries the family
// label. Entry shards carry the backup labels only.
func IsManifest(cm *corev1api.ConfigMap) bool {
	uid := cm.Labels[velerov1api.BackupUIDLabel]
	return uid != "" && cm.Labels[FamilyLabel] == "true" && cm.Name == ManifestName(uid)
}

// IsEntryShard reports whether the ConfigMap is an entry shard of a record
// family: named retained-snapshots-<backup-UID>-<n> with n >= 1 for its own
// backup UID label, and without the family label.
func IsEntryShard(cm *corev1api.ConfigMap) bool {
	uid := cm.Labels[velerov1api.BackupUIDLabel]
	if uid == "" || cm.Labels[FamilyLabel] == "true" {
		return false
	}
	_, ok := entryShardIndex(cm.Name, FamilyNamePrefix(uid))
	return ok
}

// entryShardIndex parses the index of an entry shard name under the prefix.
func entryShardIndex(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	var index int
	if _, err := fmt.Sscanf(name[len(prefix):], "%d", &index); err != nil || fmt.Sprintf("%s%d", prefix, index) != name {
		return 0, false
	}
	if index < firstEntryShardIndex {
		return 0, false
	}
	return index, true
}

// FamilyLabels returns the labels of shard 0, the manifest, of a backup's
// record family: the backup labels and the family label the retained-snapshot
// controller is keyed by.
func FamilyLabels(backup *velerov1api.Backup) map[string]string {
	labels := EntryShardLabels(backup)
	labels[FamilyLabel] = "true"
	return labels
}

// EntryShardLabels returns the labels of the entry shards of a backup's record
// family: the backup labels only, so that an entry shard without a shard 0 is
// inert for the retained-snapshot controller and still discoverable, by the
// backup UID, for the gather that merges into it and for the sweep of a family
// whose Backup is gone.
func EntryShardLabels(backup *velerov1api.Backup) map[string]string {
	return map[string]string{
		velerov1api.BackupNameLabel: label.GetValidName(backup.Name),
		velerov1api.BackupUIDLabel:  string(backup.UID),
	}
}

// Entry is one retained snapshot record in a shard, keyed by the DataUpload
// name. The snapshot fields carry the record gathered from the DataUpload
// status, the backup tarball or the repository tag; a record whose
// CSISnapshotHandle is empty is a pre-rollback backup whose handle still has
// to be resolved from the repository snapshot's cbt-change-id tag before the
// repository data is deleted.
type Entry struct {
	// CSISnapshotHandle is the CSI snapshot handle of the retained snapshot.
	CSISnapshotHandle string `json:"csiSnapshotHandle,omitempty"`
	// Driver is the name of the CSI driver that created the snapshot.
	Driver string `json:"driver,omitempty"`
	// DeletionSecretRef names the secret the driver requires to delete the snapshot.
	DeletionSecretRef *corev1api.SecretReference `json:"deletionSecretRef,omitempty"`
	// ManagedByLabelValue is the managed-by label value of the backup content.
	ManagedByLabelValue string `json:"managedByLabelValue,omitempty"`
	// Snapshot identifies the repository snapshot that holds the handle as its
	// cbt-change-id tag, for a backup taken before Velero recorded retained
	// snapshots. The handle must be resolved from it before the repository
	// data is deleted, because deleting that data destroys the tag.
	Snapshot *repotypes.SnapshotIdentifier `json:"snapshot,omitempty"`
	// Attempts is the persisted number of DeleteSnapshot failures that came
	// from the call itself. A retry of `velero backup delete` is a new
	// DeleteBackupRequest and any in-memory count would reset on it, so the
	// counter lives here.
	Attempts int `json:"attempts,omitempty"`
	// Trusted is true when the record was sourced from DataUpload.status, which
	// entered through the API; tarball- and tag-sourced values are written by
	// anyone who can write the backup storage, so they are preferred only
	// where no trusted value exists.
	Trusted bool `json:"trusted,omitempty"`
}

// IsRetentionEvaluated reports whether the DataUpload carries the
// retention-evaluated marker: the retainSnapshot key of spec.dataMoverConfig,
// written with its explicit value for every velero-block DataUpload created
// by a build with this design, whether or not a policy matched. A DataUpload
// without the marker is from a pre-rollback build and is the only kind the
// legacy tag recovery of design section 7 applies to.
func IsRetentionEvaluated(du *velerov2alpha1api.DataUpload) bool {
	_, marked := du.Spec.DataMoverConfig[velerov2alpha1api.RetainSnapshotDataMoverConfigKey]
	return marked
}

// FromDataUploadStatus builds the entry of a DataUpload's persisted record.
func FromDataUploadStatus(du *velerov2alpha1api.DataUpload) *Entry {
	record := du.Status.RetainedCSIStorageSnapshot
	if record == nil {
		return nil
	}
	return &Entry{
		CSISnapshotHandle:   record.CSISnapshotHandle,
		Driver:              record.Driver,
		DeletionSecretRef:   record.DeletionSecretRef,
		ManagedByLabelValue: record.ManagedByLabelValue,
		Trusted:             true,
	}
}

// Record returns the retained snapshot record of the entry.
func (e *Entry) Record() *velerov2alpha1api.RetainedCSIStorageSnapshot {
	return &velerov2alpha1api.RetainedCSIStorageSnapshot{
		CSISnapshotHandle:   e.CSISnapshotHandle,
		Driver:              e.Driver,
		DeletionSecretRef:   e.DeletionSecretRef,
		ManagedByLabelValue: e.ManagedByLabelValue,
	}
}

func marshalEntry(entry *Entry) (string, error) {
	b, err := json.Marshal(entry)
	if err != nil {
		return "", errors.Wrap(err, "error marshaling the retained snapshot entry")
	}
	return string(b), nil
}

// UnmarshalEntry decodes one entry value.
func UnmarshalEntry(value string) (*Entry, error) {
	entry := &Entry{}
	if err := json.Unmarshal([]byte(value), entry); err != nil {
		return nil, errors.Wrap(err, "error unmarshaling the retained snapshot entry")
	}
	return entry, nil
}

// Family is the shard family of one backup's records: the manifest, shard 0,
// when the family is activated, and the entry shards -1 through -n. "The
// family empties" means every entry shard is empty of pending entries.
type Family struct {
	// Manifest is shard 0, nil while the family is inert.
	Manifest *corev1api.ConfigMap
	// Shards are the entry shards, in name order.
	Shards []*corev1api.ConfigMap
}

// LoadFamily loads a backup's shard family. With a shard 0 it reads the
// manifest, then exactly entry shards 1 through shardCount by name; a shard
// the manifest counts but that is gone is skipped. Without a shard 0 the
// family is inert and the entry shards are discovered by the backup labels
// and the name prefix, which is the view the gather merges into and the
// manifest write counts. A backup whose records were never gathered, or whose
// family a previous attempt already emptied and deleted, returns an empty
// family. The reader may be an uncached API reader: the backup-deletion
// controller's safety decisions must not be made on an informer-lagged read
// that misses a shard a gather just created.
func LoadFamily(ctx context.Context, c client.Reader, backup *velerov1api.Backup) (*Family, error) {
	family := &Family{}
	manifest := &corev1api.ConfigMap{}
	err := c.Get(ctx, client.ObjectKey{Namespace: backup.Namespace, Name: ManifestName(string(backup.UID))}, manifest)
	switch {
	case err == nil:
		family.Manifest = manifest
		count, err := shardCountOf(manifest)
		if err != nil {
			return nil, err
		}
		prefix := FamilyNamePrefix(string(backup.UID))
		for index := firstEntryShardIndex; index <= count; index++ {
			shard := &corev1api.ConfigMap{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: backup.Namespace, Name: fmt.Sprintf("%s%d", prefix, index)}, shard); err != nil {
				if isNotFound(err) {
					continue
				}
				return nil, errors.Wrapf(err, "error getting the retained snapshot record shard %s%d", prefix, index)
			}
			family.Shards = append(family.Shards, shard)
		}
		return family, nil
	case isNotFound(err):
		shards, err := listEntryShards(ctx, c, backup)
		if err != nil {
			return nil, err
		}
		family.Shards = shards
		return family, nil
	default:
		return nil, errors.Wrap(err, "error getting the retained snapshot record manifest")
	}
}

// ListInertShards discovers the entry shards of a backup by its labels and the
// family name prefix, whether or not the family has a shard 0: the view the
// gather merges into, the manifest write counts, and the sweep of a family
// whose Backup is gone removes. A reader working a live deletion uses
// LoadFamily instead, which reads exactly the shards the manifest counts.
func ListInertShards(ctx context.Context, c client.Reader, backup *velerov1api.Backup) ([]*corev1api.ConfigMap, error) {
	return listEntryShards(ctx, c, backup)
}

// listEntryShards discovers the entry shards of a backup by its labels and the
// family name prefix, the inert view a manifest write counts.
func listEntryShards(ctx context.Context, c client.Reader, backup *velerov1api.Backup) ([]*corev1api.ConfigMap, error) {
	list := &corev1api.ConfigMapList{}
	if err := c.List(ctx, list, &client.ListOptions{
		Namespace:     backup.Namespace,
		LabelSelector: labels.SelectorFromSet(EntryShardLabels(backup)),
	}); err != nil {
		return nil, errors.Wrap(err, "error listing the retained snapshot record shards")
	}
	prefix := FamilyNamePrefix(string(backup.UID))
	shards := []*corev1api.ConfigMap{}
	for i := range list.Items {
		if _, ok := entryShardIndex(list.Items[i].Name, prefix); ok && list.Items[i].Labels[FamilyLabel] != "true" {
			shards = append(shards, &list.Items[i])
		}
	}
	sort.Slice(shards, func(i, j int) bool {
		return shards[i].Name < shards[j].Name
	})
	return shards, nil
}

func shardCountOf(manifest *corev1api.ConfigMap) (int, error) {
	var count int
	if _, err := fmt.Sscanf(manifest.Data[ShardCountKey], "%d", &count); err != nil || count < 0 {
		return 0, errors.Errorf("the retained snapshot record manifest %s carries no valid %s: %q", manifest.Name, ShardCountKey, manifest.Data[ShardCountKey])
	}
	return count, nil
}

// IsEmpty reports whether no entry shard of the family carries a pending
// entry. A shard with no pending entries still counts its completed set, and
// an inert family is never treated as complete or empty for the activation
// decision; see HasPendingWork.
func (f *Family) IsEmpty() bool {
	return f.PendingEntries() == 0
}

// PendingEntries returns the number of pending entries across the family.
func (f *Family) PendingEntries() int {
	pending := 0
	for _, shard := range f.Shards {
		pending += len(pendingKeys(shard))
	}
	return pending
}

// HasPendingWork reports whether the family has work for the retained-snapshot
// controller: a shard 0 and a pending entry. An inert family has no work but
// also does not count as finished for a request that has not activated it
// yet; the request decides through IsActivated.
func (f *Family) HasPendingWork() bool {
	return f.IsActivated() && f.PendingEntries() > 0
}

// IsActivated reports whether the family has its shard 0, the manifest.
func (f *Family) IsActivated() bool {
	return f.Manifest != nil
}

// Entry returns the pending entry of the named DataUpload.
func (f *Family) Entry(duName string) (*Entry, bool) {
	for _, shard := range f.Shards {
		if value, ok := shard.Data[duName]; ok {
			entry, err := UnmarshalEntry(value)
			if err != nil {
				return nil, true
			}
			return entry, true
		}
	}
	return nil, false
}

// CompletedSet returns the names whose snapshots were confirmed deleted.
func (f *Family) CompletedSet() map[string]struct{} {
	completed := map[string]struct{}{}
	for _, shard := range f.Shards {
		var names []string
		if value, ok := shard.Data[CompletedKey]; ok && value != "" {
			if err := json.Unmarshal([]byte(value), &names); err != nil {
				continue
			}
		}
		for _, name := range names {
			completed[name] = struct{}{}
		}
	}
	return completed
}

func pendingKeys(shard *corev1api.ConfigMap) []string {
	var keys []string
	for key := range shard.Data {
		if key == ShardCountKey || key == CompletedKey {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func shardSize(shard *corev1api.ConfigMap) int {
	size := 0
	for key, value := range shard.Data {
		size += len(key) + len(value)
	}
	return size
}

// WriteEntries merges entries into the backup's entry shards -1 through -n,
// create-or-update, keyed by the DataUpload name. The entry shards are written
// inert, with the backup labels but not the family label; the manifest, shard
// 0, is WriteManifest's. An existing shard from an earlier, failed attempt of
// the same deletion is merged into, never rejected. An entry whose DataUpload
// name is in a shard's completed set is not resurrected; where both the family
// and the given entries carry a record for the same DataUpload, the given
// entry wins, except that its persisted attempt count merges with, never
// resets, the count already in the family. No shard is written for zero
// entries.
func WriteEntries(ctx context.Context, c client.Client, backup *velerov1api.Backup, entries map[string]*Entry) error {
	return writeShardedEntries(ctx, c, backup, EntryShardLabels(backup), FamilyNamePrefix(string(backup.UID)), firstEntryShardIndex, entries)
}

// WriteOrphanEntry merges one escalated or skipped entry into the
// orphan-snapshot ConfigMaps, keyed by <backup-UID>.<DataUpload-name>, which
// satisfies the ConfigMap data key charset and cannot collide across backups
// once the backup label is absent. The orphan ConfigMaps are not labeled by
// backup and outlive the Backup; removing an entry after its snapshot is
// cleaned up by hand is the operator's action.
func WriteOrphanEntry(ctx context.Context, c client.Client, backup *velerov1api.Backup, duName string, entry *Entry) error {
	entries := map[string]*Entry{string(backup.UID) + "." + duName: entry}
	return writeShardedEntries(ctx, c, backup, map[string]string{OrphanLabel: "true"}, OrphanNamePrefix(), 0, entries)
}

// OrphanEntry is one entry of the orphan-snapshot ConfigMaps, with the
// backup UID and DataUpload name its key encodes and the shard that holds it.
type OrphanEntry struct {
	Shard      string
	Key        string
	BackupUID  string
	DataUpload string
	Entry      *Entry
}

// SplitOrphanKey splits an orphan entry key into its backup UID and DataUpload
// name. A UID carries no dot, so the first dot is the separator; the shard's
// bookkeeping keys (completed, shardCount) carry none and are not entries.
func SplitOrphanKey(key string) (uid, duName string, ok bool) {
	i := strings.IndexByte(key, '.')
	if i <= 0 || i == len(key)-1 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}

// LoadOrphanEntries lists every entry of the orphan-snapshot ConfigMaps of the
// namespace, in shard then key order. An undecodable entry is returned with an
// empty Entry so a caller can still name it.
func LoadOrphanEntries(ctx context.Context, c client.Reader, namespace string) ([]OrphanEntry, error) {
	list := &corev1api.ConfigMapList{}
	if err := c.List(ctx, list, &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: labels.SelectorFromSet(map[string]string{OrphanLabel: "true"}),
	}); err != nil {
		return nil, errors.Wrap(err, "error listing the orphan-snapshot ConfigMaps")
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	out := []OrphanEntry{}
	for i := range list.Items {
		shard := &list.Items[i]
		keys := make([]string, 0, len(shard.Data))
		for key := range shard.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			uid, duName, ok := SplitOrphanKey(key)
			if !ok {
				continue
			}
			entry, err := UnmarshalEntry(shard.Data[key])
			if err != nil {
				entry = &Entry{}
			}
			out = append(out, OrphanEntry{Shard: shard.Name, Key: key, BackupUID: uid, DataUpload: duName, Entry: entry})
		}
	}
	return out, nil
}

// RemoveOrphanEntriesForSnapshot removes the orphan entries that name the
// given driver and snapshot handle, and returns them. An orphan entry goes
// stale when its Backup object was gone but the tarball still existed: a BSL
// resync recreates the Backup under a new UID, and a later backup deletion
// removes the snapshot through the tarball record. The entry's UID is then not
// the backup's, so the match is by the snapshot itself (design section 4).
func RemoveOrphanEntriesForSnapshot(ctx context.Context, c client.Client, namespace, driver, handle string) ([]OrphanEntry, error) {
	if handle == "" {
		return nil, nil
	}
	entries, err := LoadOrphanEntries(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	removed := []OrphanEntry{}
	byShard := map[string][]OrphanEntry{}
	for _, o := range entries {
		if o.Entry.CSISnapshotHandle == handle && (driver == "" || o.Entry.Driver == "" || o.Entry.Driver == driver) {
			byShard[o.Shard] = append(byShard[o.Shard], o)
		}
	}
	for shardName, matches := range byShard {
		shard := &corev1api.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: shardName}, shard); err != nil {
			if isNotFound(err) {
				continue
			}
			return removed, errors.Wrapf(err, "error getting the orphan-snapshot ConfigMap %s", shardName)
		}
		for _, o := range matches {
			delete(shard.Data, o.Key)
		}
		if err := c.Update(ctx, shard); err != nil {
			return removed, errors.Wrapf(err, "error updating the orphan-snapshot ConfigMap %s", shardName)
		}
		removed = append(removed, matches...)
	}
	sort.Slice(removed, func(i, j int) bool {
		if removed[i].Shard != removed[j].Shard {
			return removed[i].Shard < removed[j].Shard
		}
		return removed[i].Key < removed[j].Key
	})
	return removed, nil
}

// writeShardedEntries merges entries into the shards carrying shardLabels under
// namePrefix, numbering new shards from firstIndex upward. The family's entry
// shards start at 1, shard 0 being the manifest; the orphan shards start at 0.
func writeShardedEntries(ctx context.Context, c client.Client, backup *velerov1api.Backup, shardLabels map[string]string, namePrefix string, firstIndex int, entries map[string]*Entry) error {
	if len(entries) == 0 {
		return nil
	}

	// Discover the shards carrying the same labels and prefix: an orphan write
	// and a family write must not see each other's shards, so both the labels
	// and the name prefix narrow the list. The family's shard 0 carries the
	// family label on top of these labels and is listed too; it holds no
	// entries and is never written here.
	list := &corev1api.ConfigMapList{}
	if err := c.List(ctx, list, &client.ListOptions{
		Namespace:     backup.Namespace,
		LabelSelector: labels.SelectorFromSet(shardLabels),
	}); err != nil {
		return errors.Wrap(err, "error listing the retained snapshot shards")
	}
	shards := map[string]*corev1api.ConfigMap{}
	nextIndex := firstIndex
	for i := range list.Items {
		cm := &list.Items[i]
		if !strings.HasPrefix(cm.Name, namePrefix) || cm.Labels[FamilyLabel] == "true" {
			continue
		}
		var index int
		if _, err := fmt.Sscanf(cm.Name[len(namePrefix):], "%d", &index); err != nil || fmt.Sprintf("%s%d", namePrefix, index) != cm.Name || index < firstIndex {
			continue
		}
		shards[cm.Name] = cm
		if index >= nextIndex {
			nextIndex = index + 1
		}
	}

	completed := map[string]struct{}{}
	for _, shard := range shards {
		for _, name := range decodeCompleted(shard) {
			completed[name] = struct{}{}
		}
	}

	// Order the names so the shard assignment is deterministic across retries.
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	// Only the shards an entry was written into are persisted: the delete
	// action gathers once per DataUpload in the tarball, and rewriting every
	// shard on each of those calls would make the API traffic quadratic in
	// the number of records and starve the single-worker controllers behind
	// it.
	dirty := map[string]struct{}{}
	for _, name := range names {
		if _, done := completed[name]; done {
			// The snapshot was already confirmed deleted by an earlier
			// attempt; a regather from the unchanged tarball must not
			// recreate the entry.
			continue
		}
		entry := entries[name]
		// Merge the persisted state instead of resetting it: the attempt
		// count, and a handle a previous attempt resolved from the repository
		// snapshot's tag. The regather's entry still carries an empty handle
		// -- the tarball never changes -- while the resolved handle is the
		// only copy left once the repository data is deleted; losing it turns
		// a resolvable record into a dropped one and leaks the snapshot.
		for _, shard := range shards {
			if value, ok := shard.Data[name]; ok {
				if existing, err := UnmarshalEntry(value); err == nil {
					entry = mergeEntry(existing, entry)
				}
			}
		}

		value, err := marshalEntry(entry)
		if err != nil {
			return err
		}
		shard := findShardWithRoom(shards, name, value)
		if shard == nil {
			shard = &corev1api.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: backup.Namespace,
					Name:      fmt.Sprintf("%s%d", namePrefix, nextIndex),
					Labels:    shardLabels,
					// The full backup name lives in an annotation because the
					// backup-name label cannot carry it: label values are
					// capped at 63 characters and the sanitized label hashes
					// longer names, which cannot be turned back into the
					// Backup's name.
					Annotations: map[string]string{velerov1api.RetainedSnapshotsBackupNameAnnotation: backup.Name},
				},
				Data: map[string]string{},
			}
			nextIndex++
		}
		if shard.Data == nil {
			shard.Data = map[string]string{}
		}
		if shard.Annotations == nil {
			shard.Annotations = map[string]string{}
		}
		shard.Annotations[velerov1api.RetainedSnapshotsBackupNameAnnotation] = backup.Name
		shard.Data[name] = value
		shards[shard.Name] = shard
		dirty[shard.Name] = struct{}{}
	}

	for name, shard := range shards {
		if _, changed := dirty[name]; !changed && shard.ResourceVersion != "" {
			continue
		}
		if shard.ResourceVersion == "" {
			if err := c.Create(ctx, shard); err != nil {
				return errors.Wrapf(err, "error creating the retained snapshot shard %s", shard.Name)
			}
			continue
		}
		if err := c.Update(ctx, shard); err != nil {
			return errors.Wrapf(err, "error updating the retained snapshot shard %s", shard.Name)
		}
	}
	return nil
}

// mergeEntry merges an incoming entry into the one the family already holds
// for the same DataUpload. The persisted attempt count is never reset, and a
// handle a previous attempt resolved from the repository snapshot's tag is
// kept when the incoming entry carries none. A family entry gathered from a
// live DataUpload keeps its trusted provenance and its handle: a tarball
// record never overwrites it, only its attempt count merges (design section
// 5). Without that rule a regather after a failed attempt -- the live
// DataUploads are gone by then -- would downgrade every record to untrusted,
// and a rewritten tarball could swap the handle a trusted record selected.
func mergeEntry(existing, incoming *Entry) *Entry {
	if existing.Trusted && !incoming.Trusted {
		merged := *existing
		if incoming.Attempts > merged.Attempts {
			merged.Attempts = incoming.Attempts
		}
		return &merged
	}
	if existing.Attempts > incoming.Attempts {
		incoming.Attempts = existing.Attempts
	}
	if incoming.CSISnapshotHandle == "" && existing.CSISnapshotHandle != "" {
		incoming.CSISnapshotHandle = existing.CSISnapshotHandle
	}
	return incoming
}

// findShardWithRoom returns the shard the key belongs in: the shard that
// already carries the key, searched across the whole family first, so a retry
// never duplicates an entry in a second shard -- a duplicate would survive
// MarkCompleted, which removes only one copy, and be deleted twice. A key not
// yet present goes to a shard with room for it.
func findShardWithRoom(shards map[string]*corev1api.ConfigMap, key, value string) *corev1api.ConfigMap {
	shardNames := make([]string, 0, len(shards))
	for name := range shards {
		shardNames = append(shardNames, name)
	}
	sort.Strings(shardNames)
	for _, name := range shardNames {
		if _, exists := shards[name].Data[key]; exists {
			return shards[name]
		}
	}
	for _, name := range shardNames {
		if shardSize(shards[name])+len(key)+len(value) < dataBudget {
			return shards[name]
		}
	}
	return nil
}

func isNotFound(err error) bool {
	return err != nil && apierrors.IsNotFound(err)
}

func decodeCompleted(shard *corev1api.ConfigMap) []string {
	var names []string
	if value, ok := shard.Data[CompletedKey]; ok && value != "" {
		if err := json.Unmarshal([]byte(value), &names); err != nil {
			return nil
		}
	}
	return names
}

// MarkCompleted removes the entry of the named DataUpload from its shard and
// records the name in the shard's completed set, so the entry is removed only
// after the snapshot is confirmed deleted and a later regather does not
// recreate it.
func MarkCompleted(ctx context.Context, c client.Client, backup *velerov1api.Backup, duName string) error {
	return TombstoneEntry(ctx, c, backup, duName)
}

// TombstoneEntry removes the entry of the named DataUpload from its shard and
// records the name in the shard's completed set, for a record this controller
// will not delete: escalated, skipped, or undecodable. The tombstone is what
// suppresses a regather from the unchanged tarball; without it, a retry of
// the backup deletion would resurrect the entry with its attempt count reset
// to zero and hammer a driver that already failed the bounded number of
// DeleteSnapshot calls.
func TombstoneEntry(ctx context.Context, c client.Client, backup *velerov1api.Backup, duName string) error {
	family, err := LoadFamily(ctx, c, backup)
	if err != nil {
		return err
	}
	for _, shard := range family.Shards {
		if _, ok := shard.Data[duName]; !ok {
			continue
		}
		delete(shard.Data, duName)
		completed := decodeCompleted(shard)
		completed = append(completed, duName)
		b, err := json.Marshal(completed)
		if err != nil {
			return errors.Wrap(err, "error marshaling the completed set")
		}
		shard.Data[CompletedKey] = string(b)
		return c.Update(ctx, shard)
	}
	return nil
}

// UpdateEntry persists a mutation of one pending entry, e.g. the resolved
// handle of a pre-rollback record or the incremented attempt count.
func UpdateEntry(ctx context.Context, c client.Client, backup *velerov1api.Backup, duName string, entry *Entry) error {
	family, err := LoadFamily(ctx, c, backup)
	if err != nil {
		return err
	}
	value, err := marshalEntry(entry)
	if err != nil {
		return err
	}
	for _, shard := range family.Shards {
		if _, ok := shard.Data[duName]; !ok {
			continue
		}
		shard.Data[duName] = value
		return c.Update(ctx, shard)
	}
	return errors.Errorf("no pending entry for DataUpload %s in the retained snapshot record family of backup %s", duName, backup.Name)
}

// RemoveEntry drops an entry without recording it as completed, for a record
// that turned out to hold nothing deletable, e.g. a repository snapshot
// without the tag.
func RemoveEntry(ctx context.Context, c client.Client, backup *velerov1api.Backup, duName string) error {
	family, err := LoadFamily(ctx, c, backup)
	if err != nil {
		return err
	}
	for _, shard := range family.Shards {
		if _, ok := shard.Data[duName]; !ok {
			continue
		}
		delete(shard.Data, duName)
		return c.Update(ctx, shard)
	}
	return nil
}

// WriteManifest writes shard 0 of the family, the manifest, with the count of
// the entry shards discovered by the backup labels in its data and the family
// label, which is the single write that activates the family: the
// retained-snapshot controller is keyed by shard 0 and reads exactly the entry
// shards it counts. It is written only after every prerequisite step of the
// deletion -- repository data and DataUploads -- has succeeded, so a deletion
// whose repository step failed gathers records but never has them deleted. It
// is idempotent, create-or-update, which is how the request's re-entry repairs
// a manifest write a crash interrupted. No manifest is written for a family
// without entry shards, so a backup with zero entries has no family.
func WriteManifest(ctx context.Context, c client.Client, backup *velerov1api.Backup) error {
	shards, err := listEntryShards(ctx, c, backup)
	if err != nil {
		return err
	}
	if len(shards) == 0 {
		return nil
	}
	prefix := FamilyNamePrefix(string(backup.UID))
	count := 0
	for _, shard := range shards {
		if index, ok := entryShardIndex(shard.Name, prefix); ok && index > count {
			count = index
		}
	}

	manifest := &corev1api.ConfigMap{}
	err = c.Get(ctx, client.ObjectKey{Namespace: backup.Namespace, Name: ManifestName(string(backup.UID))}, manifest)
	switch {
	case isNotFound(err):
		manifest = &corev1api.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   backup.Namespace,
				Name:        ManifestName(string(backup.UID)),
				Labels:      FamilyLabels(backup),
				Annotations: map[string]string{velerov1api.RetainedSnapshotsBackupNameAnnotation: backup.Name},
			},
			Data: map[string]string{ShardCountKey: strconv.Itoa(count)},
		}
		if err := c.Create(ctx, manifest); err != nil && !apierrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "error creating the retained snapshot record manifest %s", manifest.Name)
		}
		return nil
	case err != nil:
		return errors.Wrap(err, "error getting the retained snapshot record manifest")
	}
	if manifest.Data[ShardCountKey] == strconv.Itoa(count) && manifest.Labels[FamilyLabel] == "true" {
		return nil
	}
	if manifest.Data == nil {
		manifest.Data = map[string]string{}
	}
	manifest.Data[ShardCountKey] = strconv.Itoa(count)
	if manifest.Labels == nil {
		manifest.Labels = map[string]string{}
	}
	for key, value := range FamilyLabels(backup) {
		manifest.Labels[key] = value
	}
	if err := c.Update(ctx, manifest); err != nil {
		return errors.Wrapf(err, "error updating the retained snapshot record manifest %s", manifest.Name)
	}
	return nil
}

// DeleteManifest removes shard 0 of the family, so no unowned work continues
// after the DeleteBackupRequest waiting on the family is failed: a credential,
// clear-status or unknown failure marks the request Processed and deletes
// shard 0, and the retry is a new request that regathers into the inert entry
// shards and writes shard 0 again (design section 4). A manifest that is
// already gone is not an error.
func DeleteManifest(ctx context.Context, c client.Client, backup *velerov1api.Backup) error {
	manifest := &corev1api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: backup.Namespace, Name: ManifestName(string(backup.UID))}}
	if err := c.Delete(ctx, manifest); err != nil && !isNotFound(err) {
		return errors.Wrapf(err, "error deleting the retained snapshot record manifest %s", manifest.Name)
	}
	return nil
}

// Delete removes the shard family of the backup: shard 0 first, then the
// entry shards, so a crash inside the tail leaves only inert shards behind,
// which the sweep of a family whose Backup is gone removes. The tail of the
// deletion removes the family itself; it is not left to the Backup's garbage
// collection.
func Delete(ctx context.Context, c client.Client, backup *velerov1api.Backup) error {
	if err := DeleteManifest(ctx, c, backup); err != nil {
		return err
	}
	shards, err := listEntryShards(ctx, c, backup)
	if err != nil {
		return err
	}
	for _, shard := range shards {
		if err := c.Delete(ctx, shard); err != nil && !isNotFound(err) {
			return errors.Wrapf(err, "error deleting the retained snapshot shard %s", shard.Name)
		}
	}
	return nil
}
