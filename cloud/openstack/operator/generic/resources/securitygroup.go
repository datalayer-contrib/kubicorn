// Copyright © 2017 The Kubicorn Authors
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

package resources

import (
	"fmt"
	"strconv"

	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/rules"
	"github.com/gophercloud/gophercloud/pagination"
	"github.com/kubicorn/kubicorn/apis/cluster"
	"github.com/kubicorn/kubicorn/cloud"
	"github.com/kubicorn/kubicorn/pkg/compare"
	"github.com/kubicorn/kubicorn/pkg/logger"
)

var _ cloud.Resource = &SecurityGroup{}

type SecurityGroup struct {
	Shared
	IngressRules []*SecurityGroupRule
	Firewall     *cluster.Firewall
	ServerPool   *cluster.ServerPool
}

type SecurityGroupRule struct {
	FromPort int
	ToPort   int
	IPPrefix string
	Protocol string
}

func (r *SecurityGroup) Actual(immutable *cluster.Cluster) (actual *cluster.Cluster, resource cloud.Resource, err error) {
	logger.Debug("secgroup.Actual")
	newResource := new(SecurityGroup)
	if r.Firewall.Identifier != "" {
		res := groups.List(Sdk.Network, groups.ListOpts{
			Name: r.Name,
		})
		if res.Err != nil {
			return nil, nil, err
		}
		err = res.EachPage(func(page pagination.Page) (bool, error) {
			list, err := groups.ExtractGroups(page)
			if err != nil {
				return false, err
			}
			if len(list) > 1 {
				return false, fmt.Errorf("Found more than one security group with name [%s]", newResource.Name)
			}
			if len(list) == 1 {
				newResource.Name = list[0].Name
				newResource.Identifier = list[0].ID
			}
			return false, nil
		})
		if err != nil {
			return nil, nil, err
		}
	}

	newResource.importFirewallRules(r.Firewall)
	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *SecurityGroup) Expected(immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("secgroup.Expected")
	newResource := &SecurityGroup{
		Shared: Shared{
			Name:       r.Name,
			Identifier: r.Firewall.Identifier,
		},
	}
	newResource.importFirewallRules(r.Firewall)
	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *SecurityGroup) Apply(actual, expected cloud.Resource, immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("secgroup.Apply")
	secgroup := expected.(*SecurityGroup)
	isEqual, err := compare.IsEqual(actual.(*SecurityGroup), expected.(*SecurityGroup))
	if err != nil {
		return nil, nil, err
	}
	if isEqual {
		return immutable, secgroup, nil
	}

	resGroup := groups.Create(Sdk.Network, groups.CreateOpts{
		Name: secgroup.Name,
	})
	outputGroup, err := resGroup.Extract()
	if err != nil {
		return nil, nil, err
	}

	// Ingress rules
	for _, rule := range secgroup.IngressRules {
		res := rules.Create(Sdk.Network, rules.CreateOpts{
			Direction:      rules.DirIngress,
			EtherType:      rules.EtherType4,
			SecGroupID:     outputGroup.ID,
			PortRangeMin:   rule.FromPort,
			PortRangeMax:   rule.ToPort,
			Protocol:       rules.ProtocolTCP,
			RemoteIPPrefix: rule.IPPrefix,
		})
		secRule, err := res.Extract()
		if err != nil {
			return nil, nil, res.Err
		}
		logger.Debug("Created SecurityGroup ingress rule [%s]", secRule.ID)
	}

	logger.Success("Created SecurityGroup [%s]", outputGroup.ID)

	newResource := &SecurityGroup{
		Shared: Shared{
			Name:       secgroup.Name,
			Identifier: outputGroup.ID,
		},
		IngressRules: secgroup.IngressRules,
	}
	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *SecurityGroup) Delete(actual cloud.Resource, immutable *cluster.Cluster) (*cluster.Cluster, cloud.Resource, error) {
	logger.Debug("secgroup.Delete")
	secgroup := actual.(*SecurityGroup)

	if secgroup.Identifier != "" {
		if res := groups.Delete(Sdk.Network, secgroup.Identifier); res.Err != nil {
			return nil, nil, res.Err
		}

		logger.Success("Deleted SecurityGroup [%s]", actual.(*SecurityGroup).Identifier)
	}

	newResource := &SecurityGroup{
		Shared: Shared{
			Name: secgroup.Name,
		},
		IngressRules: secgroup.IngressRules,
	}
	newCluster := r.immutableRender(newResource, immutable)
	return newCluster, newResource, nil
}

func (r *SecurityGroup) immutableRender(newResource cloud.Resource, inaccurateCluster *cluster.Cluster) *cluster.Cluster {
	logger.Debug("secgroup.Render")
	secgroup := &SecurityGroup{}
	secgroup.Name = newResource.(*SecurityGroup).Name
	secgroup.Identifier = newResource.(*SecurityGroup).Identifier
	secgroup.IngressRules = newResource.(*SecurityGroup).IngressRules
	newCluster := inaccurateCluster
	found := false

	var (
		ingressRules []*cluster.IngressRule
	)

	for _, ingressRule := range secgroup.IngressRules {
		ingressRules = append(ingressRules, &cluster.IngressRule{
			IngressSource:   ingressRule.IPPrefix,
			IngressFromPort: strconv.Itoa(ingressRule.FromPort),
			IngressToPort:   strconv.Itoa(ingressRule.ToPort),
			IngressProtocol: ingressRule.Protocol,
		})
	}
	machineProviderConfigs := newCluster.MachineProviderConfigs()
	for i := 0; i < len(machineProviderConfigs); i++ {
		machineProviderConfig := machineProviderConfigs[i]
		for j := 0; j < len(machineProviderConfig.ServerPool.Firewalls); j++ {
			if machineProviderConfig.ServerPool.Firewalls[j].Name == secgroup.Name {
				found = true
				machineProviderConfig.ServerPool.Firewalls[j].Identifier = secgroup.Identifier
				machineProviderConfig.ServerPool.Firewalls[j].IngressRules = ingressRules
				machineProviderConfigs[i] = machineProviderConfig
				newCluster.SetMachineProviderConfigs(machineProviderConfigs)
			}
		}
	}
	if !found {
		for i := 0; i < len(machineProviderConfigs); i++ {
			machineProviderConfig := machineProviderConfigs[i]
			if machineProviderConfig.Name == newResource.(*SecurityGroup).Name {
				found = true
				machineProviderConfig.ServerPool.Firewalls = append(newCluster.ServerPools()[i].Firewalls, &cluster.Firewall{
					Name:         secgroup.Name,
					Identifier:   secgroup.Identifier,
					IngressRules: ingressRules,
				})
				machineProviderConfigs[i] = machineProviderConfig
				newCluster.SetMachineProviderConfigs(machineProviderConfigs)
			}
		}
	}

	if !found {
		for i := 0; i < len(machineProviderConfigs); i++ {
			machineProviderConfig := machineProviderConfigs[i]
			if machineProviderConfig.Name == secgroup.Name {
				newCluster.ServerPools()[i].Firewalls = append(newCluster.ServerPools()[i].Firewalls, &cluster.Firewall{
					Name:         secgroup.Name,
					Identifier:   secgroup.Identifier,
					IngressRules: ingressRules,
				})
				machineProviderConfigs[i] = machineProviderConfig
				found = true
				newCluster.SetMachineProviderConfigs(machineProviderConfigs)
			}
		}
	}

	return newCluster
}

func (r *SecurityGroup) importFirewallRules(fw *cluster.Firewall) error {
	for _, rule := range fw.IngressRules {
		fromPort, err := strToInt(rule.IngressFromPort)
		if err != nil {
			return err
		}
		toPort, err := strToInt(rule.IngressToPort)
		if err != nil {
			return err
		}
		r.IngressRules = append(r.IngressRules, &SecurityGroupRule{
			FromPort: fromPort,
			ToPort:   toPort,
			IPPrefix: rule.IngressSource,
			Protocol: rule.IngressProtocol,
		})
	}
	return nil
}

func strToInt(str string) (int, error) {
	if str == "" {
		return 0, nil
	}
	return strconv.Atoi(str)
}
