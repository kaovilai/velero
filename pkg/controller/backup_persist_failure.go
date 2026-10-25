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

package controller

import (
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
)

// persistFailureTracker records, per backup, when writing the backup's
// metadata to the backup storage location started failing. Controllers use it
// to stop retrying and fail the backup once the failure has lasted longer than
// the resource timeout, instead of leaving the backup in a non-terminal phase
// forever (e.g. when the bucket is over quota or an object lock policy rejects
// the write). The state is kept in memory only, so a server restart starts a
// new window.
type persistFailureTracker struct {
	lock  sync.Mutex
	since map[types.UID]time.Time
}

func newPersistFailureTracker() *persistFailureTracker {
	return &persistFailureTracker{since: map[types.UID]time.Time{}}
}

// failingFor records a failed write for the backup at now and returns how long
// writes for the backup have been failing.
func (t *persistFailureTracker) failingFor(uid types.UID, now time.Time) time.Duration {
	t.lock.Lock()
	defer t.lock.Unlock()
	since, ok := t.since[uid]
	if !ok {
		t.since[uid] = now
		return 0
	}
	return now.Sub(since)
}

// clear forgets any recorded failure for the backup.
func (t *persistFailureTracker) clear(uid types.UID) {
	t.lock.Lock()
	defer t.lock.Unlock()
	delete(t.since, uid)
}

// markBackupFailedOnPersistError moves the backup to Failed with a failure
// reason naming the backup storage location write that kept failing.
func markBackupFailedOnPersistError(backup *velerov1api.Backup, failingFor time.Duration, err error, now time.Time) {
	backup.Status.Phase = velerov1api.BackupPhaseFailed
	backup.Status.FailureReason = fmt.Sprintf("unable to write backup metadata to backup storage location %q for %s: %v",
		backup.Spec.StorageLocation, failingFor.Round(time.Second), err)
	backup.Status.CompletionTimestamp = &metav1.Time{Time: now}
}
