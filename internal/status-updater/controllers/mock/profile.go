package mock

import (
	"context"
	"fmt"

	commonprofile "github.com/run-ai/fake-gpu-operator/internal/common/profile"
	"github.com/run-ai/fake-gpu-operator/internal/common/topology"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// RenderMerged loads the profile ConfigMap referenced by gpu.Profile,
// deep-merges gpu.Overrides on top, and returns the merged profile map —
// the single load/merge point shared by RenderConfig and the MIG-capability
// check.
func RenderMerged(kube kubernetes.Interface, namespace string, gpu topology.GpuConfig) (map[string]interface{}, error) {
	if gpu.Profile == "" {
		return nil, fmt.Errorf("mock pool requires a non-empty profile")
	}

	base, err := commonprofile.Load(kube, namespace, gpu.Profile)
	if err != nil {
		return nil, fmt.Errorf("loading profile %q: %w", gpu.Profile, err)
	}

	return commonprofile.Merge(base, gpu.Overrides), nil
}

// RenderConfig produces the serialized YAML body for a per-pool nvml-mock
// ConfigMap's `config.yaml` key, plus the GpuSpec extracted from the merged
// profile (used by the DaemonSet builder for literal env vars).
//
// Returns an error if the profile name is empty, the profile ConfigMap is
// missing, or the profile YAML is malformed.
func RenderConfig(kube kubernetes.Interface, namespace string, gpu topology.GpuConfig) ([]byte, commonprofile.GpuSpec, error) {
	merged, err := RenderMerged(kube, namespace, gpu)
	if err != nil {
		return nil, commonprofile.GpuSpec{}, err
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		return nil, commonprofile.GpuSpec{}, fmt.Errorf("serializing merged profile: %w", err)
	}
	return out, commonprofile.Extract(merged), nil
}

// MigEnabled reports whether a merged profile turns MIG mode on. The MIG
// partition table itself is delivered separately (see LoadMigTable) — mode
// alone does not make a board MIG-capable on nvml-mock >= v0.2.
func MigEnabled(merged map[string]interface{}) bool {
	deviceDefaults, _ := merged["device_defaults"].(map[string]interface{})
	mig, _ := deviceDefaults["mig"].(map[string]interface{})
	mode, _ := mig["mode_current"].(string)
	return mode == "enabled"
}

// LoadMigTable reads the MIG profile-table ConfigMap for a profile
// ("mig-profiles-<profile>"), returning the raw table YAML for the per-pool
// mock ConfigMap's config.mig.yaml key. nvml-mock >= v0.2 (Mokka) resolves
// the table at load time via $MOCK_MIG_PROFILES_CONFIG or "as a sibling of
// the config path" — the per-pool ConfigMap mount at /etc/nvml-mock makes the
// rendered sibling file exactly that, no extra volume or env needed.
//
// Returns (nil, nil) when the table ConfigMap does not exist: the pool runs
// with MIG mode on but no capability table, which the engine treats as a
// MIG-less board (same as upstream shipping mock-nvml-config-<product>.yaml
// without a .mig variant).
func LoadMigTable(kube kubernetes.Interface, namespace, profileName string) ([]byte, error) {
	cmName := commonprofile.CmMigProfilesPrefix + profileName
	cm, err := kube.CoreV1().ConfigMaps(namespace).Get(
		context.TODO(), cmName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading MIG profile table %q: %w", cmName, err)
	}

	data, ok := cm.Data[commonprofile.CmMigProfilesKey]
	if !ok {
		return nil, fmt.Errorf("MIG profile table ConfigMap %q missing key %q", cmName, commonprofile.CmMigProfilesKey)
	}
	return []byte(data), nil
}
