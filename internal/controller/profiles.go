package controller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/profile"
	"github.com/neuralmagic/vllm-vcr-operator/internal/registry"
)

// failure is one reason a configured profile could not be published, together with
// whether a later attempt can fix it. Keeping the retryability beside its reason
// stops the two travelling as separate parallel slices.
type failure struct {
	// reason names the setting and the problem, never a credential value.
	reason string
	// retryable is true when a later attempt can succeed without a spec change: a
	// registry outage or an unreadable pull Secret. False means only a different
	// spec can fix it.
	retryable bool
}

// profileReconciliation is the outcome of reconciling spec.profiles.
type profileReconciliation struct {
	// published is the status.profiles content, in spec order so an unchanged
	// reconcile produces an identical status.
	published []vcrv1alpha1.ResolvedProfile
	// failures holds one entry per profile image that could not be published, in
	// spec order.
	failures []failure
	// configured is the number of profile images in the spec.
	configured int
}

// fail records one profile that could not be published.
func (p *profileReconciliation) fail(reason string, retryable bool) {
	p.failures = append(p.failures, failure{reason: reason, retryable: retryable})
}

// unusable returns the reasons only a spec change can fix.
func (p profileReconciliation) unusable() []string { return p.reasons(false) }

// unavailable returns the reasons that are retried automatically.
func (p profileReconciliation) unavailable() []string { return p.reasons(true) }

// retry reports whether any failure is retryable, which is what asks the work
// queue for another reconcile.
func (p profileReconciliation) retry() bool {
	for _, f := range p.failures {
		if f.retryable {
			return true
		}
	}
	return false
}

// detail joins every failure reason in spec order, so the message names everything
// that is wrong rather than only the first problem.
func (p profileReconciliation) detail() string {
	reasons := make([]string, 0, len(p.failures))
	for _, f := range p.failures {
		reasons = append(reasons, f.reason)
	}
	return strings.Join(reasons, "; ")
}

func (p profileReconciliation) reasons(retryable bool) []string {
	reasons := make([]string, 0, len(p.failures))
	for _, f := range p.failures {
		if f.retryable == retryable {
			reasons = append(reasons, f.reason)
		}
	}
	return reasons
}

// condition renders the ProfilesReconciled condition.
func (p profileReconciliation) condition() metav1.Condition {
	switch {
	case len(p.unusable()) > 0:
		// A metadata contract violation outranks a retryable failure: the catalog
		// cannot be completed until the configured image changes.
		return metav1.Condition{
			Status:  metav1.ConditionFalse,
			Reason:  vcrv1alpha1.ReasonProfileUnusable,
			Message: p.detail(),
		}
	case len(p.unavailable()) > 0:
		return metav1.Condition{
			Status:  metav1.ConditionFalse,
			Reason:  vcrv1alpha1.ReasonRegistryUnavailable,
			Message: p.detail(),
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

// reconcileProfiles resolves every configured profile image into a published
// status entry.
//
// Failure of one profile never erases another: every configured image keeps an
// entry, and an unusable one carries the reason it cannot be selected. The
// catalog is built from all entries so a recorded identity that resolves to two
// digests is reported here rather than at admission time.
func (r *VcrGpuClusterReconciler) reconcileProfiles(ctx context.Context, env *vcrv1alpha1.VcrGpuCluster) profileReconciliation {
	result := profileReconciliation{configured: len(env.Spec.Profiles)}
	seen := make(map[string]struct{}, len(env.Spec.Profiles))
	candidates := make([]profile.Candidate, 0, len(env.Spec.Profiles))

	for _, source := range env.Spec.Profiles {
		// A cluster names one registry; a profile entry names what to look for inside it.
		reference, err := registry.JoinReference(env.Spec.ProfileRegistry, source.Image)
		if err != nil {
			reason := err.Error()
			result.fail(reason, false)
			result.published = append(result.published, unusableProfile(source.Image, "", reason))
			candidates = append(candidates, profile.Candidate{Image: source.Image, Message: reason})
			continue
		}
		if reference != source.Image {
			source.Image = reference
		}

		if _, duplicate := seen[source.Image]; duplicate {
			// status.profiles is keyed by image, so a duplicate image would be a
			// duplicate map key. Report it instead of publishing one of them.
			result.fail(fmt.Sprintf(
				"profile image %q is listed more than once: status.profiles is keyed by image", source.Image), false)
			continue
		}
		seen[source.Image] = struct{}{}

		outcome := r.reconcileProfile(ctx, env, source)
		result.published = append(result.published, outcome.entry)
		candidates = append(candidates, outcome.candidate)
		if outcome.failure != nil {
			result.failures = append(result.failures, *outcome.failure)
		}
	}

	if _, err := profile.NewCatalog(candidates); err != nil {
		result.fail(err.Error(), false)
	}
	return result
}

// outcome is what reconciling one profile image produced: the entry to publish, the
// catalog candidate, and the failure when the entry is not usable.
type outcome struct {
	entry     vcrv1alpha1.ResolvedProfile
	candidate profile.Candidate
	// failure is nil when the entry was resolved and can be selected.
	failure *failure
}

// failedOutcome renders the outcome for a profile image that cannot be published.
func failedOutcome(entry vcrv1alpha1.ResolvedProfile, candidate profile.Candidate, reason string, retryable bool) outcome {
	return outcome{entry: entry, candidate: candidate, failure: &failure{reason: reason, retryable: retryable}}
}

// reconcileProfile resolves one configured profile image.
func (r *VcrGpuClusterReconciler) reconcileProfile(
	ctx context.Context,
	env *vcrv1alpha1.VcrGpuCluster,
	source vcrv1alpha1.ProfileSource,
) outcome {
	if prior, ok := previouslyReconciled(env.Status.Profiles, source.Image); ok {
		// Republish the reconciled entry verbatim. Its digest is already pinned,
		// so re-resolving a tag could only change the profile a running
		// simulation was admitted with, and it would cost a registry round trip.
		if metadata, err := profile.Parse([]byte(prior.Metadata)); err == nil {
			return outcome{entry: prior, candidate: profile.Candidate{
				Image:    prior.Image,
				Digest:   prior.Digest,
				Metadata: metadata,
			}}
		}
	}

	credentials, err := r.pullCredentials(ctx, env, source)
	if err != nil {
		reason := fmt.Sprintf("profile image %q: %s", source.Image, err)
		return failedOutcome(unusableProfile(source.Image, "", reason),
			profile.Candidate{Image: source.Image, Message: reason}, reason, true)
	}

	resolved, err := r.NewResolver(registry.Options{Credentials: credentials.authenticate}).Resolve(ctx, source.Image)
	if err != nil {
		// A registry failure, including a rejected credential, is retryable:
		// registries flap and pull Secrets get created. The resolver guarantees
		// no credential material in the error.
		reason := err.Error()
		return failedOutcome(unusableProfile(source.Image, "", reason),
			profile.Candidate{Image: source.Image, Message: reason}, reason, true)
	}

	metadata, err := profile.Parse(resolved.Metadata)
	if err != nil {
		// The document resolved but violates the contract. Retrying cannot fix
		// it; the configured image has to change. Metadata is not published
		// because status.profiles carries only validated documents.
		reason := fmt.Sprintf("profile image %q: %s", source.Image, err)
		return failedOutcome(unusableProfile(source.Image, resolved.Digest, reason),
			profile.Candidate{Image: source.Image, Digest: resolved.Digest, Message: reason}, reason, false)
	}

	return outcome{
		entry: vcrv1alpha1.ResolvedProfile{
			Image:        source.Image,
			Digest:       resolved.Digest,
			Metadata:     string(resolved.Metadata),
			ReconciledAt: metav1.NewTime(r.Now()),
		},
		candidate: profile.Candidate{
			Image:    source.Image,
			Digest:   resolved.Digest,
			Metadata: metadata,
		},
	}
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

	if state.Desired == 0 {
		// A freshly created DaemonSet reports no status at all. Saying "0 of 0
		// node(s) ready" claims a fact about the cluster that has not been observed
		// yet; name the actual state instead.
		return metav1.Condition{
			Status: metav1.ConditionFalse,
			Reason: vcrv1alpha1.ReasonDaemonSetNotReady,
			Message: fmt.Sprintf("device plugin DaemonSet %s reports no scheduled node yet%s",
				location, exclusion),
		}
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
func readyCondition(status *vcrv1alpha1.VcrGpuClusterStatus) metav1.Condition {
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
		Message: fmt.Sprintf("VcrGpuCluster is ready: %d profile image(s) published; %s",
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
