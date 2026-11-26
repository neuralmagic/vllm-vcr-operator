package webhook_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/client-go/tools/record"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/webhook"
)

// TestWebhookPathsMatchTheInstalledManifests asserts the paths this package
// registers are the ones config/webhook installs. They are hand-written, so the
// test is what keeps the two from diverging.
func TestWebhookPathsMatchTheInstalledManifests(t *testing.T) {
	cases := map[string]string{
		webhook.PodMutatorPath:           "mutating_webhook.yaml",
		webhook.EnvironmentValidatorPath: "validating_webhook.yaml",
	}
	for path, manifest := range cases {
		content, err := os.ReadFile(filepath.Join("..", "..", "config", "webhook", manifest))
		if err != nil {
			t.Fatalf("read %s: %v", manifest, err)
		}
		if !strings.Contains(string(content), "path: "+path) {
			t.Errorf("%s does not register the path %q this package serves", manifest, path)
		}
	}
}

// TestImageResolutionFromEnv asserts the deploy-time image wiring and its documented
// placeholders.
func TestImageResolutionFromEnv(t *testing.T) {
	if got := webhook.FrontendImageFromEnv(func(string) string { return "" }); got != webhook.DefaultFrontendImage {
		t.Errorf("frontend default = %q, want %q", got, webhook.DefaultFrontendImage)
	}
	if got := webhook.BackendImageFromEnv(func(string) string { return "" }); got != webhook.DefaultBackendImage {
		t.Errorf("backend default = %q, want %q", got, webhook.DefaultBackendImage)
	}

	if got := webhook.FrontendImageFromEnv(func(key string) string {
		if key != webhook.EnvFrontendImage {
			t.Errorf("looked up %q, want %q", key, webhook.EnvFrontendImage)
		}
		return "registry.example.com/frontend:test"
	}); got != "registry.example.com/frontend:test" {
		t.Errorf("frontend override = %q", got)
	}
	if got := webhook.BackendImageFromEnv(func(key string) string {
		if key != webhook.EnvBackendImage {
			t.Errorf("looked up %q, want %q", key, webhook.EnvBackendImage)
		}
		return "registry.example.com/backend:test"
	}); got != "registry.example.com/backend:test" {
		t.Errorf("backend override = %q", got)
	}
}

// TestNewPodMutatorRequiresWiring asserts the operator fails at startup, with an
// actionable message, when a setting it cannot default is missing.
func TestNewPodMutatorRequiresWiring(t *testing.T) {
	c := testClient(t)
	cases := []struct {
		name    string
		options webhook.Options
		want    string
	}{
		{
			name: "no client",
			options: webhook.Options{
				OperatorNamespace: operatorNamespace,
				FrontendImage:     testFrontend,
				BackendImage:      testBackend,
			},
			want: "client",
		},
		{
			name: "no operator namespace",
			options: webhook.Options{
				Client:        c,
				FrontendImage: testFrontend,
				BackendImage:  testBackend,
			},
			want: "operator namespace",
		},
		{
			name: "no frontend image",
			options: webhook.Options{
				Client:            c,
				OperatorNamespace: operatorNamespace,
				BackendImage:      testBackend,
			},
			want: webhook.EnvFrontendImage,
		},
		{
			name: "no backend image",
			options: webhook.Options{
				Client:            c,
				OperatorNamespace: operatorNamespace,
				FrontendImage:     testFrontend,
			},
			want: webhook.EnvBackendImage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutator, err := webhook.NewPodMutator(tc.options)
			if err == nil {
				t.Fatalf("NewPodMutator accepted %s, returned %v", tc.name, mutator)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestOperatorProblemIsEvented asserts an operator-caused denial is visible on the
// environment, so a user does not have to infer it from rejected Pods.
func TestOperatorProblemIsEvented(t *testing.T) {
	env := testEnvironment(t, func(e *vcrv1alpha1.VcrGpuCluster) {
		conflicting := resolvedProfile(t, testMetadata())
		conflicting.Digest = "sha256:ffffffffffffffff"
		e.Status.Profiles = append(e.Status.Profiles, conflicting)
	})
	recorder := record.NewFakeRecorder(1)
	mutator, err := webhook.NewPodMutator(webhook.Options{
		Client:            testClient(t, env),
		OperatorNamespace: operatorNamespace,
		FrontendImage:     testFrontend,
		BackendImage:      testBackend,
		Recorder:          recorder,
	})
	if err != nil {
		t.Fatalf("new PodMutator: %v", err)
	}

	resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))
	if resp.Allowed {
		t.Fatal("GPU Pod admitted with a conflicting catalog")
	}
	if message := messageOf(resp); !strings.Contains(message, "operator problem") {
		t.Errorf("denial message %q does not identify an operator problem", message)
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "AdmissionDenied") {
			t.Errorf("event %q does not carry the admission reason", event)
		}
	default:
		t.Error("no event was emitted for the operator problem")
	}
}

// TestOptionsDefaultReaderAndClock asserts the documented defaults, so an integrator
// that only supplies the required fields still works.
func TestOptionsDefaultReaderAndClock(t *testing.T) {
	c := testClient(t, testEnvironment(t))
	mutator, err := webhook.NewPodMutator(webhook.Options{
		Client:            c,
		OperatorNamespace: operatorNamespace,
		FrontendImage:     testFrontend,
		BackendImage:      testBackend,
	})
	if err != nil {
		t.Fatalf("new PodMutator with only required fields: %v", err)
	}
	if resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod())); !resp.Allowed {
		t.Errorf("admission without explicit Reader/Now failed: %s", messageOf(resp))
	}
}

// TestAdmissionDecisionsAreEvented asserts the two outcomes a test author needs
// to see from Kubernetes alone: an accepted translation announces the pinned
// profile, and a refusal announces why. Events are queued off the admission path,
// so the assertions wait for the forwarder rather than assuming synchronous
// delivery.
func TestAdmissionDecisionsAreEvented(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		env := testEnvironment(t)
		recorder := record.NewFakeRecorder(16)
		mutator, err := webhook.NewPodMutator(webhook.Options{
			Client:            testClient(t, env),
			OperatorNamespace: operatorNamespace,
			FrontendImage:     testFrontend,
			BackendImage:      testBackend,
			Recorder:          recorder,
		})
		if err != nil {
			t.Fatalf("new PodMutator: %v", err)
		}

		resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, testPod()))
		if !resp.Allowed {
			t.Fatalf("GPU Pod denied: %s", messageOf(resp))
		}
		event := waitForEvent(t, recorder, "SimulationInjected")
		if !strings.Contains(event, "mock-profile") && !strings.Contains(event, "profile ") {
			t.Errorf("event %q does not name the selected profile", event)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		env := testEnvironment(t)
		recorder := record.NewFakeRecorder(16)
		mutator, err := webhook.NewPodMutator(webhook.Options{
			Client:            testClient(t, env),
			OperatorNamespace: operatorNamespace,
			FrontendImage:     testFrontend,
			BackendImage:      testBackend,
			Recorder:          recorder,
		})
		if err != nil {
			t.Fatalf("new PodMutator: %v", err)
		}

		pod := testPod()
		pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args, "--pipeline-parallel-size=2")
		resp := mutator.Admit(t.Context(), admissionRequest(t, admissionv1.Create, pod))
		if resp.Allowed {
			t.Fatal("Pod with pipeline parallelism admitted")
		}
		event := waitForEvent(t, recorder, "SimulationRejected")
		if !strings.Contains(event, "pipeline") {
			t.Errorf("event %q does not name the refused setting", event)
		}
	})
}

// waitForEvent blocks until an event with the given reason is recorded.
func waitForEvent(t *testing.T, recorder *record.FakeRecorder, reason string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, reason) {
				return event
			}
		case <-deadline:
			t.Fatalf("no %s event was recorded", reason)
			return ""
		}
	}
}
