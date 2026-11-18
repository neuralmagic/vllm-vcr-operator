// Package translator decides whether a GPU-requesting vLLM workload can be
// simulated and, if it can, produces the admission plan internal/runtime applies
// to the Pod.
//
// The translator is the operator's settings policy (docs/settings-policy.md). It
// resolves nothing itself: internal/vllm already reports the workload's effective
// vLLM configuration, internal/profile already reports the recorded profiles, and
// the caller already resolved the SimulationEnvironment. What this package adds is
// the mapping from those three inputs to one concrete runtime layout:
//
//   - every setting the workload actually set has an explicit disposition
//     (preserve, translate, require a matching profile, require a profile
//     capability, or reject), read from a policy table rather than a switch;
//   - combinations the specification calls out are validated as combinations
//     (coverage boundaries, chunking versus batching, caching versus KV events,
//     device count versus topology);
//   - exactly one profile is selected, pinned by digest;
//   - the frontend, simulator, and provisioner invocations are produced, together
//     with sizing, placement intent, pull-secret names, provenance, and warnings.
//
// The package is pure: it makes no Kubernetes client calls and reads no
// filesystem or registry content. Rejection is the default outcome for anything
// the policy does not authorize, and every rejection names the offending setting
// and the reason without ever including a credential value.
package translator

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/plan"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/vllm"
)

// Environment is the resolved SimulationEnvironment configuration admission
// needs. The caller resolves the custom resource into this value; the translator
// never reads the API server.
type Environment struct {
	// GPUType is the simulated accelerator type from the environment
	// configuration. It participates in profile selection so that recorded
	// timing is never inferred from device count alone.
	GPUType string

	// DevicesPerNode is the number of synthetic devices advertised per
	// simulation node.
	DevicesPerNode int32

	// SupportedVLLMVersion is the vLLM release this operator build translates
	// against. Empty means the release compiled into this build
	// (names.SupportedVLLMVersion); any other value is a configuration error.
	SupportedVLLMVersion string

	// LogLevel is the simulator log filter. Empty means "info".
	LogLevel string

	// Images are the operator-provided image references for the injected
	// runtime. The profile image comes from the selected profile, not from here.
	Images Images

	// Sizing is the real CPU/memory sizing applied to the injected runtime.
	Sizing plan.Resources

	// NodeSelector selects the simulation nodes. It is applied to every admitted
	// Pod, because a Pod that cannot land on a simulation node cannot work.
	NodeSelector map[string]string

	// Tolerations are the tolerations admitted Pods receive so that they can run
	// on tainted simulation nodes.
	Tolerations []corev1.Toleration

	// PullSecrets are the environment-wide Secret names required to pull profile
	// images. Only names are used, never values.
	PullSecrets []string

	// ProfilePullSecrets overrides PullSecrets per configured profile image
	// reference (spec.profiles[].pullSecretName). The key is the image reference
	// as configured, which is what the reconciled catalog reports back.
	ProfilePullSecrets map[string]string

	// RuntimeClasses are the container runtime classes that require an
	// accelerator runtime and are therefore removed. Empty means the default set
	// (NVIDIA and other accelerator runtimes).
	RuntimeClasses []string
}

// Images are the operator-provided runtime images.
type Images struct {
	// Frontend is the Python vLLM frontend image: a CPU build of the pinned vLLM
	// release with `vllm` on PATH.
	Frontend string
	// Backend is the vllm-vcr simulator image.
	Backend string
}

// Request is one workload translation request.
type Request struct {
	// Config is the workload's effective vLLM configuration, resolved by
	// internal/vllm. Required.
	Config *vllm.Config

	// Catalog is the reconciled profile catalog. Required.
	Catalog *profile.Catalog

	// Environment is the resolved environment configuration. Required.
	Environment Environment

	// GPUDevices is the synthetic device quantity the workload requests, read
	// from its Pod spec.
	GPUDevices int32

	// PodIdentity identifies the Pod being admitted, for example its name or its
	// "ip:port". It is used to build the KV-event topic llm-d expects. When it is
	// empty the simulator derives the topic from its engine id and the plan
	// records a warning.
	PodIdentity string
}

// Translate applies the settings policy to one workload and returns the admission
// plan, or the reasons the workload cannot be simulated.
//
// The steps are ordered so that the most actionable rejection wins: input and
// policy failures are reported before profile selection, and selection failures
// before profile-dependent combination failures.
func Translate(req Request) (*plan.Admission, error) {
	if req.Config == nil {
		return nil, errors.New("translator: no vLLM configuration: the workload invocation must be resolved before translation")
	}
	if req.Catalog == nil {
		return nil, errors.New("translator: no profile catalog: profile metadata must be reconciled before admission")
	}

	env, err := req.Environment.resolve()
	if err != nil {
		return nil, err
	}
	cfg := req.Config
	if cfg.Version != "" && !sameRelease(cfg.Version, env.SupportedVLLMVersion) {
		return nil, Rejections{{
			Setting: "vllm version",
			Reason: fmt.Sprintf("the invocation was resolved against %q but this operator translates %q: exact release agreement is required",
				cfg.Version, env.SupportedVLLMVersion),
		}}
	}

	// 1. Policy pass: every setting the workload set gets its disposition.
	frontendArgs, capabilities, rejections, warnings := applyPolicy(cfg)
	rejections = append(rejections, checkInvocation(cfg)...)

	// 2. Requirements and profile selection.
	requirements := buildRequirements(cfg, env.GPUType, req.GPUDevices)
	if len(rejections) == 0 {
		if rej := checkDeviceCount(req, env); rej != nil {
			rejections = append(rejections, *rej)
		}
	}
	if len(rejections) > 0 {
		return nil, rejections
	}

	selection, err := req.Catalog.Select(requirements)
	if err != nil {
		return nil, Rejections{{Setting: "profile", Reason: err.Error()}}
	}
	metadata := selection.Metadata

	// 3. Profile-dependent policy: coverage, capability set, combinations.
	rejections = append(rejections, checkCoverage(requirements, metadata)...)
	capabilities = append(capabilities, invocationCapabilities(cfg)...)
	rejections = append(rejections, checkCapabilities(metadata, capabilities)...)

	effective, effRejections := resolveEffective(cfg, metadata, req, env)
	rejections = append(rejections, effRejections...)
	if len(rejections) > 0 {
		return nil, rejections
	}
	warnings = append(warnings, effective.warnings...)

	warnings = append(warnings, replayWarnings(metadata)...)

	// 4. Produce the plan.
	frontend := frontendInvocation(cfg, effective, env, frontendArgs)
	backend := backendInvocation(cfg, effective, env, metadata, req.PodIdentity)
	provisioner, err := provisionerInvocation(selection)
	if err != nil {
		return nil, err
	}

	pullSecrets, profilePullSecret := pullSecretsFor(env, selection.Image)
	admission := &plan.Admission{
		Profile: plan.Profile{ID: selection.ID, Image: selection.Image, Digest: selection.Digest},
		Model: plan.Model{
			Name:        modelName(cfg),
			Revision:    resolvedRevision(cfg, metadata),
			ServedNames: append([]string(nil), cfg.Model.ServedModelNames...),
		},
		GPUDevices:        req.GPUDevices,
		Frontend:          frontend,
		Backend:           backend,
		Provisioner:       provisioner,
		Resources:         env.Sizing,
		Placement:         placementFor(env),
		PullSecrets:       pullSecrets,
		ProfilePullSecret: profilePullSecret,
		Provenance: plan.Provenance{
			ProfileID:    selection.ID,
			ProfileImage: plan.Profile{Image: selection.Image, Digest: selection.Digest}.PinnedImage(),
			Model:        plan.Model{Name: modelName(cfg), Revision: resolvedRevision(cfg, metadata)}.Reference(),
			VLLMVersion:  env.SupportedVLLMVersion,
		},
		Warnings: dedupeStrings(warnings),
	}
	return admission, nil
}

// resolve validates the environment and applies its defaults.
func (e Environment) resolve() (Environment, error) {
	var failures []error
	e.GPUType = strings.TrimSpace(e.GPUType)
	if e.GPUType == "" {
		failures = append(failures, errors.New("translator: environment gpuType is required: the simulated accelerator cannot be inferred from device count"))
	}
	if e.DevicesPerNode <= 0 {
		failures = append(failures, fmt.Errorf("translator: environment devicesPerNode must be greater than 0, got %d", e.DevicesPerNode))
	}
	switch {
	case e.SupportedVLLMVersion == "":
		e.SupportedVLLMVersion = names.SupportedVLLMVersion
	case !sameRelease(e.SupportedVLLMVersion, names.SupportedVLLMVersion):
		failures = append(failures, fmt.Errorf(
			"translator: environment supportedVLLMVersion %q is not the release this operator build translates (%q): the release is fixed per operator build",
			e.SupportedVLLMVersion, names.SupportedVLLMVersion))
	}
	if strings.TrimSpace(e.Images.Frontend) == "" {
		failures = append(failures, errors.New("translator: no frontend image: the operator must provide the Python vLLM frontend image it injects"))
	}
	if strings.TrimSpace(e.Images.Backend) == "" {
		failures = append(failures, errors.New("translator: no backend image: the operator must provide the vllm-vcr simulator image it injects"))
	}
	if strings.TrimSpace(e.LogLevel) == "" {
		e.LogLevel = "info"
	}
	// Injected containers never request the synthetic device: the workload's own
	// container keeps the allocation (docs/contracts/admission.md).
	gpu := corev1.ResourceName(names.SyntheticResourceName)
	for _, sizing := range []struct {
		name        string
		requirement corev1.ResourceRequirements
	}{
		{"runtime.frontend", e.Sizing.Frontend},
		{"runtime.backend", e.Sizing.Backend},
		{"runtime.provisioner", e.Sizing.Provisioner},
	} {
		if _, ok := sizing.requirement.Requests[gpu]; ok {
			failures = append(failures, fmt.Errorf(
				"translator: environment %s must not request %s: injected containers never request the synthetic device",
				sizing.name, names.SyntheticResourceName))
		}
		if _, ok := sizing.requirement.Limits[gpu]; ok {
			failures = append(failures, fmt.Errorf(
				"translator: environment %s must not limit %s: injected containers never request the synthetic device",
				sizing.name, names.SyntheticResourceName))
		}
	}
	if len(e.RuntimeClasses) == 0 {
		e.RuntimeClasses = append([]string(nil), defaultAcceleratorRuntimeClasses...)
	}
	e.NodeSelector = copyStringMap(e.NodeSelector)
	e.Tolerations = copyTolerations(e.Tolerations)
	return e, errors.Join(failures...)
}

// sameRelease compares two vLLM release strings, tolerating a leading "v".
func sameRelease(a, b string) bool {
	normalize := func(v string) string {
		return strings.TrimPrefix(strings.TrimSpace(v), "v")
	}
	return normalize(a) == normalize(b)
}

// applyPolicy evaluates the settings policy table for every setting the workload
// actually set.
//
// Origins are consulted rather than values: a resolved default is not a workload
// request, and the policy must not reject a workload for what vLLM happens to
// default to.
func applyPolicy(cfg *vllm.Config) (frontendArgs []string, capabilities []capabilityRequest, rejections Rejections, warnings []string) {
	for _, rule := range policy {
		if !cfg.IsExplicit(rule.Setting) {
			continue
		}
		if rule.Disposition == DispositionReject {
			if rule.Check == nil {
				rejections = append(rejections, Rejection{Setting: string(rule.Setting), Reason: rule.Reason})
				continue
			}
			if err := rule.Check(cfg); err != nil {
				rejections = append(rejections, Rejection{
					Setting: string(rule.Setting),
					Reason:  fmt.Sprintf("%s (%s)", rule.Reason, err.Error()),
				})
				continue
			}
		}
		if rule.Disposition == DispositionRequireCapability && rule.Capability != "" {
			capabilities = append(capabilities, capabilityRequest{
				capability: parserCapability(cfg, rule.Capability),
				setting:    string(rule.Setting),
			})
		}
		if rule.Frontend != nil {
			frontendArgs = append(frontendArgs, rule.Frontend(cfg)...)
		}
		if rule.Warning != "" {
			warnings = append(warnings, fmt.Sprintf("%s: %s", rule.Setting, rule.Warning))
		}
	}
	return frontendArgs, capabilities, rejections, warnings
}

// checkInvocation validates aspects of the workload that are not single settings:
// the credential-bearing settings whose value cannot enter a plan, the
// environment variables whose treatment is undefined, and the explicit
// pooler/runner selections that require a profile capability.
func checkInvocation(cfg *vllm.Config) Rejections {
	var rejections Rejections

	if cfg.IsExplicit(vllm.SettingAPIKey) && cfg.Origin(vllm.SettingAPIKey) == vllm.OriginConfigFile {
		rejections = append(rejections, Rejection{
			Setting: string(vllm.SettingAPIKey),
			Reason: "an API key supplied through the config file cannot be re-emitted without retaining its value; " +
				"pass it with --api-key on the command line or with the VLLM_API_KEY environment variable",
		})
	}

	for _, variable := range cfg.Environment.Unmodeled {
		rejections = append(rejections, Rejection{
			Setting: variable,
			Reason:  familyUnknown + ": the variable can change vLLM or tokenizer behaviour, and its policy treatment is undefined",
		})
	}

	return rejections
}

// capabilityRequest is one capability the workload selects, together with the
// setting that selected it, so a rejection names the workload's own configuration.
type capabilityRequest struct {
	capability string
	setting    string
}

// parserCapability qualifies a parser-family capability with the parser the
// workload selected, so that a profile which validated one tool parser does not
// silently authorize a different one. The qualified form is the profile image
// contract's "toolCallParser:<name>" / "reasoningParser:<name>".
func parserCapability(cfg *vllm.Config, capability string) string {
	switch capability {
	case profile.CapabilityToolCallParsers:
		if name := strings.TrimSpace(stringValue(cfg.Frontend.ToolCallParser)); name != "" {
			return "toolCallParser:" + name
		}
	case profile.CapabilityReasoningParsers:
		if name := strings.TrimSpace(stringValue(cfg.Frontend.ReasoningParser)); name != "" {
			return "reasoningParser:" + name
		}
	}
	return capability
}

// invocationCapabilities reports the profile capabilities the workload's
// execution-model selection implies. The runner and conversion mode decide whether
// the frontend serves generation or pooling output, and a profile only vouches for
// the output kinds its capability set grants.
func invocationCapabilities(cfg *vllm.Config) []capabilityRequest {
	var capabilities []capabilityRequest
	switch strings.ToLower(stringValue(cfg.Model.Runner)) {
	case "pooling":
		capabilities = append(capabilities, capabilityRequest{
			capability: profile.CapabilityEmbeddings,
			setting:    string(vllm.SettingRunner),
		})
	}
	switch strings.ToLower(stringValue(cfg.Model.Convert)) {
	case "embed", "classify":
		capabilities = append(capabilities, capabilityRequest{
			capability: profile.CapabilityEmbeddings,
			setting:    string(vllm.SettingConvert),
		})
	}
	return capabilities
}

// buildRequirements turns the workload's explicit settings into the constraints
// profile selection must satisfy. Unset optional settings leave the profile's
// recorded value standing.
func buildRequirements(cfg *vllm.Config, gpuType string, gpuDevices int32) profile.Requirements {
	req := profile.Requirements{
		Model:             modelName(cfg),
		ModelRevision:     stringValue(cfg.Model.Revision),
		Tokenizer:         stringValue(cfg.Tokenizer.Name),
		TokenizerRevision: stringValue(cfg.Tokenizer.Revision),
		GPUType:           gpuType,
		GPUCount:          gpuDevices,
	}
	if cfg.ChatTemplate.Set() {
		req.ChatTemplateHash = "sha256:" + cfg.ChatTemplate.SHA256
	}
	if cfg.IsExplicit(vllm.SettingTensorParallelSize) && cfg.Parallel.TensorParallelSize != nil {
		req.TensorParallel = int32(*cfg.Parallel.TensorParallelSize)
	}
	if cfg.IsExplicit(vllm.SettingDtype) {
		req.DType = stringValue(cfg.Precision.DType)
	}
	if cfg.IsExplicit(vllm.SettingQuantization) {
		req.Quantization = stringValue(cfg.Precision.Quantization)
	}
	if cfg.IsExplicit(vllm.SettingBlockSize) && cfg.Cache.BlockSize != nil {
		req.BlockSize = int32(*cfg.Cache.BlockSize)
	}
	if cfg.IsExplicit(vllm.SettingPrefixCaching) {
		enabled := cfg.Cache.EnablePrefixCaching != nil && *cfg.Cache.EnablePrefixCaching
		req.EnablePrefixCaching = &enabled
	}
	// Limits are requirements only when the workload set them: a resolved vLLM
	// default is not a workload request and must not be measured against the
	// profile's coverage.
	if cfg.IsExplicit(vllm.SettingMaxModelLen) && cfg.Limits.MaxModelLen != nil && *cfg.Limits.MaxModelLen > 0 {
		// "-1"/"auto" is not a requirement: it asks for the model maximum, which
		// the profile's declared coverage bound represents.
		req.MaxModelLen = int64Value(cfg.Limits.MaxModelLen)
	}
	if cfg.IsExplicit(vllm.SettingMaxNumSeqs) {
		req.MaxNumSeqs = int64Value(cfg.Limits.MaxNumSeqs)
	}
	if cfg.IsExplicit(vllm.SettingMaxNumBatchedTokens) {
		req.MaxNumBatchedTokens = int64Value(cfg.Limits.MaxNumBatchedTokens)
	}
	if cfg.IsExplicit(vllm.SettingLongPrefillTokens) {
		req.LongPrefillTokenThreshold = int64Value(cfg.Scheduling.LongPrefillTokenThreshold)
	}
	if cfg.IsExplicit(vllm.SettingNumGPUBlocks) {
		req.KVBlocks = int64Value(cfg.Memory.NumGPUBlocksOverride)
	}
	if cfg.IsExplicit(vllm.SettingSchedulingPolicy) {
		req.SchedulingPolicy = stringValue(cfg.Scheduling.Policy)
	}
	if cfg.IsExplicit(vllm.SettingGPUMemUtilization) {
		req.GPUMemoryUtilization = cfg.Memory.GPUMemoryUtilization
	}
	return req
}

// checkDeviceCount rejects a workload whose device request the environment cannot
// advertise, and a tensor-parallel request that disagrees with the device count.
func checkDeviceCount(req Request, env Environment) *Rejection {
	if req.GPUDevices <= 0 {
		return &Rejection{
			Setting: names.SyntheticResourceName,
			Reason:  "the workload requests no synthetic device, so no recorded topology can be selected; a GPU-requesting workload is required",
		}
	}
	if env.DevicesPerNode > 0 && req.GPUDevices > env.DevicesPerNode {
		return &Rejection{
			Setting: names.SyntheticResourceName,
			Reason: fmt.Sprintf("the workload requests %d devices but the environment advertises %d per node: the request cannot be scheduled on a simulation node",
				req.GPUDevices, env.DevicesPerNode),
		}
	}
	tp := req.Config.Parallel.TensorParallelSize
	if req.Config.IsExplicit(vllm.SettingTensorParallelSize) && tp != nil && *tp != int(req.GPUDevices) {
		return &Rejection{
			Setting: string(vllm.SettingTensorParallelSize),
			Reason: fmt.Sprintf("tensor parallel size %d is inconsistent with the %d requested device(s): the recorded topology must match the requested device quantity",
				*tp, req.GPUDevices),
		}
	}
	return nil
}

// checkCoverage reports the profile override limits the workload exceeds.
func checkCoverage(req profile.Requirements, metadata *profile.Metadata) Rejections {
	err := profile.ValidateOverrides(req, metadata)
	if err == nil {
		return nil
	}
	return Rejections{{Setting: "profile coverage", Reason: err.Error()}}
}

// checkCapabilities reports every capability the workload selected but the
// profile's validated capability set does not grant.
func checkCapabilities(metadata *profile.Metadata, capabilities []capabilityRequest) Rejections {
	var rejections Rejections
	seen := make(map[string]struct{}, len(capabilities))
	for _, request := range capabilities {
		if _, duplicate := seen[request.capability]; duplicate {
			continue
		}
		seen[request.capability] = struct{}{}
		if err := profile.RequireCapability(metadata, request.capability); err != nil {
			rejections = append(rejections, Rejection{Setting: request.setting, Reason: err.Error()})
		}
	}
	return rejections
}

// resolvedRevision returns the model revision the simulation is pinned to: the
// workload's own pin when it set one, otherwise the revision the selected profile
// recorded. Provenance must identify the model actually served.
func resolvedRevision(cfg *vllm.Config, metadata *profile.Metadata) string {
	if revision := strings.TrimSpace(stringValue(cfg.Model.Revision)); revision != "" {
		return revision
	}
	if metadata != nil {
		return metadata.Model.Revision
	}
	return ""
}

// modelName returns the workload's underlying model identity.
func modelName(cfg *vllm.Config) string {
	return strings.TrimSpace(stringValue(cfg.Model.Name))
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func int64Value(v *int) *int64 {
	if v == nil {
		return nil
	}
	out := int64(*v)
	return &out
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyTolerations(in []corev1.Toleration) []corev1.Toleration {
	if in == nil {
		return nil
	}
	out := make([]corev1.Toleration, len(in))
	copy(out, in)
	return out
}

// dedupeStrings removes duplicates while preserving order, so a plan is
// deterministic.
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, value := range in {
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// replayWarnings records how content replay behaves for the selected profile.
//
// Recorded token ids are replayed only when the profile authorizes it. Otherwise
// outputs are the simulator's seeded random tokens, which is valid for protocol and
// routing checks but never for content assertions, and the plan says so.
func replayWarnings(metadata *profile.Metadata) []string {
	if metadata.Supports(profile.CapabilityContentReplay) {
		return []string{
			"content replay: recorded token ids are served for matching requests; unmatched requests fall back to random tokens, so the match rate must be verified through the simulator control API",
		}
	}
	return []string{
		"content replay: profile capabilities.contentReplay is not granted, so responses carry simulated random tokens and content-identical assertions are not authorized",
	}
}
