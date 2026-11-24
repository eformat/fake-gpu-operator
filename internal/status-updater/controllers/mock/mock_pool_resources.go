package mock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/run-ai/fake-gpu-operator/internal/common/constants"
	commonprofile "github.com/run-ai/fake-gpu-operator/internal/common/profile"
	"github.com/run-ai/fake-gpu-operator/internal/common/topology"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// ReconcileParams holds the controller's runtime configuration.
type ReconcileParams struct {
	Namespace string
	// Image is the full nvml-mock image reference (e.g.
	// "ghcr.io/nvidia/nvml-mock:v0.1.0"), supplied via the NVML_MOCK_IMAGE
	// env var the chart plumbs from values.yaml's nvmlMock.image.repository
	// + tag. Every per-pool DaemonSet gets the same image — there's no
	// per-pool image override mechanism by design.
	Image           string
	ImagePullPolicy corev1.PullPolicy
	// LaunchContract selects the container spec the per-pool DaemonSets get:
	//   "legacy" (default) — nvml-mock v0.1.0: /scripts/entrypoint.sh driven
	//     by the toolkit-ready wrapper.
	//   "mokka" — nvml-mock >= v0.2: /usr/local/bin/node-agent start, the
	//     upstream Mokka chart's contract (no entrypoint scripts, no marker
	//     wrapper; the agent stages and tears down its own trees).
	// Supplied via NVML_MOCK_LAUNCH_CONTRACT. Bumping the image across the
	// v0.1/v0.2 boundary requires flipping this — see BuildDaemonSet.
	LaunchContract string
}

// ComputeMockPoolResources walks all mock pools in the topology config and
// produces the per-pool ConfigMap + DaemonSet pairs the controller should
// ensure exist. Pools are iterated in sorted order so the returned slices
// are deterministic across calls.
func ComputeMockPoolResources(
	kube kubernetes.Interface,
	cfg *topology.ClusterConfig,
	params ReconcileParams,
) ([]*corev1.ConfigMap, []*appsv1.DaemonSet, error) {
	poolNames := make([]string, 0, len(cfg.NodePools))
	for name := range cfg.NodePools {
		poolNames = append(poolNames, name)
	}
	sort.Strings(poolNames)

	pullPolicy := params.ImagePullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullAlways
	}

	var configMaps []*corev1.ConfigMap
	var daemonSets []*appsv1.DaemonSet
	for _, name := range poolNames {
		pool := cfg.NodePools[name]
		if pool.Gpu.Backend != constants.BackendMock {
			continue
		}

		merged, err := RenderMerged(kube, params.Namespace, pool.Gpu)
		if err != nil {
			return nil, nil, fmt.Errorf("rendering config for pool %q: %w", name, err)
		}
		configYAML, err := yaml.Marshal(merged)
		if err != nil {
			return nil, nil, fmt.Errorf("serializing config for pool %q: %w", name, err)
		}
		spec := commonprofile.Extract(merged)

		// Pools whose profile turns MIG mode on also get the product's MIG
		// profile table, rendered as the sibling file (mig-profiles.yaml)
		// next to config.yaml that nvml-mock >= v0.2 resolves it from.
		var migTableYAML []byte
		if MigEnabled(merged) {
			migTableYAML, err = LoadMigTable(kube, params.Namespace, pool.Gpu.Profile)
			if err != nil {
				return nil, nil, fmt.Errorf("loading MIG table for pool %q: %w", name, err)
			}
		}

		configMaps = append(configMaps, BuildConfigMap(params.Namespace, name, configYAML, migTableYAML))
		daemonSets = append(daemonSets, BuildDaemonSet(BuildDaemonSetParams{
			Namespace:        params.Namespace,
			Pool:             name,
			NodePoolLabelKey: cfg.NodePoolLabelKey,
			Image:            params.Image,
			ImagePullPolicy:  pullPolicy,
			LaunchContract:   params.LaunchContract,
			GpuCount:         spec.GpuCount,
			DriverVersion:    spec.DriverVersion,
			ConfigHash:       configHash(configYAML, migTableYAML),
		}))
	}
	return configMaps, daemonSets, nil
}

// configHash produces a hex SHA-256 of the rendered config YAML and the
// optional MIG table. Stamped on the DaemonSet pod template via the
// ConfigHashAnnotation so a CM content change forces a rolling restart.
func configHash(body []byte, migTable []byte) string {
	h := sha256.New()
	h.Write(body)
	if len(migTable) > 0 {
		h.Write([]byte("\n---\n"))
		h.Write(migTable)
	}
	return hex.EncodeToString(h.Sum(nil))
}
