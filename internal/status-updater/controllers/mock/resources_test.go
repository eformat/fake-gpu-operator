package mock

import (
	"testing"

	"github.com/run-ai/fake-gpu-operator/internal/common/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildConfigMap(t *testing.T) {
	cm := BuildConfigMap("ns", "training", []byte("driver_version: \"550\""), nil)

	assert.Equal(t, "nvml-mock-training", cm.Name)
	assert.Equal(t, "ns", cm.Namespace)
	assert.Equal(t, constants.LabelManagedByValue, cm.Labels[constants.LabelManagedBy])
	assert.Equal(t, constants.ComponentNvmlMock, cm.Labels[constants.LabelComponent])
	assert.Equal(t, "training", cm.Labels[constants.LabelPool])
	assert.Equal(t, "driver_version: \"550\"", cm.Data["config.yaml"])
	assert.NotContains(t, cm.Data, "mig-profiles.yaml", "no MIG table must land without one")
}

func TestBuildConfigMap_WithMigTable(t *testing.T) {
	cm := BuildConfigMap("ns", "training", []byte("driver_version: \"550\""), []byte("version: 1\nsupported_profiles: []"))

	assert.Equal(t, "driver_version: \"550\"", cm.Data["config.yaml"])
	assert.Equal(t, "version: 1\nsupported_profiles: []", cm.Data["mig-profiles.yaml"],
		"the MIG table renders as the sibling file the Mokka engine resolves")
}

func TestBuildDaemonSet_Shape(t *testing.T) {
	ds := BuildDaemonSet(BuildDaemonSetParams{
		Namespace:        "ns",
		Pool:             "training",
		NodePoolLabelKey: "run.ai/simulated-gpu-node-pool",
		Image:            "ghcr.io/nvidia/nvml-mock:v0.1.0",
		ImagePullPolicy:  corev1.PullIfNotPresent,
		ConfigHash:       "abc123",
	})

	require.Equal(t, "nvml-mock-training", ds.Name)
	assert.Equal(t, "ns", ds.Namespace)
	assert.Equal(t, constants.LabelManagedByValue, ds.Labels[constants.LabelManagedBy])
	assert.Equal(t, constants.ComponentNvmlMock, ds.Labels[constants.LabelComponent])
	assert.Equal(t, "training", ds.Labels[constants.LabelPool])

	pod := ds.Spec.Template
	assert.Equal(t, "training", pod.Spec.NodeSelector["run.ai/simulated-gpu-node-pool"])
	assert.Equal(t, "nvml-mock", pod.Spec.ServiceAccountName)
	assert.Equal(t, "abc123", pod.Annotations[ConfigHashAnnotation])

	assert.Empty(t, pod.Spec.InitContainers, "no init container expected — upstream nvml-mock now places device nodes under driverRoot/dev")

	require.Len(t, pod.Spec.Containers, 1)
	c := pod.Spec.Containers[0]
	assert.Equal(t, "nvml-mock", c.Name)
	assert.Equal(t, "ghcr.io/nvidia/nvml-mock:v0.1.0", c.Image)
	assert.Equal(t, corev1.PullIfNotPresent, c.ImagePullPolicy)
	require.NotNil(t, c.SecurityContext)
	require.NotNil(t, c.SecurityContext.Privileged)
	assert.True(t, *c.SecurityContext.Privileged)

	envNames := map[string]bool{}
	for _, e := range c.Env {
		envNames[e.Name] = true
	}
	assert.True(t, envNames["GPU_COUNT"])
	assert.True(t, envNames["DRIVER_VERSION"])
	assert.True(t, envNames["NODE_NAME"])

	mountPaths := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mountPaths[m.MountPath] = true
	}
	assert.True(t, mountPaths["/host/var/lib/nvml-mock"])
	assert.True(t, mountPaths["/etc/nvml-mock"])
	assert.True(t, mountPaths["/host/var/run/cdi"])
	assert.True(t, mountPaths["/host/run/nvidia"])
}

func TestBuildDaemonSet_WritesToolkitReadyMarker(t *testing.T) {
	ds := BuildDaemonSet(BuildDaemonSetParams{
		Namespace: "ns", Pool: "training",
		NodePoolLabelKey: "k", Image: "img", ImagePullPolicy: corev1.PullAlways,
		ConfigHash: "x",
	})
	c := ds.Spec.Template.Spec.Containers[0]

	// Wrapper backgrounds upstream entrypoint.sh, waits for setup completion
	// (driver symlink), touches the marker, then `wait`s to inherit
	// entrypoint.sh's sleep. Upstream's script is preserved verbatim so
	// future setup steps run unchanged.
	require.Equal(t, []string{"/bin/sh", "-c"}, c.Command)
	require.Len(t, c.Args, 1)
	assert.Contains(t, c.Args[0], "/scripts/entrypoint.sh &", "must background upstream entrypoint")
	assert.Contains(t, c.Args[0], "/host/run/nvidia/driver", "must poll for setup-completion signal")
	assert.Contains(t, c.Args[0], "/host/run/nvidia/validations/toolkit-ready", "must write the marker")
	assert.Contains(t, c.Args[0], "wait $ENTRY", "must wait on backgrounded entrypoint to keep container alive")

	// preStop invokes upstream cleanup but does NOT remove the marker —
	// on DS recreate the old pod's preStop races with the new pod's setup
	// wrapper for the same host path.
	require.NotNil(t, c.Lifecycle)
	require.NotNil(t, c.Lifecycle.PreStop)
	require.NotNil(t, c.Lifecycle.PreStop.Exec)
	assert.Equal(t, []string{"/scripts/cleanup.sh"}, c.Lifecycle.PreStop.Exec.Command)
}

func TestBuildDaemonSet_ConfigMapVolumePointsAtPerPoolCM(t *testing.T) {
	ds := BuildDaemonSet(BuildDaemonSetParams{
		Namespace: "ns", Pool: "training",
		NodePoolLabelKey: "k", Image: "img", ImagePullPolicy: corev1.PullAlways,
		ConfigHash: "x",
	})
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.Name == "gpu-config" {
			require.NotNil(t, v.ConfigMap)
			assert.Equal(t, "nvml-mock-training", v.ConfigMap.Name)
			return
		}
	}
	t.Fatal("gpu-config volume not found")
}

func TestBuildDaemonSet_MokkaContract(t *testing.T) {
	ds := BuildDaemonSet(BuildDaemonSetParams{
		Namespace:        "ns",
		Pool:             "training",
		NodePoolLabelKey: "run.ai/simulated-gpu-node-pool",
		Image:            "ghcr.io/nvidia/nvml-mock@sha256:1730a101817c21a9745af75a7a1dce0b066370b24b80cd987b4fba3ae7df1042",
		ImagePullPolicy:  corev1.PullIfNotPresent,
		LaunchContract:   ContractMokka,
		ConfigHash:       "abc123",
	})
	c := ds.Spec.Template.Spec.Containers[0]

	// The Mokka image has no /scripts — the agent is the entrypoint.
	assert.Equal(t, "/usr/local/bin/node-agent", c.Command[0])
	assert.Contains(t, c.Command, "start")
	assert.Contains(t, c.Command, "--config=/etc/nvml-mock/config.yaml")
	assert.Contains(t, c.Command, "--host-root=/host")
	assert.NotContains(t, c.Args, nil)
	assert.Nil(t, c.Lifecycle, "no entrypoint wrapper, no preStop cleanup script")
	assert.True(t, *c.SecurityContext.Privileged, "the agent stages trees and bind-mounts the driver root")

	// Env contract: overrides under the driver tree, MIG table via sibling
	// resolution (no MOCK_MIG_PROFILES_CONFIG — the CM key covers it).
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "/etc/nvml-mock/config.yaml", env["MOCK_NVML_CONFIG"])
	assert.Equal(t, "/var/lib/nvml-mock/driver/config/overrides.yaml", env["MOCK_NVML_OVERRIDES"])
	assert.NotContains(t, env, "MOCK_MIG_PROFILES_CONFIG",
		"the table resolves as a sibling of the config path — no env needed")
	assert.Equal(t, "off", env["MOCK_IB"])

	// Volume contract: CDI at /host/run/cdi (not /host/var/run/cdi), the
	// driver-root bind-mount propagating to workloads, the same tree at both
	// /var/lib/nvml-mock and its --host-root view, and the node's sysfs.
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m
	}
	assert.Contains(t, mounts, "/host/run/cdi", "the Mokka agent writes CDI under host-root/run/cdi")
	assert.NotContains(t, mounts, "/host/var/run/cdi")
	assert.Contains(t, mounts, "/var/lib/nvml-mock", "MOCK_PCI_ROOT/MOCK_NVML_OVERRIDES resolve here")
	assert.Contains(t, mounts, "/host/sys")
	driverLink, ok := mounts["/host/run/nvidia"]
	require.True(t, ok)
	require.NotNil(t, driverLink.MountPropagation)
	assert.Equal(t, corev1.MountPropagationBidirectional, *driverLink.MountPropagation,
		"workloads that mounted /run/nvidia first must see the agent's driver-root mount")

	volNames := map[string]bool{}
	for _, v := range ds.Spec.Template.Spec.Volumes {
		volNames[v.Name] = true
	}
	assert.True(t, volNames["mokka-root"])
	assert.True(t, volNames["node-sysfs"])
	assert.True(t, volNames["nfd-features"])
}

func TestBuildDaemonSet_EmptyContractIsLegacy(t *testing.T) {
	// The zero-value params (no LaunchContract set) must keep rendering the
	// v0.1.0 contract — existing consumers rely on it.
	ds := BuildDaemonSet(BuildDaemonSetParams{
		Namespace:        "ns",
		Pool:             "training",
		NodePoolLabelKey: "run.ai/simulated-gpu-node-pool",
		Image:            "ghcr.io/nvidia/nvml-mock:sha-1706195",
		ConfigHash:       "abc123",
	})
	c := ds.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "/bin/sh", c.Command[0])
	assert.Contains(t, c.Args[0], "/scripts/entrypoint.sh &")
}
