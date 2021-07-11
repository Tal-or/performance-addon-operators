package machineconfig

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"text/template"

	"github.com/coreos/go-systemd/unit"
	igntypes "github.com/coreos/ignition/v2/config/v3_2/types"
	performancev2 "github.com/openshift-kni/performance-addon-operators/api/v2"
	"github.com/openshift-kni/performance-addon-operators/pkg/controller/performanceprofile/components"
	profile2 "github.com/openshift-kni/performance-addon-operators/pkg/controller/performanceprofile/components/profile"
	pinfo "github.com/openshift-kni/performance-addon-operators/pkg/controller/performanceprofile/components/profileinfo"
	machineconfigv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/pointer"
)

const (
	defaultIgnitionVersion       = "3.2.0"
	defaultIgnitionContentSource = "data:text/plain;charset=utf-8;base64"
)

const (
	// MCKernelRT is the value of the kernel setting in MachineConfig for the RT kernel
	MCKernelRT = "realtime"
	// MCKernelDefault is the value of the kernel setting in MachineConfig for the default kernel
	MCKernelDefault = "default"
	// HighPerformanceRuntime contains the name of the high-performance runtime
	HighPerformanceRuntime = "high-performance"

	hugepagesAllocation = "hugepages-allocation"
	bashScriptsDir      = "/usr/local/bin"
	crioConfd           = "/etc/crio/crio.conf.d"
	crioRuntimesConfig  = "99-runtimes.conf"
	ociHooks            = "low-latency-hooks"
	// OCIHooksConfigDir is the default directory for the OCI hooks
	OCIHooksConfigDir = "/etc/containers/oci/hooks.d"
	// OCIHooksConfig file contains the low latency hooks configuration
	OCIHooksConfig     = "99-low-latency-hooks"
	ociTemplateRPSMask = "RPSMask"
	udevRulesDir       = "/etc/udev/rules.d"
	udevRpsRule        = "99-netdev-rps"
	setRPSMask         = "set-rps-mask"
)

const (
	systemdSectionUnit     = "Unit"
	systemdSectionService  = "Service"
	systemdSectionInstall  = "Install"
	systemdDescription     = "Description"
	systemdBefore          = "Before"
	systemdEnvironment     = "Environment"
	systemdType            = "Type"
	systemdRemainAfterExit = "RemainAfterExit"
	systemdExecStart       = "ExecStart"
	systemdWantedBy        = "WantedBy"
)

const (
	systemdServiceKubelet     = "kubelet.service"
	systemdServiceTypeOneshot = "oneshot"
	systemdTargetMultiUser    = "multi-user.target"
	systemdTrue               = "true"
)

const (
	environmentHugepagesSize  = "HUGEPAGES_SIZE"
	environmentHugepagesCount = "HUGEPAGES_COUNT"
	environmentNUMANode       = "NUMA_NODE"
)

const (
	templateReservedCpus = "ReservedCpus"
)

// New returns new machine configuration object for performance sensitive workloads
func New(assetsDir string, profile *pinfo.PerformanceProfileInfo) (*machineconfigv1.MachineConfig, error) {
	name := GetMachineConfigName(profile)
	mc := &machineconfigv1.MachineConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: machineconfigv1.GroupVersion.String(),
			Kind:       "MachineConfig",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: profile2.GetMachineConfigLabel(profile),
		},
		Spec: machineconfigv1.MachineConfigSpec{},
	}

	ignitionConfig, err := getIgnitionConfig(assetsDir, profile)
	if err != nil {
		return nil, err
	}

	rawIgnition, err := json.Marshal(ignitionConfig)
	if err != nil {
		return nil, err
	}
	mc.Spec.Config = runtime.RawExtension{Raw: rawIgnition}

	enableRTKernel := profile.Spec.RealTimeKernel != nil &&
		profile.Spec.RealTimeKernel.Enabled != nil &&
		*profile.Spec.RealTimeKernel.Enabled

	if enableRTKernel {
		mc.Spec.KernelType = MCKernelRT
	} else {
		mc.Spec.KernelType = MCKernelDefault
	}

	return mc, nil
}

// GetMachineConfigName generates machine config name from the performance profile
func GetMachineConfigName(profile *pinfo.PerformanceProfileInfo) string {
	name := components.GetComponentName(profile.Name, components.ComponentNamePrefix)
	return fmt.Sprintf("50-%s", name)
}

func getIgnitionConfig(assetsDir string, profile *pinfo.PerformanceProfileInfo) (*igntypes.Config, error) {
	ignitionConfig := &igntypes.Config{
		Ignition: igntypes.Ignition{
			Version: defaultIgnitionVersion,
		},
		Storage: igntypes.Storage{
			Files: []igntypes.File{},
		},
	}

	// add script files under the node /usr/local/bin directory
	mode := 0700
	for _, script := range []string{hugepagesAllocation, ociHooks, setRPSMask} {
		src := filepath.Join(assetsDir, "scripts", fmt.Sprintf("%s.sh", script))
		if err := addFile(ignitionConfig, src, getBashScriptPath(script), &mode); err != nil {
			return nil, err
		}
	}

	// add crio config snippet under the node /etc/crio/crio.conf.d/ directory
	crioConfdRuntimesMode := 0644
	crioConfigSnippetContent, err := addCrioConfigSnippet(profile, filepath.Join(assetsDir, "configs", crioRuntimesConfig))
	if err != nil {
		return nil, err
	}

	if err := addContent(
		ignitionConfig,
		crioConfigSnippetContent,
		filepath.Join(crioConfd, crioRuntimesConfig),
		&crioConfdRuntimesMode,
	); err != nil {
		return nil, err
	}

	// add crio hooks config  under the node cri-o hook directory
	crioHooksConfigsMode := 0644
	config := fmt.Sprintf("%s.json", OCIHooksConfig)
	ociHooksConfigContent, err := GetOCIHooksConfigContent(filepath.Join(assetsDir, "configs", config), profile)
	if err != nil {
		return nil, err
	}

	if err := addContent(
		ignitionConfig,
		ociHooksConfigContent,
		filepath.Join(OCIHooksConfigDir, config),
		&crioHooksConfigsMode,
	); err != nil {
		return nil, err
	}

	// add rps udev rule
	rpsRuleMode := 0644
	rule := fmt.Sprintf("%s.rules", udevRpsRule)
	if err := addFile(
		ignitionConfig,
		filepath.Join(assetsDir, "configs", rule),
		filepath.Join(udevRulesDir, rule),
		&rpsRuleMode,
	); err != nil {
		return nil, err
	}

	if profile.Spec.HugePages != nil {
		for _, page := range profile.Spec.HugePages.Pages {
			// we already allocated non NUMA specific hugepages via kernel arguments
			if page.Node == nil {
				continue
			}

			hugepagesSize, err := GetHugepagesSizeKilobytes(page.Size)
			if err != nil {
				return nil, err
			}

			hugepagesService, err := getSystemdContent(getHugepagesAllocationUnitOptions(
				hugepagesSize,
				page.Count,
				*page.Node,
			))
			if err != nil {
				return nil, err
			}

			ignitionConfig.Systemd.Units = append(ignitionConfig.Systemd.Units, igntypes.Unit{
				Contents: &hugepagesService,
				Enabled:  pointer.BoolPtr(true),
				Name:     getSystemdService(fmt.Sprintf("%s-%skB-NUMA%d", hugepagesAllocation, hugepagesSize, *page.Node)),
			})
		}
	}

	if profile.Spec.CPU != nil && profile.Spec.CPU.Reserved != nil {
		rpsMask, err := components.CPUListToMaskList(string(*profile.Spec.CPU.Reserved))
		if err != nil {
			return nil, err
		}

		rpsService, err := getSystemdContent(getRPSUnitOptions(rpsMask))
		if err != nil {
			return nil, err
		}

		ignitionConfig.Systemd.Units = append(ignitionConfig.Systemd.Units, igntypes.Unit{
			Contents: &rpsService,
			Name:     getSystemdService("update-rps@"),
		})
	}

	return ignitionConfig, nil
}

func getBashScriptPath(scriptName string) string {
	return fmt.Sprintf("%s/%s.sh", bashScriptsDir, scriptName)
}

func getSystemdEnvironment(key string, value string) string {
	return fmt.Sprintf("%s=%s", key, value)
}

func getSystemdService(serviceName string) string {
	return fmt.Sprintf("%s.service", serviceName)
}

func getSystemdContent(options []*unit.UnitOption) (string, error) {
	outReader := unit.Serialize(options)
	outBytes, err := ioutil.ReadAll(outReader)
	if err != nil {
		return "", err
	}
	return string(outBytes), nil
}

// GetOCIHooksConfigContent reads and returns the content of the OCI hook file
func GetOCIHooksConfigContent(configFile string, profile *pinfo.PerformanceProfileInfo) ([]byte, error) {
	content, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}

	rpsMask := "0" // RPS disabled
	if profile.Spec.CPU != nil && profile.Spec.CPU.Reserved != nil {
		rpsMask, err = components.CPUListToMaskList(string(*profile.Spec.CPU.Reserved))
		if err != nil {
			return nil, err
		}
	}

	outContent := &bytes.Buffer{}
	templateArgs := map[string]string{ociTemplateRPSMask: rpsMask}
	template := template.Must(template.New("crio").Parse(string(content)))
	if err := template.Execute(outContent, templateArgs); err != nil {
		return nil, err
	}

	return outContent.Bytes(), nil
}

// GetHugepagesSizeKilobytes retruns hugepages size in kilobytes
func GetHugepagesSizeKilobytes(hugepagesSize performancev2.HugePageSize) (string, error) {
	switch hugepagesSize {
	case "1G":
		return "1048576", nil
	case "2M":
		return "2048", nil
	default:
		return "", fmt.Errorf("can not convert size %q to kilobytes", hugepagesSize)
	}
}

func getHugepagesAllocationUnitOptions(hugepagesSize string, hugepagesCount int32, numaNode int32) []*unit.UnitOption {
	return []*unit.UnitOption{
		// [Unit]
		// Description
		unit.NewUnitOption(systemdSectionUnit, systemdDescription, fmt.Sprintf("Hugepages-%skB allocation on the node %d", hugepagesSize, numaNode)),
		// Before
		unit.NewUnitOption(systemdSectionUnit, systemdBefore, systemdServiceKubelet),
		// [Service]
		// Environment
		unit.NewUnitOption(systemdSectionService, systemdEnvironment, getSystemdEnvironment(environmentHugepagesCount, fmt.Sprint(hugepagesCount))),
		unit.NewUnitOption(systemdSectionService, systemdEnvironment, getSystemdEnvironment(environmentHugepagesSize, hugepagesSize)),
		unit.NewUnitOption(systemdSectionService, systemdEnvironment, getSystemdEnvironment(environmentNUMANode, fmt.Sprint(numaNode))),
		// Type
		unit.NewUnitOption(systemdSectionService, systemdType, systemdServiceTypeOneshot),
		// RemainAfterExit
		unit.NewUnitOption(systemdSectionService, systemdRemainAfterExit, systemdTrue),
		// ExecStart
		unit.NewUnitOption(systemdSectionService, systemdExecStart, getBashScriptPath(hugepagesAllocation)),
		// [Install]
		// WantedBy
		unit.NewUnitOption(systemdSectionInstall, systemdWantedBy, systemdTargetMultiUser),
	}
}

func getRPSUnitOptions(rpsMask string) []*unit.UnitOption {
	cmd := fmt.Sprintf("%s %%i %s", getBashScriptPath(setRPSMask), rpsMask)
	return []*unit.UnitOption{
		// [Unit]
		// Description
		unit.NewUnitOption(systemdSectionUnit, systemdDescription, "Sets network devices RPS mask"),
		// [Service]
		// Type
		unit.NewUnitOption(systemdSectionService, systemdType, systemdServiceTypeOneshot),
		// ExecStart
		unit.NewUnitOption(systemdSectionService, systemdExecStart, cmd),
	}
}

func addContent(ignitionConfig *igntypes.Config, content []byte, dst string, mode *int) error {
	contentBase64 := base64.StdEncoding.EncodeToString(content)
	ignitionConfig.Storage.Files = append(ignitionConfig.Storage.Files, igntypes.File{
		Node: igntypes.Node{
			Path: dst,
		},
		FileEmbedded1: igntypes.FileEmbedded1{
			Contents: igntypes.Resource{
				Source: pointer.StringPtr(fmt.Sprintf("%s,%s", defaultIgnitionContentSource, contentBase64)),
			},
			Mode: mode,
		},
	})
	return nil
}

func addCrioConfigSnippet(profile *pinfo.PerformanceProfileInfo, src string) ([]byte, error) {
	templateArgs := make(map[string]string)
	if profile.Spec.CPU.Reserved != nil {
		templateArgs[templateReservedCpus] = string(*profile.Spec.CPU.Reserved)
	}

	content, err := ioutil.ReadFile(src)
	if err != nil {
		return nil, err
	}

	crioConfig := &bytes.Buffer{}
	profileTemplate := template.Must(template.New("crioConfig").Parse(string(content)))
	if err := profileTemplate.Execute(crioConfig, templateArgs); err != nil {
		return nil, err
	}

	return crioConfig.Bytes(), nil
}

func addFile(ignitionConfig *igntypes.Config, src string, dst string, mode *int) error {
	content, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}

	return addContent(ignitionConfig, content, dst, mode)
}
