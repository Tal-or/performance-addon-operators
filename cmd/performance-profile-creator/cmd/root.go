/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2021 Red Hat, Inc.
 */

package cmd

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v2"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/component-helpers/scheduling/corev1"

	"k8s.io/utils/pointer"

	performancev2 "github.com/openshift-kni/performance-addon-operators/api/v2"
	machineconfigv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	v1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ClusterScopedResources defines the subpath, relative to the top-level must-gather directory.
	// A top-level must-gather directory is of the following format:
	// must-gather-dir/quay-io-openshift-kni-performance-addon-operator-must-gather-sha256-<Image SHA>
	// Here we find the cluster-scoped definitions saved by must-gather
	ClusterScopedResources = "cluster-scoped-resources"
	// CoreNodes defines the subpath, relative to ClusterScopedResources, on which we find node-specific data
	CoreNodes = "core/nodes"
	// MCPools defines the subpath, relative to ClusterScopedResources, on which we find the machine config pool definitions
	MCPools = "machineconfiguration.openshift.io/machineconfigpools"
	// YAMLSuffix is the extension of the yaml files saved by must-gather
	YAMLSuffix = ".yaml"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "performance-profile-creator",
	Short: "A tool that automates creation of Performance Profiles",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		mcpName := cmd.Flag("mcp-name").Value.String()
		mustGatherDirPath := cmd.Flag("must-gather-dir-path").Value.String()
		mcp, err := getMCP(mustGatherDirPath, mcpName)
		if err != nil {
			return fmt.Errorf("Error obtaining MachineConfigPool %s: %v", mcpName, err)
		}
		labelSelector := mcp.Spec.NodeSelector
		nodes, err := getNodeList(mustGatherDirPath)
		if err != nil {
			return fmt.Errorf("Error obtaining Nods %s: %v", mcpName, err)
		}

		matchedNodes := make([]*v1.Node, 0)
		for _, node := range nodes {
			matches, _ := corev1.MatchNodeSelectorTerms(node, getNodeSelectorFromLabelSelector(labelSelector))
			if matches {
				log.Infof("%s is targetted by %s MCP", node.GetName(), mcpName)
				matchedNodes = append(matchedNodes, node)
			}
		}
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		profileName := cmd.Flag("profile-name").Value.String()
		createProfile(profileName)
	},
}

func getNodeSelectorFromLabelSelector(labelSelector *metav1.LabelSelector) *v1.NodeSelector {

	matchExpressions := make([]v1.NodeSelectorRequirement, 0)
	for key, value := range labelSelector.MatchLabels {
		matchExpressions = append(matchExpressions, v1.NodeSelectorRequirement{
			Key:      key,
			Operator: v1.NodeSelectorOpIn,
			Values:   []string{value},
		})
	}
	matchFields := make([]v1.NodeSelectorRequirement, 0)
	for _, labelSelectorRequirement := range labelSelector.MatchExpressions {
		matchExpressions = append(matchFields, v1.NodeSelectorRequirement{
			Key:      labelSelectorRequirement.Key,
			Operator: v1.NodeSelectorOperator(string(labelSelectorRequirement.Operator)),
			Values:   labelSelectorRequirement.Values,
		})
	}

	nodeSelectorTerms := []v1.NodeSelectorTerm{
		{
			MatchExpressions: matchExpressions,
			MatchFields:      matchFields,
		},
	}
	nodeSelector := &v1.NodeSelector{
		NodeSelectorTerms: nodeSelectorTerms,
	}

	return nodeSelector

}

func getMustGatherFullPaths(mustGatherPath string, suffix string) (string, error) {
	// The glob pattern below depends on the must gather image name. It is assumed here
	// that the image would have "performance-addon-operator-must-gather" substring in the name.
	paths, err := filepath.Glob(mustGatherPath + "/*performance-addon-operator-must-gather*/" + suffix)

	// Out of all the paths that match the glob pattern we choose the path that was modified most recently.
	var latestTime time.Time
	var lastModifiedPath string

	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("Error obtaining the path stats %s: %v", path, err)
		}
		if info.ModTime().After(latestTime) {
			lastModifiedPath = path
		}
	}
	if len(paths) > 1 {
		log.Infof("Multiple matches for the specified must gather directory path: %s and suffix: %s", mustGatherPath, suffix)
		log.Infof("Selecting the most fresh path (the path that was last modified): %s", lastModifiedPath)
	}
	return lastModifiedPath, err
}

func getNode(mustGatherDirPath, nodeName string) (*v1.Node, error) {
	var node v1.Node
	nodePathSuffix := path.Join(ClusterScopedResources, CoreNodes, nodeName)
	path, err := getMustGatherFullPaths(mustGatherDirPath, nodePathSuffix)
	if err != nil {
		return nil, fmt.Errorf("Error obtaining MachineConfigPool %s: %v", nodeName, err)
	}

	src, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Error opening %q: %v", path, err)
	}
	defer src.Close()

	dec := k8syaml.NewYAMLOrJSONDecoder(src, 1024)
	if err := dec.Decode(&node); err != nil {
		return nil, fmt.Errorf("Error opening %q: %v", path, err)
	}
	return &node, nil
}

func getNodeList(mustGatherDirPath string) ([]*v1.Node, error) {
	machines := make([]*v1.Node, 0)

	nodePathSuffix := path.Join(ClusterScopedResources, CoreNodes)
	nodePath, err := getMustGatherFullPaths(mustGatherDirPath, nodePathSuffix)
	if err != nil {
		return nil, fmt.Errorf("Error obtaining Nodes: %v", err)
	}
	nodes, err := ioutil.ReadDir(nodePath)
	if err != nil {
		return nil, fmt.Errorf("failed to list mustGatherPath directories: %v", err)
	}
	for _, node := range nodes {
		nodeName := node.Name()
		node, err := getNode(mustGatherDirPath, nodeName)
		if err != nil {
			return nil, fmt.Errorf("Error obtaining Nodes %s: %v", nodeName, err)
		}
		machines = append(machines, node)
	}
	return machines, nil
}

func getMCP(mustGatherDirPath, mcpName string) (*machineconfigv1.MachineConfigPool, error) {
	var mcp machineconfigv1.MachineConfigPool

	mcpPathSuffix := path.Join(ClusterScopedResources, MCPools, mcpName+YAMLSuffix)
	mcpPath, err := getMustGatherFullPaths(mustGatherDirPath, mcpPathSuffix)
	if err != nil {
		return nil, fmt.Errorf("Error obtaining MachineConfigPool %s: %v", mcpName, err)
	}

	src, err := os.Open(mcpPath)
	if err != nil {
		return nil, fmt.Errorf("Error opening %q: %v", mcpPath, err)
	}
	defer src.Close()
	dec := k8syaml.NewYAMLOrJSONDecoder(src, 1024)
	if err := dec.Decode(&mcp); err != nil {
		return nil, fmt.Errorf("Error opening %q: %v", mcpPath, err)
	}
	return &mcp, nil
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error while executing root command: %v", err)
		os.Exit(1)
	}
}

type profileCreatorArgs struct {
	powerConsumptionMode string
	mustGatherDirPath    string
	profileName          string
	reservedCPUCount     int
	splitCPUsAcrossNUMA  bool
	disableHT            bool
	rtKernel             bool
	userLevelNetworking  bool
	mcpName              string
}

func init() {
	args := &profileCreatorArgs{}
	rootCmd.PersistentFlags().IntVarP(&args.reservedCPUCount, "reserved-cpu-count", "R", 0, "Number of reserved CPUs (required)")
	rootCmd.MarkPersistentFlagRequired("reserved-cpu-count")
	rootCmd.PersistentFlags().StringVarP(&args.mcpName, "mcp-name", "T", "worker-cnf", "MCP name corresponding to the target machines (required)")
	rootCmd.MarkPersistentFlagRequired("mcp-name")
	rootCmd.PersistentFlags().BoolVarP(&args.splitCPUsAcrossNUMA, "split-cpus-across-numa", "S", true, "Split the CPUs across NUMA nodes")
	rootCmd.PersistentFlags().BoolVarP(&args.disableHT, "disable-ht", "H", false, "Disable Hyperthreading")
	rootCmd.PersistentFlags().BoolVarP(&args.rtKernel, "rt-kernel", "K", true, "Enable Real Time Kernel (required)")
	rootCmd.MarkPersistentFlagRequired("rt-kernel")
	rootCmd.PersistentFlags().BoolVarP(&args.userLevelNetworking, "user-level-networking", "U", false, "Run with User level Networking(DPDK) enabled")
	rootCmd.PersistentFlags().StringVarP(&args.powerConsumptionMode, "power-consumption-mode", "P", "cstate", "The power consumption mode")
	rootCmd.PersistentFlags().StringVarP(&args.mustGatherDirPath, "must-gather-dir-path", "M", "must-gather", "Must gather directory path")
	rootCmd.PersistentFlags().StringVarP(&args.profileName, "profile-name", "N", "performance", "Name of the performance profile to be created")

	// TODO: Input validation
	// 1) Make flags required/optional
	// 2) e.g.check to make sure that power consumption string is in {CSTATE NO_CSTATE IDLE_POLL}
}

func createProfile(profileName string) {

	// TODO: Get the name from MCP if not specified in the command line arguments
	profile := &performancev2.PerformanceProfile{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PerformanceProfile",
			APIVersion: performancev2.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: profileName,
		},
		Spec: performancev2.PerformanceProfileSpec{
			RealTimeKernel: &performancev2.RealTimeKernel{
				Enabled: pointer.BoolPtr(true),
			},
			AdditionalKernelArgs: []string{},
			NUMA: &performancev2.NUMA{
				TopologyPolicy: pointer.StringPtr("restricted"),
			},
		},
	}

	var performanceProfileData []byte
	var err error

	if performanceProfileData, err = yaml.Marshal(&profile); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to Marshal sample performance profile: %v", err)
	}

	fmt.Printf("%s", string(performanceProfileData))

}
