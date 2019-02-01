/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package strategies

import (
	"strings"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"

	"github.com/kubernetes-incubator/descheduler/cmd/descheduler/app/options"
	"github.com/kubernetes-incubator/descheduler/pkg/api"
	"github.com/kubernetes-incubator/descheduler/pkg/descheduler/evictions"
	nodeutil "github.com/kubernetes-incubator/descheduler/pkg/descheduler/node"
	podutil "github.com/kubernetes-incubator/descheduler/pkg/descheduler/pod"
)

//type creator string
type DuplicatePodsMap map[string][]*v1.Pod

// RemoveDuplicatePods removes the duplicate pods on node. This strategy evicts all duplicate pods on node.
// A pod is said to be a duplicate of other if both of them are from same creator, kind and are within the same
// namespace. As of now, this strategy won't evict daemonsets, mirror pods, critical pods and pods with local storages.
func RemoveDuplicatePods(ds *options.DeschedulerServer, strategy api.DeschedulerStrategy, policyGroupVersion string, nodes []*v1.Node, nodepodCount nodePodEvictedCount) {
	if !strategy.Enabled {
		return
	}
	deleteDuplicatePods(ds.Client, policyGroupVersion, nodes, ds.DryRun, nodepodCount, ds.MaxNoOfPodsToEvictPerNode)
}

// deleteDuplicatePods evicts the pod from node and returns the count of evicted pods.
func deleteDuplicatePods(client clientset.Interface, policyGroupVersion string, nodes []*v1.Node, dryRun bool, nodepodCount nodePodEvictedCount, maxPodsToEvict int) int {
	podsEvicted := 0
	dpmByNode, creatorIsSaturated := computeCreatorSaturation(client, nodes)
	for _, node := range nodes {
		glog.V(1).Infof("Processing node: %#v", node.Name)
		dpm := dpmByNode[node]
		for creator, pods := range dpm {
			if len(pods) > 1 && !creatorIsSaturated[creator] {
				glog.V(1).Infof("%#v", creator)
				// i = 0 does not evict the first pod
				for i := 1; i < len(pods); i++ {
					if maxPodsToEvict > 0 && nodepodCount[node]+1 > maxPodsToEvict {
						break
					}
					success, err := evictions.EvictPod(client, pods[i], policyGroupVersion, dryRun)
					if !success {
						glog.Infof("Error when evicting pod: %#v (%#v)", pods[i].Name, err)
					} else {
						nodepodCount[node]++
						glog.V(1).Infof("Evicted pod: %#v (%#v)", pods[i].Name, err)
					}
				}
			}
		}
		podsEvicted += nodepodCount[node]
	}
	return podsEvicted
}

// computeCreatorSaturation finds if creators in the cluster are _saturated_.
// A creator is _saturated_ if atleast one pod is running on every possible nodes.
// In such a case, pods of this creator are not evicted from any nodes even if duplicates are present.
func computeCreatorSaturation(client clientset.Interface, nodes []*v1.Node) (map[*v1.Node]DuplicatePodsMap, map[string]bool) {
	dpmByNode := make(map[*v1.Node]DuplicatePodsMap)
	creatorAssignedNodes := make(map[string][]*v1.Node)
	for _, node := range nodes {
		dpmByNode[node] = ListDuplicatePodsOnANode(client, node)
		for creator := range dpmByNode[node] {
			creatorAssignedNodes[creator] = append(creatorAssignedNodes[creator], node)
		}
	}

	creatorPossibleNodes := make(map[string][]*v1.Node)
	for creator, nodeList := range creatorAssignedNodes {
		creatorNode := nodeList[0]
		creatorPod := dpmByNode[creatorNode][creator][0]
		for _, node := range nodes {
			if nodeutil.PodFitsCurrentNode(creatorPod, node) && nodeutil.PodToleratesNodeTaints(creatorPod, node) {
				creatorPossibleNodes[creator] = append(creatorPossibleNodes[creator], node)
			}
		}
	}

	creatorIsSaturated := make(map[string]bool)
	for creator, nodeList := range creatorAssignedNodes {
		creatorIsSaturated[creator] = (len(creatorPossibleNodes[creator]) == len(nodeList))
		glog.V(1).Infof("Creator %#v is saturated: %#v", creator, creatorIsSaturated[creator])
	}

	return dpmByNode, creatorIsSaturated
}

// ListDuplicatePodsOnANode lists duplicate pods on a given node.
func ListDuplicatePodsOnANode(client clientset.Interface, node *v1.Node) DuplicatePodsMap {
	pods, err := podutil.ListEvictablePodsOnNode(client, node)
	if err != nil {
		return nil
	}
	return FindDuplicatePods(pods)
}

// FindDuplicatePods takes a list of pods and returns a duplicatePodsMap.
func FindDuplicatePods(pods []*v1.Pod) DuplicatePodsMap {
	dpm := DuplicatePodsMap{}
	for _, pod := range pods {
		// Ignoring the error here as in the ListDuplicatePodsOnNode function we call ListEvictablePodsOnNode
		// which checks for error.
		ownerRefList := podutil.OwnerRef(pod)
		for _, ownerRef := range ownerRefList {
			// ownerRef doesn't need namespace since the assumption is owner needs to be in the same namespace.
			s := strings.Join([]string{ownerRef.Kind, ownerRef.Name}, "/")
			dpm[s] = append(dpm[s], pod)
		}
	}
	return dpm
}
