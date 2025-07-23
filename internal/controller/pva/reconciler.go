package pva

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/metrics"
	"github.com/gardener/pvc-autoscaler/internal/metrics/source"
	"github.com/gardener/pvc-autoscaler/internal/utils"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
)

// UnknownUtilizationValue is the value which will be used when the free
// space/inodes utilization is unknown.
const UnknownUtilizationValue = "unknown"

// ErrNoMetricsSource is returned when the [Runner] is configured without a
// metrics source.
var ErrNoMetricsSource = errors.New("no metrics source provided")

// ErrVolumeModeIsNotFilesystem is an error which is returned if a target PVC
// for resizing is not using the Filesystem VolumeMode.
var ErrVolumeModeIsNotFilesystem = errors.New("volume mode is not filesystem")

// ErrStorageClassNotFound is an error which is returned when the storage class
// for a PVC is not found.
var ErrStorageClassNotFound = errors.New("no storage class found")

// ErrStorageClassDoesNotSupportExpansion is an error which is returned when an
// annotated PVC uses a storage class that does not support volume expansion.
var ErrStorageClassDoesNotSupportExpansion = errors.New("storage class does not support expansion")

// ErrNoClient is an error which is returned when the periodic [Runner] was
// configured configured without a Kubernetes API client.
var ErrNoClient = errors.New("no client provided")

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("pva", req.NamespacedName)

	pva := &v1alpha1.PersistentVolumeAutoscaler{}
	if err := r.Client.Get(ctx, req.NamespacedName, pva); err != nil {
		return reconcile.Result{}, err
	}

	switch pva.Spec.TargetRef.Kind {
	case "StatefulSet":
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pva.Spec.TargetRef.Name,
				Namespace: pva.Spec.TargetRef.Namespace,
			},
		}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
			return reconcile.Result{}, fmt.Errorf("couldn't retrieve target statefulset: %w", err)
		}
		pvcs, err := r.getPVCsFromStatefulSet(ctx, sts)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("couldn't retrieve persistent volume claims: %w", err)
		}

		pvaPatch := client.MergeFrom(pva.DeepCopy())
		for _, pvc := range pvcs {
			if err := r.reconcilePVC(ctx, logger, pva, pvc); err != nil {
				return reconcile.Result{}, fmt.Errorf("couldn't reconcile pvc %s: %w", client.ObjectKeyFromObject(pvc), err)
			}
		}
		// Update the status here
		if err := r.Client.Patch(ctx, pva, pvaPatch); err != nil {
			return reconcile.Result{}, fmt.Errorf("could not patch pva %s: %w", client.ObjectKeyFromObject(pva), err)
		}
	default:
		return reconcile.Result{}, nil
	}

	return reconcile.Result{RequeueAfter: r.ResyncPeriod}, nil
}

func (r *Reconciler) getPVCsFromStatefulSet(ctx context.Context, sts *appsv1.StatefulSet) ([]*corev1.PersistentVolumeClaim, error) {
	if len(sts.Spec.VolumeClaimTemplates) == 0 {
		return nil, fmt.Errorf("no persistent volume claims specified in statefulset %s", client.ObjectKeyFromObject(sts))
	}

	if sts.Spec.Replicas == nil {
		return nil, nil
	}

	pvcsToScale := []*corev1.PersistentVolumeClaim{}
	for _, pvc := range sts.Spec.VolumeClaimTemplates {
		for ordinal := sts.Spec.Ordinals.Start; ordinal <= sts.Spec.Ordinals.Start+*sts.Spec.Replicas; ordinal++ {
			name := pvc.Name + "-" + sts.Name + "-" + fmt.Sprintf("%d", ordinal)
			tmp := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: sts.Namespace,
				},
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(tmp), tmp); err != nil {
				return nil, fmt.Errorf("could nto retrieve pvc %s: %w", client.ObjectKeyFromObject(tmp), err)
			}
			pvcsToScale = append(pvcsToScale, tmp)
		}
	}
	return pvcsToScale, nil
}

func (r *Reconciler) reconcilePVC(ctx context.Context, logger logr.Logger, pva *v1alpha1.PersistentVolumeAutoscaler, pvc *corev1.PersistentVolumeClaim) error {
	logger = logger.WithValues("pvc", pvc.Name)
	idx := slices.IndexFunc(pva.Status.PVCs, func(pvcStatus v1alpha1.PersistentVolumeClaimStatus) bool {
		return pvcStatus.Name == pvc.Name
	})
	volumeMetrics := r.MetricsStorage.GetMetric(client.ObjectKeyFromObject(pvc))
	r.updatePVAStatusForPVC(pva, idx, volumeMetrics)

	currSpecSize := pvc.Spec.Resources.Requests.Storage()
	currStatusSize := pvc.Status.Capacity.Storage()

	// Make sure that the PVC is not being modified at the moment.  Note,
	// that we are not treating the following status conditions as errors,
	// as these are transient conditions.
	if utils.IsPersistentVolumeClaimConditionTrue(pvc, corev1.PersistentVolumeClaimResizing) {
		logger.Info("resize has been started")
		condition := metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: "Resize has been started",
		}
		pva.SetConditionForPVC(condition, pvc.Name)
		return nil
	}

	if utils.IsPersistentVolumeClaimConditionTrue(pvc, corev1.PersistentVolumeClaimFileSystemResizePending) {
		logger.Info("filesystem resize is pending")
		condition := metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: "File system resize is pending",
		}
		pva.SetConditionForPVC(condition, pvc.Name)
		return nil
	}

	if utils.IsPersistentVolumeClaimConditionTrue(pvc, corev1.PersistentVolumeClaimVolumeModifyingVolume) {
		logger.Info("volume is being modified")
		condition := metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: "Volume is being modified",
		}
		pva.SetConditionForPVC(condition, pvc.Name)
		return nil
	}

	// If previously recorded size is equal to the current status it means
	// we are still waiting for the resize to complete
	if pva.Status.PVCs[idx].PrevSize.Equal(*currStatusSize) {
		logger.Info("persistent volume claim is still being resized")
		condition := metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: "Persistent volume claim is still being resized",
		}
		pva.SetConditionForPVC(condition, pvc.Name)
		return nil
	}

	// Calculate the new size
	increaseBy, err := utils.ParsePercentage(pva.Spec.IncreaseBy)
	if err != nil {
		eerr := fmt.Errorf("cannot parse increase-by value: %w", err)
		condition := metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionUnknown,
			Reason:  "Reconciling",
			Message: eerr.Error(),
		}
		// TODO: set this on a more general condition
		pva.SetConditionForPVC(condition, pvc.Name)
		return nil
	}

	increment := float64(currSpecSize.Value()) * (increaseBy / 100.0)
	newSizeBytes := int64(math.Ceil((float64(currSpecSize.Value())+increment)/1073741824)) * 1073741824
	newSize := resource.NewQuantity(newSizeBytes, resource.BinarySI)

	// Check that we've got a valid new size. If we end up in any of these
	// cases below, it pretty much means the logic is broken, so we don't
	// want to retry any of them.
	cmp := newSize.Cmp(*currSpecSize)
	switch cmp {
	case 0:
		logger.Info("new and current size are the same")
		return nil
	case -1:
		logger.Info("new size is less than current")
		return nil
	}

	// We don't want to exceed the max capacity
	if newSize.Value() > pva.Spec.MaxCapacity.Value() {
		r.EventRecorder.Eventf(
			pvc,
			corev1.EventTypeWarning,
			"MaxCapacityReached",
			"max capacity (%s) has been reached, will not resize",
			pva.Spec.MaxCapacity.String(),
		)
		logger.Info("max capacity reached")
		metrics.MaxCapacityReachedTotal.WithLabelValues(pvc.Namespace, pvc.Name).Inc()
		condition := metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: "Max capacity reached",
		}

		pva.SetConditionForPVC(condition, pvc.Name)
		return nil
	}

	// And finally we should be good to resize now
	logger.Info("resizing persistent volume claim", "from", currSpecSize.String(), "to", newSize.String())
	metrics.ResizedTotal.WithLabelValues(pvc.Namespace, pvc.Name).Inc()
	r.EventRecorder.Eventf(
		pvc,
		corev1.EventTypeNormal,
		"ResizingStorage",
		"resizing storage from %s to %s",
		currSpecSize.String(),
		newSize.String(),
	)

	// Update PVC and PVCA resources
	// TODO: pvc should probably be retrieved here in case someone patched it meanwhile
	pvcPatch := client.MergeFrom(pvc.DeepCopy())
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = *newSize
	if err := r.Client.Patch(ctx, pvc, pvcPatch); err != nil {
		return err
	}

	pva.Status.PVCs[idx].PrevSize = *currStatusSize
	pva.Status.PVCs[idx].NewSize = *newSize

	condition := metav1.Condition{
		Type:    utils.ConditionTypeHealthy,
		Status:  metav1.ConditionFalse,
		Reason:  "Reconciling",
		Message: fmt.Sprintf("Resizing from %s to %s", currSpecSize.String(), newSize.String()),
	}

	pva.SetConditionForPVC(condition, pvc.Name)
	return nil
}

// updatePVCAStatus updates the status of the
// [v1alpha1.PersistentVolumeClaimAutoscaler] with the latest observed
// information about the target [corev1.PersistentVolumeClaim].
func (r *Reconciler) updatePVAStatusForPVC(obj *v1alpha1.PersistentVolumeAutoscaler, idx int, volInfo *source.VolumeInfo) {
	now := time.Now()
	nextCheck := now.Add(r.ResyncPeriod)

	freeSpaceStr := UnknownUtilizationValue
	usedSpaceStr := UnknownUtilizationValue
	freeInodesStr := UnknownUtilizationValue
	usedInodesStr := UnknownUtilizationValue

	if volInfo != nil {
		if freeSpace, err := volInfo.FreeSpacePercentage(); err == nil {
			freeSpaceStr = fmt.Sprintf("%.2f%%", freeSpace)
		}

		if usedSpace, err := volInfo.UsedSpacePercentage(); err == nil {
			usedSpaceStr = fmt.Sprintf("%.2f%%", usedSpace)
		}

		if freeInodes, err := volInfo.FreeInodesPercentage(); err == nil {
			freeInodesStr = fmt.Sprintf("%.2f%%", freeInodes)
		}

		if usedInodes, err := volInfo.UsedInodesPercentage(); err == nil {
			usedInodesStr = fmt.Sprintf("%.2f%%", usedInodes)
		}
	}

	obj.Status.PVCs[idx].LastCheck = metav1.NewTime(now)
	obj.Status.PVCs[idx].NextCheck = metav1.NewTime(nextCheck)
	obj.Status.PVCs[idx].UsedSpacePercentage = usedSpaceStr
	obj.Status.PVCs[idx].FreeSpacePercentage = freeSpaceStr
	obj.Status.PVCs[idx].UsedInodesPercentage = usedInodesStr
	obj.Status.PVCs[idx].FreeInodesPercentage = freeInodesStr
}

// shouldReconcilePVC is a predicate which checks whether the
// [corev1.PersistentVolumeClaim] object targeted by
// [v1alpha1.PersistentVolumeClaimAutoscaler] should be considered for
// reconciliation.
func (r *Reconciler) shouldReconcilePVC(ctx context.Context, pvca *v1alpha1.PersistentVolumeClaimAutoscaler, volInfo *metricssource.VolumeInfo) (bool, error) {
	pvcObjKey := client.ObjectKey{Namespace: pvca.Namespace, Name: pvca.Spec.ScaleTargetRef.Name}
	pvcObj := &corev1.PersistentVolumeClaim{}
	if err := r.client.Get(ctx, pvcObjKey, pvcObj); err != nil {
		return false, err
	}

	if err := r.updatePVCAStatus(ctx, pvca, volInfo); err != nil {
		return false, err
	}

	// No metrics found, nothing to do for now
	if volInfo == nil {
		return false, common.ErrNoMetrics
	}

	// Validate the spec
	if err := r.validatePVCA(pvca); err != nil {
		return false, err
	}

	// Validate the PVC itself against the spec
	currStatusSize := pvcObj.Status.Capacity.Storage()
	if currStatusSize.IsZero() {
		return false, fmt.Errorf(".status.capacity.storage is invalid: %s", currStatusSize.String())
	}

	if pvca.Spec.MaxCapacity.Value() < currStatusSize.Value() {
		return false, fmt.Errorf("max capacity (%s) cannot be less than current size (%s)", pvca.Spec.MaxCapacity.String(), currStatusSize.String())
	}

	// We need a StorageClass with expansion support
	scName := ptr.Deref(pvcObj.Spec.StorageClassName, "")
	if scName == "" {
		return false, ErrStorageClassNotFound
	}

	var sc storagev1.StorageClass
	scKey := types.NamespacedName{Name: scName}
	if err := r.client.Get(ctx, scKey, &sc); err != nil {
		return false, err
	}

	if !ptr.Deref(sc.AllowVolumeExpansion, false) {
		return false, ErrStorageClassDoesNotSupportExpansion
	}

	// Detect whether the metrics source is reporting stale data.  Stale
	// metrics data would be when the volume info metrics reported by the
	// metrics source are deviate from the current PVC size indicated by
	// `.status.capacity.storage'
	if statusSize, ok := currStatusSize.AsInt64(); ok {
		delta := statusSize - int64(volInfo.CapacityBytes)
		if delta < 0 {
			delta = -delta
		}
		if delta > common.ScalingResolutionBytes/2 {
			return false, common.ErrStaleMetrics
		}
	}

	// Getting an error from FreeSpacePercentage() means that the
	// capacity for the volume is zero, which in turn means that we
	// didn't get any metrics for it.
	freeSpace, err := volInfo.FreeSpacePercentage()
	if err != nil {
		return false, common.ErrNoMetrics
	}

	// Even, if we don't have inode metrics we still want to proceed here.
	freeInodes, err := volInfo.FreeInodesPercentage()
	if err != nil {
		return false, common.ErrNoMetrics
	}

	threshold, err := utils.ParsePercentage(pvca.Spec.Threshold)
	if err != nil {
		return false, fmt.Errorf("cannot parse threshold: %w", err)
	}

	// VolumeMode should be Filesystem
	if pvcObj.Spec.VolumeMode == nil {
		return false, nil
	}
	if *pvcObj.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return false, ErrVolumeModeIsNotFilesystem
	}

	// The PVC should be bound
	if pvcObj.Status.Phase != corev1.ClaimBound {
		return false, nil
	}

	switch {
	// Free space reached threshold
	case freeSpace < threshold:
		r.EventRecorder.Eventf(
			pvcObj,
			corev1.EventTypeWarning,
			"FreeSpaceThresholdReached",
			"free space (%.2f%%) is less than the configured threshold (%.2f%%)",
			freeSpace,
			threshold,
		)
		metrics.ThresholdReachedTotal.WithLabelValues(pvcObj.Namespace, pvcObj.Name, "space").Inc()
		return true, nil

	// Free inodes reached threshold
	case volInfo.CapacityInodes > 0.0 && (freeInodes < threshold):
		r.EventRecorder.Eventf(
			pvcObj,
			corev1.EventTypeWarning,
			"FreeInodesThresholdReached",
			"free inodes (%.2f%%) are less than the configured threshold (%.2f%%)",
			freeInodes,
			threshold,
		)
		metrics.ThresholdReachedTotal.WithLabelValues(pvcObj.Namespace, pvcObj.Name, "inodes").Inc()
		return true, nil

	// No need to reconcile the PVC for now
	default:
		return false, nil
	}
}

// validatePVCA sanity checks the spec in order to ensure it contains valid
// values. Returns nil if the spec is valid, and non-nil error otherwise.
func (r *Reconciler) validatePVA(obj *v1alpha1.PersistentVolumeClaimAutoscaler) error {
	threshold, err := utils.ParsePercentage(obj.Spec.Threshold)
	if err != nil {
		return fmt.Errorf("cannot parse threshold: %w", err)
	}
	if threshold == 0.0 {
		return fmt.Errorf("invalid threshold: %w", common.ErrZeroPercentage)
	}

	if obj.Spec.MaxCapacity.IsZero() {
		return fmt.Errorf("invalid max capacity: %w", common.ErrNoMaxCapacity)
	}

	increaseBy, err := utils.ParsePercentage(obj.Spec.IncreaseBy)
	if err != nil {
		return fmt.Errorf("cannot parse increase-by value: %w", err)
	}
	if increaseBy == 0.0 {
		return fmt.Errorf("invalid increase-by: %w", common.ErrZeroPercentage)
	}

	return nil
}
