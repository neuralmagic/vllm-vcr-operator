// Package fixtures_test holds the checks that keep the integration fixtures and
// the documentation honest.
//
// The fixtures are manifests, not code: without these checks a renamed field, a
// dropped GPU request, or a simulator-specific label could slip into the very
// workload the operator's primary acceptance interface depends on. The checks
// read the fixtures the way a cluster does (typed Kubernetes objects) and read
// the documented profile the way the operator does (internal/profile).
package fixtures_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/opendatahub-io/vllm-vcr-operator/api/v1alpha1"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/profile"
)

// repoRoot is the repository root, resolved from this package's directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

// TestEveryFixtureManifestParses asserts every manifest shipped as a fixture or
// an example is valid YAML, and that every manifest meant for a cluster decodes
// into an object with apiVersion and kind. A fixture that does not parse cannot
// be applied, so it cannot be evidence for anything.
func TestEveryFixtureManifestParses(t *testing.T) {
	root := repoRoot(t)
	dirs := []string{
		filepath.Join(root, "test", "fixtures"),
		filepath.Join(root, "examples"),
	}

	checked := 0
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !isManifest(entry.Name()) {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var object struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
			}
			if err := yaml.Unmarshal(raw, &object); err != nil {
				t.Errorf("%s does not parse as YAML: %v", path, err)
				return nil
			}
			if len(raw) == 0 {
				t.Errorf("%s is empty", path)
			}
			if strings.HasSuffix(entry.Name(), ".values.yaml") {
				// Helm values, not a manifest: it has no apiVersion or kind, and a
				// real chart reads it.
				return nil
			}
			if object.APIVersion == "" || object.Kind == "" {
				t.Errorf("%s is not a Kubernetes object: apiVersion=%q kind=%q", path, object.APIVersion, object.Kind)
			}
			checked++
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if checked == 0 {
		t.Fatal("no fixture manifests were checked")
	}
}

// TestSimulationEnvironmentFixtureDecodesIntoTheAPI asserts the H200 environment
// fixture is a valid SimulationEnvironment for the current API: the singleton
// name, the simulated accelerator, the per-node device count, and runtime sizing
// that does not request the synthetic resource (only the workload's own container
// holds that allocation).
func TestSimulationEnvironmentFixtureDecodesIntoTheAPI(t *testing.T) {
	path := filepath.Join("environments", "h200.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var environment v1alpha1.SimulationEnvironment
	if err := yaml.UnmarshalStrict(raw, &environment); err != nil {
		t.Fatalf("%s does not decode into v1alpha1.SimulationEnvironment: %v", path, err)
	}

	if environment.Name != v1alpha1.SimulationEnvironmentNameDefault {
		t.Errorf("name = %q, want the only accepted name %q", environment.Name, v1alpha1.SimulationEnvironmentNameDefault)
	}
	if environment.Spec.GPUType != "H200" {
		t.Errorf("gpuType = %q", environment.Spec.GPUType)
	}
	if environment.Spec.DevicesPerNode != 8 {
		t.Errorf("devicesPerNode = %d", environment.Spec.DevicesPerNode)
	}
	if len(environment.Spec.NodeSelection.NodeSelector) == 0 {
		t.Error("simulationNodes.nodeSelector is empty: no node would receive the device plugin")
	}
	if len(environment.Spec.Profiles) != 1 {
		t.Fatalf("profiles = %d, want the single placeholder profile", len(environment.Spec.Profiles))
	}
	if image := environment.Spec.Profiles[0].Image; !strings.Contains(image, "@sha256:") {
		t.Errorf("profile image %q is not digest-pinned", image)
	}

	for _, container := range []struct {
		name     string
		requests corev1.ResourceRequirements
	}{
		{"frontend", environment.Spec.Runtime.Frontend},
		{"backend", environment.Spec.Runtime.Backend},
	} {
		for _, list := range []corev1.ResourceList{container.requests.Requests, container.requests.Limits} {
			if _, ok := list[corev1.ResourceName(names.SyntheticResourceName)]; ok {
				t.Errorf("runtime.%s requests %s: only the workload's own container may hold the synthetic allocation",
					container.name, names.SyntheticResourceName)
			}
		}
	}
}

// TestModelserverFixturesStayUnmodifiedGPUWorkloads asserts the fixture that
// pins the operator's primary acceptance interface: a real, GPU-requesting llm-d
// modelserver that carries llm-d's discovery labels and ports and no
// simulator-specific opt-in of any kind.
func TestModelserverFixturesStayUnmodifiedGPUWorkloads(t *testing.T) {
	for _, fixture := range []struct {
		file          string
		publishesKV   bool
		wantReplicas  int32
		wantGPUAmount string
	}{
		{file: "mock-prefix-cache-deployment.yaml", wantReplicas: 3, wantGPUAmount: "1"},
		{file: "mock-prefix-cache-kv-events-deployment.yaml", publishesKV: true, wantReplicas: 3, wantGPUAmount: "1"},
	} {
		t.Run(fixture.file, func(t *testing.T) {
			path := filepath.Join("llmd", fixture.file)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			var deployment appsv1.Deployment
			if err := yaml.UnmarshalStrict(raw, &deployment); err != nil {
				t.Fatalf("%s does not decode into apps/v1 Deployment: %v", path, err)
			}

			if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != fixture.wantReplicas {
				t.Errorf("replicas = %v, want %d", deployment.Spec.Replicas, fixture.wantReplicas)
			}

			pod := deployment.Spec.Template
			for _, label := range []string{"llm-d.ai/guide", "llm-d.ai/model", "llm-d.ai/inference-serving"} {
				if pod.Labels[label] == "" {
					t.Errorf("pod label %q is missing: llm-d discovery selects on it", label)
				}
			}
			if pod.Labels["llm-d.ai/inference-serving"] != "true" {
				t.Errorf("llm-d.ai/inference-serving = %q, want \"true\"", pod.Labels["llm-d.ai/inference-serving"])
			}

			// No simulator opt-in: admission must detect the GPU request alone.
			for key := range pod.Annotations {
				if strings.HasPrefix(key, names.AnnotationPrefix) {
					t.Errorf("pod annotation %q is simulator-specific: the fixture must not opt in", key)
				}
			}
			for key := range pod.Labels {
				if strings.HasPrefix(key, names.LabelPrefix) {
					t.Errorf("pod label %q is simulator-specific: the fixture must not opt in", key)
				}
			}
			for key := range deployment.Labels {
				if strings.HasPrefix(key, names.LabelPrefix) {
					t.Errorf("deployment label %q is simulator-specific: the fixture must not opt in", key)
				}
			}

			if len(pod.Spec.Containers) != 1 {
				t.Fatalf("containers = %d, want exactly the modelserver", len(pod.Spec.Containers))
			}
			container := pod.Spec.Containers[0]
			if container.Name != "modelserver" {
				t.Errorf("container name = %q", container.Name)
			}
			for _, resources := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
				quantity, ok := resources[corev1.ResourceName(names.SyntheticResourceName)]
				if !ok {
					t.Fatalf("container does not request %s: admission would leave it unchanged", names.SyntheticResourceName)
				}
				if got := quantity.String(); got != fixture.wantGPUAmount {
					t.Errorf("%s = %q, want %q", names.SyntheticResourceName, got, fixture.wantGPUAmount)
				}
			}

			if len(container.Command) == 0 || container.Command[0] != "vllm" {
				t.Errorf("command = %v: the fixture must be a real vLLM invocation, not a simulator entrypoint", container.Command)
			}

			for _, port := range []int32{names.DefaultInferencePort, names.KVEventsPort} {
				if !hasPort(container.Ports, port) {
					t.Errorf("container does not declare port %d", port)
				}
			}
			for _, probe := range []struct {
				name  string
				probe *corev1.Probe
			}{
				{"liveness", container.LivenessProbe},
				{"readiness", container.ReadinessProbe},
				{"startup", container.StartupProbe},
			} {
				if probe.probe == nil || probe.probe.HTTPGet == nil {
					t.Errorf("%s probe is missing", probe.name)
					continue
				}
				if probe.probe.HTTPGet.Port.String() != "modelserver" {
					t.Errorf("%s probe targets port %q, want the modelserver port", probe.name, probe.probe.HTTPGet.Port.String())
				}
			}

			kvEventsConfigured := false
			for _, arg := range container.Args {
				if strings.HasPrefix(arg, "--kv-events-config=") {
					kvEventsConfigured = true
				}
			}
			if kvEventsConfigured != fixture.publishesKV {
				t.Errorf("--kv-events-config present = %v, want %v", kvEventsConfigured, fixture.publishesKV)
			}
			if fixture.publishesKV && !strings.Contains(strings.Join(container.Args, " "), `"enable_kv_cache_events":true`) {
				t.Error("the kv-events fixture does not enable KV-cache events")
			}
		})
	}
}

// TestPlaceholderProfileSatisfiesTheProfileImageContract asserts the checked-in
// profile fixture is a valid profile document that claims nothing: no recorded
// tokens, no latency model, no KV events, and no capabilities. A fixture that
// claimed a capability would authorize serving features no recording supports.
func TestPlaceholderProfileSatisfiesTheProfileImageContract(t *testing.T) {
	dir := filepath.Join("profiles", "qwen3-0.6b-h200-tp1-bf16")
	raw, err := os.ReadFile(filepath.Join(dir, names.ProfileMetadataFileName))
	if err != nil {
		t.Fatalf("read profile fixture: %v", err)
	}

	metadata, err := profile.Parse(raw)
	if err != nil {
		t.Fatalf("the profile fixture is not a valid profile document: %v", err)
	}
	if !strings.HasPrefix(metadata.ID, "placeholder-") {
		t.Errorf("id = %q, want the placeholder prefix so no reader mistakes it for a release", metadata.ID)
	}
	if metadata.Trace.RecordedTokens {
		t.Error("the placeholder profile claims recorded tokens")
	}
	if metadata.Trace.LatencyModel {
		t.Error("the placeholder profile claims a latency model")
	}
	if metadata.Trace.KVEvents {
		t.Error("the placeholder profile claims KV-cache events")
	}
	if metadata.Capabilities.ChatCompletions || metadata.Capabilities.Completions || metadata.Capabilities.Streaming || metadata.Capabilities.ContentReplay || metadata.Capabilities.KVEventReplay {
		t.Errorf("the placeholder profile grants capabilities: %+v", metadata.Capabilities)
	}
	if metadata.GPU.Type != "H200" || metadata.GPU.Count != 1 {
		t.Errorf("gpu = %+v, want the H200 single-device topology", metadata.GPU)
	}

	tracePath := filepath.Join(dir, metadata.Trace.File)
	traceRaw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read the declared trace: %v", err)
	}
	if len(traceRaw) == 0 {
		t.Fatal("the declared trace is empty")
	}
	reader, err := gzip.NewReader(bytes.NewReader(traceRaw))
	if err != nil {
		t.Fatalf("the declared trace is not gzip-compressed: %v", err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompress the declared trace: %v", err)
	}
	if !strings.Contains(string(decoded), "placeholder") {
		t.Errorf("the trace does not identify itself as a placeholder: %s", decoded)
	}
}

// TestProfileExportAcceptsThePlaceholderProfile asserts the fixture is usable by
// the operator's provisioning entrypoint: vcr-profile-export validates the
// metadata, finds the declared trace, and writes both documents into the target
// directory. This is the one provisioning check that needs no images.
func TestProfileExportAcceptsThePlaceholderProfile(t *testing.T) {
	source := filepath.Join(repoRoot(t), "test", "fixtures", "profiles", "qwen3-0.6b-h200-tp1-bf16")
	target := filepath.Join(t.TempDir(), "profile")

	command := exec.Command("go", "run", "./cmd/profileexport", "--source", source, target)
	command.Dir = repoRoot(t)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("vcr-profile-export rejected the profile fixture: %v\n%s", err, output)
	}

	for _, name := range []string{names.ProfileMetadataFileName, "trace.jsonl.gz"} {
		info, err := os.Stat(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("the export did not produce %q: %v", name, err)
		}
		if info.Size() == 0 {
			t.Errorf("the export produced an empty %q", name)
		}
	}

	var summary struct {
		ProfileID string `json:"profileId"`
	}
	if err := json.Unmarshal(output, &summary); err != nil {
		t.Fatalf("the export summary is not JSON: %v\n%s", err, output)
	}
	if !strings.HasPrefix(summary.ProfileID, "placeholder-") {
		t.Errorf("exported profileId = %q", summary.ProfileID)
	}
}

// TestRouterValuesAgreeWithTheFixtures asserts the router reference and the
// modelserver fixtures describe the same integration: the discovery label, the
// inference port, the KV-event port, and the block size the router hashes at.
//
// A silent disagreement here is the worst failure mode the fixture set has: the
// router would keep returning 200 while prefix-cache locality never happens, and
// no assertion elsewhere would notice.
func TestRouterValuesAgreeWithTheFixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("router", "precise-prefix-cache-routing.values.yaml"))
	if err != nil {
		t.Fatalf("read router values: %v", err)
	}
	// The values file carries more than these fields (chart-specific
	// configuration this repository does not read), so decoding is not strict.
	var values struct {
		Router struct {
			EPP struct {
				PluginsCustomConfig map[string]string `json:"pluginsCustomConfig"`
			} `json:"epp"`
			ModelServers struct {
				TargetPorts []struct {
					Number int32 `json:"number"`
				} `json:"targetPorts"`
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"modelServers"`
		} `json:"router"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("router values do not decode: %v", err)
	}

	var plugins string
	for _, content := range values.Router.EPP.PluginsCustomConfig {
		plugins += content
	}
	if plugins == "" {
		t.Fatal("router values carry no EPP plugin configuration")
	}
	if !strings.Contains(plugins, fmt.Sprintf("socketPort: %d", names.KVEventsPort)) {
		t.Errorf("the EPP does not discover pods on the KV-event port %d", names.KVEventsPort)
	}
	if len(values.Router.ModelServers.TargetPorts) != 1 || values.Router.ModelServers.TargetPorts[0].Number != names.DefaultInferencePort {
		t.Errorf("modelServers.targetPorts = %+v, want the inference port %d", values.Router.ModelServers.TargetPorts, names.DefaultInferencePort)
	}

	// The block size must equal the profile's recorded cache.blockSize, which is
	// what the operator passes to the simulator as --tokens-per-block.
	metadataRaw, err := os.ReadFile(filepath.Join("profiles", "qwen3-0.6b-h200-tp1-bf16", names.ProfileMetadataFileName))
	if err != nil {
		t.Fatalf("read the profile fixture: %v", err)
	}
	metadata, err := profile.Parse(metadataRaw)
	if err != nil {
		t.Fatalf("parse the profile fixture: %v", err)
	}
	if !strings.Contains(plugins, fmt.Sprintf("blockSize: %d", metadata.Cache.BlockSize)) {
		t.Errorf("the EPP hashes at a block size the profile does not record (profile: %d)", metadata.Cache.BlockSize)
	}

	// The router selects pods by label, so the label the fixtures carry must be
	// the label the router matches on.
	deploymentRaw, err := os.ReadFile(filepath.Join("llmd", "mock-prefix-cache-deployment.yaml"))
	if err != nil {
		t.Fatalf("read the modelserver fixture: %v", err)
	}
	var deployment appsv1.Deployment
	if err := yaml.UnmarshalStrict(deploymentRaw, &deployment); err != nil {
		t.Fatalf("decode the modelserver fixture: %v", err)
	}
	if len(values.Router.ModelServers.MatchLabels) == 0 {
		t.Fatal("router values select no model servers")
	}
	for key, want := range values.Router.ModelServers.MatchLabels {
		if got := deployment.Spec.Template.Labels[key]; got != want {
			t.Errorf("modelserver fixture label %q = %q, router matches %q", key, got, want)
		}
	}
}

// markdownLink matches an inline markdown link's target.
var markdownLink = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

// TestDocumentedRelativeLinksResolve asserts every relative link in this
// repository's Markdown resolves to a file or directory that exists, so a
// document cannot point a reader at a page that was never written.
func TestDocumentedRelativeLinksResolve(t *testing.T) {
	root := repoRoot(t)
	checked := 0

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "bin", "evidence":
				if entry.Name() == ".git" || entry.Name() == "bin" {
					return fs.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(raw), -1) {
			target := match[1]
			if strings.HasPrefix(target, "#") || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if index := strings.IndexAny(target, "#?"); index >= 0 {
				target = target[:index]
			}
			if target == "" {
				continue
			}
			checked++
			resolved := filepath.Join(filepath.Dir(path), filepath.FromSlash(target))
			if _, err := os.Stat(resolved); err != nil {
				relative, relErr := filepath.Rel(root, path)
				if relErr != nil {
					relative = path
				}
				t.Errorf("%s links to %q, which does not exist", relative, match[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
	if checked == 0 {
		t.Fatal("no relative Markdown links were checked")
	}
}

// isManifest reports whether name is a YAML manifest file.
func isManifest(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// hasPort reports whether ports declares containerPort.
func hasPort(ports []corev1.ContainerPort, containerPort int32) bool {
	for _, port := range ports {
		if port.ContainerPort == containerPort {
			return true
		}
	}
	return false
}
