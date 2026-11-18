package controller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vcrv1alpha1 "github.com/opendatahub-io/vllm-vcr-operator/api/v1alpha1"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/registry"
)

// profileReconciliation is the outcome of reconciling spec.profiles.
type profileReconciliation struct {
	// published is the status.profiles content, in spec order so an unchanged
	// reconcile produces an identical status.
	published []vcrv1alpha1.ResolvedProfile
	// retry is true when at least one entry failed for a reason a later attempt
	// can fix without a spec change: a registry outage or an unreadable pull
	// Secret.
	retry bool
	// unusable lists per-image reasons that only a spec change can fix.
	unusable []string
	// unavailable lists per-image reasons that are retried automatically.
	unavailable []string
	// configured is the number of profile images in the spec.
	configured int
}

// condition renders the ProfilesReconciled condition.
func (p profileReconciliation) condition() metav1.Condition {
	switch {
	case len(p.unusable) > 0:
		// A metadata contract violation outranks a retryable failure: the catalog
		// cannot be completed until the configured image changes.
		return metav1.Condition{
			Status:  metav1.ConditionFalse,
			Reason:  vcrv1alpha1.ReasonProfileUnusable,
			Message: joinReasons(p.unusable, p.unavailable),
		}
	case len(p.unavailable) > 0:
		return metav1.Condition{
			Status:  metav1.ConditionFalse,
			Reason:  vcrv1alpha1.ReasonRegistryUnavailable,
			Message: joinReasons(p.unavailable, nil),
		}
	case p.configured == 0:
		return metav1.Condition{
			Status: metav1.ConditionTrue,
			Reason: vcrv1alpha1.ReasonAllProfilesUsable,
			// An empty catalog is not an error, but it makes every GPU workload
			// unadmittable, so say so instead of claiming a vacuous success.
			Message: "no profile images are configured: admission rejects GPU workloads until spec.profiles references a profile image",
		}
	default:
		return metav1.Condition{
			Status:  metav1.ConditionTrue,
			Reason:  vcrv1alpha1.ReasonAllProfilesUsable,
			Message: fmt.Sprintf("%d profile image(s) reconciled: admission pins the published digests", len(p.published)),
		}
	}
}

// joinReasons combines per-image reasons, keeping spec order.
func joinReasons(primary, secondary []string) string {
	reasons := make([]string, 0, len(primary)+len(secondary))
	reasons = append(reasons, primary...)
	reasons = append(reasons, secondary...)
	return strings.Join(reasons, "; ")
}

// reconcileProfiles resolves every configured profile image into a published
// status entry.
//
// Failure of one profile never erases another: every configured image keeps an
// entry, and an unusable one carries the reason it cannot be selected. The
// catalog is built from all entries so a recorded identity that resolves to two
// digests is reported here rather than at admission time.
func (r *SimulationEnvironmentReconciler) reconcileProfiles(ctx context.Context, env *vcrv1alpha1.SimulationEnvironment) profileReconciliation {
	result := profileReconciliation{configured: len(env.Spec.Profiles)}
	seen := make(map[string]struct{}, len(env.Spec.Profiles))
	candidates := make([]profile.Candidate, 0, len(env.Spec.Profiles))

	for _, source := range env.Spec.Profiles {
		if _, duplicate := seen[source.Image]; duplicate {
			// status.profiles is keyed by image, so a duplicate image would be a
			// duplicate map key. Report it instead of publishing one of them.
			result.unusable = append(result.unusable, fmt.Sprintf(
				"profile image %q is listed more than once: status.profiles is keyed by image", source.Image))
			continue
		}
		seen[source.Image] = struct{}{}

		entry, candidate, reason, retryable := r.reconcileProfile(ctx, env, source)
		result.published = append(result.published, entry)
		candidates = append(candidates, candidate)
		switch {
		case reason == "":
		case retryable:
			result.retry = true
			result.unavailable = append(result.unavailable, reason)
		default:
			result.unusable = append(result.unusable, reason)
		}
	}

	if _, err := profile.NewCatalog(candidates); err != nil {
		result.unusable = append(result.unusable, err.Error())
	}
	return result
}

// reconcileProfile resolves one configured profile image. It returns the entry to
// publish, the candidate for the catalog, a reason when the entry is unusable,
// and whether the failure is retryable.
func (r *SimulationEnvironmentReconciler) reconcileProfile(
	ctx context.Context,
	env *vcrv1alpha1.SimulationEnvironment,
	source vcrv1alpha1.ProfileSource,
) (vcrv1alpha1.ResolvedProfile, profile.Candidate, string, bool) {
	if prior, ok := previouslyReconciled(env.Status.Profiles, source.Image); ok {
		// Republish the reconciled entry verbatim. Its digest is already pinned,
		// so re-resolving a tag could only change the profile a running
		// simulation was admitted with, and it would cost a registry round trip.
		if metadata, err := profile.Parse([]byte(prior.Metadata)); err == nil {
			return prior, profile.Candidate{
				Image:    prior.Image,
				Digest:   prior.Digest,
				Metadata: metadata,
			}, "", false
		}
	}

	credentials, err := r.pullCredentials(ctx, env, source)
	if err != nil {
		reason := fmt.Sprintf("profile image %q: %s", source.Image, err)
		return unusableProfile(source.Image, "", reason), profile.Candidate{Image: source.Image, Message: reason}, reason, true
	}

	resolved, err := r.NewResolver(registry.Options{Credentials: credentials.authenticate}).Resolve(ctx, source.Image)
	if err != nil {
		// A registry failure, including a rejected credential, is retryable:
		// registries flap and pull Secrets get created. The resolver guarantees
		// no credential material in the error.
		reason := err.Error()
		return unusableProfile(source.Image, "", reason), profile.Candidate{Image: source.Image, Message: reason}, reason, true
	}

	metadata, err := profile.Parse(resolved.Metadata)
	if err != nil {
		// The document resolved but violates the contract. Retrying cannot fix
		// it; the configured image has to change. Metadata is not published
		// because status.profiles carries only validated documents.
		reason := fmt.Sprintf("profile image %q: %s", source.Image, err)
		return unusableProfile(source.Image, resolved.Digest, reason),
			profile.Candidate{Image: source.Image, Digest: resolved.Digest, Message: reason}, reason, false
	}

	return vcrv1alpha1.ResolvedProfile{
			Image:        source.Image,
			Digest:       resolved.Digest,
			Metadata:     string(resolved.Metadata),
			ReconciledAt: metav1.NewTime(r.Now()),
		}, profile.Candidate{
			Image:    source.Image,
			Digest:   resolved.Digest,
			Metadata: metadata,
		}, "", false
}

// unusableProfile renders a published entry for a profile that cannot be used.
func unusableProfile(image, digest, message string) vcrv1alpha1.ResolvedProfile {
	return vcrv1alpha1.ResolvedProfile{Image: image, Digest: digest, Message: message}
}

// previouslyReconciled returns the usable status entry for image, if any.
func previouslyReconciled(published []vcrv1alpha1.ResolvedProfile, image string) (vcrv1alpha1.ResolvedProfile, bool) {
	for _, entry := range published {
		if entry.Image != image {
			continue
		}
		if entry.Message != "" || entry.Digest == "" || entry.Metadata == "" {
			return vcrv1alpha1.ResolvedProfile{}, false
		}
		return entry, true
	}
	return vcrv1alpha1.ResolvedProfile{}, false
}

// devicePluginCondition renders the DevicePluginReady condition from the observed
// DaemonSet state.
func devicePluginCondition(state DevicePluginState, reconciler *DevicePluginReconciler) metav1.Condition {
	location := fmt.Sprintf("%s/%s", reconciler.OperatorNamespace, names.DevicePluginName)
	exclusion := ""
	if len(state.ExcludedNodes) > 0 {
		exclusion = fmt.Sprintf(
			"; %d node(s) excluded because a real GPU provider already advertises %s there: %s",
			len(state.ExcludedNodes), names.SyntheticResourceName, strings.Join(state.ExcludedNodes, ", "))
	}

	if state.Desired > 0 && state.Ready == state.Desired {
		return metav1.Condition{
			Status: metav1.ConditionTrue,
			Reason: vcrv1alpha1.ReasonDaemonSetReady,
			Message: fmt.Sprintf("device plugin DaemonSet %s is ready on %d of %d node(s)%s",
				location, state.Ready, state.Desired, exclusion),
		}
	}
	return metav1.Condition{
		Status: metav1.ConditionFalse,
		Reason: vcrv1alpha1.ReasonDaemonSetNotReady,
		Message: fmt.Sprintf("device plugin DaemonSet %s is not ready: %d of %d node(s) ready%s",
			location, state.Ready, state.Desired, exclusion),
	}
}

// readyCondition summarises the other two conditions. The message is the failing
// condition's message so the reason for unreadiness is never hidden.
func readyCondition(status *vcrv1alpha1.SimulationEnvironmentStatus) metav1.Condition {
	profiles := meta.FindStatusCondition(status.Conditions, vcrv1alpha1.ConditionProfilesReconciled)
	if profiles == nil || profiles.Status != metav1.ConditionTrue {
		return metav1.Condition{
			Status:  metav1.ConditionFalse,
			Reason:  vcrv1alpha1.ReasonProfilesNotReconciled,
			Message: messageOf(profiles, "no ProfilesReconciled condition is published"),
		}
	}
	devicePlugin := meta.FindStatusCondition(status.Conditions, vcrv1alpha1.ConditionDevicePluginReady)
	if devicePlugin == nil || devicePlugin.Status != metav1.ConditionTrue {
		return metav1.Condition{
			Status:  metav1.ConditionFalse,
			Reason:  vcrv1alpha1.ReasonDevicePluginNotReady,
			Message: messageOf(devicePlugin, "no DevicePluginReady condition is published"),
		}
	}
	return metav1.Condition{
		Status: metav1.ConditionTrue,
		Reason: vcrv1alpha1.ReasonEnvironmentReady,
		Message: fmt.Sprintf("SimulationEnvironment is ready: %d profile image(s) published; %s",
			len(status.Profiles), devicePlugin.Message),
	}
}

// messageOf returns the condition message, or fallback when it is absent.
func messageOf(condition *metav1.Condition, fallback string) string {
	if condition == nil || condition.Message == "" {
		return fallback
	}
	return condition.Message
}
