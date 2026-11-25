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

package retainedsnapshot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/builder"
	velerotest "github.com/velero-io/velero/pkg/test"
)

func testBackup() *velerov1api.Backup {
	backup := builder.ForBackup(velerov1api.DefaultNamespace, "foo").StorageLocation("default").Result()
	backup.UID = "backup-uid"
	return backup
}

func testEntry(handle string) *Entry {
	return &Entry{
		CSISnapshotHandle: handle,
		Driver:            "rbd.csi.ceph.com",
		Trusted:           true,
	}
}

func dataUploadWithRecord(name string, handle string) *velerov2alpha1api.DataUpload {
	du := builder.ForDataUpload(velerov1api.DefaultNamespace, name).
		Labels(map[string]string{velerov1api.BackupNameLabel: "foo"}).
		SnapshotType(velerov2alpha1api.SnapshotTypeCSI).
		SourceNamespace("app").
		SourcePVC("data").
		DataMover("velero-block").
		Result()
	du.Status.RetainedCSIStorageSnapshot = &velerov2alpha1api.RetainedCSIStorageSnapshot{
		CSISnapshotHandle: handle,
		Driver:            "rbd.csi.ceph.com",
	}
	return du
}

func listShards(t *testing.T, c client.Client) []*corev1api.ConfigMap {
	t.Helper()
	family, err := LoadFamily(t.Context(), c, testBackup())
	require.NoError(t, err)
	return family.Shards
}

// TestWriteEntriesGathersByKey proves the family is keyed by the DataUpload
// name, written as inert entry shards from -1 with no shard 0, and merged on a
// retry instead of rejected.
func TestWriteEntriesGathersByKey(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": testEntry("handle-1"),
		"du-2": testEntry("handle-2"),
	}))

	shards := listShards(t, c)
	require.Len(t, shards, 1)
	shard := shards[0]
	assert.Equal(t, FamilyNamePrefix("backup-uid")+"1", shard.Name, "entry shards start at 1; shard 0 is the manifest")
	assert.Equal(t, EntryShardLabels(backup), shard.Labels, "an entry shard carries the backup labels, not the family label")
	assert.NotContains(t, shard.Data, ShardCountKey)
	family, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	assert.Nil(t, family.Manifest, "the gather writes no shard 0")
	entry, err := UnmarshalEntry(shard.Data["du-1"])
	require.NoError(t, err)
	assert.Equal(t, "handle-1", entry.CSISnapshotHandle)
	assert.True(t, entry.Trusted)

	// A retry of the same deletion merges into the existing family: a plain
	// create would fail on AlreadyExists and turn every later retry into a
	// permanent failure until an operator removes the map by hand.
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-2": testEntry("handle-2"),
		"du-3": testEntry("handle-3"),
	}))
	shards = listShards(t, c)
	require.Len(t, shards, 1)
	assert.Len(t, shardPendingKeys(t, shards[0]), 3)
}

// TestWriteEntriesShardsBeforeLimit proves the records are split into shards
// before the 1 MiB ConfigMap data limit, so a backup with thousands of
// retained snapshots does not fail the gather itself.
func TestWriteEntriesShardsBeforeLimit(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	entries := map[string]*Entry{}
	for i := 0; i < 5; i++ {
		entry := testEntry(strings.Repeat("h", 300*1024))
		entries[duName(i)] = entry
	}
	require.NoError(t, WriteEntries(t.Context(), c, backup, entries))

	shards := listShards(t, c)
	require.Greater(t, len(shards), 1, "five 300 KiB records must be split into shards")
	total := 0
	for _, shard := range shards {
		for key, value := range shard.Data {
			if key == ShardCountKey || key == CompletedKey {
				continue
			}
			total++
			require.Less(t, len(key)+len(value), dataBudget, "a shard must stay under the ConfigMap data limit")
		}
	}
	assert.Equal(t, 5, total)
}

func duName(i int) string {
	if i < 10 {
		return "du-00" + string(rune('0'+i))
	}
	return "du-0" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// TestCompletedSetSuppressesReplay proves that an entry removed after its
// snapshot was confirmed deleted leaves the DataUpload name in the completed
// set, and a later regather from the unchanged tarball does not recreate it.
func TestCompletedSetSuppressesReplay(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": testEntry("handle-1"),
		"du-2": testEntry("handle-2"),
	}))
	require.NoError(t, MarkCompleted(t.Context(), c, backup, "du-1"))

	family, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	_, pending := family.Entry("du-1")
	assert.False(t, pending, "the completed entry is removed")
	assert.Contains(t, family.CompletedSet(), "du-1")

	// The regather must not resurrect the completed entry.
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": testEntry("handle-1"),
	}))
	family, err = LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	_, pending = family.Entry("du-1")
	assert.False(t, pending, "a regather does not recreate a completed entry")
	assert.Equal(t, 1, family.PendingEntries())
}

// TestWriteEntriesMergesAttemptCount proves that an entry that escalated
// carries its persisted attempt count, and a re-encounter merges that count
// instead of resetting it, so a driver that fails slowly still reaches the
// bound across retries of the backup deletion.
func TestWriteEntriesMergesAttemptCount(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": {CSISnapshotHandle: "handle-1", Driver: "d", Attempts: 3},
	}))

	// A regather with a fresh entry keeps the persisted count.
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": testEntry("handle-1"),
	}))
	family, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	entry, ok := family.Entry("du-1")
	require.True(t, ok)
	assert.Equal(t, 3, entry.Attempts)
}

// TestWriteEntriesKeepsTrustedEntryOnRegather proves the trusted-entry rule
// of design section 5: a family entry gathered from a live DataUpload keeps
// its trusted provenance and its handle on a later regather; a tarball record
// never overwrites it, only its attempt count merges. Without it, a regather
// after a failed attempt -- the live DataUploads are gone by then -- downgrades
// every record to untrusted, and a rewritten tarball could swap the handle.
func TestWriteEntriesKeepsTrustedEntryOnRegather(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	// The controller's own gather from the live DataUpload: trusted.
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": {CSISnapshotHandle: "live-handle", Driver: "d", Trusted: true,
			DeletionSecretRef: &corev1api.SecretReference{Name: "live-secret", Namespace: "ns"}},
	}))

	// A retry regathers from the tarball after the DataUpload is gone: the
	// record is untrusted, names a different handle, and carries the attempt
	// count the orphan merge handed it.
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": {CSISnapshotHandle: "tarball-handle", Driver: "evil", Trusted: false, Attempts: 2,
			DeletionSecretRef: &corev1api.SecretReference{Name: "other-secret", Namespace: "ns"}},
	}))

	family, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	entry, ok := family.Entry("du-1")
	require.True(t, ok)
	assert.True(t, entry.Trusted, "the trusted provenance survives the regather")
	assert.Equal(t, "live-handle", entry.CSISnapshotHandle, "the tarball record never overwrites a trusted handle")
	assert.Equal(t, "d", entry.Driver)
	assert.Equal(t, "live-secret", entry.DeletionSecretRef.Name)
	assert.Equal(t, 2, entry.Attempts, "only the attempt count merges")

	// A trusted regather of a trusted entry still takes the incoming record,
	// as the local record wins.
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": {CSISnapshotHandle: "live-handle-2", Driver: "d", Trusted: true},
	}))
	family, err = LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	entry, ok = family.Entry("du-1")
	require.True(t, ok)
	assert.Equal(t, "live-handle-2", entry.CSISnapshotHandle)
	assert.Equal(t, 2, entry.Attempts, "the persisted count is never reset")
}

// TestWriteEntriesDoesNotDuplicateKeyAcrossShards proves that a retry writes
// an existing key into the shard that already carries it, not into a
// different shard with room: a duplicate would survive MarkCompleted, which
// removes only one copy, and the snapshot would be deleted twice.
func TestWriteEntriesDoesNotDuplicateKeyAcrossShards(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	// Fill the first shard and spill into a second, then place du-x in the
	// first shard, which is nearly full, while the second has room.
	big := func() *Entry { return testEntry(strings.Repeat("h", 300*1024)) }
	entries := map[string]*Entry{
		"big-00": big(), "big-01": big(), "big-02": big(), "big-03": big(),
		"du-x": testEntry("handle-x"),
	}
	require.NoError(t, WriteEntries(t.Context(), c, backup, entries))
	shards := listShards(t, c)
	require.Len(t, shards, 2, "four 300 KiB records plus a small one must spill into a second shard")
	inShard := 0
	for _, shard := range shards {
		if _, ok := shard.Data["du-x"]; ok {
			inShard++
		}
	}
	require.Equal(t, 1, inShard, "du-x must live in exactly one shard after the first write")

	// A retry of the same entry must not duplicate it in the other shard.
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{"du-x": testEntry("handle-x")}))
	shards = listShards(t, c)
	require.Len(t, shards, 2)
	inShard = 0
	for _, shard := range shards {
		if _, ok := shard.Data["du-x"]; ok {
			inShard++
		}
	}
	assert.Equal(t, 1, inShard, "a retry must write the key into the shard that already carries it, not a second one")
}

// TestWriteEntriesUpdatesOnlyChangedShards proves the gather persists only
// the shards it wrote an entry into: the delete action gathers once per
// DataUpload in the tarball, and rewriting every shard on each call would
// make the API traffic quadratic in the number of records.
func TestWriteEntriesUpdatesOnlyChangedShards(t *testing.T) {
	fakeClient := velerotest.NewFakeControllerRuntimeClient(t)
	var updates int
	c := interceptor.NewClient(fakeClient.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates++
			return cl.Update(ctx, obj, opts...)
		},
	})
	backup := testBackup()

	big := func() *Entry { return testEntry(strings.Repeat("h", 300*1024)) }
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"big-00": big(), "big-01": big(), "big-02": big(), "big-03": big(),
	}))
	require.Len(t, listShards(t, c), 2)

	// A gather of one small entry touches only the shard it lands in.
	updates = 0
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{"du-1": testEntry("handle-1")}))
	assert.Equal(t, 1, updates, "only the shard the entry was written into is updated")
}

// TestTombstoneEntrySuppressesRegather proves the escalated-entry tombstone:
// a record moved to the orphan-snapshot ConfigMap leaves the DataUpload name
// in the completed set, so a regather from the unchanged tarball does not
// resurrect it with its attempt count reset to zero.
func TestTombstoneEntrySuppressesRegather(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": {CSISnapshotHandle: "handle-1", Driver: "d", Attempts: MaxAttempts},
	}))
	require.NoError(t, TombstoneEntry(t.Context(), c, backup, "du-1"))

	family, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	_, pending := family.Entry("du-1")
	assert.False(t, pending, "the escalated entry is removed")
	assert.Contains(t, family.CompletedSet(), "du-1")

	// The regather from the unchanged tarball must not recreate the entry.
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": testEntry("handle-1"),
	}))
	family, err = LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	assert.Zero(t, family.PendingEntries(), "a regather does not recreate an escalated entry")
}

// TestWriteManifestActivates proves the shard-0 semantics: the entry shards
// are inert until shard 0 exists, the manifest counts the entry shards and
// carries the family label, its write is idempotent, deleting it deactivates
// the family, and no manifest is written for a family with zero entries.
func TestWriteManifestActivates(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	// Zero entries: no family at all.
	require.NoError(t, WriteManifest(t.Context(), c, backup))
	family, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	assert.Nil(t, family.Manifest, "no manifest is written for a backup with zero entries")
	assert.Empty(t, family.Shards)

	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{
		"du-1": testEntry("handle-1"),
	}))

	family, err = LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	assert.False(t, family.IsActivated())
	assert.Equal(t, 1, family.PendingEntries(), "the inert view still counts the pending entries")
	assert.False(t, family.HasPendingWork(), "an inert family has no work for the retained-snapshot controller")

	require.NoError(t, WriteManifest(t.Context(), c, backup))
	family, err = LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	require.NotNil(t, family.Manifest)
	assert.Equal(t, ManifestName("backup-uid"), family.Manifest.Name)
	assert.Equal(t, "1", family.Manifest.Data[ShardCountKey])
	assert.Equal(t, FamilyLabels(backup), family.Manifest.Labels)
	assert.Equal(t, backup.Name, family.Manifest.Annotations[velerov1api.RetainedSnapshotsBackupNameAnnotation])
	assert.True(t, IsManifest(family.Manifest))
	assert.False(t, IsEntryShard(family.Manifest))
	require.Len(t, family.Shards, 1)
	assert.True(t, IsEntryShard(family.Shards[0]))
	assert.True(t, family.IsActivated())
	assert.True(t, family.HasPendingWork())

	// The write is idempotent.
	require.NoError(t, WriteManifest(t.Context(), c, backup))
	again, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	assert.Equal(t, family.Manifest.ResourceVersion, again.Manifest.ResourceVersion, "an unchanged manifest is not rewritten")

	// Deleting shard 0 deactivates the family; the entry shards stay for the
	// retry's regather.
	require.NoError(t, DeleteManifest(t.Context(), c, backup))
	family, err = LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	assert.False(t, family.IsActivated())
	assert.Equal(t, 1, family.PendingEntries())
	require.NoError(t, DeleteManifest(t.Context(), c, backup), "deleting a gone manifest is not an error")
}

// TestManifestSurvivesCrashBetweenShards proves the crash coverage of the
// shard-0 design: entry shards written before a crash have no shard 0 and are
// inert; a later manifest write counts exactly the shards that exist and the
// reader loads exactly those by name; the tail deletes shard 0 first, so a
// crash inside the tail leaves only inert shards behind.
func TestManifestSurvivesCrashBetweenShards(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	// A gather large enough to split into several entry shards, then a crash
	// before shard 0 is written.
	entries := map[string]*Entry{}
	for i := 0; i < 5; i++ {
		entries[duName(i)] = testEntry(strings.Repeat("h", 300*1024))
	}
	require.NoError(t, WriteEntries(t.Context(), c, backup, entries))
	inert, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	require.Greater(t, len(inert.Shards), 1)
	assert.Nil(t, inert.Manifest)
	assert.False(t, inert.HasPendingWork(), "nothing is active until shard 0 exists")
	for i, shard := range inert.Shards {
		assert.Equal(t, fmt.Sprintf("%s%d", FamilyNamePrefix("backup-uid"), i+1), shard.Name, "entry shards are numbered from 1 without gaps")
	}

	// The retry's regather merges into the inert shards, and the manifest
	// write counts them.
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{duName(0): testEntry(strings.Repeat("h", 300*1024))}))
	require.NoError(t, WriteManifest(t.Context(), c, backup))
	active, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	require.NotNil(t, active.Manifest)
	assert.Equal(t, strconv.Itoa(len(inert.Shards)), active.Manifest.Data[ShardCountKey])
	assert.Len(t, active.Shards, len(inert.Shards), "the reader loads exactly the counted shards")
	assert.Equal(t, 5, active.PendingEntries())
	assert.True(t, active.HasPendingWork())

	// A shard the manifest does not count is not part of the activated view,
	// but the inert listing and the sweep still see it.
	stray := &corev1api.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: backup.Namespace,
			Name:      fmt.Sprintf("%s%d", FamilyNamePrefix("backup-uid"), len(inert.Shards)+1),
			Labels:    EntryShardLabels(backup),
		},
		Data: map[string]string{"du-stray": mustMarshal(t, testEntry("stray"))},
	}
	require.NoError(t, c.Create(t.Context(), stray))
	counted, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	assert.Len(t, counted.Shards, len(inert.Shards), "the activated view reads exactly shards 1..shardCount")
	all, err := ListInertShards(t.Context(), c, backup)
	require.NoError(t, err)
	assert.Len(t, all, len(inert.Shards)+1)
	// A manifest repair counts it in.
	require.NoError(t, WriteManifest(t.Context(), c, backup))
	repaired, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	assert.Len(t, repaired.Shards, len(inert.Shards)+1)

	// The tail: shard 0 first, then the entry shards.
	require.NoError(t, Delete(t.Context(), c, backup))
	gone, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	assert.Nil(t, gone.Manifest)
	assert.Empty(t, gone.Shards)
	left, err := ListInertShards(t.Context(), c, backup)
	require.NoError(t, err)
	assert.Empty(t, left)
}

func mustMarshal(t *testing.T, entry *Entry) string {
	t.Helper()
	value, err := marshalEntry(entry)
	require.NoError(t, err)
	return value
}

// TestOrphanEntriesOutliveBackup proves the orphan-snapshot ConfigMap is not
// labeled by backup and is keyed by <backup-UID>.<DataUpload-name>, so its
// entries cannot collide across backups once the backup label is absent.
func TestOrphanEntriesOutliveBackup(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	require.NoError(t, WriteOrphanEntry(t.Context(), c, backup, "du-1", testEntry("handle-1")))

	list := &corev1api.ConfigMapList{}
	require.NoError(t, c.List(t.Context(), list, client.MatchingLabels{OrphanLabel: "true"}))
	require.Len(t, list.Items, 1)
	orphan := list.Items[0]
	assert.NotContains(t, orphan.Labels, velerov1api.BackupNameLabel, "the orphan ConfigMap is not labeled by backup")
	assert.Contains(t, orphan.Data, "backup-uid.du-1")

	// The orphan write and the family write must not see each other's shards.
	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{"du-2": testEntry("handle-2")}))
	family, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	require.Len(t, family.Shards, 1)
	assert.NotContains(t, family.Shards[0].Data, "backup-uid.du-1")
}

// TestSplitOrphanKey proves the key split: the first dot separates the UID,
// which carries none, from a DataUpload name that may carry dots; the shard's
// bookkeeping keys are not entries.
func TestSplitOrphanKey(t *testing.T) {
	uid, du, ok := SplitOrphanKey("backup-uid.my.backup-abcde")
	assert.True(t, ok)
	assert.Equal(t, "backup-uid", uid)
	assert.Equal(t, "my.backup-abcde", du)
	for _, key := range []string{ShardCountKey, CompletedKey, "", ".du", "uid."} {
		_, _, ok := SplitOrphanKey(key)
		assert.False(t, ok, key)
	}
}

// TestRemoveOrphanEntriesForSnapshot proves the stale-orphan cleanup of design
// section 4: an orphan entry recorded under an earlier UID of the backup is
// matched by the snapshot itself, removed once that snapshot is deleted, and
// returned for the event; entries of other snapshots and other drivers stay.
func TestRemoveOrphanEntriesForSnapshot(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	earlier := testBackup()
	earlier.UID = "earlier-uid"
	require.NoError(t, WriteOrphanEntry(t.Context(), c, earlier, "du-1", testEntry("handle-1")))
	require.NoError(t, WriteOrphanEntry(t.Context(), c, earlier, "du-2", testEntry("handle-2")))
	other := testEntry("handle-1")
	other.Driver = "other.csi.example.com"
	require.NoError(t, WriteOrphanEntry(t.Context(), c, earlier, "du-3", other))

	entries, err := LoadOrphanEntries(t.Context(), c, earlier.Namespace)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, "earlier-uid", entries[0].BackupUID)
	assert.Equal(t, "du-1", entries[0].DataUpload)
	assert.Equal(t, "handle-1", entries[0].Entry.CSISnapshotHandle)

	removed, err := RemoveOrphanEntriesForSnapshot(t.Context(), c, earlier.Namespace, "rbd.csi.ceph.com", "handle-1")
	require.NoError(t, err)
	require.Len(t, removed, 1)
	assert.Equal(t, "earlier-uid.du-1", removed[0].Key)

	entries, err = LoadOrphanEntries(t.Context(), c, earlier.Namespace)
	require.NoError(t, err)
	keys := []string{}
	for _, o := range entries {
		keys = append(keys, o.Key)
	}
	assert.ElementsMatch(t, []string{"earlier-uid.du-2", "earlier-uid.du-3"}, keys, "other snapshots and other drivers stay")

	// Nothing to remove is not an error.
	removed, err = RemoveOrphanEntriesForSnapshot(t.Context(), c, earlier.Namespace, "rbd.csi.ceph.com", "handle-9")
	require.NoError(t, err)
	assert.Empty(t, removed)
	removed, err = RemoveOrphanEntriesForSnapshot(t.Context(), c, earlier.Namespace, "rbd.csi.ceph.com", "")
	require.NoError(t, err)
	assert.Empty(t, removed)
}

// TestDeleteRemovesFamily proves the tail's own Backup deletion is not the GC
// trigger: the tail removes the family itself.
func TestDeleteRemovesFamily(t *testing.T) {
	c := velerotest.NewFakeControllerRuntimeClient(t)
	backup := testBackup()

	require.NoError(t, WriteEntries(t.Context(), c, backup, map[string]*Entry{"du-1": testEntry("handle-1")}))
	require.NoError(t, Delete(t.Context(), c, backup))

	family, err := LoadFamily(t.Context(), c, backup)
	require.NoError(t, err)
	assert.Empty(t, family.Shards)
}

// TestFromDataUploadStatus proves the entry is built from the DataUpload's
// persisted record and marked trusted, and that a DataUpload without a record
// contributes nothing.
func TestFromDataUploadStatus(t *testing.T) {
	entry := FromDataUploadStatus(dataUploadWithRecord("du-1", "handle-1"))
	require.NotNil(t, entry)
	assert.Equal(t, "handle-1", entry.CSISnapshotHandle)
	assert.True(t, entry.Trusted)

	du := dataUploadWithRecord("du-1", "handle-1")
	du.Status.RetainedCSIStorageSnapshot = nil
	assert.Nil(t, FromDataUploadStatus(du))

	// A released record no longer owns a snapshot: no entry, so the backup
	// deletion skips it and the local-wins merge drops the tarball's copy.
	released := dataUploadWithRecord("du-1", "handle-1")
	released.Status.RetainedCSIStorageSnapshot.Released = true
	released.Status.RetainedCSIStorageSnapshot.ReleasedBy = "later-uid"
	assert.Nil(t, FromDataUploadStatus(released))
}

// TestEntryRecord proves the entry's record carries the deletion fields the
// temp VolumeSnapshotContent is rebuilt from.
func TestEntryRecord(t *testing.T) {
	entry := &Entry{
		CSISnapshotHandle:   "handle-1",
		Driver:              "rbd.csi.ceph.com",
		ManagedByLabelValue: "node-a",
		DeletionSecretRef: &corev1api.SecretReference{
			Name:      "rook-csi-rbd-provisioner",
			Namespace: "rook-ceph",
		},
	}
	record := entry.Record()
	assert.Equal(t, "handle-1", record.CSISnapshotHandle)
	assert.Equal(t, "rbd.csi.ceph.com", record.Driver)
	assert.Equal(t, "node-a", record.ManagedByLabelValue)
	assert.Equal(t, "rook-csi-rbd-provisioner", record.DeletionSecretRef.Name)
}

// pendingKeys is exported for the controller package through the family API;
// shardPendingKeys is a test helper mirroring it.
func shardPendingKeys(t *testing.T, shard *corev1api.ConfigMap) []string {
	t.Helper()
	keys := []string{}
	for key := range shard.Data {
		if key == ShardCountKey || key == CompletedKey {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}
