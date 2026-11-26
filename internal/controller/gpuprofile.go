package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/profile"
)

// ConditionReady is the condition type a VcrGpuProfile reports.
const conditionProfileReady = "Ready"

// VcrGpuProfileReconciler resolves a namespace's profile selection against the
// cluster's catalog.
//
// Resolution happens here rather than at admission so that the answer is visible before
// anyone submits a workload: a selection naming an identity the catalog does not offer is
// a mistake worth reporting on the object, not a surprise at Pod creation.
type VcrGpuProfileReconciler struct {
	client.Client

	// Now is the clock used for conditions; tests set it.
	Now func() time.Time
}

// NewVcrGpuProfileReconciler builds the reconciler.
func NewVcrGpuProfileReconciler(c client.Client) *VcrGpuProfileReconciler {
	return &VcrGpuProfileReconciler{Client: c, Now: time.Now}
}

// Reconcile resolves one selection and publishes the result in its status.
func (r *VcrGpuProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	selection := &vcrv1alpha1.VcrGpuProfile{}
	if err := r.Get(ctx, req.NamespacedName, selection); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	cluster := &vcrv1alpha1.VcrGpuCluster{}
	if err := r.Get(ctx, client.ObjectKey{Name: vcrv1alpha1.VcrGpuClusterNameDefault}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			// No cluster configuration, nothing to resolve against. That is an operator
			// state, not a mistake in this object.
			return ctrl.Result{}, r.publish(ctx, selection, nil,
				conditionProfileReady, metav1.ConditionFalse, "NoCluster",
				fmt.Sprintf("no VcrGpuCluster named %q exists, so no catalog can be resolved against",
					vcrv1alpha1.VcrGpuClusterNameDefault))
		}
		return ctrl.Result{}, fmt.Errorf("get VcrGpuCluster: %w", err)
	}

	lookup := resolveSelection(cluster, selection.Spec.Profile)
	if lookup.resolved == nil {
		reason, unusable := "NotOffered", lookup.describeUnusable(selection.Spec.Profile)
		message := fmt.Sprintf("profile %q is not offered by the cluster's catalog; offered profiles: %s",
			selection.Spec.Profile, describeOffered(lookup.offered))
		if unusable != "" {
			reason = "Unusable"
			message = fmt.Sprintf("profile %q is published by the cluster's catalog but cannot be used: %s",
				selection.Spec.Profile, unusable)
		}
		return ctrl.Result{}, r.publish(ctx, selection, nil,
			conditionProfileReady, metav1.ConditionFalse, reason, message)
	}
	return ctrl.Result{}, r.publish(ctx, selection, lookup.resolved,
		conditionProfileReady, metav1.ConditionTrue, "Resolved",
		fmt.Sprintf("profile %q resolves to %s", lookup.resolved.ID, lookup.resolved.Image))
}

// selectionLookup is what the catalog says about the requested identity.
type selectionLookup struct {
	// resolved is the entry the identity names, when it names a usable one.
	resolved *vcrv1alpha1.VcrGpuProfileResolution
	// offered lists every identity the catalog publishes.
	offered []string
	// unusable maps an identity the catalog publishes to the reason it cannot be used,
	// so a selection naming one is told the difference between "unknown" and "broken".
	unusable map[string]string
}

// resolveSelection answers what the requested identity means in this catalog.
//
// Nothing is skipped silently: an entry whose metadata cannot be read is reported as
// published-but-unusable. Reporting it as absent would send the user looking for a
// profile that is right there, and would hide a broken catalog behind an empty list.
func resolveSelection(cluster *vcrv1alpha1.VcrGpuCluster, id string) selectionLookup {
	lookup := selectionLookup{unusable: map[string]string{}}
	for _, entry := range cluster.Status.Profiles {
		if entry.Message != "" {
			// The cluster reconciler already refused this entry; its identity is still
			// knowable when the metadata parses.
			if metadata, err := profile.Parse([]byte(entry.Metadata)); err == nil {
				lookup.offered = append(lookup.offered, metadata.ID)
				lookup.unusable[metadata.ID] = entry.Message
			}
			continue
		}
		metadata, err := profile.Parse([]byte(entry.Metadata))
		if err != nil {
			lookup.unusable[entry.Image] = fmt.Sprintf("the catalog entry for %q is not usable: %v", entry.Image, err)
			continue
		}
		lookup.offered = append(lookup.offered, metadata.ID)
		if metadata.ID != id {
			continue
		}
		lookup.resolved = &vcrv1alpha1.VcrGpuProfileResolution{
			ID:          metadata.ID,
			Image:       entry.Image,
			Digest:      entry.Digest,
			Model:       metadata.Model.Name,
			GPUType:     metadata.GPU.Type,
			GPUCount:    metadata.GPU.Count,
			VLLMVersion: metadata.VLLM.Version,
		}
	}
	sort.Strings(lookup.offered)
	return lookup
}

// describeUnusable reports why a published identity cannot be used, when it is the
// requested one.
func (l selectionLookup) describeUnusable(id string) string {
	reason, known := l.unusable[id]
	if !known {
		return ""
	}
	return reason
}

func describeOffered(ids []string) string {
	if len(ids) == 0 {
		return "none (the catalog is empty or unreconciled)"
	}
	return fmt.Sprint(ids)
}

// publish writes the resolution and the condition that explains it.
func (r *VcrGpuProfileReconciler) publish(ctx context.Context, selection *vcrv1alpha1.VcrGpuProfile,
	resolved *vcrv1alpha1.VcrGpuProfileResolution,
	conditionType string, status metav1.ConditionStatus, reason, message string) error {

	patched := selection.DeepCopy()
	patched.Status.Resolved = resolved
	patched.Status.ObservedGeneration = patched.Generation
	meta.SetStatusCondition(&patched.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: patched.Generation,
		LastTransitionTime: metav1.NewTime(r.Now()),
	})
	if equalityIgnoringTransitionTime(selection.Status.Conditions, patched.Status.Conditions) &&
		equalResolution(selection.Status.Resolved, patched.Status.Resolved) &&
		selection.Status.ObservedGeneration == patched.Status.ObservedGeneration {
		return nil
	}
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(selection)); err != nil {
		return fmt.Errorf("update VcrGpuProfile status: %w", err)
	}
	return nil
}

// equalityIgnoringTransitionTime keeps a reconcile from rewriting an unchanged status.
func equalityIgnoringTransitionTime(before, after []metav1.Condition) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		a, b := before[i], after[i]
		if a.Type != b.Type || a.Status != b.Status || a.Reason != b.Reason || a.Message != b.Message ||
			a.ObservedGeneration != b.ObservedGeneration {
			return false
		}
	}
	return true
}

func equalResolution(a, b *vcrv1alpha1.VcrGpuProfileResolution) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// SetupWithManager registers the reconciler.
//
// A selection is re-resolved whenever the cluster's catalog changes: the catalog is what
// an id resolves against, so a newly registered profile or a moved digest changes what
// the selection pins, and its status has to say so.
func (r *VcrGpuProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vcrv1alpha1.VcrGpuProfile{}).
		Watches(&vcrv1alpha1.VcrGpuCluster{}, handler.EnqueueRequestsFromMapFunc(r.selectionsFor)).
		Complete(r)
}

// selectionsFor maps a cluster change onto every selection in every namespace.
func (r *VcrGpuProfileReconciler) selectionsFor(ctx context.Context, _ client.Object) []reconcile.Request {
	listed := &vcrv1alpha1.VcrGpuProfileList{}
	if err := r.List(ctx, listed); err != nil {
		// A failed list only delays a status refresh; the work queue retries on the next
		// cluster event and no admission decision depends on this status.
		return nil
	}
	requests := make([]reconcile.Request, 0, len(listed.Items))
	for _, item := range listed.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: item.Namespace, Name: item.Name},
		})
	}
	return requests
}
