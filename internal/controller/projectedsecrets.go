package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/projection"
)

// ProjectedSecretReconciler keeps the operator-managed pull-secret copies in
// workload namespaces correct after admission created them.
//
// Admission must write the copy before the Pod starts, but admission alone cannot
// keep it right: a rotated source Secret has to reach every copy, and a copy whose
// Pods are gone has to disappear. Leaving that to admission leaks one Secret per
// workload namespace forever and lets a rotated credential sit stale next to a
// working Pod.
type ProjectedSecretReconciler struct {
	// Client reads and writes the copies and lists Pods.
	Client client.Client

	// Reader performs direct reads of the source Secrets in the operator namespace.
	Reader client.Reader

	// OperatorNamespace holds the source Secrets named by the environment.
	OperatorNamespace string

	// Interval is how often the copies are swept. Default: 2 minutes.
	Interval time.Duration

	// Now stamps updates. Default: time.Now.
	Now func() time.Time

	// Log receives one line per removed or refreshed copy. Default: discard.
	Log logr.Logger
}

// DefaultProjectedSecretInterval is how often the copies are reconciled.
const DefaultProjectedSecretInterval = 2 * time.Minute

func (r *ProjectedSecretReconciler) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return DefaultProjectedSecretInterval
}

func (r *ProjectedSecretReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// Start runs the sweep until ctx is cancelled: once at startup, then every interval.
func (r *ProjectedSecretReconciler) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.interval())
	defer ticker.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.Log.Error(err, "reconciling projected pull Secrets")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Reconcile refreshes every projected copy whose inputs changed and deletes the
// copies no Pod in their namespace references.
func (r *ProjectedSecretReconciler) Reconcile(ctx context.Context) error {
	if r.Client == nil || r.Reader == nil {
		return fmt.Errorf("projected secret reconciliation needs a client and a reader")
	}
	if r.OperatorNamespace == "" {
		return fmt.Errorf("projected secret reconciliation needs the operator namespace: it holds the source Secrets")
	}

	copies := &corev1.SecretList{}
	if err := r.Client.List(ctx, copies, client.MatchingLabels{
		names.ManagedByLabel: names.OperatorName,
	}); err != nil {
		return fmt.Errorf("list projected pull Secrets: %w", err)
	}

	referencedByNamespace := map[string]map[string]bool{}
	for i := range copies.Items {
		secret := &copies.Items[i]
		if !projection.IsProjected(secret) {
			continue
		}

		// Refresh from the recorded inputs so a rotated source Secret reaches the
		// copy; an unchanged payload is not rewritten.
		if sources := projection.SourcesOf(secret); len(sources) > 0 {
			merged, err := projection.Merge(ctx, r.Reader, r.OperatorNamespace, sources,
				projection.ImageHostOf(secret), projection.ExplicitOf(secret))
			switch {
			case err != nil:
				// One unreadable or now-ambiguous source must not stall the sweep for
				// every other copy, which would leave all of them stale forever.
				r.Log.Error(err, "refreshing a projected pull Secret",
					"namespace", secret.Namespace, "name", secret.Name)
			case merged == nil:
				// The sources no longer carry a credential for this copy's registry, so
				// the copy is the only place the old credential still exists. Keeping it
				// would leave a credential the environment no longer sanctions next to a
				// live Pod; admission rebuilds the copy if a Pod needs it again.
				if err := r.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("delete projected pull Secret %s/%s that no longer projects a credential: %w",
						secret.Namespace, secret.Name, err)
				}
				r.Log.Info("removed a projected pull Secret whose sources no longer carry its credential",
					"namespace", secret.Namespace, "name", secret.Name)
				continue
			default:
				if err := projection.Ensure(ctx, r.Client, r.Reader, projection.Copy{
					Merged:    *merged,
					Namespace: secret.Namespace,
					Name:      secret.Name,
					AppliedTo: secret.Annotations[projection.AppliedToAnnotation],
					UpdatedAt: r.now().Format(time.RFC3339),
				}); err != nil {
					return err
				}
			}
		}

		namespaces, ok := referencedByNamespace[secret.Namespace]
		if !ok {
			referenced, err := r.referencedSecrets(ctx, secret.Namespace)
			if err != nil {
				return err
			}
			namespaces = referenced
			referencedByNamespace[secret.Namespace] = namespaces
		}
		if namespaces[secret.Name] {
			continue
		}
		if err := r.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete unreferenced projected pull Secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
		r.Log.Info("removed an unreferenced projected pull Secret",
			"namespace", secret.Namespace, "name", secret.Name)
	}
	return nil
}

// referencedSecrets returns the pull-secret names any Pod in the namespace uses.
func (r *ProjectedSecretReconciler) referencedSecrets(ctx context.Context, namespace string) (map[string]bool, error) {
	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Pods in %s: %w", namespace, err)
	}
	used := make(map[string]bool, len(pods.Items))
	for _, pod := range pods.Items {
		for _, reference := range pod.Spec.ImagePullSecrets {
			used[reference.Name] = true
		}
	}
	return used, nil
}
