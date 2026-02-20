// Copyright (c) 2026 Tigera, Inc. All rights reserved.
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

// This file provides utilities for creating KubeVirt clients.
package kubevirt

import (
	"fmt"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"kubevirt.io/client-go/kubecli"
)

// GetVirtClientFromConfig creates a KubeVirt client from a clientcmd.ClientConfig
func GetVirtClientFromConfig(clientConfig clientcmd.ClientConfig) (kubecli.KubevirtClient, error) {
	virtClient, err := kubecli.GetKubevirtClientFromClientConfig(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("cannot obtain KubeVirt client: %w", err)
	}
	return virtClient, nil
}

// GetVirtClientFromRestConfig creates a KubeVirt client from a rest.Config
func GetVirtClientFromRestConfig(restConfig *rest.Config) (kubecli.KubevirtClient, error) {
	virtClient, err := kubecli.GetKubevirtClientFromRESTConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("cannot obtain KubeVirt client: %w", err)
	}
	return virtClient, nil
}

func IsKubeVirtInstalled(discoveryClient discovery.DiscoveryInterface) (bool, error) {
	apiGroupList, err := discoveryClient.ServerGroups()
	if err != nil {
		log.Debugf("Cannot obtain API group list: %s", err)
		return false, err
	}

	for _, group := range apiGroupList.Groups {
		if group.Name == "kubevirt.io" {
			return true, nil
		}
	}

	log.Debugf("Kubevirt is not installed in the cluster")
	return false, nil
}

// virtClientAdapter adapts the real kubecli.KubevirtClient to our VirtClientInterface.
type virtClientAdapter struct {
	client kubecli.KubevirtClient
}

// NewVirtClientAdapter wraps a real KubeVirt client with our interface.
func NewVirtClientAdapter(client kubecli.KubevirtClient) VirtClientInterface {
	return &virtClientAdapter{client: client}
}

// VirtualMachineInstance implements VirtClientInterface.
func (v *virtClientAdapter) VirtualMachineInstance(namespace string) VMIInterface {
	return v.client.VirtualMachineInstance(namespace)
}

// VirtualMachine implements VirtClientInterface.
func (v *virtClientAdapter) VirtualMachine(namespace string) VMInterface {
	return v.client.VirtualMachine(namespace)
}

// VirtualMachineInstanceMigration implements VirtClientInterface.
func (v *virtClientAdapter) VirtualMachineInstanceMigration(namespace string) VMIMInterface {
	return v.client.VirtualMachineInstanceMigration(namespace)
}

// tryCreateVirtClient attempts to create a KubeVirt client.
// Returns nil if KubeVirt is not available.
func TryCreateVirtClient(restConfig *rest.Config) (VirtClientInterface, error) {
	if restConfig == nil {
		log.Debug("No REST config provided.")
		return nil, fmt.Errorf("no REST config provided")
	}

	// Check if KubeVirt API group is available before attempting to create the client
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	isKubevirtInstalled, err := IsKubeVirtInstalled(discoveryClient)
	if err != nil {
		log.Debugf("failed to detect kubevirt installation: %w", err)
	} else if !isKubevirtInstalled {
		// KubeVirt is not installed, so we return nil without an error.
		return nil, nil
	}

	// Attempt to create a KubeVirt client from the REST config
	virtClient, err := GetVirtClientFromRestConfig(restConfig)
	if err != nil {
		return nil, err
	}

	// Wrap the client with our interface adapter
	log.Info("Successfully created KubeVirt client")
	return NewVirtClientAdapter(virtClient), nil
}
