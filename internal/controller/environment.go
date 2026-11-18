package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vcrv1alpha1 "github.com/opendatahub-io/vllm-vcr-operator/api/v1alpha1"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/registry"
)

// Condition types published on the SimulationEnvironment.
const (
	// ConditionReady is true only when every configured profile has usable
	// metadata and the device plugin runs on every node it is scheduled for.
	ConditionReady = "Ready"
	// ConditionProfilesReconciled reports whether status.profiles is a complete,
	// usable catalog for admission.
	ConditionProfilesReconciled = "ProfilesReconciled"
	// ConditionDevicePluginReady reports whether the synthetic device plugin is
	// advertising capacity on the simulation nodes.
	ConditionDevicePluginReady = "DevicePluginReady"
)

// Condition reasons. Each one names the action an operator must take.
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
)

// DefaultRetryInterval is how long a failed profile resolution waits before it
// is retried. Registry outages and missing pull Secrets heal without a spec
// change, so they are retried with backoff.
const DefaultRetryInterval = 30 * time.Second

// Options configures a SimulationEnvironmentReconciler.
type Options struct {
	// Client reads and writes the environment, the device plugin resources, and
	// Nodes. It is the manager's cached client in the operator binary.
	Client client.Client

	// OperatorNamespace holds the operator-owned Secrets, the device plugin
	// DaemonSet, and its device count ConfigMap.
	OperatorNamespace string

	// DevicePluginImage is the device-plugin container image. Resolve it with
	// DevicePluginImageFromEnv so the image stays deploy-time configurable.
	DevicePluginImage string

	// NewResolver builds the profile resolver for one set of pull credentials.
	// It defaults to registry.New. Tests inject a resolver so no registry is
	// contacted.
	NewResolver ResolverFactory

	// Now is the clock used for status.profiles[].reconciledAt. Default: time.Now.
	Now func() time.Time

	// RetryInterval is the requeue delay for retryable resolution failures.
	// Default: DefaultRetryInterval.
	RetryInterval time.Duration

	// Recorder emits diagnostics for condition changes. It never carries Secret
	// values. Default: no events.
	Recorder record.EventRecorder
}

// ResolverFactory builds a registry resolver for the pull credentials of one
// profile image.
type ResolverFactory func(registry.Options) registry.Resolver

// SimulationEnvironmentReconciler reconciles the singleton SimulationEnvironment:
// the published profile catalog, the environment conditions, and the synthetic
// device-plugin DaemonSet.
//
// It never mutates Pods. Admission pins the digest published here on each
// admitted Pod, so changing a profile cannot retrofit a running simulation:
// restarting the workload applies a new profile.
type SimulationEnvironmentReconciler struct {
	client.Client

	// OperatorNamespace holds the operator-owned Secrets, the device plugin
	// DaemonSet, and its device count ConfigMap.
	OperatorNamespace string

	// DevicePlugin reconciles the synthetic device plugin.
	DevicePlugin *DevicePluginReconciler

	// NewResolver builds the profile resolver for one set of pull credentials.
	NewResolver ResolverFactory

	// Now is the clock used for reconciledAt.
	Now func() time.Time

	// RetryInterval is the requeue delay for retryable resolution failures.
	RetryInterval time.Duration

	// Recorder emits diagnostics for condition changes.
	Recorder record.EventRecorder
}

// NewSimulationEnvironmentReconciler builds the reconciler from opts and
// validates the settings that cannot be defaulted.
func NewSimulationEnvironmentReconciler(opts Options) (*SimulationEnvironmentReconciler, error) {
	switch {
	case opts.Client == nil:
		return nil, fmt.Errorf("simulation environment reconciler: a client is required")
	case opts.OperatorNamespace == "":
		return nil, fmt.Errorf(
			"simulation environment reconciler: an operator namespace is required: it holds the pull Secrets, the device plugin DaemonSet, and its device count ConfigMap")
	case opts.DevicePluginImage == "":
		return nil, fmt.Errorf(
			"simulation environment reconciler: a device plugin image is required: set %s", EnvDevicePluginImage)
	}

	newResolver := opts.NewResolver
	if newResolver == nil {
		newResolver = func(o registry.Options) registry.Resolver { return registry.New(o) }
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	retryInterval := opts.RetryInterval
	if retryInterval <= 0 {
		retryInterval = DefaultRetryInterval
	}

	reconciler := &SimulationEnvironmentReconciler{
		Client:            opts.Client,
		OperatorNamespace: opts.OperatorNamespace,
		NewResolver:       newResolver,
		Now:               now,
		RetryInterval:     retryInterval,
		Recorder:          opts.Recorder,
	}
	reconciler.DevicePlugin = &DevicePluginReconciler{
		Client:            opts.Client,
		OperatorNamespace: opts.OperatorNamespace,
		Image:             opts.DevicePluginImage,
	}
	return reconciler, nil
}

// SetupWithManager registers the reconciler with the manager. The operator-owned
// DaemonSet and device count ConfigMap are watched through their owner reference,
// so a deleted or progressing device plugin is reconciled without polling.
func (r *SimulationEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vcrv1alpha1.SimulationEnvironment{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&corev1.ConfigMap{}).
		Named("simulationenvironment").
		Complete(r)
}

// Reconcile reconciles the singleton environment named by the request.
//
// The operator reads the singleton configuration and publishes the reconciled
// catalog in its status. Pull Secrets are read in the operator namespace only,
// and their values never reach status, conditions, or events.
func (r *SimulationEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	env := &vcrv1alpha1.SimulationEnvironment{}
	if err := r.Get(ctx, req.NamespacedName, env); err != nil {
		if apierrors.IsNotFound(err) {
			// The device plugin DaemonSet and the device count ConfigMap are owned
			// by the environment, so Kubernetes garbage collects them.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get SimulationEnvironment %s: %w", req.NamespacedName, err)
	}
	return r.reconcileEnvironment(ctx, env)
}

// reconcileEnvironment reconciles one environment and publishes its status.
func (r *SimulationEnvironmentReconciler) reconcileEnvironment(ctx context.Context, env *vcrv1alpha1.SimulationEnvironment) (ctrl.Result, error) {
	if env.Name != vcrv1alpha1.SimulationEnvironmentNameDefault {
		// The CRD's CEL rule already rejects any other name. Reaching this point
		// means the rule was bypassed, so fail loudly instead of reconciling a
		// second device plugin that would fight over the same extended resource.
		return ctrl.Result{}, fmt.Errorf(
			"SimulationEnvironment %q is not supported: the only accepted name is %q",
			env.Name, vcrv1alpha1.SimulationEnvironmentNameDefault)
	}

	profiles := r.reconcileProfiles(ctx, env)
	devicePlugin, err := r.DevicePlugin.Reconcile(ctx, env)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.publishStatus(ctx, env, profiles, devicePlugin); err != nil {
		if apierrors.IsConflict(err) {
			// The retry budget was spent on a concurrent writer. That is not an
			// actionable error: ask the work queue for another reconcile, which
			// re-reads the object and publishes from the version it finds.
			return ctrl.Result{RequeueAfter: r.RetryInterval}, nil
		}
		return ctrl.Result{}, err
	}
	if profiles.retry {
		return ctrl.Result{RequeueAfter: r.RetryInterval}, nil
	}
	return ctrl.Result{}, nil
}

// publishStatus writes the reconciled catalog and the three conditions, and only
// touches the API when something actually changed.
//
// A status write can lose a race with another writer of the same object (a
// finalizer, another manager, a user edit). That conflict is retried here with a
// fresh read of the environment, because the desired status depends on the
// version the API server holds: the conditions carry the transition times of
// unchanged conditions and ObservedGeneration carries the current generation. Only
// a spent retry budget is handed back to the caller to requeue.
func (r *SimulationEnvironmentReconciler) publishStatus(
	ctx context.Context,
	env *vcrv1alpha1.SimulationEnvironment,
	profiles profileReconciliation,
	devicePlugin DevicePluginState,
) error {
	key := client.ObjectKeyFromObject(env)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &vcrv1alpha1.SimulationEnvironment{}
		if err := r.Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				// The environment was deleted while it was being published. There is
				// nothing left to write, and its device plugin is garbage collected
				// through the owner reference.
				return nil
			}
			return fmt.Errorf("get SimulationEnvironment %s to publish its status: %w", env.Name, err)
		}

		desired := r.desiredStatus(current, profiles, devicePlugin)
		if equality.Semantic.DeepEqual(current.Status, desired) {
			return nil
		}
		previous := *current.Status.DeepCopy()
		current.Status = desired
		if err := r.Status().Update(ctx, current); err != nil {
			// Retried only when this is a conflict; every other error is final.
			return err
		}
		// Event once, after the write the API server accepted.
		r.recordConditionChanges(current, previous)
		return nil
	})
	if err != nil {
		return fmt.Errorf("update SimulationEnvironment %s status: %w", env.Name, err)
	}
	return nil
}

// desiredStatus builds the status the reconciler publishes for env.
func (r *SimulationEnvironmentReconciler) desiredStatus(
	env *vcrv1alpha1.SimulationEnvironment,
	profiles profileReconciliation,
	devicePlugin DevicePluginState,
) vcrv1alpha1.SimulationEnvironmentStatus {
	status := vcrv1alpha1.SimulationEnvironmentStatus{
		ObservedGeneration:   env.Generation,
		SupportedVLLMVersion: names.SupportedVLLMVersion,
		Profiles:             profiles.published,
		Conditions:           append([]metav1.Condition(nil), env.Status.Conditions...),
	}

	setCondition(&status, ConditionProfilesReconciled, profiles.condition())
	setCondition(&status, ConditionDevicePluginReady, devicePluginCondition(devicePlugin, r.DevicePlugin))
	setCondition(&status, ConditionReady, readyCondition(&status))
	return status
}

// recordConditionChanges emits one warning event when the reconciled profile
// catalog becomes unusable, so a failure that needs a human is visible with
// `kubectl describe` and not only in the condition.
//
// Device plugin readiness is not evented: it is expected to be False for the
// first moments after creation and is continuously reported in the
// DevicePluginReady condition. Event messages are condition messages: they name
// images and Secrets but never Secret values.
func (r *SimulationEnvironmentReconciler) recordConditionChanges(env *vcrv1alpha1.SimulationEnvironment, previous vcrv1alpha1.SimulationEnvironmentStatus) {
	if r.Recorder == nil {
		return
	}
	profiles := meta.FindStatusCondition(env.Status.Conditions, ConditionProfilesReconciled)
	if profiles == nil || profiles.Status != metav1.ConditionFalse {
		return
	}
	if prior := meta.FindStatusCondition(previous.Conditions, ConditionProfilesReconciled); prior != nil &&
		prior.Status == profiles.Status && prior.Reason == profiles.Reason && prior.Message == profiles.Message {
		return
	}
	r.Recorder.Event(env, corev1.EventTypeWarning, profiles.Reason, profiles.Message)
}

// setCondition applies one condition to status, preserving the transition time of
// an unchanged condition.
func setCondition(status *vcrv1alpha1.SimulationEnvironmentStatus, conditionType string, condition metav1.Condition) {
	condition.Type = conditionType
	condition.ObservedGeneration = status.ObservedGeneration
	meta.SetStatusCondition(&status.Conditions, condition)
}
