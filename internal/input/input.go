// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package input

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/metrics"
	metricssource "github.com/gardener/pvc-autoscaler/internal/metrics/source"
)

// UnknownUtilizationValue is the value which will be used when the free
// space/inodes utilization is unknown.
const UnknownUtilizationValue = "unknown"

// ErrNoMetricsSource is returned when the [Runner] is configured without a
// metrics source.
var ErrNoMetricsSource = errors.New("no metrics source provided")

// ErrNoMetricsStorage is returned when the [Runner] is configured without a
// metrics source.
var ErrNoMetricsStorage = errors.New("no metrics source provided")

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

// Runner is a [sigs.k8s.io/controller-runtime/pkg/manager.Runnable], which
// enqueues [v1alpha1.PersistentVolumeClaimAutoscaler] items for reconciling on
// regular basis.
type Runner struct {
	interval      time.Duration
	metricsSource metricssource.Source
	storage       *metrics.Storage
}

var _ manager.Runnable = &Runner{}

// Option is a function which configures the [Runner].
type Option func(r *Runner)

// New creates a new [Runner] with the given options.
func New(opts ...Option) (*Runner, error) {
	r := &Runner{}
	for _, opt := range opts {
		opt(r)
	}

	if r.metricsSource == nil {
		return nil, ErrNoMetricsSource
	}

	if r.storage == nil {
		return nil, ErrNoMetricsStorage
	}

	return r, nil
}

// WithMetricsStorage configures the [Runner] with the given client.
func WithMetricsStorage(s *metrics.Storage) Option {
	opt := func(r *Runner) {
		r.storage = s
	}

	return opt
}

// WithInterval configures the [Runner] with the given interval.
func WithInterval(interval time.Duration) Option {
	opt := func(r *Runner) {
		r.interval = interval
	}

	return opt
}

// WithMetricsSource configures the [Runner] to use the given source of metrics.
func WithMetricsSource(src metricssource.Source) Option {
	opt := func(r *Runner) {
		r.metricsSource = src
	}

	return opt
}

// Start implements the
// [sigs.k8s.io/controller-runtime/pkg/manager.Runnable] interface.
func (r *Runner) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	logger := log.FromContext(ctx, "controller", common.ControllerName)
	defer ticker.Stop()

	if err := r.loadMetrics(ctx); err != nil {
		logger.Error(err, "failed to load metrics")
	}

	for {
		select {
		case <-ticker.C:
			if err := r.loadMetrics(ctx); err != nil {
				logger.Error(err, "failed to load metrics")
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// enqueueObjects enqueues the [v1alpha1.PersitentVolumeClaimAutoscaler]
// resources for reconciliation.
func (r *Runner) loadMetrics(ctx context.Context) error {
	metricsData, err := r.metricsSource.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get metrics: %w", err)
	}

	logger := log.FromContext(ctx, "controller", common.ControllerName)
	logger.Info("loading metrics into storage")

	r.storage.SetMap(metricsData)
	return nil
}
