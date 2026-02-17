// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package target

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
)

// NewCachedDiscoveryClient creates a new cached discovery client from the given
// REST config.
func NewCachedDiscoveryClient(cfg *rest.Config) (discovery.CachedDiscoveryInterface, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}

	return memory.NewMemCacheClient(discoveryClient), nil
}

// NewRESTMapper creates a new REST mapper using the given cached discovery
// client. The mapper can be used to resolve GroupKind to GroupVersionResource.
func NewRESTMapper(cachedDiscoveryClient discovery.CachedDiscoveryInterface) (apimeta.RESTMapper, error) {
	groupResources, err := restmapper.GetAPIGroupResources(cachedDiscoveryClient)
	if err != nil {
		return nil, err
	}

	return restmapper.NewDiscoveryRESTMapper(groupResources), nil
}

// NewScaleClient creates a new scale client using the given REST config,
// REST mapper, and cached discovery client.
func NewScaleClient(cfg *rest.Config, mapper apimeta.RESTMapper, cachedDiscoveryClient discovery.CachedDiscoveryInterface) (scale.ScalesGetter, error) {
	scaleKindResolver := scale.NewDiscoveryScaleKindResolver(cachedDiscoveryClient)
	return scale.NewForConfig(cfg, mapper, dynamic.LegacyAPIPathResolverFunc, scaleKindResolver)
}

// NewScaleClientWithDiscovery creates a scale client along with its required
// cached discovery client and REST mapper from the given REST config.
// This is a convenience function that combines NewCachedDiscoveryClient,
// NewRESTMapper, and NewScaleClient.
func NewScaleClientWithDiscovery(cfg *rest.Config) (scale.ScalesGetter, apimeta.RESTMapper, error) {
	cachedDiscoveryClient, err := NewCachedDiscoveryClient(cfg)
	if err != nil {
		return nil, nil, err
	}

	restMapper, err := NewRESTMapper(cachedDiscoveryClient)
	if err != nil {
		return nil, nil, err
	}

	scaleClient, err := NewScaleClient(cfg, restMapper, cachedDiscoveryClient)
	if err != nil {
		return nil, nil, err
	}

	return scaleClient, restMapper, nil
}
