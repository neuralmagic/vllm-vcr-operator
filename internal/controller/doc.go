// +kubebuilder:rbac:groups=vcr.opendatahub-io,resources=vcrgpuclusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=vcr.opendatahub-io,resources=vcrgpuclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vcr.opendatahub-io,resources=vcrgpuprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=vcr.opendatahub-io,resources=vcrgpuprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Package controller reconciles the cluster-scoped VcrGpuCluster: the
// reconciled profile catalog published in status, and the device-plugin
// DaemonSet that advertises synthetic GPUs on simulation nodes.
//
// # What this package owns
//
//   - The singleton VcrGpuCluster (name "default") and its status:
//     supportedVLLMVersion, the resolved profile catalog, and the Ready,
//     ProfilesReconciled and DevicePluginReady conditions.
//   - The synthetic-GPU device-plugin DaemonSet and the ConfigMap that carries
//     its device count file. Both live in the operator namespace and are owned
//     by the cluster-scoped VcrGpuCluster, so deleting the environment
//     deletes them.
//
// It never mutates Pods. Admission pins the reconciled profile digest on each
// admitted Pod, so a profile change in the environment cannot retrofit a
// running Pod: restarting the workload is what applies a new profile.
//
// # Profile reconciliation
//
// Profile metadata is resolved outside admission so that the webhook never
// contacts a registry. For every spec.profiles[].image this package resolves the
// image to a digest, parses the metadata with internal/profile, and publishes
// the result in status.profiles. Entries that were already reconciled are
// republished verbatim: a resolved digest is stable, so a tag move cannot
// silently change the profile an admitted Pod pinned, and no registry call is
// made when nothing changed.
//
// Pull credentials come from Secrets in the operator namespace named by
// spec.imagePullSecrets and by a profile's pullSecretName override. Only the
// registry host is ever used as a map key, and no credential value appears in
// the returned errors, in status, in conditions, or in events.
//
// # Device-plugin placement
//
// The DaemonSet selects nodes with spec.simulationNodes.nodeSelector. Nodes that
// already advertise nvidia.com/gpu from another provider are excluded
// declaratively: the rendered Pod has a required node affinity that rejects any
// node carrying one of RealGPUProviderLabels, the labels GPU feature discovery
// and the NVIDIA device plugin put on real GPU nodes. Excluded nodes are also
// reported in the DevicePluginReady message so the exclusion is observable, and
// no node labels are ever written by the operator.
package controller
