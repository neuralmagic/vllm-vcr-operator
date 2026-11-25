package runtime

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

// acceleratorHostPaths are host directories that expose real accelerator devices or
// their driver userspace. The simulation nodes have neither, so a workload volume
// backed by one of them is removed together with every mount that references it.
var acceleratorHostPaths = []string{
	names.DevicePluginDir,
	"/usr/local/nvidia",
	"/usr/lib64/libnvidia",
	"/usr/lib/x86_64-linux-gnu/libnvidia",
	"/var/lib/nvidia",
	"/run/nvidia",
	"/dev/nvidia",
}

// removeAcceleratorVolumes removes the workload's accelerator device volumes and
// their mounts.
//
// The synthetic device plugin replaces the real one, so a hostPath mount of the
// device-plugin directory or of the driver libraries would either fail to exist on a
// simulation node or shadow the synthetic advertisement. Unrelated hostPaths (for
// example a model cache) are preserved.
func removeAcceleratorVolumes(pod *corev1.Pod) {
	removed := make(map[string]struct{}, len(pod.Spec.Volumes))
	kept := make([]corev1.Volume, 0, len(pod.Spec.Volumes))
	for _, volume := range pod.Spec.Volumes {
		if isAcceleratorVolume(volume) {
			removed[volume.Name] = struct{}{}
			continue
		}
		kept = append(kept, volume)
	}
	if len(removed) == 0 {
		return
	}
	pod.Spec.Volumes = kept

	for i := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[i].VolumeMounts = withoutMounts(pod.Spec.InitContainers[i].VolumeMounts, removed)
	}
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].VolumeMounts = withoutMounts(pod.Spec.Containers[i].VolumeMounts, removed)
	}
}

// isAcceleratorVolume reports whether a volume exposes accelerator devices or their
// driver userspace.
func isAcceleratorVolume(volume corev1.Volume) bool {
	if volume.HostPath == nil {
		return false
	}
	path := strings.ToLower(strings.TrimSpace(volume.HostPath.Path))
	if path == "" {
		return false
	}
	for _, prefix := range acceleratorHostPaths {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// withoutMounts returns the mounts that do not reference a removed volume.
func withoutMounts(mounts []corev1.VolumeMount, removed map[string]struct{}) []corev1.VolumeMount {
	if len(mounts) == 0 {
		return mounts
	}
	kept := make([]corev1.VolumeMount, 0, len(mounts))
	for _, mount := range mounts {
		if _, drop := removed[mount.Name]; drop {
			continue
		}
		kept = append(kept, mount)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}
