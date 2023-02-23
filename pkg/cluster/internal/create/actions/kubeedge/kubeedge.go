/*
Copyright 2019 The Kubernetes Authors.

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

// Package kubeedge implements the kubeedge action
package kubeedge

import (
	"fmt"
	"strings"

	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
	"sigs.k8s.io/kind/pkg/cluster/shared/create/actions"
	"sigs.k8s.io/kind/pkg/cluster/shared/providers/docker"
	"sigs.k8s.io/kind/pkg/errors"
	"sigs.k8s.io/kind/pkg/exec"
)

// controlPlaneIP is IP address that edgecore register to
var controlPlaneIP = ""

// KubeEdgeToken is token that edgecore used to register to cloudcore
var KubeEdgeToken string

// Action implements action for creating the node config files
type Action struct {
	AdvertiseAddress string
	ContainerMode    bool
}

// NewAction returns a new action for creating the config files
func NewAction(address string, containerMode bool) actions.Action {
	return &Action{
		AdvertiseAddress: address,
		ContainerMode:    containerMode,
	}
}

// Execute runs the action
func (a *Action) Execute(ctx *actions.ActionContext) error {
	ctx.Status.Start("Starting KubeEdge Edgecore 📜")
	defer ctx.Status.End(false)

	if err := a.preProcess(ctx); err != nil {
		return fmt.Errorf("failed do pre process: %v", err)
	}

	// bootstrap edgecore: this operation should be on edge-node
	if err := a.BootstrapEdgecore(ctx); err != nil {
		return err
	}

	// mark success
	ctx.Status.End(true)
	return nil
}

// nolint
var patch = `"spec": {
	"template": {
		"spec": {
			"affinity": {
				"nodeAffinity": {
					"requiredDuringSchedulingIgnoredDuringExecution": {
						"nodeSelectorTerms": [
							{
								"matchExpressions": [
									{
										"key": "node-role.kubernetes.io/edge",
										"operator": "DoesNotExist"
									}
								]
							}
						]
					}
				}
			},
		}
	},
}`

// this patch cmd is from the above json
var kubeProxyNotScheduleOnEdgeNode string = `kubectl patch daemonset kube-proxy -n kube-system -p '{"spec": {"template": {"spec": {"affinity": {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {"nodeSelectorTerms": [{"matchExpressions": [{"key": "node-role.kubernetes.io/edge", "operator": "DoesNotExist"}]}]}}}}}}}'`

func (a *Action) preProcess(ctx *actions.ActionContext) error {
	allNodes, err := ctx.Nodes()
	if err != nil {
		return err
	}

	node, err := nodeutils.BootstrapControlPlaneNode(allNodes)
	if err != nil {
		return err
	}

	// check control plane ready
	name := ctx.Config.Name
	nodeName := fmt.Sprintf("node/%s-control-plane", name)
	cmd := node.Command("kubectl", "wait", "--for=condition=Ready", nodeName, "--timeout=180s")
	lines, err := exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to wait the control-plane ready %v ", lines))
	}

	//cmd = node.Command("bash", "-c", kindnetNotScheduleOnEdgeNode)
	//lines, err = exec.CombinedOutputLines(cmd)
	//ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	//if err != nil {
	//	return errors.Wrap(err, fmt.Sprintf("failed to stop kindnet scheduled to edge nodes %v ", lines))
	//}

	// edge-node not schedule kube-proxy
	cmd = node.Command("bash", "-c", kubeProxyNotScheduleOnEdgeNode)
	lines, err = exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("failed to stop daemonset kube-proxy scheduled to the edge nodes %v ", lines))
	}
	return nil
}

// BootstrapEdgecore
func (a *Action) BootstrapEdgecore(ctx *actions.ActionContext) error {
	allNodes, err := ctx.Nodes()
	if err != nil {
		return err
	}

	controlPlane, err := nodeutils.BootstrapControlPlaneNode(allNodes)
	if err != nil {
		return err
	}

	ip, _, _ := controlPlane.IP()
	controlPlaneIP = ip

	if a.AdvertiseAddress != "" {
		controlPlaneIP = a.AdvertiseAddress
	}

	// then join edge nodes if any
	// The below operation, we should exec in the edge nodes, but not master
	edgeNodes, err := docker.ListEdgeNodesByLabel(ctx.Config.Name)
	if err != nil {
		return err
	}

	if len(edgeNodes) == 0 {
		//return fmt.Errorf("edge node not exist")
	}

	if len(edgeNodes) > 0 {
		if err := a.joinEdgeNodes(ctx, edgeNodes); err != nil {
			return err
		}
	}

	return nil
}

func (a *Action) joinEdgeNodes(
	ctx *actions.ActionContext,
	edgeNodes []nodes.Node,
) error {
	// create the workers concurrently
	fns := []func() error{}
	for _, node := range edgeNodes {
		node := node // capture loop variable
		fns = append(fns, func() error {
			return a.runStartEdgecore(ctx, node)
		})
	}
	if err := errors.UntilErrorConcurrent(fns); err != nil {
		return err
	}

	ctx.Status.End(true)
	return nil
}

// runKubeadmJoin executes kubeadm join command
func (a *Action) runStartEdgecore(ctx *actions.ActionContext, node nodes.Node) error {
	if err := stopKubelet(ctx, node); err != nil {
		return errors.Wrap(err, "failed to stop kubelet")
	}

	// generate config
	cmd := node.Command("bash", "-c", "edgecore --defaultconfig > /etc/kubeedge/config/edgecore.yaml")
	lines, err := exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("failed to generate cloudcore config: %v", err)
	}

	cmd = node.Command("bash", "-c", fmt.Sprintf(`sed -i -e "s|token: .*|token: %s|g" /etc/kubeedge/config/edgecore.yaml`, KubeEdgeToken))
	lines, err = exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("failed to modify token: %v", err)
	}

	// modify runtime to containerd
	cmd = node.Command("bash", "-c", fmt.Sprintf(`sed -i -e "s|remoteImageEndpoint: .*|remoteImageEndpoint: %s|g" /etc/kubeedge/config/edgecore.yaml`, "unix:///var/run/containerd/containerd.sock"))
	lines, err = exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("failed to modify remoteImageEndpoint: %v", err)
	}
	cmd = node.Command("bash", "-c", fmt.Sprintf(`sed -i -e "s|remoteRuntimeEndpoint: .*|remoteRuntimeEndpoint: %s|g" /etc/kubeedge/config/edgecore.yaml`, "unix:///var/run/containerd/containerd.sock"))
	lines, err = exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("failed to modify remoteRuntimeEndpoint: %v", err)
	}
	cmd = node.Command("bash", "-c", fmt.Sprintf(`sed -i -e "s|containerRuntime: .*|containerRuntime: %s|g" /etc/kubeedge/config/edgecore.yaml`, "remote"))
	lines, err = exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("failed to modify containerRuntime to remote: %v", err)
	}

	// modify edgeHub.httpServer websocker.server ip cloudcore ip or control-plane ip
	cmd = node.Command("bash", "-c", fmt.Sprintf(`sed -i -e "s|httpServer: .*|httpServer: %s|g" /etc/kubeedge/config/edgecore.yaml`, "https://"+controlPlaneIP+":10002"))
	lines, err = exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("failed to modify httpServer: %v", err)
	}
	cmd = node.Command("bash", "-c", fmt.Sprintf(`sed -i -e "s|server: .*10000|server: %s|g" /etc/kubeedge/config/edgecore.yaml`, controlPlaneIP+":10000"))
	lines, err = exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("failed to modify httpServer: %v", err)
	}

	cmd = node.Command("bash", "-c", `sed -i -e "s|mqttMode: .*|mqttMode: 0|g" /etc/kubeedge/config/edgecore.yaml`)
	lines, err = exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("failed to modify mqttMode: %v", err)
	}

	cmd = node.Command("bash", "-c", `sed -i -e "s|/tmp/etc/resolv|/etc/resolv|g" /etc/kubeedge/config/edgecore.yaml`)
	lines, err = exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("failed to modify resolv: %v", err)
	}

	cmd = node.Command("bash", "-c", "systemctl daemon-reload && systemctl enable edgecore && systemctl start edgecore")
	lines, err = exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return fmt.Errorf("failed to start cloudcore: %v", err)
	}

	return nil
}

// stopKubelet stop kubelet service and delete kubelet node
func stopKubelet(ctx *actions.ActionContext, node nodes.Node) error {
	// first stop kubelet service on edge-node
	cmd := node.Command("bash", "-c", "systemctl stop kubelet.service && systemctl disable kubelet.service && rm /etc/systemd/system/kubelet.service")
	lines, err := exec.CombinedOutputLines(cmd)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return errors.Wrap(err, "failed to stop kubelet")
	}

	// then call k8s api to delete kubelet node on master node
	// or edgecore will not register successfully, such as updating label "node-role.kubernetes.io/edge": "" will fail
	allNodes, err := ctx.Nodes()
	if err != nil {
		return err
	}

	// master node/ control plane
	controlPlane, err := nodeutils.BootstrapControlPlaneNode(allNodes)
	if err != nil {
		return err
	}

	s := fmt.Sprintf("kubectl delete node %s --wait", node.String())
	delete := controlPlane.Command("bash", "-c", s)
	lines, err = exec.CombinedOutputLines(delete)
	ctx.Logger.V(3).Info(strings.Join(lines, "\n"))
	if err != nil {
		return errors.Wrap(err, "failed to delete node")
	}

	return nil
}
