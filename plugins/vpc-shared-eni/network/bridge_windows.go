// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package network

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/aws/amazon-vpc-cni-plugins/network/vpc"

	"github.com/Microsoft/hcsshim"
	"github.com/Microsoft/hcsshim/hcn"
	log "github.com/cihub/seelog"
)

const (
	// hnsL2Bridge is the HNS network type used by this plugin on Windows.
	hnsL2Bridge = "l2bridge"

	// hnsNetworkNameFormat is the format used for generating bridge names (e.g. "vpcbr1").
	hnsNetworkNameFormat = "%sbr%s"

	// hnsEndpointNameFormat is the format of the names generated for HNS endpoints.
	hnsEndpointNameFormat = "cid-%s"
)

// nsType identifies the namespace type for the containers.
type nsType int

const (
	// infraContainerNS identifies an Infra container NS for networking setup.
	infraContainerNS nsType = iota
	// appContainerNS identifies sharing of infra container NS for networking setup.
	appContainerNS
	// hcnNamespace identifies HCN NS for networking setup.
	hcnNamespace
)

var (
	// hnsMinVersion is the minimum version of HNS supported by this plugin.
	hnsMinVersion = hcsshim.HNSVersion1803
)

// hnsRoutePolicy is an HNS route policy.
// This definition really needs to be in Microsoft's hcsshim package.
type hnsRoutePolicy struct {
	hcsshim.Policy
	DestinationPrefix string `json:"DestinationPrefix,omitempty"`
	NeedEncap         bool   `json:"NeedEncap,omitempty"`
}

// BridgeBuilder implements NetworkBuilder interface by bridging containers to an ENI on Windows.
type BridgeBuilder struct{}

// FindOrCreateNetwork creates a new HNS network.
func (nb *BridgeBuilder) FindOrCreateNetwork(nw *Network) error {
	// Check that the HNS version is supported.
	err := nb.checkHNSVersion()
	if err != nil {
		return err
	}

	// HNS API does not support creating virtual switches in compartments other than the host's.
	if nw.BridgeNetNSPath != "" {
		return fmt.Errorf("Bridge must be in host network namespace on Windows")
	}

	// Check if the network already exists.
	networkName := nb.generateHNSNetworkName(nw)
	hnsNetwork, err := hcsshim.GetHNSNetworkByName(networkName)
	if err == nil {
		log.Infof("Found existing HNS network %s.", networkName)
		return nil
	}

	// Initialize the HNS network.
	hnsNetwork = &hcsshim.HNSNetwork{
		Name:               networkName,
		Type:               hnsL2Bridge,
		NetworkAdapterName: nw.SharedENI.GetLinkName(),

		Subnets: []hcsshim.Subnet{
			{
				AddressPrefix:  vpc.GetSubnetPrefix(&nw.ENIIPAddresses[0]).String(),
				GatewayAddress: nw.GatewayIPAddress.String(),
			},
		},
	}

	buf, err := json.Marshal(hnsNetwork)
	if err != nil {
		return err
	}
	hnsRequest := string(buf)

	// Create the HNS network.
	log.Infof("Creating HNS network: %+v", hnsRequest)
	hnsResponse, err := hcsshim.HNSNetworkRequest("POST", "", hnsRequest)
	if err != nil {
		log.Errorf("Failed to create HNS network: %v.", err)
		return err
	}

	log.Infof("Received HNS network response: %+v.", hnsResponse)

	return nil
}

// DeleteNetwork deletes an existing HNS network.
func (nb *BridgeBuilder) DeleteNetwork(nw *Network) error {
	// Find the HNS network ID.
	networkName := nb.generateHNSNetworkName(nw)
	hnsNetwork, err := hcsshim.GetHNSNetworkByName(networkName)
	if err != nil {
		return err
	}

	// Delete the HNS network.
	log.Infof("Deleting HNS network name: %s ID: %s", networkName, hnsNetwork.Id)
	_, err = hcsshim.HNSNetworkRequest("DELETE", hnsNetwork.Id, "")
	if err != nil {
		log.Errorf("Failed to delete HNS network: %v.", err)
	}

	return err
}

// FindOrCreateEndpoint creates a new HNS endpoint in the network.
func (nb *BridgeBuilder) FindOrCreateEndpoint(nw *Network, ep *Endpoint) error {
	// This plugin does not yet support IPv6, or multiple IPv4 addresses.
	if len(ep.IPAddresses) > 1 || ep.IPAddresses[0].IP.To4() == nil {
		return fmt.Errorf("Only a single IPv4 address per endpoint is supported on Windows")
	}

	// Query the namespace identifier.
	nsType, namespaceIdentifier := nb.getNamespaceIdentifier(ep)

	// Check if the endpoint already exists.
	endpointName := nb.generateHNSEndpointName(ep, namespaceIdentifier)
	hnsEndpoint, err := hcsshim.GetHNSEndpointByName(endpointName)
	if err == nil {
		log.Infof("Found existing HNS endpoint %s.", endpointName)
		if nsType == infraContainerNS || nsType == hcnNamespace {
			// This is a benign duplicate create call for an existing endpoint.
			// The endpoint was already attached in a previous call. Ignore and return success.
			log.Infof("HNS endpoint %s is already attached to container ID %s.",
				endpointName, ep.ContainerID)
		} else {
			// Attach the existing endpoint to the container's network namespace.
			// Attachment of endpoint to each container would occur only when using HNS V1 APIs.
			err = nb.attachEndpointV1(hnsEndpoint, ep.ContainerID)
		}

		ep.MACAddress, _ = net.ParseMAC(hnsEndpoint.MacAddress)
		return err
	} else {
		if nsType != infraContainerNS && nsType != hcnNamespace {
			// The endpoint referenced in the container netns does not exist.
			log.Errorf("Failed to find endpoint %s for container %s.", endpointName, ep.ContainerID)
			return fmt.Errorf("failed to find endpoint %s: %v", endpointName, err)
		}
	}

	// Initialize the HNS endpoint.
	hnsEndpoint = &hcsshim.HNSEndpoint{
		Name:               endpointName,
		VirtualNetworkName: nb.generateHNSNetworkName(nw),
		DNSSuffix:          strings.Join(nw.DNSSuffixSearchList, ","),
		DNSServerList:      strings.Join(nw.DNSServers, ","),
	}

	// Set the endpoint IP address.
	hnsEndpoint.IPAddress = ep.IPAddresses[0].IP
	pl, _ := ep.IPAddresses[0].Mask.Size()
	hnsEndpoint.PrefixLength = uint8(pl)

	// SNAT endpoint traffic to ENI primary IP address...
	var snatExceptions []string
	if nw.VPCCIDRs == nil {
		// ...except if the destination is in the same subnet as the ENI.
		snatExceptions = []string{vpc.GetSubnetPrefix(&nw.ENIIPAddresses[0]).String()}
	} else {
		// ...or, if known, the same VPC.
		for _, cidr := range nw.VPCCIDRs {
			snatExceptions = append(snatExceptions, cidr.String())
		}
	}
	if nw.ServiceCIDR != "" {
		// ...or the destination is a service endpoint.
		snatExceptions = append(snatExceptions, nw.ServiceCIDR)
	}

	err = nb.addEndpointPolicy(
		hnsEndpoint,
		hcsshim.OutboundNatPolicy{
			Policy: hcsshim.Policy{Type: hcsshim.OutboundNat},
			// Implicit VIP: nw.ENIIPAddresses[0].IP.String(),
			Exceptions: snatExceptions,
		})
	if err != nil {
		log.Errorf("Failed to add endpoint SNAT policy: %v.", err)
		return err
	}

	// Route traffic sent to service endpoints to the host. The load balancer running
	// in the host network namespace then forwards traffic to its final destination.
	if nw.ServiceCIDR != "" {
		// Set route policy for service subnet.
		// NextHop is implicitly the host.
		err = nb.addEndpointPolicy(
			hnsEndpoint,
			hnsRoutePolicy{
				Policy:            hcsshim.Policy{Type: hcsshim.Route},
				DestinationPrefix: nw.ServiceCIDR,
				NeedEncap:         true,
			})
		if err != nil {
			log.Errorf("Failed to add endpoint route policy for service subnet: %v.", err)
			return err
		}

		// Set route policy for host primary IP address.
		err = nb.addEndpointPolicy(
			hnsEndpoint,
			hnsRoutePolicy{
				Policy:            hcsshim.Policy{Type: hcsshim.Route},
				DestinationPrefix: nw.ENIIPAddresses[0].IP.String() + "/32",
				NeedEncap:         true,
			})
		if err != nil {
			log.Errorf("Failed to add endpoint route policy for host: %v.", err)
			return err
		}
	}

	// Encode the endpoint request.
	buf, err := json.Marshal(hnsEndpoint)
	if err != nil {
		return err
	}
	hnsRequest := string(buf)

	// Create the HNS endpoint.
	log.Infof("Creating HNS endpoint: %+v", hnsRequest)
	hnsResponse, err := hcsshim.HNSEndpointRequest("POST", "", hnsRequest)
	if err != nil {
		log.Errorf("Failed to create HNS endpoint: %v.", err)
		return err
	}

	log.Infof("Received HNS endpoint response: %+v.", hnsResponse)

	// Attach the HNS endpoint to the container's network namespace.
	if nsType == infraContainerNS {
		err = nb.attachEndpointV1(hnsResponse, ep.ContainerID)
	}
	if nsType == hcnNamespace {
		err = nb.attachEndpointV2(hnsResponse, namespaceIdentifier)
	}
	if err != nil {
		// Cleanup the failed endpoint.
		log.Infof("Deleting the failed HNS endpoint %s.", hnsResponse.Id)
		_, delErr := hcsshim.HNSEndpointRequest("DELETE", hnsResponse.Id, "")
		if delErr != nil {
			log.Errorf("Failed to delete HNS endpoint: %v.", delErr)
		}

		return err
	}

	// Return network interface MAC address.
	ep.MACAddress, _ = net.ParseMAC(hnsResponse.MacAddress)

	return nil
}

// DeleteEndpoint deletes an existing HNS endpoint.
func (nb *BridgeBuilder) DeleteEndpoint(nw *Network, ep *Endpoint) error {
	// Query the namespace identifier.
	nsType, namespaceIdentifier := nb.getNamespaceIdentifier(ep)

	// Find the HNS endpoint ID.
	endpointName := nb.generateHNSEndpointName(ep, namespaceIdentifier)
	hnsEndpoint, err := hcsshim.GetHNSEndpointByName(endpointName)
	if err != nil {
		return err
	}

	// Detach the HNS endpoint from the container's network namespace.
	log.Infof("Detaching HNS endpoint %s from container %s netns.", hnsEndpoint.Id, ep.ContainerID)
	if nsType == hcnNamespace {
		// Detach the HNS endpoint from the namespace, if we can.
		// HCN Namespace and HNS Endpoint have a 1-1 relationship, therefore,
		// even if detachment of endpoint from namespace fails, we can still proceed to delete it.
		err = hcn.RemoveNamespaceEndpoint(namespaceIdentifier, hnsEndpoint.Id)
		if err != nil {
			log.Errorf("Failed to detach endpoint, ignoring: %v", err)
		}
	} else {
		err = hcsshim.HotDetachEndpoint(ep.ContainerID, hnsEndpoint.Id)
		if err != nil && err != hcsshim.ErrComputeSystemDoesNotExist {
			return err
		}

		// The rest of the delete logic applies to infrastructure container only.
		if nsType == appContainerNS {
			// For non-infra containers, the network must not be deleted.
			return nil
		}
	}

	// Delete the HNS endpoint.
	log.Infof("Deleting HNS endpoint name: %s ID: %s", endpointName, hnsEndpoint.Id)
	_, err = hcsshim.HNSEndpointRequest("DELETE", hnsEndpoint.Id, "")
	if err != nil {
		log.Errorf("Failed to delete HNS endpoint: %v.", err)
	}

	return err
}

// attachEndpointV1 attaches an HNS endpoint to a container's network namespace using HNS V1 APIs.
func (nb *BridgeBuilder) attachEndpointV1(ep *hcsshim.HNSEndpoint, containerID string) error {
	log.Infof("Attaching HNS endpoint %s to container %s.", ep.Id, containerID)
	err := hcsshim.HotAttachEndpoint(containerID, ep.Id)
	if err != nil {
		// Attach can fail if the container is no longer running and/or its network namespace
		// has been cleaned up.
		log.Errorf("Failed to attach HNS endpoint %s: %v.", ep.Id, err)
	}

	return err
}

// attachEndpointV2 attaches an HNS endpoint to a network namespace using HNS V2 APIs.
func (nb *BridgeBuilder) attachEndpointV2(ep *hcsshim.HNSEndpoint, netNSName string) error {
	log.Infof("Adding HNS endpoint %s to ns %s.", ep.Id, netNSName)

	// Check if endpoint is already in target namespace.
	nsEndpoints, err := hcn.GetNamespaceEndpointIds(netNSName)
	if err != nil {
		log.Errorf("Failed to get endpoints from namespace %s: %v.", netNSName, err)
		return err
	}
	for _, endpointID := range nsEndpoints {
		if ep.Id == endpointID {
			log.Infof("HNS endpoint %s is already in ns %s.", endpointID, netNSName)
			return nil
		}
	}

	// Add the endpoint to the target namespace.
	err = hcn.AddNamespaceEndpoint(netNSName, ep.Id)
	if err != nil {
		log.Errorf("Failed to attach HNS endpoint %s: %v.", ep.Id, err)
	}

	return err
}

// addEndpointPolicy adds a policy to an HNS endpoint.
func (nb *BridgeBuilder) addEndpointPolicy(ep *hcsshim.HNSEndpoint, policy interface{}) error {
	buf, err := json.Marshal(policy)
	if err != nil {
		log.Errorf("Failed to encode policy: %v.", err)
		return err
	}

	ep.Policies = append(ep.Policies, buf)

	return nil
}

// getNamespaceIdentifier identifies the namespace type and returns the appropriate identifier.
func (nb *BridgeBuilder) getNamespaceIdentifier(ep *Endpoint) (nsType, string) {
	// Orchestrators like Kubernetes and ECS group a set of containers into deployment units called
	// pods or tasks. The orchestrator agent injects a special container called infrastructure
	// (a.k.a. pause) container into each group to create and share namespaces with the other
	// containers in the same group.
	//
	// Normally, the CNI plugin is called only once, for the infrastructure container. It does not
	// need to know about infrastructure containers and is not even aware of the other containers
	// in the group. However, on older versions of Kubernetes and Windows (pre-1809), CNI plugin is
	// called for each container in the pod separately so that the plugin can attach the endpoint
	// to each container. The logic below is necessary to detect infrastructure containers and
	// maintain compatibility with those older versions.

	const containerPrefix string = "container:"
	var netNSType nsType
	var namespaceIdentifier string

	if ep.NetNSName == "none" || ep.NetNSName == "" {
		// This is the first, i.e. infrastructure, container in the group.
		// The namespace identifier for such containers would be their container ID.
		netNSType = infraContainerNS
		namespaceIdentifier = ep.ContainerID
	} else if strings.HasPrefix(ep.NetNSName, containerPrefix) {
		// This is a workload container sharing the netns of a previously created infra container.
		// The namespace identifier for such containers would be the infra container's ID.
		netNSType = appContainerNS
		namespaceIdentifier = strings.TrimPrefix(ep.NetNSName, containerPrefix)
		log.Infof("Container %s shares netns of container %s.", ep.ContainerID, namespaceIdentifier)
	} else {
		// This plugin invocation does not need an infra container and uses an existing HCN Namespace.
		// The namespace identifier would be the HCN Namespace id.
		netNSType = hcnNamespace
		namespaceIdentifier = ep.NetNSName
		log.Infof("Container %s is in network namespace %s.", ep.ContainerID, namespaceIdentifier)
	}

	return netNSType, namespaceIdentifier
}

// checkHNSVersion returns whether the Windows Host Networking Service version is supported.
func (nb *BridgeBuilder) checkHNSVersion() error {
	hnsGlobals, err := hcsshim.GetHNSGlobals()
	if err != nil {
		return err
	}

	hnsVersion := hnsGlobals.Version
	log.Infof("Running on HNS version: %+v", hnsVersion)

	supported := hnsVersion.Major > hnsMinVersion.Major ||
		(hnsVersion.Major == hnsMinVersion.Major && hnsVersion.Minor >= hnsMinVersion.Minor)

	if !supported {
		return fmt.Errorf("HNS is older than the minimum supported version %v", hnsMinVersion)
	}

	return nil
}

// generateHNSNetworkName generates a deterministic unique name for an HNS network.
func (nb *BridgeBuilder) generateHNSNetworkName(nw *Network) string {
	// Use the MAC address of the shared ENI as the deterministic unique identifier.
	id := strings.Replace(nw.SharedENI.GetMACAddress().String(), ":", "", -1)
	return fmt.Sprintf(hnsNetworkNameFormat, nw.Name, id)
}

// generateHNSEndpointName generates a deterministic unique name for an HNS endpoint.
func (nb *BridgeBuilder) generateHNSEndpointName(ep *Endpoint, id string) string {
	// Use the given optional identifier or the container ID itself as the unique identifier.
	if id == "" {
		id = ep.ContainerID
	}

	return fmt.Sprintf(hnsEndpointNameFormat, id)
}
