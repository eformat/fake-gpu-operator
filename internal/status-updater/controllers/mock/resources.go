package mock

import (
	"fmt"

	"github.com/run-ai/fake-gpu-operator/internal/common/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	// ConfigHashAnnotation is stamped on the DaemonSet pod template so a
	// ConfigMap content change forces a pod rollout.
	ConfigHashAnnotation = "fake-gpu-operator/config-hash"

	// configKey is the data key in the per-pool ConfigMap. Mirrors
	// nvml-mock's chart, which mounts the file at /etc/nvml-mock/config.yaml.
	configKey = "config.yaml"

	// migProfilesKey is the optional second data key in the per-pool
	// ConfigMap: the product's MIG partition table, rendered under the name
	// the engine derives for the sibling of config.yaml — config.mig.yaml
	// (extension replacement; nvml-mock >= v0.2 / Mokka resolves it there
	// when MOCK_MIG_PROFILES_CONFIG is unset).
	migProfilesKey = "config.mig.yaml"
)

// resourceName produces the per-pool resource name shared by the DaemonSet
// and the ConfigMap (also used as the diff key).
func resourceName(pool string) string {
	return "nvml-mock-" + pool
}

// managedLabels returns the label set every controller-owned resource carries.
func managedLabels(pool string) map[string]string {
	return map[string]string{
		constants.LabelManagedBy: constants.LabelManagedByValue,
		constants.LabelComponent: constants.ComponentNvmlMock,
		constants.LabelPool:      pool,
	}
}

// BuildConfigMap produces the per-pool nvml-mock ConfigMap whose `config.yaml`
// key is mounted into the DaemonSet at /etc/nvml-mock/config.yaml. When
// migTableYAML is non-empty it is added under config.mig.yaml — the
// "sibling of the config path" nvml-mock >= v0.2 (Mokka) resolves the board's
// MIG partition table from at load time.
func BuildConfigMap(namespace, pool string, configYAML, migTableYAML []byte) *corev1.ConfigMap {
	data := map[string]string{
		configKey: string(configYAML),
	}
	if len(migTableYAML) > 0 {
		data[migProfilesKey] = string(migTableYAML)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(pool),
			Namespace: namespace,
			Labels:    managedLabels(pool),
		},
		Data: data,
	}
}

// BuildDaemonSetParams captures everything BuildDaemonSet needs.
type BuildDaemonSetParams struct {
	Namespace        string
	Pool             string
	NodePoolLabelKey string
	Image            string
	ImagePullPolicy  corev1.PullPolicy
	// LaunchContract selects the container contract: ContractLegacy
	// (nvml-mock v0.1.0, default) or ContractMokka (nvml-mock >= v0.2).
	// Must match the pinned image — the two contracts are incompatible.
	LaunchContract string
	// GpuCount and DriverVersion are extracted from the resolved profile and
	// stamped onto the container as literal env vars. nvml-mock's setup.sh
	// requires both as integers/strings; the upstream chart sets them the same
	// way (no ConfigMap indirection) since the ConfigMap only carries the
	// profile YAML at /config/config.yaml.
	GpuCount      int
	DriverVersion string
	// ConfigHash is the SHA of the ConfigMap's config.yaml + mig-profiles.yaml
	// content; stamped on the pod template so changes force a rolling restart.
	ConfigHash string
}

// Launch contract values for BuildDaemonSetParams.LaunchContract. Empty
// selects the legacy contract (the pre-Mokka default).
const (
	ContractLegacy = "legacy"
	ContractMokka  = "mokka"
)

// BuildDaemonSet produces the per-pool nvml-mock DaemonSet.
//
// Two container contracts are supported, selected by LaunchContract:
//
//   - ContractLegacy (default): mirrors upstream nvml-mock v0.1.0
//     templates/daemonset.yaml — /scripts/entrypoint.sh with the
//     toolkit-ready marker wrapper.
//   - ContractMokka: mirrors upstream nvml-mock >= v0.2 (Mokka)
//     deployments/nvml-mock/helm/nvml-mock/templates/daemonset.yaml —
//     /usr/local/bin/node-agent start. No entrypoint scripts exist in that
//     image and no marker wrapper is needed: the agent stages its own trees,
//     bind-mounts the driver root, and handles teardown via SIGTERM
//     (--shutdown-timeout). Container-runtime (NRI) integration is
//     deliberately NOT enabled — FGO's DRA pools serve claims through the
//     DRA driver's kubelet-plugin, not through containerd injection, and the
//     agent must never touch the node's containerd config.
//
// When bumping nvml-mock, re-read upstream's daemonset.yaml and reconcile
// the matching branch.
func BuildDaemonSet(p BuildDaemonSetParams) *appsv1.DaemonSet {
	labels := managedLabels(p.Pool)
	name := resourceName(p.Pool)

	container, volumes := legacyContainer(p)
	if p.LaunchContract == ContractMokka {
		container, volumes = mokkaContainer(p)
	}

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{ConfigHashAnnotation: p.ConfigHash},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "nvml-mock",
					NodeSelector:       map[string]string{p.NodePoolLabelKey: p.Pool},
					Containers:         []corev1.Container{container},
					Volumes:            volumes,
				},
			},
		},
	}
}

// legacyContainer builds the nvml-mock v0.1.0 container spec.
func legacyContainer(p BuildDaemonSetParams) (corev1.Container, []corev1.Volume) {
	name := resourceName(p.Pool)
	container := corev1.Container{
		Name:            "nvml-mock",
		Image:           p.Image,
		ImagePullPolicy: p.ImagePullPolicy,
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
		// Background upstream's entrypoint.sh, wait for its setup.sh to
		// signal completion (creating /run/nvidia/driver as a symlink
		// near its end), touch /run/nvidia/validations/toolkit-ready,
		// then `wait` to inherit entrypoint.sh's sleep-infinity. Without
		// the marker, gpu-operator operand pods (device-plugin, GFD)
		// block at Init:0/1 forever and `nvidia.com/gpu` never becomes
		// allocatable. We do this in the container's own process tree
		// (rather than a postStart hook) to avoid kubelet→containerd
		// gRPC failures during the toolkit DS's containerd reload.
		// Tracked upstream at NVIDIA/k8s-test-infra#346; drop this
		// wrapper when the marker write lands in nvml-mock's setup.sh.
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			"/scripts/entrypoint.sh & ENTRY=$!; " +
				"while ! [ -L /host/run/nvidia/driver ] && kill -0 $ENTRY 2>/dev/null; do sleep 1; done; " +
				"[ -L /host/run/nvidia/driver ] && mkdir -p /host/run/nvidia/validations && touch /host/run/nvidia/validations/toolkit-ready; " +
				"wait $ENTRY",
		},
		Lifecycle: &corev1.Lifecycle{
			// Deliberately does NOT remove the toolkit-ready marker:
			// on DaemonSet recreate the old pod's preStop races with
			// the new pod's setup wrapper (both touch the same host
			// path) and an `rm` here can blow away the marker the new
			// pod just wrote. The marker is sticky-positive — leaving
			// it on shutdown is harmless because operand pods that
			// scheduled while we were running already passed their
			// init poll.
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{Command: []string{"/scripts/cleanup.sh"}},
			},
		},
		Env: []corev1.EnvVar{
			{Name: "GPU_COUNT", Value: fmt.Sprintf("%d", p.GpuCount)},
			{Name: "DRIVER_VERSION", Value: p.DriverVersion},
			{
				Name:      "NODE_NAME",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "host-nvml-mock", MountPath: "/host/var/lib/nvml-mock"},
			{Name: "gpu-config", MountPath: "/etc/nvml-mock"},
			{Name: "host-cdi", MountPath: "/host/var/run/cdi"},
			{Name: "host-run-nvidia", MountPath: "/host/run/nvidia"},
		},
	}
	volumes := []corev1.Volume{
		{
			Name: "host-nvml-mock",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/lib/nvml-mock",
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		},
		{
			Name: "gpu-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
				},
			},
		},
		{
			Name: "host-cdi",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/run/cdi",
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		},
		{
			Name: "host-run-nvidia",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/run/nvidia",
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		},
	}
	return container, volumes
}

// mokkaContainer builds the nvml-mock >= v0.2 (Mokka) container spec,
// mirroring upstream deployments/nvml-mock/helm/nvml-mock/templates/daemonset.yaml
// reduced to what FGO's DRA pools need: the node-agent only — no NRI
// sidecar, no allocation-watcher, no IMEX binaries init, no fabricmanager
// (A100 and narrower), and no --topology (single-node pools stage no cluster
// NVLink document). The MIG partition table needs no env: the per-pool
// ConfigMap carries it as mig-profiles.yaml, the sibling of config.yaml the
// engine resolves it from.
func mokkaContainer(p BuildDaemonSetParams) (corev1.Container, []corev1.Volume) {
	name := resourceName(p.Pool)
	container := corev1.Container{
		Name:            "nvml-mock",
		Image:           p.Image,
		ImagePullPolicy: p.ImagePullPolicy,
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
		Command: []string{
			"/usr/local/bin/node-agent", "start",
			"--config=/etc/nvml-mock/config.yaml",
			"--host-root=/host",
			// The agent's health endpoints live here in upstream's chart;
			// 9090 is the agent's default and collides with nothing, but
			// pin the upstream port so probes and exec'd tools agree.
			"--health-addr=:9091",
			"--log-level=info",
			// Teardown (claim revoke, tree discard) gets the upstream
			// grace period; the DS pod's terminationGracePeriodSeconds
			// stays the k8s default and only needs to exceed this.
			"--shutdown-timeout=30s",
		},
		Env: []corev1.EnvVar{
			{Name: "GPU_COUNT", Value: fmt.Sprintf("%d", p.GpuCount)},
			{Name: "DRIVER_VERSION", Value: p.DriverVersion},
			{
				Name:      "NODE_NAME",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
			},
			// The config the agent renders from, and what exec'd nvidia-smi
			// reads. The engine resolves the MIG table as the sibling file
			// (/etc/nvml-mock/mig-profiles.yaml) when this is unset.
			{Name: "MOCK_NVML_CONFIG", Value: "/etc/nvml-mock/config.yaml"},
			// Overrides live under the driver tree; requires the mokka-root
			// mount at /var/lib/nvml-mock below.
			{Name: "MOCK_NVML_OVERRIDES", Value: "/var/lib/nvml-mock/driver/config/overrides.yaml"},
			// Host-absolute roots for the LD_PRELOAD path-rewriting shims
			// (exec'd tools only; the Go agent issues syscalls directly).
			{Name: "MOCK_PCI_ROOT", Value: "/var/lib/nvml-mock"},
			{Name: "MOCK_IB_ROOT", Value: "/var/lib/nvml-mock/ib"},
			{Name: "MOCK_IB", Value: "off"},
			{Name: "MOCK_IB_PING_FABRIC", Value: "0"},
			{
				Name:  "LD_PRELOAD",
				Value: "/usr/local/lib/libibmockumad.so.1:/usr/local/lib/libibmockverbs.so.1:/usr/local/lib/libibmocksys.so.1:/usr/local/lib/libmockfs.so.1",
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			// The simulated driver tree, at the path its contents name
			// (MOCK_PCI_ROOT / MOCK_NVML_OVERRIDES are host-absolute).
			{Name: "mokka-root", MountPath: "/var/lib/nvml-mock"},
			// The same tree at the agent's --host-root view.
			{Name: "mokka-root", MountPath: "/host/var/lib/nvml-mock"},
			// gpudriver.Apply bind-mounts the driver root here. Bidirectional
			// so the mount propagates to consumer workloads that mounted
			// this path before the agent did.
			{
				Name:             "driver-root-link",
				MountPath:        "/host/run/nvidia",
				MountPropagation: ptr.To(corev1.MountPropagationBidirectional),
			},
			// cdi.Apply writes the CDI specs the kubelet-plugin consumes.
			// Host path /var/run/cdi == /run/cdi on the host; the agent
			// writes under --host-root/run/cdi.
			{Name: "cdi-specs", MountPath: "/host/run/cdi"},
			{Name: "gpu-config", MountPath: "/etc/nvml-mock", ReadOnly: true},
			// The agent copies the node's DMI attributes into the rendered
			// tree, limiting CPU affinity to the node's online CPUs.
			{Name: "node-sysfs", MountPath: "/host/sys", ReadOnly: true},
			// The feature file NFD's local source turns into the pci-10de
			// node label.
			{Name: "nfd-features", MountPath: "/host/etc/kubernetes/node-feature-discovery/features.d"},
		},
	}
	volumes := []corev1.Volume{
		{
			Name: "mokka-root",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/lib/nvml-mock",
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		},
		{
			Name: "gpu-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
				},
			},
		},
		{
			Name: "cdi-specs",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/var/run/cdi",
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		},
		{
			Name: "driver-root-link",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/run/nvidia",
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		},
		{
			Name: "node-sysfs",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/sys",
					Type: ptr.To(corev1.HostPathDirectory),
				},
			},
		},
		{
			Name: "nfd-features",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/etc/kubernetes/node-feature-discovery/features.d",
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		},
	}
	return container, volumes
}
