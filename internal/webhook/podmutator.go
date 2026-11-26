package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
	"github.com/neuralmagic/vllm-vcr-operator/internal/plan"
	"github.com/neuralmagic/vllm-vcr-operator/internal/rejection"
	"github.com/neuralmagic/vllm-vcr-operator/internal/runtime"
	"github.com/neuralmagic/vllm-vcr-operator/internal/translator"
	"github.com/neuralmagic/vllm-vcr-operator/internal/vllm"
)

// PodMutator translates GPU-requesting vLLM workloads into the simulated runtime
// layout (docs/contracts/admission.md).
//
// It is a thin admission shell: internal/vllm resolves the workload invocation,
// internal/translator decides whether it can be simulated, and internal/runtime
// applies the plan. The only decision this type adds is the deployment-level one
// (read the singleton environment, rebuild its published catalog) plus pull-secret
// projection.
type PodMutator struct {
	client            client.Client
	reader            client.Reader
	operatorNamespace string
	frontendImage     string
	backendImage      string
	now               func() time.Time
	recorder          record.EventRecorder

	// events decouples admission from the event sink. A synchronous Event call can
	// block (a full recorder queue stalls the API server's admission round trip),
	// and an admission decision must never depend on telemetry. The queue is
	// bounded and drops under pressure; the log line already carries the decision.
	events chan recordedEvent
}

// recordedEvent is one queued event, kept as plain values so the queue never holds
// references to objects the API server may still be defaulting.
type recordedEvent struct {
	namespace   string
	podName     string
	generateRef string
	eventType   string
	reason      string
	message     string
}

// NewPodMutator builds a Pod mutator from opts and validates the settings that
// cannot be defaulted.
func NewPodMutator(opts Options) (*PodMutator, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	opts = opts.resolved()
	m := &PodMutator{
		client:            opts.Client,
		reader:            opts.Reader,
		operatorNamespace: opts.OperatorNamespace,
		frontendImage:     opts.FrontendImage,
		backendImage:      opts.BackendImage,
		now:               opts.Now,
		recorder:          opts.Recorder,
		events:            make(chan recordedEvent, eventQueueDepth),
	}
	// The forwarder is process-lifetime: the mutator lives as long as the manager,
	// and tests are short-lived processes. It is what keeps a slow event sink off
	// the admission path.
	go m.drainEvents()
	return m, nil
}

// Handle implements the controller-runtime admission handler interface.
func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	return admission.Response{AdmissionResponse: *m.Admit(ctx, &req.AdmissionRequest)}
}

// Admit returns the admission decision for one Pod.
//
// It is exported so tests and the handler share exactly one code path. The
// returned response is either a denial, an allow with an empty patch, or an allow
// with the JSON patch that installs the simulated runtime layout.
func (m *PodMutator) Admit(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	log := ctrl.LoggerFrom(ctx)
	dryRunRequest := req.DryRun != nil && *req.DryRun
	if req == nil {
		return operatorDenied("the admission request is empty")
	}
	// Only creation injects the runtime layout; updates (including the ones a
	// controller makes to a Pod it owns) are left alone.
	if req.Operation != admissionv1.Create {
		return allowed()
	}

	pod := &corev1.Pod{}
	if err := decodeObject(req.Object.Raw, pod, "Pod"); err != nil {
		return operatorDenied(err.Error())
	}
	// A generateName-only Pod has no name yet; the request carries the resolved
	// identity, which is what provenance and the projected pull secret use.
	if pod.Name == "" {
		pod.Name = req.Name
	}
	if pod.Namespace == "" {
		pod.Namespace = req.Namespace
	}

	if !runtime.RequestsSyntheticDevice(pod) {
		return allowed()
	}
	if runtime.AlreadyInjected(pod) {
		return allowed()
	}

	workload, gpuDevices, denial := workloadContainer(pod)
	if denial != nil {
		return denial.response()
	}

	env, denial := m.environment(ctx)
	if denial != nil {
		m.recordOperatorProblem(nil, log, denial.detail)
		return m.deny(pod, log, dryRunRequest, denial)
	}
	catalog, err := catalogFor(env)
	if err != nil {
		denial := operatorFailure("%v", err)
		m.recordOperatorProblem(env, log, denial.detail)
		return m.deny(pod, log, dryRunRequest, denial)
	}

	// A namespace may pin the recording its simulated GPUs run. The selection narrows the
	// catalog to one identity; it never relaxes the workload's own requirements.
	selection, denial := m.profileSelection(ctx, pod.Namespace)
	if denial != nil {
		m.recordOperatorProblem(env, log, denial.detail)
		return m.deny(pod, log, dryRunRequest, denial)
	}
	if selection.ProfileID != "" {
		offered := catalog.IDs()
		if !slices.Contains(offered, selection.ProfileID) {
			return m.deny(pod, log, dryRunRequest, workloadFailure(
				"VcrGpuProfile %q in namespace %q selects profile %q, which the cluster's catalog does not offer; offered profiles: %s",
				selection.Name, pod.Namespace, selection.ProfileID, offeredProfiles(offered)))
		}
	}

	config, err := vllm.Resolve(vllm.Invocation{
		Container: *workload,
		Volumes:   volumeMap(pod),
	})
	if err != nil {
		if denial := rejectionDenial(err); denial != nil {
			return m.deny(pod, log, dryRunRequest, denial)
		}
		// internal/vllm reports everything it can establish safely; anything else
		// is an operator-side failure, not something the user can fix.
		return m.deny(pod, log, dryRunRequest,
			operatorFailure("the workload invocation cannot be resolved: %v", err))
	}

	admitted, err := translator.Translate(translator.Request{
		Config:      config,
		Catalog:     catalog,
		Environment: translatorEnvironment(env, translator.Images{Frontend: m.frontendImage, Backend: m.backendImage}),
		GPUDevices:  gpuDevices,
		ProfileID:   selection.ProfileID,
		PodIdentity: pod.Name,
	})
	if err != nil {
		// The settings policy produces the same structured rejection as the
		// resolver, so both stages are reported by one function; anything else is
		// an operator-side failure.
		denial := rejectionDenial(err)
		if denial == nil {
			denial = operatorFailure("%v", err)
		}
		return m.deny(pod, log, dryRunRequest, denial)
	}

	mutated, err := runtime.Mutate(pod, admitted)
	if err != nil {
		return m.deny(pod, log, dryRunRequest, workloadFailure("%s", trimErrorPrefix(err, "runtime")))
	}

	// Pull-secret projection is the only side effect of admission, and only a real
	// request may cause it. The projected name is derived from the payload, so a
	// dry-run request produces the same patch without creating anything.
	dryRun := req.DryRun != nil && *req.DryRun
	projected, err := m.projectPullSecrets(ctx, pod, admitted, dryRun)
	if err != nil {
		denial := operatorFailure("%v", err)
		m.recordOperatorProblem(env, log, denial.detail)
		return m.deny(pod, log, dryRunRequest, denial)
	}
	if projected != "" {
		addPullSecret(mutated, projected)
	}

	patch, err := podPatch(req.Object.Raw, mutated)
	if err != nil {
		denial := operatorFailure("%v", err)
		m.recordOperatorProblem(env, log, denial.detail)
		return m.deny(pod, log, dryRunRequest, denial)
	}
	if len(patch) == 0 {
		return allowed()
	}
	m.recordInjection(log, pod, admitted, dryRun, len(patch))
	response := allowedWithPatch(patch, admissionv1.PatchTypeJSONPatch)
	response.Warnings = admitted.Warnings
	return response
}

// maxEventMessage bounds event text: the API server rejects messages over 1KB and
// a rejection that cannot be recorded is worse than a shortened one.
const maxEventMessage = 900

// eventQueueDepth bounds queued events. Admission drops instead of blocking past
// this depth; the decision is logged either way.
const eventQueueDepth = 512

// deny records a refusal and returns its admission response.
//
// Every denial is logged and, unless the request is a dry run, evented: a test
// author sees the reason in `kubectl describe pod` (or on the namespace for a
// controller-created Pod whose name the API server has not generated yet), and the
// operator log carries the same decision for whoever debugs the webhook.
func (m *PodMutator) deny(pod *corev1.Pod, log logr.Logger, dryRun bool, denial *admissionDenial) *admissionv1.AdmissionResponse {
	detail := truncate(denial.detail, maxEventMessage)
	log.Info("admission denied", "namespace", pod.Namespace, "pod", podName(pod),
		"reason", detail)
	if !dryRun {
		m.event(pod, corev1.EventTypeWarning, "SimulationRejected", detail)
	}
	return denial.response()
}

// recordInjection logs an accepted translation and, unless the request is a dry
// run, announces it. The event names the profile and its digest, which is the
// identity a test can assert on without reading annotations.
func (m *PodMutator) recordInjection(log logr.Logger, pod *corev1.Pod, admitted *plan.Admission, dryRun bool, patchSize int) {
	profileID, digest := admitted.Profile.ID, admitted.Profile.Digest
	log.Info("admission translated the workload for simulation",
		"namespace", pod.Namespace, "pod", podName(pod),
		"profile", profileID, "profileDigest", digest,
		"model", admitted.Model.Reference(),
		"gpuDevices", admitted.GPUDevices,
		"frontendImage", admitted.Frontend.Image, "backendImage", admitted.Backend.Image,
		"warnings", len(admitted.Warnings), "patchBytes", patchSize, "dryRun", dryRun)
	if dryRun {
		return
	}
	message := fmt.Sprintf("serving simulated with profile %s (%s); model %s; %d GPU device(s)",
		profileID, digest, admitted.Model.Reference(), admitted.GPUDevices)
	if len(admitted.Warnings) > 0 {
		message += fmt.Sprintf("; %d warning(s)", len(admitted.Warnings))
	}
	m.event(pod, corev1.EventTypeNormal, "SimulationInjected", truncate(message, maxEventMessage))
}

// event emits an event for the Pod, or for its namespace when the Pod has no name
// yet: a controller-created Pod carries generateName, and an event needs a real
// involved-object name.
func (m *PodMutator) event(pod *corev1.Pod, eventType, reason, message string) {
	if m.recorder == nil {
		return
	}
	queued := recordedEvent{
		namespace:   pod.Namespace,
		podName:     pod.Name,
		generateRef: pod.GenerateName,
		eventType:   eventType,
		reason:      reason,
		message:     message,
	}
	select {
	case m.events <- queued:
	default:
		// Dropping is deliberate: the decision is already logged with the same
		// detail, and blocking admission to record an event would be worse.
	}
}

// drainEvents forwards queued events to the recorder until the queue closes. It
// runs in its own goroutine so a slow sink cannot delay admission.
func (m *PodMutator) drainEvents() {
	for queued := range m.events {
		if queued.podName != "" {
			pod := &corev1.Pod{}
			pod.Namespace, pod.Name = queued.namespace, queued.podName
			m.recorder.Event(pod, queued.eventType, queued.reason, queued.message)
			continue
		}
		namespace := &corev1.Namespace{}
		namespace.Name = queued.namespace
		m.recorder.Event(namespace, queued.eventType, queued.reason, queued.message)
	}
}

func podName(pod *corev1.Pod) string {
	if pod.Name != "" {
		return pod.Name
	}
	return pod.GenerateName + "*"
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// rejectionDenial renders a structured rejection as a workload denial, or nil when
// err carries no rejection.
//
// internal/vllm and internal/translator produce the same rejection type
// (internal/rejection), so one function reports either stage: a workload must see
// the setting and the reason whichever decision refused it, and the webhook must not
// grow a second parser for a second type.
func rejectionDenial(err error) *admissionDenial {
	rejections, ok := rejection.Rejected(err)
	if !ok {
		return nil
	}
	described := make([]string, len(rejections))
	for i, r := range rejections {
		described[i] = describeSetting(r.Setting, r.Value, r.Reason)
	}
	return workloadFailure("%s", strings.Join(described, "; "))
}

// describeSetting renders one rejection as "setting: reason" (or
// "setting=value: reason"), never including a credential: both resolvers redact
// credential values before they reach a rejection.
func describeSetting(setting, value, reason string) string {
	if value == "" {
		return fmt.Sprintf("%s: %s", setting, reason)
	}
	return fmt.Sprintf("%s=%s: %s", setting, value, reason)
}

// trimErrorPrefix drops a package prefix such as "runtime: " so denial messages
// read as one sentence.
func trimErrorPrefix(err error, prefix string) string {
	return strings.TrimPrefix(err.Error(), prefix+": ")
}

// workloadContainer returns the container whose vLLM invocation is resolved and
// whose device allocation is preserved.
//
// Exactly one container may own the synthetic allocation: a Pod with two GPU
// consumers cannot be translated without deciding which one the allocation
// belongs to, so the Pod as a whole is rejected rather than translated partially.
func workloadContainer(pod *corev1.Pod) (*corev1.Container, int32, *admissionDenial) {
	owners := runtime.SyntheticDeviceOwners(pod.Spec.Containers)
	var ownerNames []string
	for _, index := range owners {
		ownerNames = append(ownerNames, pod.Spec.Containers[index].Name)
	}

	switch {
	case len(owners) > 1:
		return nil, 0, workloadFailure(
			"containers %s all request %s: exactly one workload container may own the synthetic allocation",
			strings.Join(ownerNames, ", "), names.SyntheticResourceName)
	case len(owners) == 0:
		return nil, 0, workloadFailure(
			"only an init container requests %s: the Pod has no workload container whose entrypoint can be translated",
			names.SyntheticResourceName)
	}
	index := owners[0]

	quantity, _ := runtime.SyntheticDeviceQuantity(pod.Spec.Containers[index].Resources)
	if quantity.MilliValue()%1000 != 0 {
		return nil, 0, workloadFailure(
			"the %s allocation %s is not a whole number of devices",
			names.SyntheticResourceName, quantity.String())
	}
	devices := quantity.Value()
	if devices > math.MaxInt32 {
		return nil, 0, workloadFailure(
			"the %s allocation %s exceeds the supported maximum of %d devices",
			names.SyntheticResourceName, quantity.String(), math.MaxInt32)
	}
	// A zero or negative allocation is left to internal/translator, which reports
	// it against the resource name with the policy's own wording.
	return &pod.Spec.Containers[index], int32(devices), nil
}

// podPatch renders the difference between the submitted Pod and the mutated one.
//
// The original bytes are the patch base so that nothing the webhook did not
// deliberately change can appear as a removal.
func podPatch(original []byte, mutated *corev1.Pod) ([]byte, error) {
	mutatedJSON, err := json.Marshal(mutated)
	if err != nil {
		return nil, fmt.Errorf("the mutated Pod cannot be encoded: %w", err)
	}
	operations, err := jsonpatch.CreatePatch(original, mutatedJSON)
	if err != nil {
		return nil, fmt.Errorf("the Pod mutation cannot be rendered as a patch: %w", err)
	}
	if len(operations) == 0 {
		return nil, nil
	}
	sortOperations(operations)
	patch, err := json.Marshal(operations)
	if err != nil {
		return nil, fmt.Errorf("the Pod patch cannot be encoded: %w", err)
	}
	return patch, nil
}

// addPullSecret references the projected pull secret, keeping the workload's own
// imagePullSecrets in place.
func addPullSecret(pod *corev1.Pod, name string) {
	for _, reference := range pod.Spec.ImagePullSecrets {
		if reference.Name == name {
			return
		}
	}
	pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets,
		corev1.LocalObjectReference{Name: name})
}

// sortOperations puts a diff in a deterministic order without changing what it
// means.
//
// The diff walks JSON objects through Go maps, so sibling operations are emitted in
// map-iteration order. Two operations at independent paths can be swapped safely,
// but operations inside one JSON array cannot: an add must run from the lowest
// index up (a higher index does not exist yet) and a remove must run from the
// highest index down (an earlier removal shifts the later one). The diff never
// emits both a path and one of its descendants, so no other dependency exists.
func sortOperations(operations []jsonpatch.Operation) {
	sort.SliceStable(operations, func(i, j int) bool {
		return compareOperations(operations[i], operations[j]) < 0
	})
}

// compareOperations orders two patch operations deterministically.
func compareOperations(a, b jsonpatch.Operation) int {
	as := strings.Split(strings.TrimPrefix(a.Path, "/"), "/")
	bs := strings.Split(strings.TrimPrefix(b.Path, "/"), "/")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		aIndex, aIsIndex := arrayIndex(as[i])
		bIndex, bIsIndex := arrayIndex(bs[i])
		if aIsIndex && bIsIndex && aIndex != bIndex {
			if a.Operation == "remove" && b.Operation == "remove" {
				// Highest index first: removing a lower index would shift it.
				return bIndex - aIndex
			}
			return aIndex - bIndex
		}
		return strings.Compare(as[i], bs[i])
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return strings.Compare(a.Operation, b.Operation)
}

// arrayIndex parses a JSON pointer segment as an array index.
func arrayIndex(segment string) (int, bool) {
	if segment == "" {
		return 0, false
	}
	for i := 0; i < len(segment); i++ {
		if segment[i] < '0' || segment[i] > '9' {
			return 0, false
		}
	}
	index, err := strconv.Atoi(segment)
	if err != nil {
		return 0, false
	}
	return index, true
}

// offeredProfiles renders the catalog's identities for a denial.
func offeredProfiles(ids []string) string {
	if len(ids) == 0 {
		return "none (the catalog is empty)"
	}
	return strings.Join(ids, ", ")
}
