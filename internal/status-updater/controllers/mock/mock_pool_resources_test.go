package mock

import (
	"testing"

	commonprofile "github.com/run-ai/fake-gpu-operator/internal/common/profile"
	"github.com/run-ai/fake-gpu-operator/internal/common/topology"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func mkProfileCM(name, ns string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{commonprofile.LabelGpuProfile: "true"},
		},
		Data: map[string]string{
			commonprofile.CmProfileKey: `system:
  driver_version: "550"`,
		},
	}
}

func TestComputeMockPoolResources_OnlyMockPools(t *testing.T) {
	kube := fake.NewSimpleClientset(mkProfileCM("gpu-profile-a100", "ns"))
	cfg := &topology.ClusterConfig{
		NodePoolLabelKey: "k",
		NodePools: map[string]topology.NodePoolConfig{
			"fake-pool":      {Gpu: topology.GpuConfig{Backend: "fake", Profile: "a100"}},
			"mock-train":     {Gpu: topology.GpuConfig{Backend: "mock", Profile: "a100"}},
			"mock-inference": {Gpu: topology.GpuConfig{Backend: "mock", Profile: "a100"}},
		},
	}
	params := ReconcileParams{
		Namespace:       "ns",
		Image:           "ghcr.io/nvidia/nvml-mock:v0.1.0",
		ImagePullPolicy: corev1.PullIfNotPresent,
	}

	configMaps, daemonSets, err := ComputeMockPoolResources(kube, cfg, params)
	require.NoError(t, err)

	// Two mock pools × (1 CM + 1 DS); fake pool produces neither.
	assert.Len(t, configMaps, 2, "one CM per mock pool")
	assert.Len(t, daemonSets, 2, "one DS per mock pool")

	cmNames := map[string]bool{}
	for _, cm := range configMaps {
		cmNames[cm.Name] = true
	}
	dsNames := map[string]bool{}
	for _, ds := range daemonSets {
		dsNames[ds.Name] = true
	}
	assert.True(t, cmNames["nvml-mock-mock-train"])
	assert.True(t, cmNames["nvml-mock-mock-inference"])
	assert.True(t, dsNames["nvml-mock-mock-train"])
	assert.True(t, dsNames["nvml-mock-mock-inference"])
	assert.False(t, dsNames["nvml-mock-fake-pool"])
}

func TestComputeMockPoolResources_DeterministicOrder(t *testing.T) {
	kube := fake.NewSimpleClientset(mkProfileCM("gpu-profile-a100", "ns"))
	cfg := &topology.ClusterConfig{
		NodePoolLabelKey: "k",
		NodePools: map[string]topology.NodePoolConfig{
			"zzz": {Gpu: topology.GpuConfig{Backend: "mock", Profile: "a100"}},
			"aaa": {Gpu: topology.GpuConfig{Backend: "mock", Profile: "a100"}},
			"mmm": {Gpu: topology.GpuConfig{Backend: "mock", Profile: "a100"}},
		},
	}
	params := ReconcileParams{Namespace: "ns", Image: "img:t"}

	configMaps, daemonSets, err := ComputeMockPoolResources(kube, cfg, params)
	require.NoError(t, err)

	require.Len(t, configMaps, 3)
	require.Len(t, daemonSets, 3)
	assert.Equal(t, "nvml-mock-aaa", configMaps[0].Name)
	assert.Equal(t, "nvml-mock-mmm", configMaps[1].Name)
	assert.Equal(t, "nvml-mock-zzz", configMaps[2].Name)
	assert.Equal(t, "nvml-mock-aaa", daemonSets[0].Name)
	assert.Equal(t, "nvml-mock-mmm", daemonSets[1].Name)
	assert.Equal(t, "nvml-mock-zzz", daemonSets[2].Name)
}

func TestComputeMockPoolResources_PropagatesProfileError(t *testing.T) {
	// No profile CMs in cluster → load fails for the mock pool.
	kube := fake.NewSimpleClientset()
	cfg := &topology.ClusterConfig{
		NodePoolLabelKey: "k",
		NodePools: map[string]topology.NodePoolConfig{
			"mock-pool": {Gpu: topology.GpuConfig{Backend: "mock", Profile: "a100"}},
		},
	}
	_, _, err := ComputeMockPoolResources(kube, cfg, ReconcileParams{Namespace: "ns"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mock-pool")
}

func mkMigProfileCM(name, ns string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string]string{
			commonprofile.CmMigProfilesKey: "version: 1\nsupported_profiles:\n  - name: \"1g.5gb\"\n    slices: 1",
		},
	}
}

func TestComputeMockPoolResources_MigTableRendering(t *testing.T) {
	// A pool whose profile enables MIG mode gets the product's table as the
	// sibling key; a MIG-less pool (no table CM in cluster) renders config
	// only; and a table change rolls the DS (hash covers both keys).
	profileYAML := `system:
  driver_version: "550"
device_defaults:
  mig:
    mode_current: "enabled"`
	migPoolProfile := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-profile-a100", Namespace: "ns"},
		Data:       map[string]string{commonprofile.CmProfileKey: profileYAML},
	}
	plainProfile := mkProfileCM("gpu-profile-l40s", "ns")

	kube := fake.NewSimpleClientset(migPoolProfile, plainProfile, mkMigProfileCM("mig-profiles-a100", "ns"))
	cfg := &topology.ClusterConfig{
		NodePoolLabelKey: "k",
		NodePools: map[string]topology.NodePoolConfig{
			"dra-mig":   {Gpu: topology.GpuConfig{Backend: "mock", Profile: "a100"}},
			"dra-share": {Gpu: topology.GpuConfig{Backend: "mock", Profile: "l40s"}},
		},
	}

	configMaps, _, err := ComputeMockPoolResources(kube, cfg, ReconcileParams{Namespace: "ns"})
	require.NoError(t, err)

	byName := map[string]*corev1.ConfigMap{}
	for _, cm := range configMaps {
		byName[cm.Name] = cm
	}

	migCM := byName["nvml-mock-dra-mig"]
	require.NotNil(t, migCM)
	assert.Contains(t, migCM.Data[commonprofile.CmMigProfilesKey], "1g.5gb",
		"MIG-mode pool carries the product table as the sibling file")

	shareCM := byName["nvml-mock-dra-share"]
	require.NotNil(t, shareCM)
	assert.NotContains(t, shareCM.Data, commonprofile.CmMigProfilesKey,
		"a pool without MIG mode stays MIG-less even if a table existed")
}

func TestComputeMockPoolResources_MigModeWithoutTable(t *testing.T) {
	// MIG mode on but the product's table CM is absent → render config only,
	// no error (the board simply stays MIG-less, mirroring upstream shipping
	// configs without a .mig variant).
	profileYAML := `system:
  driver_version: "550"
device_defaults:
  mig:
    mode_current: "enabled"`
	profile := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-profile-a100", Namespace: "ns"},
		Data:       map[string]string{commonprofile.CmProfileKey: profileYAML},
	}
	kube := fake.NewSimpleClientset(profile)
	cfg := &topology.ClusterConfig{
		NodePoolLabelKey: "k",
		NodePools: map[string]topology.NodePoolConfig{
			"dra-mig": {Gpu: topology.GpuConfig{Backend: "mock", Profile: "a100"}},
		},
	}
	configMaps, _, err := ComputeMockPoolResources(kube, cfg, ReconcileParams{Namespace: "ns"})
	require.NoError(t, err)
	require.Len(t, configMaps, 1)
	assert.NotContains(t, configMaps[0].Data, commonprofile.CmMigProfilesKey)
}
