/*


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

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	powerv1alpha1 "gitlab.devtools.intel.com/OrchSW/CNO/power-operator.git/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gitlab.devtools.intel.com/OrchSW/CNO/power-operator.git/pkg/appqos"
	"gitlab.devtools.intel.com/OrchSW/CNO/power-operator.git/pkg/podresourcesclient"
	"gitlab.devtools.intel.com/OrchSW/CNO/power-operator.git/pkg/podstate"
	"gitlab.devtools.intel.com/OrchSW/CNO/power-operator.git/pkg/util"
)

const (
	PowerProfileAnnotation = "PowerProfile"
	ResourcePrefix         = "power.intel.com/"
)

// PowerPodReconciler reconciles a PowerPod object
type PowerPodReconciler struct {
	client.Client
	Log                logr.Logger
	Scheme             *runtime.Scheme
	State              podstate.State
	PodResourcesClient podresourcesclient.PodResourcesClient
	AppQoSClient       *appqos.AppQoSClient
}

// +kubebuilder:rbac:groups=power.intel.com,resources=powerpods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=power.intel.com,resources=powerpods/status,verbs=get;update;patch

func (r *PowerPodReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	logger := r.Log.WithValues("powerpod", req.NamespacedName)

	pod := &corev1.Pod{}
	err := r.Get(context.TODO(), req.NamespacedName, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			// Maybe do a check to see if the Pod is still recorded in State for some reason

			return ctrl.Result{}, nil
		}

		logger.Error(err, "error while trying to retrieve Pod")
		return ctrl.Result{}, err
	}

	if !pod.ObjectMeta.DeletionTimestamp.IsZero() {
		// If the Pod's DeletionTimestamp is not zero then the Pod has been deleted

		powerPodState := r.State.GetPodFromState(pod.GetName())
		workloadToCPUsRemoved := make(map[string][]int, 0)
		for _, container := range powerPodState.Containers {
			workload := fmt.Sprintf("%s%s", container.PowerProfile, WorkloadNameSuffix)
			cpus := container.ExclusiveCPUs
			if _, exists := workloadToCPUsRemoved[workload]; exists {
				workloadToCPUsRemoved[workload] = append(workloadToCPUsRemoved[workload], cpus...)
			} else {
				workloadToCPUsRemoved[workload] = cpus
			}
		}

		for workloadName, cpus := range workloadToCPUsRemoved {
			workload := &powerv1alpha1.PowerWorkload{}
			err = r.Get(context.TODO(), client.ObjectKey{
				Namespace: req.NamespacedName.Namespace,
				Name:      workloadName,
			}, workload)

			if err != nil {
				if errors.IsNotFound(err) {
					return ctrl.Result{}, nil
				}
				logger.Error(err, "error while trying to retrieve PowerWorkload")
				return ctrl.Result{}, err
			}

			updatedWorkloadCPUList := getNewWorkloadCPUList(powerPodState.Node, cpus, workload.Spec.Nodes)
			for i, node := range workload.Spec.Nodes {
				if node.Name == powerPodState.Node {
					if len(updatedWorkloadCPUList) == 0 {
						workload.Spec.Nodes = append(workload.Spec.Nodes[:i], workload.Spec.Nodes[i+1:]...)
					} else {
						workload.Spec.Nodes[i].CpuIds = updatedWorkloadCPUList
					}
					break
				}
			}

			err = r.Update(context.TODO(), workload)
			if err != nil {
				logger.Error(err, "Failed updating PowerWorkload")
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, nil
	}

	// If the Pod's DeletionTimestamp is equal to zero then the Pod has been created or updated

	// Make sure the Pod is running
	podNotRunningErr := errors.NewServiceUnavailable("pod not in running phase")
	if pod.Status.Phase != corev1.PodRunning {
		logger.Info("Pod not running", "pod status:", pod.Status.Phase)
		return ctrl.Result{}, podNotRunningErr
	}

	// Get the Containers of the Pod that are requesting exclusive CPUs
	containersRequestingExclusiveCPUs := getContainersRequestingExclusiveCPUs(pod)
	if len(containersRequestingExclusiveCPUs) == 0 {
		logger.Info("No containers are requesting exclusive CPUs")
		return ctrl.Result{}, nil
	}

	podUID := pod.GetUID()
	if podUID == "" {
		logger.Info("No pod UID found")
		return ctrl.Result{}, errors.NewServiceUnavailable("pod UID not found")
	}

	nodeAddress, err := r.getPodAddress(pod.Spec.NodeName, req)
	if err != nil {
		logger.Error(err, fmt.Sprintf("Error retrieving AppQoS address on node '%s'", pod.Spec.NodeName))
		return ctrl.Result{}, nil
	}

	powerProfiles, powerContainers, err := r.getPowerProfileRequestsFromContainers(containersRequestingExclusiveCPUs, nodeAddress, pod)
	if err != nil {
		logger.Error(err, "Error retrieving Power Profile from requests")
		return ctrl.Result{}, nil
	}

	for profile, cores := range powerProfiles {
		workloadName := fmt.Sprintf("%s%s", profile, WorkloadNameSuffix)
		workload := &powerv1alpha1.PowerWorkload{}
		err = r.Client.Get(context.TODO(), client.ObjectKey{
			Namespace: req.NamespacedName.Namespace,
			Name:      workloadName,
		}, workload)
		if err != nil {
			if errors.IsNotFound(err) {
				// This is the first Pod to request this PowerProfile, need to create corresponding PowerWorkload

				nodeInfo := []powerv1alpha1.NodeInfo{
					powerv1alpha1.NodeInfo{
						Name:   pod.Spec.NodeName,
						CpuIds: cores,
					},
				}

				workloadSpec := &powerv1alpha1.PowerWorkloadSpec{
					Nodes:        nodeInfo,
					PowerProfile: profile,
				}
				workload = &powerv1alpha1.PowerWorkload{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: req.NamespacedName.Namespace,
						Name:      workloadName,
					},
				}
				workload.Spec = *workloadSpec
				err = r.Client.Create(context.TODO(), workload)
				if err != nil {
					logger.Error(err, "error while creating PowerWorkload")
					return ctrl.Result{}, err
				}
			} else {
				logger.Error(err, fmt.Sprintf("Error retrieving PowerWorkload '%s'", workloadName))
			}

			continue
		}

		// PowerWorkload already exists so need to update it. If the Node already
		// exists in the Workload, we update the Node's CPU list, if not we create
		// the entry for the node

		nodeExisted := false
		for i, node := range workload.Spec.Nodes {
			if node.Name == pod.Spec.NodeName {
				nodeExisted = true
				workload.Spec.Nodes[i].CpuIds = appendIfUnique(node.CpuIds, cores)
				sort.Ints(workload.Spec.Nodes[i].CpuIds)
				break
			}
		}

		if !nodeExisted {
			nodeInfo := powerv1alpha1.NodeInfo{
				Name:   pod.Spec.NodeName,
				CpuIds: cores,
			}

			workload.Spec.Nodes = append(workload.Spec.Nodes, nodeInfo)
		}

		err = r.Client.Update(context.TODO(), workload)
		if err != nil {
			logger.Error(err, "error while trying to update PowerWorkload")
			continue
		}
	}

	// Finally, update the controller's State

	guaranteedPod := powerv1alpha1.GuaranteedPod{}
	guaranteedPod.Node = pod.Spec.NodeName
	guaranteedPod.Name = pod.GetName()
	guaranteedPod.UID = string(podUID)
	guaranteedPod.Containers = make([]powerv1alpha1.Container, 0)
	guaranteedPod.Containers = powerContainers
	err = r.State.UpdateStateGuaranteedPods(guaranteedPod)
	if err != nil {
		logger.Error(err, "error updating internal state")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *PowerPodReconciler) getPodAddress(nodeName string, req ctrl.Request) (string, error) {
	_ = context.Background()
	logger := r.Log.WithValues("powerworkload", req.NamespacedName)

	// TODO: DELETE WHEN APPQOS CONTAINERIZED
	if 1 == 1 {
		node := &corev1.Node{}
		err := r.Client.Get(context.TODO(), client.ObjectKey{
			Name: nodeName,
		}, node)
		if err != nil {
			return "", err
		}
		address := fmt.Sprintf("%s%s%s", "https://", node.Status.Addresses[0].Address, ":5000")
		return address, nil
	}

	pods := &corev1.PodList{}
	err := r.Client.List(context.TODO(), pods, client.MatchingLabels(client.MatchingLabels{"name": PowerPodNameConst}))
	if err != nil {
		logger.Error(err, "Failed to list AppQoS pods")
		return "", nil
	}

	appqosPod, err := util.GetPodFromNodeName(pods, nodeName)
	if err != nil {
		appqosNode := &corev1.Node{}
		err := r.Client.Get(context.TODO(), client.ObjectKey{
			Name: nodeName,
		}, appqosNode)
		if err != nil {
			logger.Error(err, "Error getting AppQoS node")
			return "", err
		}

		appqosPod, err = util.GetPodFromNodeAddresses(pods, appqosNode)
		if err != nil {
			return "", err
		}
	}

	var podIP string
	notFoundError := errors.NewServiceUnavailable("pod address not available")
	if appqosPod.Status.PodIP != "" {
		podIP = appqosPod.Status.PodIP
	} else if len(appqosPod.Status.PodIPs) != 0 {
		podIP = appqosPod.Status.PodIPs[0].IP
	} else {
		return "", notFoundError
	}

	if len(appqosPod.Spec.Containers) == 0 {
		return "", notFoundError
	}

	if len(appqosPod.Spec.Containers[0].Ports) == 0 {
		return "", notFoundError
	}

	addressPrefix := r.AppQoSClient.GetAddressPrefix()
	address := fmt.Sprintf("%s%s%s%d", addressPrefix, podIP, ":", appqosPod.Spec.Containers[0].Ports[0].ContainerPort)
	return address, nil
}

func (r *PowerPodReconciler) getPowerProfileRequestsFromContainers(containers []corev1.Container, nodeAddress string, pod *corev1.Pod) (map[string][]int, []powerv1alpha1.Container, error) {
	// Check for the following errors that can occur from a Pod requesting Power Profiles:
	//	1. A Container requesting multiple Power Profiles
	//	2. A Pod requesting multiple Power Profiles (WIP: allow for a Pod that has multiple containers to have a different Power Profile for each)
	//	3. The requested Power Profile exists in the AppQoS instance on the node

	profiles := make(map[string][]int, 0)
	powerContainers := make([]powerv1alpha1.Container, 0)

	for _, container := range containers {
		profile, err := getContainerProfileFromRequests(container)
		if err != nil {
			return map[string][]int{}, []powerv1alpha1.Container{}, err
		}

		if profile == "" {
			continue
		}

		powerProfileFromAppQoS, err := r.AppQoSClient.GetProfileByName(profile, nodeAddress)
		if err != nil {
			return map[string][]int{}, []powerv1alpha1.Container{}, err
		}

		if reflect.DeepEqual(powerProfileFromAppQoS, &appqos.PowerProfile{}) {
			powerProfileNotFoundError := errors.NewServiceUnavailable(fmt.Sprintf("Power Profile '%s' not found", profile))
			return map[string][]int{}, []powerv1alpha1.Container{}, powerProfileNotFoundError
		}

		containerID := getContainerID(pod, container.Name)
		coreIDs, err := r.PodResourcesClient.GetContainerCPUs(pod.GetName(), container.Name)
		if err != nil {
			return map[string][]int{}, []powerv1alpha1.Container{}, err
		}
		cleanCoreList := getCleanCoreList(coreIDs)

		powerContainer := &powerv1alpha1.Container{}
		powerContainer.Name = container.Name
		powerContainer.ID = strings.TrimPrefix(containerID, "docker://")
		powerContainer.ExclusiveCPUs = cleanCoreList
		powerContainer.PowerProfile = profile
		powerContainers = append(powerContainers, *powerContainer)

		if _, exists := profiles[profile]; exists {
			profiles[profile] = append(profiles[profile], cleanCoreList...)
		} else {
			profiles[profile] = cleanCoreList
		}
	}

	if len(reflect.ValueOf(profiles).MapKeys()) > 1 {
		// For now we can only have one Power Profile per Pod

		moreThanOneProfileError := errors.NewServiceUnavailable("Cannot have more than one Power Profile per Pod")
		return map[string][]int{}, []powerv1alpha1.Container{}, moreThanOneProfileError
	}

	return profiles, powerContainers, nil
}

func getNewWorkloadCPUList(nodeName string, cpuList []int, nodeInfoList []powerv1alpha1.NodeInfo) []int {
	updatedWorkloadCPUList := make([]int, 0)

	for _, node := range nodeInfoList {
		if node.Name != nodeName {
			continue
		}

		for _, cpu := range node.CpuIds {
			if !util.CPUInCPUList(cpu, cpuList) {
				updatedWorkloadCPUList = append(updatedWorkloadCPUList, cpu)
			}
		}
	}

	return updatedWorkloadCPUList
}

func appendIfUnique(cpuList []int, cpus []int) []int {
	for _, cpu := range cpus {
		if !util.CPUInCPUList(cpu, cpuList) {
			cpuList = append(cpuList, cpu)
		}
	}

	return cpuList
}

func getContainerProfileFromRequests(container corev1.Container) (string, error) {
	profileName := ""
	moreThanOneProfileError := errors.NewServiceUnavailable("Cannot have more than one Power Profile per Container")

	for resource, _ := range container.Resources.Requests {
		if strings.HasPrefix(string(resource), ResourcePrefix) {
			if profileName == "" {
				profileName = string(resource[len(ResourcePrefix):])
			} else {
				// Cannot have more than one profile for a singular container
				return "", moreThanOneProfileError
			}
		}
	}

	return profileName, nil
}

func getContainersRequestingExclusiveCPUs(pod *corev1.Pod) []corev1.Container {
	containersRequestingExclusiveCPUs := make([]corev1.Container, 0)
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		if exclusiveCPUs(pod, &container) {
			containersRequestingExclusiveCPUs = append(containersRequestingExclusiveCPUs, container)
			//containers = append(containers, container)
		}
	}

	return containersRequestingExclusiveCPUs
}

func exclusiveCPUs(pod *corev1.Pod, container *corev1.Container) bool {
	if pod.Status.QOSClass != corev1.PodQOSGuaranteed {
		return false
	}

	cpuQuantity := container.Resources.Requests[corev1.ResourceCPU]
	if cpuQuantity.Value()*1000 != cpuQuantity.MilliValue() {
		return false
	}

	return true
}

func getContainerID(pod *corev1.Pod, containerName string) string {
	for _, containerStatus := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if containerStatus.Name == containerName {
			return containerStatus.ContainerID
		}
	}

	return ""
}

func getCleanCoreList(coreIDs string) []int {
	cleanCores := make([]int, 0)
	commaSeparated := strings.Split(coreIDs, ",")
	for _, splitCore := range commaSeparated {
		hyphenSeparated := strings.Split(splitCore, "-")
		if len(hyphenSeparated) == 1 {
			intCore, _ := strconv.Atoi(hyphenSeparated[0])
			cleanCores = append(cleanCores, intCore)
		} else {
			startCore, _ := strconv.Atoi(hyphenSeparated[0])
			endCore, _ := strconv.Atoi(hyphenSeparated[len(hyphenSeparated)-1])
			for i := startCore; i <= endCore; i++ {
				cleanCores = append(cleanCores, i)
			}
		}
	}

	return cleanCores
}

func (r *PowerPodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}
