package vllm

import (
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// fileResolver answers "what is the content of this file inside the container?"
// using only the Pod spec and the content the caller could resolve.
//
// A file can be read when a volume mount covers its path and the content of that
// volume is available in Invocation.Files; without both halves the answer is
// unknown. The operator never reads ConfigMaps, Secrets, or PersistentVolumes
// during resolution, and never executes the container to discover meaning.
type fileResolver struct {
	mounts  []corev1.VolumeMount
	volumes map[string]corev1.Volume
	files   map[string][]byte
}

func newFileResolver(inv Invocation) *fileResolver {
	return &fileResolver{
		mounts:  inv.Container.VolumeMounts,
		volumes: inv.Volumes,
		files:   inv.Files,
	}
}

// read returns the content of the absolute container path wanted by setting.
//
// The file must be backed by a volume mount, and the content of that volume must
// be available in Invocation.Files. A path that is not mounted may exist in the
// image, which the operator cannot inspect, so it is rejected rather than assumed.
func (r *fileResolver) read(containerPath string, setting Setting) ([]byte, *Rejection) {
	mount, ok := r.mountFor(containerPath)
	if ok {
		if content, resolved := r.files[containerPath]; resolved {
			return content, nil
		}
	}
	if !ok {
		return nil, &Rejection{
			Setting: setting,
			Value:   containerPath,
			Reason:  "the file is not backed by a volume mount in the Pod spec, so its content is not resolvable",
		}
	}
	volume, ok := r.volumes[mount.Name]
	if !ok {
		return nil, &Rejection{
			Setting: setting,
			Value:   containerPath,
			Reason:  fmt.Sprintf("volume %q is mounted but not defined in the Pod spec", mount.Name),
		}
	}
	return nil, &Rejection{
		Setting: setting,
		Value:   containerPath,
		Reason:  fmt.Sprintf("%s content is not available to admission, so the value cannot be resolved", volumeSourceKind(volume)),
	}
}

// mountFor returns the volume mount covering containerPath, preferring the
// longest mount path.
func (r *fileResolver) mountFor(containerPath string) (corev1.VolumeMount, bool) {
	var best corev1.VolumeMount
	found := false
	for _, m := range r.mounts {
		if !pathWithin(containerPath, m.MountPath) {
			continue
		}
		if !found || len(m.MountPath) > len(best.MountPath) {
			best, found = m, true
		}
	}
	return best, found
}

// pathWithin reports whether p is mountPath or lies below it.
func pathWithin(p, mountPath string) bool {
	cleanMount := path.Clean(mountPath)
	cleanPath := path.Clean(p)
	if cleanPath == cleanMount {
		return true
	}
	return strings.HasPrefix(cleanPath, strings.TrimSuffix(cleanMount, "/")+"/")
}

// volumeSourceKind names the Pod volume source for rejection messages.
func volumeSourceKind(v corev1.Volume) string {
	switch {
	case v.ConfigMap != nil:
		return "ConfigMap volume"
	case v.Secret != nil:
		return "Secret volume"
	case v.Projected != nil:
		return "projected volume"
	case v.EmptyDir != nil:
		return "emptyDir volume"
	case v.HostPath != nil:
		return "hostPath volume"
	case v.PersistentVolumeClaim != nil:
		return "persistent volume claim"
	case v.DownwardAPI != nil:
		return "downward API volume"
	default:
		return "volume"
	}
}
