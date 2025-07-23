package pva

import (
	"time"

	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/metrics"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
)

type Reconciler struct {
	Client         client.Client
	ResyncPeriod   time.Duration
	EventRecorder  record.EventRecorder
	MetricsStorage *metrics.Storage
}

func (r *Reconciler) AddToManager(mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(common.ControllerName).
		Watches(&v1alpha1.PersistentVolumeAutoscaler{}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
