package v1alpha1

// Condition types published on the VcrGpuCluster.
//
// They live in the API package rather than in the reconciler so that every reader
// of status — admission, the device plugin wiring, and the acceptance tests — names
// the same strings without importing the controller. A condition type is part of
// the published API surface: an operator consumer may match on it.
const (
	// ConditionReady is true only when every configured profile has usable
	// metadata and the device plugin runs on every node it is scheduled for. A
	// VcrGpuProfile reports the same type for its own resolution: either way, the
	// answer to "can simulated workloads run here?" is the same question.
	ConditionReady = "Ready"
	// ConditionProfilesReconciled reports whether status.profiles is a complete,
	// usable catalog for admission.
	ConditionProfilesReconciled = "ProfilesReconciled"
	// ConditionDevicePluginReady reports whether the synthetic device plugin is
	// advertising capacity on the simulation nodes.
	ConditionDevicePluginReady = "DevicePluginReady"
)

// Condition reasons. Each one names the action an operator must take, so a reason
// is a stable, documented vocabulary rather than a free-form message.
const (
	// ReasonEnvironmentReady: profiles and device plugin are both reconciled.
	ReasonEnvironmentReady = "EnvironmentReady"
	// ReasonProfilesNotReconciled: at least one profile is unusable, so
	// admission cannot build a catalog.
	ReasonProfilesNotReconciled = "ProfilesNotReconciled"
	// ReasonDevicePluginNotReady: the device plugin DaemonSet is not ready, so
	// synthetic GPUs are not advertised on every simulation node.
	ReasonDevicePluginNotReady = "DevicePluginNotReady"

	// ReasonAllProfilesUsable: every configured profile resolved and parsed.
	ReasonAllProfilesUsable = "AllProfilesUsable"
	// ReasonProfileUnusable: a profile image resolved but its metadata violates
	// the profile image contract. Only a spec change (a different image) fixes it.
	ReasonProfileUnusable = "ProfileUnusable"
	// ReasonRegistryUnavailable: a registry or pull-credential failure prevented
	// reconciliation. It is retried automatically.
	ReasonRegistryUnavailable = "RegistryUnavailable"

	// ReasonDaemonSetReady: the device plugin runs on every scheduled node.
	ReasonDaemonSetReady = "DaemonSetReady"
	// ReasonDaemonSetNotReady: the device plugin is missing or not ready on at
	// least one scheduled node.
	ReasonDaemonSetNotReady = "DaemonSetNotReady"

	// ReasonProfileResolved: the VcrGpuProfile's selection names an identity the
	// catalog offers, so admission pins the namespace to it.
	ReasonProfileResolved = "Resolved"
	// ReasonProfileNotOffered: the catalog publishes no such identity. Only a spec
	// change (an id the catalog lists) fixes it.
	ReasonProfileNotOffered = "NotOffered"
	// ReasonProfileSelectionUnusable: the catalog offers the identity but its
	// recording violates the profile image contract.
	ReasonProfileSelectionUnusable = "Unusable"
	// ReasonNoClusterConfigured: no VcrGpuCluster exists, so there is no catalog to
	// resolve against. It is an operator state, not a mistake in the selection.
	ReasonNoClusterConfigured = "NoCluster"
)
