// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package network provides controllers which manage network resources.
package network

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/xslices"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/pkg/machinery/resources/network"
)

// ResolverMergeController merges network.ResolverSpec in network.ConfigNamespace and produces final network.ResolverSpec in network.Namespace.
type ResolverMergeController struct{}

// Name implements controller.Controller interface.
func (ctrl *ResolverMergeController) Name() string {
	return "network.ResolverMergeController"
}

// Inputs implements controller.Controller interface.
func (ctrl *ResolverMergeController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: network.ConfigNamespaceName,
			Type:      network.ResolverSpecType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: network.NamespaceName,
			Type:      network.ResolverSpecType,
			Kind:      controller.InputDestroyReady,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *ResolverMergeController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: network.ResolverSpecType,
			Kind: controller.OutputShared,
		},
	}
}

// Run implements controller.Controller interface.
//
//nolint:gocyclo
func (ctrl *ResolverMergeController) Run(ctx context.Context, r controller.Runtime, _ *zap.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		// list source network configuration resources
		list, err := safe.ReaderList[*network.ResolverSpec](ctx, r, resource.NewMetadata(network.ConfigNamespaceName, network.ResolverSpecType, "", resource.VersionUndefined))
		if err != nil {
			return fmt.Errorf("error listing source network addresses: %w", err)
		}

		// sort by config layer
		list.SortFunc(func(l, r *network.ResolverSpec) int {
			return cmp.Compare(l.TypedSpec().ConfigLayer, r.TypedSpec().ConfigLayer)
		})

		// simply merge by layers, overriding with the next configuration layer
		var final network.ResolverSpecSpec

		for res := range list.All() {
			spec := res.TypedSpec()

			final.SearchDomains = slices.Insert(final.SearchDomains, 0, spec.SearchDomains...)

			if spec.ConfigLayer == final.ConfigLayer {
				// simply append server lists on the same layer
				final.DNSServers = append(final.DNSServers, spec.DNSServers...)
			} else {
				// otherwise, do a smart merge across IPv4/IPv6
				final.ConfigLayer = spec.ConfigLayer
				mergeDNSServers(&final.DNSServers, spec.DNSServers)
			}
		}

		if final.DNSServers != nil {
			if err = safe.WriterModify(ctx, r, network.NewResolverSpec(network.NamespaceName, network.ResolverID), func(spec *network.ResolverSpec) error {
				*spec.TypedSpec() = final

				return nil
			}); err != nil {
				if state.IsPhaseConflictError(err) {
					// conflict
					final.DNSServers = nil

					r.QueueReconcile()
				} else {
					return fmt.Errorf("error updating resource: %w", err)
				}
			}
		}

		if final.DNSServers == nil {
			// remove existing
			var okToDestroy bool

			md := resource.NewMetadata(network.NamespaceName, network.ResolverSpecType, network.ResolverID, resource.VersionUndefined)

			okToDestroy, err = r.Teardown(ctx, md)
			if err != nil && !state.IsNotFoundError(err) {
				return fmt.Errorf("error cleaning up specs: %w", err)
			}

			if okToDestroy {
				if err = r.Destroy(ctx, md); err != nil {
					return fmt.Errorf("error cleaning up specs: %w", err)
				}
			}
		}

		r.ResetRestartBackoff()
	}
}

func mergeDNSServers(dst *[]netip.Addr, src []netip.Addr) {
	if *dst == nil {
		*dst = src

		return
	}

	srcHasV4 := slices.IndexFunc(src, netip.Addr.Is4) != -1
	srcHasV6 := slices.IndexFunc(src, netip.Addr.Is6) != -1
	dstHasV4 := slices.IndexFunc(*dst, netip.Addr.Is4) != -1
	dstHasV6 := slices.IndexFunc(*dst, netip.Addr.Is6) != -1

	// if old set has IPv4, and new one doesn't, preserve IPv4
	// and same vice versa for IPv6
	switch {
	case dstHasV4 && !srcHasV4:
		*dst = slices.Concat(src, xslices.Filter(*dst, netip.Addr.Is4))
	case dstHasV6 && !srcHasV6:
		*dst = slices.Concat(src, xslices.Filter(*dst, netip.Addr.Is6))
	default:
		*dst = src
	}
}
