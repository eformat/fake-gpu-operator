package profile

const (
	// LabelGpuProfile is the label used to identify GPU profile ConfigMaps.
	LabelGpuProfile = "fake-gpu-operator/gpu-profile"

	// CmProfileKey is the data key inside profile ConfigMaps.
	CmProfileKey = "profile.yaml"

	// CmNamePrefix is the prefix for profile ConfigMap names.
	CmNamePrefix = "gpu-profile-"

	// CmMigProfilesPrefix is the prefix for MIG profile-table ConfigMap
	// names (nvml-mock >= v0.2 / Mokka). A table is the per-product MIG
	// partition geometry the engine attaches to
	// device_defaults.mig.supported_profiles at load time; shipping it as a
	// ConfigMap keeps it user-overridable like the profiles themselves.
	CmMigProfilesPrefix = "mig-profiles-"

	// CmMigProfilesKey is the data key inside MIG profile-table ConfigMaps.
	// The key name matters: the per-pool mock ConfigMap is mounted at
	// /etc/nvml-mock, and the engine resolves the table "as a sibling of the
	// config path" — so the per-pool ConfigMap carries the table under this
	// key (rendered next to config.yaml) even though the table's own
	// ConfigMap uses it too.
	CmMigProfilesKey = "mig-profiles.yaml"
)
