package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/runtime"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/translator"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

// syntheticDeviceName is the extended resource that makes a Pod applicable.
const syntheticDeviceName = names.SyntheticResourceName

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
}

// NewPodMutator builds a Pod mutator from opts and validates the settings that
// cannot be defaulted.
func NewPodMutator(opts Options) (*PodMutator, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	opts = opts.resolved()
	return &PodMutator{
		client:            opts.Client,
		reader:            opts.Reader,
		operatorNamespace: opts.OperatorNamespace,
		frontendImage:     opts.FrontendImage,
		backendImage:      opts.BackendImage,
		now:               opts.Now,
		recorder:          opts.Recorder,
	}, nil
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

	if !requestsSyntheticDevice(pod) {
		return allowed()
	}
	if alreadyInjected(pod) {
		return allowed()
	}

	workload, gpuDevices, denial := workloadContainer(pod)
	if denial != nil {
		return denial.response()
	}

	env, denial := m.environment(ctx)
	if denial != nil {
		m.recordOperatorProblem(nil, log, denial.detail)
		return denial.response()
	}
	catalog, err := catalogFor(env)
	if err != nil {
		denial := operatorFailure("%v", err)
		m.recordOperatorProblem(env, log, denial.detail)
		return denial.response()
	}

	config, err := vllm.Resolve(vllm.Invocation{
		Container: *workload,
		Volumes:   volumeMap(pod),
	})
	if err != nil {
		if denial := resolutionDenial(err); denial != nil {
			return denial.response()
		}
		// internal/vllm reports everything it can establish safely; anything else
		// is an operator-side failure, not something the user can fix.
		return operatorDenied(fmt.Sprintf("the workload invocation cannot be resolved: %v", err))
	}

	admitted, err := translator.Translate(translator.Request{
		Config:      config,
		Catalog:     catalog,
		Environment: translatorEnvironment(env, translator.Images{Frontend: m.frontendImage, Backend: m.backendImage}),
		GPUDevices:  gpuDevices,
		PodIdentity: pod.Name,
	})
	if err != nil {
		return translationDenial(err).response()
	}

	mutated, err := runtime.Mutate(pod, admitted)
	if err != nil {
		return workloadFailure("%s", trimErrorPrefix(err, "runtime")).response()
	}

	// Pull-secret projection is the only side effect of admission, and only a real
	// request may cause it. The projected name is derived from the payload, so a
	// dry-run request produces the same patch without creating anything.
	dryRun := req.DryRun != nil && *req.DryRun
	projected, err := m.projectPullSecrets(ctx, pod, admitted.PullSecrets, dryRun)
	if err != nil {
		denial := operatorFailure("%v", err)
		m.recordOperatorProblem(env, log, denial.detail)
		return denial.response()
	}
	if projected != "" {
		addPullSecret(mutated, projected)
	}

	patch, err := podPatch(req.Object.Raw, mutated)
	if err != nil {
		denial := operatorFailure("%v", err)
		m.recordOperatorProblem(env, log, denial.detail)
		return denial.response()
	}
	if len(patch) == 0 {
		return allowed()
	}
	response := allowedWithPatch(patch, admissionv1.PatchTypeJSONPatch)
	response.Warnings = admitted.Warnings
	return response
}

// resolutionDenial renders internal/vllm rejections as a workload denial.
func resolutionDenial(err error) *admissionDenial {
	rejections, ok := vllm.Rejected(err)
	if !ok {
		return nil
	}
	described := make([]string, len(rejections))
	for i, rejection := range rejections {
		described[i] = describeSetting(string(rejection.Setting), rejection.Value, rejection.Reason)
	}
	return workloadFailure("%s", strings.Join(described, "; "))
}

// translationDenial renders internal/translator rejections as a workload denial
// and everything else as an operator problem.
func translationDenial(err error) *admissionDenial {
	rejections, ok := translator.Rejected(err)
	if !ok {
		return operatorFailure("%v", err)
	}
	described := make([]string, len(rejections))
	for i, rejection := range rejections {
		described[i] = describeSetting(rejection.Setting, "", rejection.Reason)
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

// requestsSyntheticDevice reports whether any container or init container
// requests or limits the synthetic device.
func requestsSyntheticDevice(pod *corev1.Pod) bool {
	for i := range pod.Spec.Containers {
		if _, ok := syntheticDeviceQuantity(pod.Spec.Containers[i].Resources); ok {
			return true
		}
	}
	for i := range pod.Spec.InitContainers {
		if _, ok := syntheticDeviceQuantity(pod.Spec.InitContainers[i].Resources); ok {
			return true
		}
	}
	return false
}

// syntheticDeviceQuantity returns the container's synthetic device allocation.
func syntheticDeviceQuantity(resources corev1.ResourceRequirements) (resource.Quantity, bool) {
	name := corev1.ResourceName(syntheticDeviceName)
	if quantity, ok := resources.Requests[name]; ok {
		return quantity, true
	}
	if quantity, ok := resources.Limits[name]; ok {
		return quantity, true
	}
	return resource.Quantity{}, false
}

// alreadyInjected reports whether this operator already applied the runtime
// layout. The annotation alone is not enough: the injected containers identify a
// complete layout, exactly as internal/runtime decides it.
func alreadyInjected(pod *corev1.Pod) bool {
	if pod.Annotations[names.InjectedAnnotation] == "" {
		return false
	}
	return hasContainer(pod.Spec.Containers, names.BackendContainerName) &&
		hasContainer(pod.Spec.InitContainers, names.ProvisionerContainerName)
}

// hasContainer reports whether the named container is present.
func hasContainer(containers []corev1.Container, name string) bool {
	for _, container := range containers {
		if container.Name == name {
			return true
		}
	}
	return false
}

// workloadContainer returns the container whose vLLM invocation is resolved and
// whose device allocation is preserved.
//
// Exactly one container may own the synthetic allocation: a Pod with two GPU
// consumers cannot be translated without deciding which one the allocation
// belongs to, so the Pod as a whole is rejected rather than translated partially.
func workloadContainer(pod *corev1.Pod) (*corev1.Container, int32, *admissionDenial) {
	var owners []string
	index := -1
	for i := range pod.Spec.Containers {
		if _, ok := syntheticDeviceQuantity(pod.Spec.Containers[i].Resources); !ok {
			continue
		}
		owners = append(owners, pod.Spec.Containers[i].Name)
		if index < 0 {
			index = i
		}
	}

	switch {
	case len(owners) > 1:
		return nil, 0, workloadFailure(
			"containers %s all request %s: exactly one workload container may own the synthetic allocation",
			strings.Join(owners, ", "), syntheticDeviceName)
	case index < 0:
		return nil, 0, workloadFailure(
			"only an init container requests %s: the Pod has no workload container whose entrypoint can be translated",
			syntheticDeviceName)
	}

	quantity, _ := syntheticDeviceQuantity(pod.Spec.Containers[index].Resources)
	if quantity.MilliValue()%1000 != 0 {
		return nil, 0, workloadFailure(
			"the %s allocation %s is not a whole number of devices",
			syntheticDeviceName, quantity.String())
	}
	devices := quantity.Value()
	if devices > math.MaxInt32 {
		return nil, 0, workloadFailure(
			"the %s allocation %s exceeds the supported maximum of %d devices",
			syntheticDeviceName, quantity.String(), math.MaxInt32)
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
