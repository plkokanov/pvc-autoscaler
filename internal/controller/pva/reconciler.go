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
	storagev1 "k8s.io/api/storage/v1"
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

// +kubebuilder:rbac:groups=autoscaling.gardener.cloud,resources=persistentvolumeautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.gardener.cloud,resources=persistentvolumeautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.gardener.cloud,resources=persistentvolumeautoscalers/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims/status,verbs=get
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("pva", req.NamespacedName)

	pva := &v1alpha1.PersistentVolumeAutoscaler{}
	if err := r.Client.Get(ctx, req.NamespacedName, pva); err != nil {
		return reconcile.Result{}, err
	}

	// switch pva.Spec.TargetRef.Kind {
	// case "StatefulSet":
	// 	sts := &appsv1.StatefulSet{
	// 		ObjectMeta: metav1.ObjectMeta{
	// 			Name:      pva.Spec.TargetRef.Name,
	// 			Namespace: pva.Spec.TargetRef.Namespace,
	// 		},
	// 	}
	// 	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
	// 		return reconcile.Result{}, fmt.Errorf("couldn't retrieve target statefulset %s: %w", client.ObjectKeyFromObject(sts), err)
	// 	}
	pvcs, err := r.getPVCsForPods(ctx, logger, pva, &pva.Spec.TargetRef)
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
	if err := r.Client.Status().Patch(ctx, pva, pvaPatch); err != nil {
		return reconcile.Result{}, fmt.Errorf("could not patch pva %s: %w", client.ObjectKeyFromObject(pva), err)
	}
	// default:
	// 	return reconcile.Result{}, nil
	// }

	return reconcile.Result{RequeueAfter: r.ResyncPeriod}, nil
}

func (r *Reconciler) getPVCsForPods(ctx context.Context, log logr.Logger, pva *v1alpha1.PersistentVolumeAutoscaler, targetRef *corev1.ObjectReference) ([]*corev1.PersistentVolumeClaim, error) {
	log.Info("listing pods in namespace", "namespace", pva.Spec.TargetRef.Namespace)

	podList := &corev1.PodList{}
	// TODO: can we optimize this list
	if err := r.Client.List(ctx, podList, &client.ListOptions{Namespace: pva.Spec.TargetRef.Namespace}); err != nil {
		return nil, fmt.Errorf("could not list pods in namespace %s: %w", pva.Spec.TargetRef.Namespace, err)
	}

	pvcsToScale := []*corev1.PersistentVolumeClaim{}

	for _, item := range podList.Items {
		controllerForPodMatches, err := r.controllerForPodContainsTarget(ctx, log, &item, ownerRefFromTargetRef(targetRef))
		if !controllerForPodMatches {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("error while trying to find matching controller for pod %s: %w", client.ObjectKeyFromObject(&item), err)
		}
		for _, volume := range item.Spec.Volumes {
			if volumeClaim := volume.PersistentVolumeClaim; volumeClaim != nil {
				// if slices.ContainsFunc(pva.Spec.PVCResizePolicies, func(pvcResizePolicy v1alpha1.PVCResizePolicy) bool {
				// 	return pvcResizePolicy.Name == volumeClaim.ClaimName || strings.HasPrefix(pvcResizePolicy.NameTemplate, volumeClaim.ClaimName)
				// }) {
				pvc := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      volumeClaim.ClaimName,
						Namespace: pva.Spec.TargetRef.Namespace,
					},
				}
				if err := r.Client.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
					return nil, fmt.Errorf("could nto retrieve pvc %s: %w", client.ObjectKeyFromObject(pvc), err)
				}
				pvcsToScale = append(pvcsToScale, pvc)
				// }
			}
		}
	}

	return pvcsToScale, nil
}

func ownerRefFromTargetRef(targetRef *corev1.ObjectReference) *metav1.OwnerReference {
	return &metav1.OwnerReference{
		APIVersion: targetRef.APIVersion,
		Kind:       targetRef.Kind,
		Name:       targetRef.Name,
	}
}

func (r *Reconciler) controllerForPodContainsTarget(ctx context.Context, log logr.Logger, pod *corev1.Pod, targetRef *metav1.OwnerReference) (bool, error) {
	log.Info("checking if owners of pod matches resize target", "pod", client.ObjectKeyFromObject(pod))

	var podControllerOwnerRef *metav1.OwnerReference
	for _, ownerRef := range pod.OwnerReferences {
		if ptr.Deref(ownerRef.Controller, false) {
			podControllerOwnerRef = &ownerRef
			break
		}
	}
	if podControllerOwnerRef == nil {
		log.Info("could not find owner controller for pod", "pod", client.ObjectKeyFromObject(pod))
		return false, nil
	}

	log.Info("found owner controller for pod", "pod", client.ObjectKeyFromObject(pod), "controller", podControllerOwnerRef.Name)

	for {
		if podControllerOwnerRef.Name == targetRef.Name &&
			podControllerOwnerRef.Kind == targetRef.Kind &&
			podControllerOwnerRef.APIVersion == targetRef.APIVersion {
			log.Info("owner controller for pod matches resize target", "pod", client.ObjectKeyFromObject(pod), "target", targetRef.Name)
			return true, nil
		}

		controllerFound := false

		metaData := &metav1.PartialObjectMetadata{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podControllerOwnerRef.Name,
				Namespace: pod.Namespace,
			},
		}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(metaData), metaData); err != nil {
			return false, fmt.Errorf("could not retrieve metadata for object %s: %w", client.ObjectKeyFromObject(metaData), err)
		}

		for _, ownerRef := range pod.OwnerReferences {
			if ptr.Deref(ownerRef.Controller, false) {
				controllerFound = true
				podControllerOwnerRef = &ownerRef
				break
			}
		}
		if controllerFound {
			log.Info("iterating to next controller for pod", "pod", client.ObjectKeyFromObject(pod), "target", podControllerOwnerRef.Name)
		}
		if !controllerFound {
			log.Info("we are currently at top most controller for pod", "pod", client.ObjectKeyFromObject(pod), "target", podControllerOwnerRef.Name)
			return false, nil
		}
	}
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
		var ordinalStart int32 = 0
		if sts.Spec.Ordinals != nil {
			ordinalStart = sts.Spec.Ordinals.Start
		}
		for ordinal := ordinalStart; ordinal < ordinalStart+ptr.Deref(sts.Spec.Replicas, 0); ordinal++ {
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

	if idx < 0 {
		pva.Status.PVCs = append(pva.Status.PVCs, v1alpha1.PersistentVolumeClaimStatus{
			Name: pvc.Name,
		})
		idx = len(pva.Status.PVCs) - 1
	}

	volumeMetrics := r.MetricsStorage.GetMetric(client.ObjectKeyFromObject(pvc))
	if volumeMetrics == nil {
		logger.Info("skipping persistentvolumeclaim", "reason", common.ErrNoMetrics.Error())
		return nil
	}

	r.updatePVAStatusForPVC(pva, idx, volumeMetrics)
	shouldReconcile, err := r.shouldReconcilePVC(ctx, pvc, pva, volumeMetrics)
	if err != nil {
		return fmt.Errorf("error while checking whether pvc %s should be reconciled: %w", client.ObjectKeyFromObject(pvc), err)
	}
	if !shouldReconcile {
		return nil
	}

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
func (r *Reconciler) shouldReconcilePVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim, pva *v1alpha1.PersistentVolumeAutoscaler, volInfo *source.VolumeInfo) (bool, error) {
	// Validate the PVC itself against the spec
	currStatusSize := pvc.Status.Capacity.Storage()
	if currStatusSize.IsZero() {
		return false, fmt.Errorf(".status.capacity.storage is invalid: %s", currStatusSize.String())
	}

	if pva.Spec.MaxCapacity.Value() < currStatusSize.Value() {
		return false, fmt.Errorf("max capacity (%s) cannot be less than current size (%s)", pva.Spec.MaxCapacity.String(), currStatusSize.String())
	}

	// We need a StorageClass with expansion support
	scName := ptr.Deref(pvc.Spec.StorageClassName, "")
	if scName == "" {
		return false, ErrStorageClassNotFound
	}

	var sc storagev1.StorageClass
	scKey := types.NamespacedName{Name: scName}
	if err := r.Client.Get(ctx, scKey, &sc); err != nil {
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

	threshold, err := utils.ParsePercentage(pva.Spec.Threshold)
	if err != nil {
		return false, fmt.Errorf("cannot parse threshold: %w", err)
	}

	// VolumeMode should be Filesystem
	if pvc.Spec.VolumeMode == nil {
		return false, nil
	}
	if *pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return false, ErrVolumeModeIsNotFilesystem
	}

	// The PVC should be bound
	if pvc.Status.Phase != corev1.ClaimBound {
		return false, nil
	}

	switch {
	// Free space reached threshold
	case freeSpace < threshold:
		r.EventRecorder.Eventf(
			pvc,
			corev1.EventTypeWarning,
			"FreeSpaceThresholdReached",
			"free space (%.2f%%) is less than the configured threshold (%.2f%%)",
			freeSpace,
			threshold,
		)
		metrics.ThresholdReachedTotal.WithLabelValues(pvc.Namespace, pvc.Name, "space").Inc()
		return true, nil

	// Free inodes reached threshold
	case volInfo.CapacityInodes > 0.0 && (freeInodes < threshold):
		r.EventRecorder.Eventf(
			pvc,
			corev1.EventTypeWarning,
			"FreeInodesThresholdReached",
			"free inodes (%.2f%%) are less than the configured threshold (%.2f%%)",
			freeInodes,
			threshold,
		)
		metrics.ThresholdReachedTotal.WithLabelValues(pvc.Namespace, pvc.Name, "inodes").Inc()
		return true, nil

	// No need to reconcile the PVC for now
	default:
		return false, nil
	}
}
