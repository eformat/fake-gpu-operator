package profile

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GpuSpec holds the extracted GPU specification fields that FGO components need.
type GpuSpec struct {
	GpuProduct    string
	GpuMemory     int // MiB
	GpuCount      int
	Architecture  string
	DriverVersion string
	CudaVersion   string
}

// Load reads a GPU profile ConfigMap by name and returns the parsed profile data.
// The ConfigMap is expected to be named "gpu-profile-{name}" in the given namespace.
func Load(kubeClient kubernetes.Interface, namespace, profileName string) (map[string]interface{}, error) {
	cmName := CmNamePrefix + profileName
	cm, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(
		context.TODO(), cmName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to load GPU profile %q: %w", profileName, err)
	}

	data, ok := cm.Data[CmProfileKey]
	if !ok {
		return nil, fmt.Errorf("GPU profile ConfigMap %q missing key %q", cmName, CmProfileKey)
	}

	var profile map[string]interface{}
	if err := yaml.Unmarshal([]byte(data), &profile); err != nil {
		return nil, fmt.Errorf("failed to parse GPU profile %q: %w", profileName, err)
	}

	return profile, nil
}

// Merge deep-merges overrides onto a base profile. Scalars are replaced;
// nested maps are recursively merged; lists are replaced — with one
// exception: the `devices` list merges by `index`, so a pool can override
// one board's block (a MIG mode flip, say) without restating every device's
// identity. That mirrors the engine's own per-device override semantics
// (mergeDeviceOverride), which is what will consume the merged config.
// Returns a new map (base is not modified).
func Merge(base, overrides map[string]interface{}) map[string]interface{} {
	if len(overrides) == 0 {
		return copyMap(base)
	}

	result := copyMap(base)
	for key, overrideVal := range overrides {
		baseVal, exists := result[key]
		if !exists {
			result[key] = overrideVal
			continue
		}

		if key == "devices" {
			if merged, mergedOK := mergeDevicesByIndex(baseVal, overrideVal); mergedOK {
				result[key] = merged
				continue
			}
		}

		baseMap, baseIsMap := baseVal.(map[string]interface{})
		overrideMap, overrideIsMap := overrideVal.(map[string]interface{})
		if baseIsMap && overrideIsMap {
			result[key] = Merge(baseMap, overrideMap)
		} else {
			result[key] = overrideVal
		}
	}

	return result
}

// mergeDevicesByIndex merges two device lists by their `index` field — the
// board coordinate the engine keys per-device overrides on. Base entries
// keep their identity (uuid, serial, pci); an override entry deep-merges
// over the base entry it names; override entries naming a board the base
// does not declare are appended verbatim. Not-ok means either side is not a
// list of maps with integer indices, and the caller falls back to list
// replacement.
func mergeDevicesByIndex(baseVal, overrideVal interface{}) ([]interface{}, bool) {
	baseList, baseOK := baseVal.([]interface{})
	overrideList, overrideOK := overrideVal.([]interface{})
	if !baseOK || !overrideOK {
		return nil, false
	}

	baseByIndex := map[int]int{}
	for i, entry := range baseList {
		m, ok := entry.(map[string]interface{})
		if !ok {
			return nil, false
		}
		idx, ok := toInt(m["index"])
		if !ok {
			return nil, false
		}
		baseByIndex[idx] = i
	}

	merged := make([]interface{}, len(baseList))
	copy(merged, baseList)
	for _, entry := range overrideList {
		m, ok := entry.(map[string]interface{})
		if !ok {
			return nil, false
		}
		idx, ok := toInt(m["index"])
		if !ok {
			return nil, false
		}
		if i, exists := baseByIndex[idx]; exists {
			merged[i] = Merge(baseList[i].(map[string]interface{}), m)
			continue
		}
		merged = append(merged, entry)
	}
	return merged, true
}

// toInt converts the JSON numbers a profile map carries (float64 from
// encoding/json, int from mapstructure-free literals) into an int.
func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	}
	return 0, false
}

// Extract pulls FGO-relevant fields from a resolved profile into a GpuSpec.
// Missing fields are returned as zero values.
func Extract(profile map[string]interface{}) GpuSpec {
	var spec GpuSpec
	if profile == nil {
		return spec
	}

	if dd, ok := getMap(profile, "device_defaults"); ok {
		spec.GpuProduct, _ = dd["name"].(string)
		spec.Architecture, _ = dd["architecture"].(string)

		if mem, ok := getMap(dd, "memory"); ok {
			spec.GpuMemory = toMiB(mem["total_bytes"])
		}
	}

	if sys, ok := getMap(profile, "system"); ok {
		spec.DriverVersion, _ = sys["driver_version"].(string)
		spec.CudaVersion, _ = sys["cuda_version"].(string)
	}

	spec.GpuCount = DeviceCount(profile)

	return spec
}

func copyMap(m map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}

func getMap(m map[string]interface{}, key string) (map[string]interface{}, bool) {
	val, ok := m[key]
	if !ok {
		return nil, false
	}
	asMap, ok := val.(map[string]interface{})
	return asMap, ok
}

// DeviceCount returns the number of GPU devices in a profile.
// It checks for an explicit "device_count" field first, then falls back to
// counting the "devices" list. Note: device_count: 0 is treated as unset
// (falls back to len(devices)), since zero GPUs is not a valid pool config.
func DeviceCount(profile map[string]interface{}) int {
	var count int
	switch n := profile["device_count"].(type) {
	case int:
		count = n
	case int64:
		count = int(n)
	case float64:
		count = int(n)
	}
	if count > 0 {
		return count
	}
	if devices, ok := profile["devices"].([]interface{}); ok {
		return len(devices)
	}
	return 0
}

// toMiB converts a bytes value (which may be int, int64, or float64 from YAML
// unmarshaling) to MiB.
func toMiB(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n / (1024 * 1024)
	case int64:
		return int(n / (1024 * 1024))
	case float64:
		return int(n / (1024 * 1024))
	default:
		return 0
	}
}
