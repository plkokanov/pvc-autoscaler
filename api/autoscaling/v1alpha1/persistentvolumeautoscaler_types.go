// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PersistentVolumeAutoscalerSpec defines the desired state of
// PersistentVolumeAutoscaler
type PersistentVolumeAutoscalerSpec struct {
	// IncreaseBy specifies an increase by percentage value (e.g. 10%, 20%,
	// etc.) by which the Persistent Volume Claim storage will be resized.
	IncreaseBy string `json:"increaseBy,omitempty"`

	// Threshold specifies the threshold value in percentage (e.g. 10%, 20%,
	// etc.) for the PVC. Once the available capacity (free space) for the
	// PVC reaches or drops below the specified threshold this will trigger
	// a resize operation by the controller.
	Threshold string `json:"threshold,omitempty"`

	// MaxCapacity specifies the maximum capacity up to which a PVC is
	// allowed to be extended. The max capacity is specified as a
	// [k8s.io/apimachinery/pkg/api/resource.Quantity] value.
	MaxCapacity resource.Quantity `json:"maxCapacity,omitempty"`

	// TargetRef specifies the reference to the parent controller for which the PVCs will be
	// managed by pvc-autoscaler.
	TargetRef corev1.ObjectReference `json:"targetRef,omitempty"`

	// TargetRef specifies the reference to the parent controller for which the PVCs will be
	// managed by pvc-autoscaler.
	PVCResizePolicies []PVCResizePolicy `json:"pvcResizePolicies,omitempty"`
}

type PVCResizePolicy struct {
	// Name is the name of the PVC to resize.
	Name string `json:"name"`

	// NameTemplate is the name template of the PVC to resize.
	NameTemplate string `json:"nameTemplate"`

	// IncreaseBy specifies an increase by percentage value (e.g. 10%, 20%,
	// etc.) by which the Persistent Volume Claim storage will be resized.
	IncreaseBy string `json:"increaseBy,omitempty"`

	// Threshold specifies the threshold value in percentage (e.g. 10%, 20%,
	// etc.) for the PVC. Once the available capacity (free space) for the
	// PVC reaches or drops below the specified threshold this will trigger
	// a resize operation by the controller.
	Threshold string `json:"threshold,omitempty"`

	// MaxCapacity specifies the maximum capacity up to which a PVC is
	// allowed to be extended. The max capacity is specified as a
	// [k8s.io/apimachinery/pkg/api/resource.Quantity] value.
	MaxCapacity resource.Quantity `json:"maxCapacity,omitempty"`
}

type PersistentVolumeAutoscalerStatus struct {
	// PVCs specifies is the status of all PVCs autoscaled by this controller.
	PVCs []PersistentVolumeClaimStatus `json:"pvcs,omitempty"`
}

// PersistentVolumeClaimStatus defines the observed state of scaling a
// PersistentVolumeClaim
type PersistentVolumeClaimStatus struct {
	// Name is the name of the PVC
	// TODO: make required field
	Name string `json:"name,omitempty"`

	// LastCheck specifies the last time the PVC was checked by the controller.
	LastCheck metav1.Time `json:"lastCheck,omitempty"`

	// NextCheck specifies the next scheduled check of the PVC by the
	// controller.
	NextCheck metav1.Time `json:"nextCheck,omitempty"`

	// UsedSpacePercentage specifies the last observed used space of the PVC
	// as a percentage.
	UsedSpacePercentage string `json:"usedSpacePercentage,omitempty"`

	// FreeSpacePercentage specifies the last observed free space of the PVC
	// as a percentage.
	FreeSpacePercentage string `json:"freeSpacePercentage,omitempty"`

	// UsedInodesPercentage specifies the last observed used inodes of the
	// PVC as a percentage.
	UsedInodesPercentage string `json:"usedInodesPercentage,omitempty"`

	// FreeInodesPercentage specifies the last observed free inodes of the
	// PVC as a percentage.
	FreeInodesPercentage string `json:"freeInodesPercentage,omitempty"`

	// PrevSize specifies the previous .status.capacity.storage value of the
	// PVC, just before resizing it.
	PrevSize resource.Quantity `json:"prevSize,omitempty"`

	// NewSize specifies the new size to which the PVC will be resized.
	NewSize resource.Quantity `json:"newSize,omitempty"`

	// Conditions specifies the status conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pva
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.scaleTargetRef.name`
// +kubebuilder:printcolumn:name="Increase By",type=string,JSONPath=`.spec.increaseBy`
// +kubebuilder:printcolumn:name="Threshold",type=string,JSONPath=`.spec.threshold`
// +kubebuilder:printcolumn:name="Max Capacity",type=string,JSONPath=`.spec.maxCapacity`

// PersistentVolumeAutoscaler is the Schema for the
// persistentvolumeautoscalers API
type PersistentVolumeAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PersistentVolumeAutoscalerSpec   `json:"spec,omitempty"`
	Status PersistentVolumeAutoscalerStatus `json:"status,omitempty"`
}

// SetCondition sets the given [metav1.Condition] for the object.
func (obj *PersistentVolumeAutoscaler) SetConditionForPVC(condition metav1.Condition, pvcName string) {
	for _, pvcStatus := range obj.Status.PVCs {
		if pvcStatus.Name == pvcName {
			if len(pvcStatus.Conditions) == 0 {
				pvcStatus.Conditions = make([]metav1.Condition, 0)
			}
			meta.SetStatusCondition(&pvcStatus.Conditions, condition)
			break
		}
	}
}

// +kubebuilder:object:root=true

// PersistentVolumeAutoscalerList contains a list of PersistentVolumeAutoscaler
type PersistentVolumeAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PersistentVolumeAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PersistentVolumeAutoscaler{}, &PersistentVolumeAutoscalerList{})
}
