package translator

import (
	"strings"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/plan"
)

// Accelerator placement data. These tables describe what a GPU-targeted Pod asks
// for so that internal/runtime can remove exactly that and keep everything else.
// They are data because the set is environment knowledge, not control flow, and
// because the acceptance tests assert the rewriting through it.
var (
	// defaultAcceleratorRuntimeClasses are container runtime classes that install
	// an accelerator runtime and cannot run on simulation nodes.
	defaultAcceleratorRuntimeClasses = []string{"nvidia", "nvidia-cdi"}

	// defaultAcceleratorNodeLabels are node labels that describe accelerator
	// hardware. A Pod requiring one of them cannot run on a simulation node.
	defaultAcceleratorNodeLabels = []string{
		names.SyntheticResourceName,
		"nvidia.com/gpu.product",
		"nvidia.com/gpu.machine",
		"nvidia.com/gpu.memory",
		"nvidia.com/gpu.count",
		"nvidia.com/gpu.mode",
		"nvidia.com/mig.config",
		"amd.com/gpu.product",
		"amd.com/gpu.memory",
		"cloud.google.com/gke-accelerator",
		"k8s.amazonaws.com/accelerator",
		"habana.ai/gaudi",
	}

	// defaultAcceleratorNodeLabelPrefixes are node label prefixes whose presence
	// means the label describes accelerator hardware.
	defaultAcceleratorNodeLabelPrefixes = []string{
		"nvidia.com/gpu.",
		"amd.com/gpu.",
		"gpu.intel.com/",
	}

	// defaultAcceleratorTolerationKeys are taint keys only accelerator nodes
	// carry. A workload toleration for one of them is GPU-only placement.
	defaultAcceleratorTolerationKeys = []string{
		names.SyntheticResourceName,
		"nvidia.com/gpu",
		"amd.com/gpu",
		"habana.ai/gaudi",
	}
)

// placementFor builds the placement rewrite for an admitted Pod.
func placementFor(env Environment) plan.Placement {
	values := []string{}
	if gpuType := strings.TrimSpace(env.GPUType); gpuType != "" {
		values = append(values, strings.ToLower(gpuType))
	}
	return plan.Placement{
		NodeSelector:                 copyStringMap(env.NodeSelector),
		Tolerations:                  copyTolerations(env.Tolerations),
		AcceleratorNodeLabels:        append([]string(nil), defaultAcceleratorNodeLabels...),
		AcceleratorNodeLabelPrefixes: append([]string(nil), defaultAcceleratorNodeLabelPrefixes...),
		AcceleratorNodeValues:        values,
		AcceleratorTolerationKeys:    append([]string(nil), defaultAcceleratorTolerationKeys...),
		RuntimeClasses:               append([]string(nil), env.RuntimeClasses...),
	}
}

// The values never leave the operator namespace except inside the projected copy
// the webhook creates, so only names are carried here.
func pullSecretsFor(env Environment, configuredImage string) []string {
	if override := strings.TrimSpace(env.ProfilePullSecrets[configuredImage]); override != "" {
		return []string{override}
	}
	secrets := make([]string, 0, len(env.PullSecrets))
	for _, name := range env.PullSecrets {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			secrets = append(secrets, trimmed)
		}
	}
	return dedupeStrings(secrets)
}
