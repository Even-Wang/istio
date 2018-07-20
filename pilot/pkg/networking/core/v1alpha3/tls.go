// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha3

import (
	"istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	istio_route "istio.io/istio/pilot/pkg/networking/core/v1alpha3/route"
	"istio.io/istio/pkg/log"
)

// Match by source labels, the listener port where traffic comes in, the gateway on which the rule is being
// bound, etc. All these can be checked statically, since we are generating the configuration for a proxy
// with predefined labels, on a specific port.
func matchTLS(match *v1alpha3.TLSMatchAttributes, proxyLabels model.LabelsCollection, gateways map[string]bool, port int) bool {
	if match == nil {
		return true
	}

	gatewayMatch := len(match.Gateways) == 0
	for _, gateway := range match.Gateways {
		gatewayMatch = gatewayMatch || gateways[gateway]
	}

	labelMatch := proxyLabels.IsSupersetOf(model.Labels(match.SourceLabels))

	portMatch := match.Port == 0 || match.Port == uint32(port)

	return gatewayMatch && labelMatch && portMatch
}

// Match by source labels, the listener port where traffic comes in, the gateway on which the rule is being
// bound, etc. All these can be checked statically, since we are generating the configuration for a proxy
// with predefined labels, on a specific port.
func matchTCP(match *v1alpha3.L4MatchAttributes, proxyLabels model.LabelsCollection, gateways map[string]bool, port int) bool {
	if match == nil {
		return true
	}

	gatewayMatch := len(match.Gateways) == 0
	for _, gateway := range match.Gateways {
		gatewayMatch = gatewayMatch || gateways[gateway]
	}

	labelMatch := proxyLabels.IsSupersetOf(model.Labels(match.SourceLabels))

	portMatch := match.Port == 0 || match.Port == uint32(port)

	return gatewayMatch && labelMatch && portMatch
}

// Select the virtual service pertaining to the service being processed.
func getVirtualServiceForHost(host model.Hostname, configs []model.Config) *v1alpha3.VirtualService {
	for _, config := range configs {
		virtualService := config.Spec.(*v1alpha3.VirtualService)
		for _, vsHost := range virtualService.Hosts {
			if model.Hostname(vsHost).Matches(host) {
				return virtualService
			}
		}
	}
	return nil
}

func buildOutboundTCPFilterChainOpts(node *model.Proxy, env *model.Environment, configs []model.Config, destinationIPAddress string,
	service *model.Service, listenPort *model.Port, proxyLabels model.LabelsCollection, gateways map[string]bool) []*filterChainOpts {

	out := make([]*filterChainOpts, 0)
	defaultRouteAdded := false
	virtualService := getVirtualServiceForHost(service.Hostname, configs)
	// Ports marked as TLS will have SNI routing if and only if they have an accompanying
	// virtual service for the same host, and the said virtual service has a TLS route block.
	// Otherwise we treat ports marked as TLS as opaque TCP services, subject to same port
	// collision handling.
	if virtualService != nil {
		for _, tls := range virtualService.Tls {
			// since we don't support weighted destinations yet there can only be exactly 1 destination
			dest := tls.Route[0].Destination
			destSvc, err := env.GetService(model.Hostname(dest.Host))
			if err != nil {
				log.Debugf("failed to retrieve service for destination %q: %v", service.Hostname, err)
				continue
			}
			clusterName := istio_route.GetDestinationCluster(dest, destSvc, listenPort.Port)
			for _, match := range tls.Match {
				if matchTLS(match, proxyLabels, gateways, listenPort.Port) {
					// Use the service's virtual address first.
					// But if a virtual service overrides it with its own destination subnet match
					// give preference to the user provided one
					destinationCIDRs := []string{destinationIPAddress}
					if len(match.DestinationSubnets) > 0 {
						destinationCIDRs = match.DestinationSubnets
					}
					out = append(out, &filterChainOpts{
						sniHosts:         match.SniHosts,
						destinationCIDRs: destinationCIDRs,
						networkFilters:   buildOutboundNetworkFilters(node, clusterName, destinationIPAddress, listenPort),
					})
				}
			}
		}

		// very basic TCP (no L4 matching)
		// break as soon as we add one network filter with no destination addresses to match
		// This is the terminating condition in the filter chain match list
		// TODO: rbac
	TcpLoop:
		for _, tcp := range virtualService.Tcp {
			// since we don't support weighted destinations yet there can only be exactly 1 destination
			dest := tcp.Route[0].Destination
			destSvc, err := env.GetService(model.Hostname(dest.Host))
			if err != nil {
				log.Debugf("failed to retrieve service for destination %q: %v", service.Hostname, err)
				continue
			}
			clusterName := istio_route.GetDestinationCluster(dest, destSvc, listenPort.Port)
			destinationCIDRs := []string{destinationIPAddress}

			if len(tcp.Match) == 0 { // implicit match
				out = append(out, &filterChainOpts{
					destinationCIDRs: destinationCIDRs,
					networkFilters:   buildOutboundNetworkFilters(node, clusterName, destinationIPAddress, listenPort),
				})
				defaultRouteAdded = true
				break TcpLoop
			}

			// Use the service's virtual address first.
			// But if a virtual service overrides it with its own destination subnet match
			// give preference to the user provided one
			virtualServiceDestinationSubnets := make([]string, 0)

			for _, match := range tcp.Match {
				if matchTCP(match, proxyLabels, gateways, listenPort.Port) {
					// Scan all the match blocks
					// if we find any match block without a runtime destination subnet match
					// i.e. match any destination address, then we treat it as the terminal match/catch all match
					// and break out of the loop.
					// But if we find only runtime destination subnet matches in all match blocks, collect them
					// (this is similar to virtual hosts in http) and create filter chain match accordingly.
					if len(match.DestinationSubnets) == 0 {
						out = append(out, &filterChainOpts{
							destinationCIDRs: destinationCIDRs,
							networkFilters:   buildOutboundNetworkFilters(node, clusterName, destinationIPAddress, listenPort),
						})
						defaultRouteAdded = true
						break TcpLoop
					} else {
						virtualServiceDestinationSubnets = append(virtualServiceDestinationSubnets, match.DestinationSubnets...)
					}
				}
			}

			if len(virtualServiceDestinationSubnets) > 0 {
				out = append(out, &filterChainOpts{
					destinationCIDRs: virtualServiceDestinationSubnets,
					networkFilters:   buildOutboundNetworkFilters(node, clusterName, "", listenPort),
				})
			}
		}
	}

	// Add a default TCP route
	if !defaultRouteAdded {
		clusterName := model.BuildSubsetKey(model.TrafficDirectionOutbound, "", service.Hostname, int(listenPort.Port))
		out = append(out, &filterChainOpts{
			destinationCIDRs: []string{destinationIPAddress},
			networkFilters:   buildOutboundNetworkFilters(node, clusterName, destinationIPAddress, listenPort),
		})
	}

	return out
}
