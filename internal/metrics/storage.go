package metrics

import (
	"sync"

	"github.com/gardener/pvc-autoscaler/internal/metrics/source"
	"k8s.io/apimachinery/pkg/types"
)

type Storage struct {
	MetricsMap source.Metrics
	lock       sync.RWMutex
}

func NewStorage() *Storage {
	return &Storage{
		MetricsMap: source.Metrics{},
	}
}

func (s *Storage) AddMetric(pvcName types.NamespacedName, volumeInfo *source.VolumeInfo) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.MetricsMap[pvcName] = volumeInfo
}

func (s *Storage) GetMetric(pvcName types.NamespacedName) *source.VolumeInfo {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.MetricsMap[pvcName]
}

func (s *Storage) SetMap(metricsMap source.Metrics) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.MetricsMap = metricsMap
}
