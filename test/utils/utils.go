// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
)

const (
	// StorageClassName is the name of the test storage class
	StorageClassName = "my-storage-class"

	// ProvisionerName is the name of the test storage class provisioner
	ProvisionerName = "my-provisioner"
)

// StorageClass is a test storage class
var StorageClass storagev1.StorageClass = storagev1.StorageClass{
	ObjectMeta: metav1.ObjectMeta{
		Name: StorageClassName,
	},
	Provisioner:          ProvisionerName,
	AllowVolumeExpansion: ptr.To(true),
	VolumeBindingMode:    ptr.To(storagev1.VolumeBindingImmediate),
	ReclaimPolicy:        ptr.To(corev1.PersistentVolumeReclaimDelete),
}

// CreatePVC is a helper function used to create a test PVC
func CreatePVC(ctx context.Context,
	k8sClient client.Client,
	name string,
	capacity string) (*corev1.PersistentVolumeClaim, error) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To(StorageClassName),
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(capacity),
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, pvc); err != nil {
		return nil, err
	}

	// Bind the PVC and update the status resources in order to make it look
	// a bit more like a "real" PVC.
	patch := client.MergeFrom(pvc.DeepCopy())
	pvc.Status = corev1.PersistentVolumeClaimStatus{
		Phase: corev1.ClaimBound,
		Capacity: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse(capacity),
		},
	}
	if err := k8sClient.Status().Patch(ctx, pvc, patch); err != nil {
		return nil, err
	}

	return pvc, nil
}

// CreateStatefulSet is a helper function used to create a test StatefulSet
func CreateStatefulSet(ctx context.Context,
	k8sClient client.Client,
	pod *corev1.Pod,
	pvc *corev1.PersistentVolumeClaim,
	name string,
	labels map[string]string) (*appsv1.StatefulSet, error) {
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: pod.ObjectMeta,
				Spec:       pod.Spec,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				Spec: pvc.Spec,
			}},
		},
	}

	if err := k8sClient.Create(ctx, statefulSet); err != nil {
		return nil, err
	}
	return statefulSet, nil
}

// CreatePod is a helper function used to create a test Pod
func CreatePod(ctx context.Context,
	k8sClient client.Client,
	pvc *corev1.PersistentVolumeClaim,
	name string,
	labels map[string]string) (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "test", Image: "test"},
			},
			Volumes: []corev1.Volume{
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvc.Name,
						},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, pod); err != nil {
		return nil, err
	}

	return pod, nil
}

// CreatePersistentVolumeClaimAutoscaler is a helper function used to create a
// test PVC Autoscaler resource.
func CreatePersistentVolumeClaimAutoscaler(ctx context.Context,
	k8sClient client.Client,
	name string,
	targetRef autoscalingv1.CrossVersionObjectReference,
	volumePolicies []v1alpha1.VolumePolicy) (*v1alpha1.PersistentVolumeClaimAutoscaler, error) {
	obj := &v1alpha1.PersistentVolumeClaimAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: v1alpha1.PersistentVolumeClaimAutoscalerSpec{
			VolumePolicies: volumePolicies,
			TargetRef:      targetRef,
		},
	}

	if err := k8sClient.Create(ctx, obj); err != nil {
		return nil, err
	}

	return obj, nil
}
