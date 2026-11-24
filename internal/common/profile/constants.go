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
	// The key name matters twice: the per-pool mock ConfigMap is mounted at
	// /etc/nvml-mock and the engine resolves the table by replacing the
	// config's extension — config.yaml -> config.mig.yaml — so the per-pool
	// ConfigMap carries the table under this exact key for the sibling
	// lookup to find it. The table's own ConfigMap uses the same key for
	// symmetry.
	CmMigProfilesKey = "config.mig.yaml"

	// LabelMigProfiles marks MIG profile-table ConfigMaps (chart-owned,
	// read by the mock controller by name). Deliberately NOT the mock
	// controller's managed label pair — the reconciler prunes anything
	// matching managed-by+component=nvml-mock that isn't in its desired
	// set, and the tables are chart-managed like the profiles themselves.
	LabelMigProfiles = "fake-gpu-operator/mig-profiles"
)
