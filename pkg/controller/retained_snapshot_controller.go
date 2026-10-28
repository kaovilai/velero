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
	"context"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	clocks "k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/velero-io/velero/pkg/apis/velero/v2alpha1"
	"github.com/velero-io/velero/pkg/constant"
	"github.com/velero-io/velero/pkg/retainedsnapshot"
	"github.com/velero-io/velero/pkg/util/datamover"
)

// retainedSnapshotReconciler keeps only the latest retained base snapshot per
// chain (BSL, source namespace, source PVC). A reconcile request is a chain:
// Name is the chain hash. It is triggered by DataUploads reaching a final phase
// and by changes of the retained snapshot records.
//
// For each live record of the chain it deletes the snapshot when:
//   - a completed block DataUpload of the chain started later (rotation), and
//     keeps the record as a tombstone until the backup is deleted;
//   - its DataUpload failed or was canceled (GC), and removes the record.
//
// Records of another cluster are left alone.
type retainedSnapshotReconciler struct {
	client    client.Client
	apiReader client.Reader
	namespace string
	logger    logrus.FieldLogger
	clock     clocks.WithTickerAndDelayedExecution
	clusterID string
}

func NewRetainedSnapshotReconciler(c client.Client, namespace string, logger logrus.FieldLogger) *retainedSnapshotReconciler {
	return &retainedSnapshotReconciler{
		client:    c,
		namespace: namespace,
		logger:    logger.WithField("controller", constant.ControllerRetainedSnapshot),
		clock:     clocks.RealClock{},
	}
}

func (r *retainedSnapshotReconciler) SetupWithManager(mgr manager.Manager) error {
	r.apiReader = mgr.GetAPIReader()

	recordPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.namespace && obj.GetLabels()[velerov1api.DataUploadRetainedSnapshotLabel] == "true" &&
			obj.GetLabels()[retainedsnapshot.ChainLabel] != ""
	})
	finishedDUPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		du, ok := obj.(*velerov2alpha1api.DataUpload)
		return ok && datamover.IsVeleroBlockDataMover(du.Spec.DataMover) && isDataUploadInFinalState(du)
	})

	return ctrl.ControllerManagedBy(mgr).
		Named(constant.ControllerRetainedSnapshot).
		Watches(&corev1api.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.namespace, Name: obj.GetLabels()[retainedsnapshot.ChainLabel]}}}
		}), ctrl.WithPredicates(recordPredicate)).
		Watches(&velerov2alpha1api.DataUpload{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			du := obj.(*velerov2alpha1api.DataUpload)
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.namespace, Name: retainedsnapshot.ChainOfDataUpload(du)}}}
		}), ctrl.WithPredicates(finishedDUPredicate)).
		Complete(r)
}

func (r *retainedSnapshotReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	chain := req.Name
	log := r.logger.WithField("chain", chain)

	cms := &corev1api.ConfigMapList{}
	if err := r.client.List(ctx, cms, &client.ListOptions{
		Namespace: r.namespace,
		LabelSelector: labels.SelectorFromSet(map[string]string{
			velerov1api.DataUploadRetainedSnapshotLabel: "true",
			retainedsnapshot.ChainLabel:                 chain,
			retainedsnapshot.StateLabel:                 retainedsnapshot.StateRetained,
		}),
	}); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "error listing retained snapshot records")
	}
	if len(cms.Items) == 0 {
		return reconcile.Result{}, nil
	}

	dus := &velerov2alpha1api.DataUploadList{}
	if err := r.client.List(ctx, dus, client.InNamespace(r.namespace)); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "error listing DataUploads")
	}
	byName := map[string]*velerov2alpha1api.DataUpload{}
	var newest time.Time
	newestDU := ""
	for i := range dus.Items {
		du := &dus.Items[i]
		if retainedsnapshot.ChainOfDataUpload(du) != chain {
			continue
		}
		byName[du.Name] = du
		if du.Status.Phase == velerov2alpha1api.DataUploadPhaseCompleted && datamover.IsVeleroBlockDataMover(du.Spec.DataMover) &&
			du.Status.StartTimestamp != nil && du.Status.StartTimestamp.Time.After(newest) {
			newest = du.Status.StartTimestamp.Time
			newestDU = du.Name
		}
	}

	if r.clusterID == "" {
		id, err := retainedsnapshot.GetClusterIDFromReader(ctx, r.reader())
		if err != nil {
			return reconcile.Result{}, err
		}
		r.clusterID = id
	}

	var errs []error
	for i := range cms.Items {
		cm := &cms.Items[i]
		record, err := retainedsnapshot.FromConfigMap(cm)
		if err != nil {
			log.WithError(err).Warnf("Skipping invalid retained snapshot record %s", cm.Name)
			continue
		}
		if retainedsnapshot.IsForeign(record.ClusterID, r.clusterID) {
			continue
		}
		rlog := log.WithFields(logrus.Fields{"record": cm.Name, "snapshotHandle": record.SnapshotHandle, "dataupload": record.DataUpload})

		du := byName[record.DataUpload]
		switch {
		case du != nil && (du.Status.Phase == velerov2alpha1api.DataUploadPhaseFailed || du.Status.Phase == velerov2alpha1api.DataUploadPhaseCanceled):
			rlog.Infof("DataUpload ended %s, deleting its retained snapshot", du.Status.Phase)
			if err := retainedsnapshot.DeleteSnapshot(ctx, r.deletionClient(), record, rlog); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := r.client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		case !record.StartTime.IsZero() && record.StartTime.Before(newest):
			rlog.Infof("Rotating out retained snapshot, DataUpload %s of the chain started later (%s > %s)",
				newestDU, newest.UTC().Format(time.RFC3339), record.StartTime.UTC().Format(time.RFC3339))
			if err := retainedsnapshot.DeleteSnapshot(ctx, r.deletionClient(), record, rlog); err != nil {
				errs = append(errs, err)
				continue
			}
			updated := cm.DeepCopy()
			retainedsnapshot.MarkDeleted(updated, r.clock.Now())
			if err := r.client.Patch(ctx, updated, client.MergeFrom(cm)); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}

	if len(errs) > 0 {
		return reconcile.Result{}, errors.Errorf("failed to process %d retained snapshot record(s) of chain %s, first error: %v", len(errs), chain, errs[0])
	}
	return reconcile.Result{}, nil
}

func (r *retainedSnapshotReconciler) reader() client.Reader {
	if r.apiReader == nil {
		return r.client
	}
	return r.apiReader
}

// deletionClient reads the temp VolumeSnapshotContents and their events from
// the API server, which the manager's cache doesn't cover.
func (r *retainedSnapshotReconciler) deletionClient() client.Client {
	if r.apiReader == nil {
		return r.client
	}
	return &apiReadClient{Client: r.client, reader: r.apiReader}
}
