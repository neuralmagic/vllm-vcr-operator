package cluster

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/names"
)

// Environment variables that configure a suite run.
const (
	// EnvContext selects the kubeconfig context of the acceptance cluster.
	EnvContext = "VCR_CLUSTER_CONTEXT"

	// DefaultContext is the context hack/kind-up.sh creates.
	DefaultContext = "kind-vcr-e2e"

	// EnvImagePrefix and EnvImageTag select the image references hack/images.sh
	// builds and loads.
	EnvImagePrefix = "VCR_IMAGE_PREFIX"
	EnvImageTag    = "VCR_IMAGE_TAG"

	// EnvRuntime selects the container runtime used for node-level operations
	// (writing containerd registry configuration, pushing the profile image).
	EnvRuntime = "VCR_IMAGE_RUNTIME"

	// EnvArtifactsDir overrides where failure diagnostics are written.
	EnvArtifactsDir = "VCR_CLUSTER_ARTIFACTS"

	// EnvRequired turns a skip into a failure, so a job that intends to run the
	// suite cannot report a green skip as success.
	EnvRequired = "VCR_CLUSTER_REQUIRED"
)

const (
	defaultImagePrefix = "docker.io/vcrlocal"
	defaultImageTag    = "test"
	defaultArtifacts   = "test/cluster/_artifacts"
)

// images holds the references hack/images.sh builds.
type images struct {
	manager        string
	devicePlugin   string
	frontendStub   string
	backendStub    string
	profileFixture string
}

// all returns every reference the suite needs loaded into the cluster's nodes.
func (i images) all() map[string]string {
	return map[string]string{
		"manager":         i.manager,
		"device plugin":   i.devicePlugin,
		"frontend stub":   i.frontendStub,
		"backend stub":    i.backendStub,
		"profile fixture": i.profileFixture,
	}
}

// environment is the resolved cluster the suite runs against.
type environment struct {
	context  string
	cluster  string // kind cluster name, derived from the context
	root     string // repository root
	runtime  string // container runtime used for node-level operations
	images   images
	required bool

	config *rest.Config
	client client.Client

	nodes []corev1.Node
	// simulationNode is the worker labelled as a simulation node,
	// otherNode is a node that is not selected by the SimulationEnvironment.
	simulationNode string
	otherNode      string
	nodeIPs        map[string]string
}

// loadEnvironment resolves the cluster and skips the test when a prerequisite is
// missing. Every skip names what is missing and how to provide it.
func loadEnvironment(t *testing.T) *environment {
	t.Helper()

	required := os.Getenv(EnvRequired) == "1"
	skip := func(format string, args ...any) {
		t.Helper()
		message := fmt.Sprintf(format, args...)
		if required {
			t.Fatalf("%s (skipped would be reported green; %s=1 requires a run)", message, EnvRequired)
		}
		t.Skipf("%s\n\nrun: hack/kind-up.sh && hack/images.sh --build --stub --load", message)
	}

	kubeContext := strings.TrimSpace(os.Getenv(EnvContext))
	if kubeContext == "" {
		kubeContext = DefaultContext
	}

	root, err := repositoryRoot()
	if err != nil {
		skip("cannot locate the repository root: %v", err)
	}

	config, err := clientConfig(kubeContext)
	if err != nil {
		skip("kubeconfig context %q is not usable: %v", kubeContext, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	clusterScheme := scheme.Scheme
	if err := vcrv1alpha1.AddToScheme(clusterScheme); err != nil {
		t.Fatalf("register the SimulationEnvironment API in the client scheme: %v", err)
	}
	if err := appsv1.AddToScheme(clusterScheme); err != nil {
		t.Fatalf("register apps/v1 in the client scheme: %v", err)
	}
	kube, err := client.New(config, client.Options{Scheme: clusterScheme})
	if err != nil {
		t.Fatalf("build the cluster client: %v", err)
	}

	var nodeList corev1.NodeList
	if err := kube.List(ctx, &nodeList); err != nil {
		skip("cluster context %q is not reachable: %v", kubeContext, err)
	}
	if len(nodeList.Items) < 2 {
		skip("cluster context %q has %d node(s): the suite needs a control-plane node and a worker so that placement is observable", kubeContext, len(nodeList.Items))
	}

	runtime, err := containerRuntime()
	if err != nil {
		skip("%v", err)
	}
	if !strings.HasPrefix(kubeContext, "kind-") {
		skip("context %q is not a kind cluster: the suite installs containerd registry configuration and pushes the profile image through the node containers, which only kind exposes", kubeContext)
	}

	prefix := env2(os.Getenv, EnvImagePrefix, defaultImagePrefix)
	tag := env2(os.Getenv, EnvImageTag, defaultImageTag)
	env := &environment{
		context:  kubeContext,
		cluster:  strings.TrimPrefix(kubeContext, "kind-"),
		root:     root,
		runtime:  runtime,
		required: required,
		config:   config,
		client:   kube,
		nodes:    nodeList.Items,
		nodeIPs:  map[string]string{},
		images: images{
			manager:        imageRef(prefix, "vllm-vcr-operator", tag),
			devicePlugin:   imageRef(prefix, "vllm-vcr-device-plugin", tag),
			frontendStub:   imageRef(prefix, "vllm-vcr-frontend-stub", tag),
			backendStub:    imageRef(prefix, "vllm-vcr-backend-stub", tag),
			profileFixture: imageRef(prefix, "vllm-vcr-profile-fixture", tag),
		},
	}

	for i := range env.nodes {
		node := &env.nodes[i]
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP {
				env.nodeIPs[node.Name] = address.Address
			}
		}
		if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
			if env.otherNode == "" {
				env.otherNode = node.Name
			}
			continue
		}
		if env.simulationNode == "" {
			env.simulationNode = node.Name
		}
	}
	if env.simulationNode == "" {
		skip("no worker node found in cluster %q: the suite needs a non-control-plane node to label as a simulation node", env.cluster)
	}
	if env.otherNode == "" {
		skip("no control-plane node found in cluster %q: the suite asserts the device plugin is scheduled on simulation nodes only", env.cluster)
	}

	// Every image must already be in the nodes' containerd: the suite does not
	// build images, and an ImagePullBackOff three steps into a 5-minute run is a
	// much worse failure than a skip that names the missing prerequisite.
	present, err := env.nodeImages(env.simulationNode)
	if err != nil {
		skip("cannot list images in node %q: %v", env.simulationNode, err)
	}
	var missing []string
	for description, ref := range env.images.all() {
		if !present[ref] {
			missing = append(missing, fmt.Sprintf("%s (%s)", description, ref))
		}
	}
	if len(missing) > 0 {
		skip("these images are not loaded into node %q: %s", env.simulationNode, strings.Join(missing, ", "))
	}

	return env
}

// env2 returns the environment value or a default.
func env2(getenv func(string) string, key, fallback string) string {
	if value := strings.TrimSpace(getenv(key)); value != "" {
		return value
	}
	return fallback
}

func imageRef(prefix, name, tag string) string { return prefix + "/" + name + ":" + tag }

// clientConfig builds a REST config for the requested context, so the suite never
// depends on which context happens to be current.
func clientConfig(kubeContext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	initial, err := rules.Load()
	if err != nil {
		return nil, err
	}
	if _, ok := initial.Contexts[kubeContext]; !ok {
		return nil, fmt.Errorf("no such context in kubeconfig %v", rules.Precedence)
	}
	return clientcmd.NewNonInteractiveClientConfig(*initial, kubeContext, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
}

// repositoryRoot walks up from the working directory until it finds go.mod.
func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// containerRuntime resolves the runtime used for node-level operations.
func containerRuntime() (string, error) {
	if value := strings.TrimSpace(os.Getenv(EnvRuntime)); value != "" {
		if _, err := exec.LookPath(value); err != nil {
			return "", fmt.Errorf("%s=%s is not installed", EnvRuntime, value)
		}
		return value, nil
	}
	if _, err := exec.LookPath("podman"); err == nil {
		return "podman", nil
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return "docker", nil
	}
	return "", fmt.Errorf("neither podman nor docker is installed: the suite needs one of them to reach the kind node containers")
}

// nodeImages lists the fully qualified image references a node's containerd has.
func (e *environment) nodeImages(node string) (map[string]bool, error) {
	out, err := e.nodeExec(node, "ctr", "-n", "k8s.io", "images", "ls", "-q")
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			present[line] = true
		}
	}
	return present, nil
}

// nodeExec runs a command inside a kind node container.
func (e *environment) nodeExec(node string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, e.runtime, append([]string{"exec", node}, args...)...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s exec %s %s: %w: %s", e.runtime, node, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// nodeWriteFile writes content into a node container, creating parent directories.
func (e *environment) nodeWriteFile(node, path string, content []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, e.runtime, "exec", "-i", node, "sh", "-c",
		fmt.Sprintf("mkdir -p %q && cat > %q", filepath.Dir(path), path))
	command.Stdin = strings.NewReader(string(content))
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s exec %s: write %s: %w: %s", e.runtime, node, path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// kubectl runs kubectl against the suite's context.
func (e *environment) kubectl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return e.kubectlContext(ctx, nil, args...)
}

// kubectlInput runs kubectl with stdin, which is how manifests are applied.
func (e *environment) kubectlInput(stdin []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return e.kubectlContext(ctx, stdin, args...)
}

func (e *environment) kubectlContext(ctx context.Context, stdin []byte, args ...string) (string, error) {
	full := append([]string{"--context", e.context}, args...)
	command := exec.CommandContext(ctx, "kubectl", full...)
	if stdin != nil {
		command.Stdin = strings.NewReader(string(stdin))
	}
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return stdout.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// applyManifests applies YAML documents (a multi-document stream is fine).
func (e *environment) applyManifests(manifests string) error {
	_, err := e.kubectlInput([]byte(manifests), "apply", "-f", "-")
	return err
}

// simulationNodeLabel is the one label the SimulationEnvironment selects.
func simulationNodeLabel() map[string]string {
	return map[string]string{"vcr.opendatahub-io/simulation": "true"}
}

// waitFor polls condition until it returns nil or the timeout elapses.
func waitFor(ctx context.Context, timeout time.Duration, interval time.Duration, condition func(context.Context) (bool, string, error)) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		ok, detail, err := condition(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		last = detail
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for: %s", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// nodeAllocatableGPU reads the synthetic resource from a node's status.
func (e *environment) nodeAllocatableGPU(ctx context.Context, node string) (int64, error) {
	var current corev1.Node
	if err := e.client.Get(ctx, client.ObjectKey{Name: node}, &current); err != nil {
		return 0, err
	}
	quantity, ok := current.Status.Allocatable[corev1.ResourceName(names.SyntheticResourceName)]
	if !ok {
		return 0, nil
	}
	return quantity.Value(), nil
}

// podReady reports whether a Pod's Ready condition is true.
func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podCondition returns one condition of a Pod, if present.
func podCondition(pod *corev1.Pod, conditionType corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == conditionType {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// containerByName returns a container spec from a Pod.
func containerByName(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// initContainerByName returns an init container spec from a Pod.
func initContainerByName(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

// containerStatus returns the status of a named container.
func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}
