package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/neuralmagic/vllm-vcr-operator/test/cluster/certs"
	apimachyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// installState is what later steps need from the installation.
type installState struct {
	// registryHost is the registry address both Pods and kubelet use, e.g.
	// "10.89.0.3:5000".
	registryHost string
	// registryNode is the node the registry is pinned to.
	registryNode string
	// profileImage is the tag reference the VcrGpuCluster names. The
	// operator resolves and pins it; the suite never asserts a digest it computed
	// itself.
	profileImage string
	// webhookCA is the PEM caBundle published on both admission registrations.
	webhookCA []byte
}

// installOperator installs config/default without cert-manager, on a cluster whose
// node images are already loaded, and prepares the test registry.
//
// The order matters and mirrors hack/install.sh: the admission registrations are
// fail-closed, so they are applied only once the manager (and therefore the webhook
// Service) is Ready. The difference is that this suite must install the operator
// images it built locally and a CA the manager trusts, which is why the manager
// Deployment is patched in-flight rather than installed with its placeholders.
func (s *suite) installOperator(t *testing.T) {
	t.Helper()

	webhookBundle, err := certs.Generate(certs.Options{
		CommonName: webhookServiceDNS(operatorNamespace),
		DNSNames: []string{
			webhookServiceDNS(operatorNamespace),
			webhookServiceDNS(operatorNamespace) + ".cluster.local",
		},
		Validity: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("generate the webhook serving certificate: %v", err)
	}
	s.install.webhookCA = webhookBundle.CACert

	registryBundle, err := certs.Generate(certs.Options{
		CommonName:  "vcr-registry",
		DNSNames:    []string{fmt.Sprintf("%s.%s.svc", registryName, registryNamespace)},
		IPAddresses: []net.IP{net.ParseIP(s.nodeIPs[s.otherNode])},
		Validity:    24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("generate the registry serving certificate: %v", err)
	}

	documents, err := s.kustomizeBuild("config/default")
	if err != nil {
		t.Fatalf("build config/default: %v", err)
	}

	var phase1, webhooks []*unstructured.Unstructured
	for _, document := range documents {
		switch document.GetKind() {
		case "Namespace":
			phase1 = append(phase1, document)
		case "Certificate", "Issuer":
			// cert-manager is not installed: the suite issues its own serving
			// certificate and publishes the caBundle itself.
			continue
		case "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration":
			webhooks = append(webhooks, document)
		default:
			phase1 = append(phase1, document)
		}
	}
	if len(phase1) == 0 || len(webhooks) != 2 {
		t.Fatalf("config/default produced %d core document(s) and %d admission registration(s); want both admission registrations", len(phase1), len(webhooks))
	}

	manager, err := findDeployment(phase1, operatorNamespace, "vllm-vcr-operator-controller-manager")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if err := s.configureManager(manager); err != nil {
		t.Fatalf("configure the manager Deployment: %v", err)
	}

	// kubectl applies a stream in file order, and the objects the manager needs at
	// startup (its namespace, its serving certificate, the registry CA) must exist
	// before the manager Deployment does.
	var stream bytes.Buffer
	appendDocument := func(object any) {
		encoded, err := yaml.Marshal(object)
		if err != nil {
			t.Fatalf("encode an installation document: %v", err)
		}
		stream.WriteString("---\n")
		stream.Write(encoded)
	}
	for _, document := range phase1 {
		if document.GetKind() == "Namespace" {
			appendDocument(document.Object)
		}
	}
	appendDocument(newWebhookCertSecret(webhookBundle.Cert, webhookBundle.Key, webhookBundle.CACert))
	appendDocument(newCATrustConfigMap(registryBundle.CACert))
	for _, document := range phase1 {
		if document.GetKind() == "Namespace" {
			continue
		}
		appendDocument(document.Object)
	}

	t.Logf("applying config/default (operator images %s, %s)", s.images.manager, s.images.devicePlugin)
	if err := s.applyManifests(stream.String()); err != nil {
		t.Fatalf("apply the operator installation: %v", err)
	}

	// The shipped namespace manifest carries the Pod Security label the device
	// plugin needs (pod-security.kubernetes.io/enforce: privileged, because the
	// plugin mounts /var/lib/kubelet/device-plugins as a hostPath). The suite
	// installs it unchanged and asserts the resulting behaviour: the DaemonSet
	// becomes Ready and synthetic capacity appears. `hack/verify-manifests.sh`
	// fails if that label regresses.

	if _, err := s.kubectl("wait", "--for=condition=Established", "--timeout=120s",
		"crd/vcrgpuclusters.vcr.opendatahub-io"); err != nil {
		s.dumpClusterState(t, "install")
		t.Fatalf("the VcrGpuCluster CRD did not become Established: %v", err)
	}
	if _, err := s.kubectl("-n", operatorNamespace, "rollout", "status",
		"deployment/vllm-vcr-operator-controller-manager", "--timeout=180s"); err != nil {
		s.dumpManifests(t, "install")
		t.Fatalf("the manager Deployment did not become Ready: %v", err)
	}

	t.Log("applying the admission registrations now that the webhook Service has endpoints")
	for _, object := range webhooks {
		encoded, err := yaml.Marshal(object.Object)
		if err != nil {
			t.Fatalf("encode %s: %v", object.GetName(), err)
		}
		if err := s.applyManifests(string(encoded)); err != nil {
			t.Fatalf("apply %s: %v", object.GetKind(), err)
		}
	}
	caBundle := base64.StdEncoding.EncodeToString(s.install.webhookCA)
	for _, configuration := range []string{
		"mutatingwebhookconfiguration/vllm-vcr-operator-mutating-webhook",
		"validatingwebhookconfiguration/vllm-vcr-operator-validating-webhook",
	} {
		patch := fmt.Sprintf(`[{"op":"add","path":"/webhooks/0/clientConfig/caBundle","value":"%s"}]`, caBundle)
		if _, err := s.kubectl("patch", configuration, "--type=json", "-p", patch); err != nil {
			t.Fatalf("publish the caBundle on %s: %v", configuration, err)
		}
	}

	s.installRegistry(t, registryBundle)
}

// configureManager points the installed manager at the images the suite built and
// at the CA that signs the test registry's certificate.
//
// The CA is published through SSL_CERT_FILE because the operator resolves profile
// metadata with go-containerregistry over HTTPS and must trust a registry the
// cluster has no public certificate for. A Secret value never enters this path: the
// CA is public material.
func (s *suite) configureManager(manager *unstructured.Unstructured) error {
	containers, found, err := unstructured.NestedSlice(manager.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		return fmt.Errorf("the manager Deployment has no containers")
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return fmt.Errorf("the manager Deployment's first container is not an object")
	}

	if err := unstructured.SetNestedField(container, s.images.manager, "image"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(container, string(corev1.PullIfNotPresent), "imagePullPolicy"); err != nil {
		return err
	}

	env, _, err := unstructured.NestedSlice(container, "env")
	if err != nil {
		return err
	}
	replaced := map[string]string{
		"VCR_FRONTEND_IMAGE":      s.images.frontendStub,
		"VCR_BACKEND_IMAGE":       s.images.backendStub,
		"VCR_DEVICE_PLUGIN_IMAGE": s.images.devicePlugin,
		"VCR_SUPERVISOR_IMAGE":    s.images.frontendStub,
		// Trust the test registry's CA. SSL_CERT_FILE is how a Go TLS client
		// replaces its root store.
		"SSL_CERT_FILE": "/etc/vcr-ca/ca.crt",
	}
	out := make([]any, 0, len(env)+len(replaced))
	for _, entry := range env {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(item, "name")
		if value, replace := replaced[name]; replace {
			item["value"] = value
			delete(item, "valueFrom")
			delete(replaced, name)
		}
		out = append(out, item)
	}
	for name, value := range replaced {
		out = append(out, map[string]any{"name": name, "value": value})
	}
	if err := unstructured.SetNestedSlice(container, out, "env"); err != nil {
		return err
	}

	mounts, _, err := unstructured.NestedSlice(container, "volumeMounts")
	if err != nil {
		return err
	}
	mounts = append(mounts, map[string]any{
		"name":      "vcr-registry-ca",
		"mountPath": "/etc/vcr-ca",
		"readOnly":  true,
	})
	if err := unstructured.SetNestedSlice(container, mounts, "volumeMounts"); err != nil {
		return err
	}
	containers[0] = container
	if err := unstructured.SetNestedSlice(manager.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		return err
	}

	volumes, _, err := unstructured.NestedSlice(manager.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return err
	}
	volumes = append(volumes, map[string]any{
		"name": "vcr-registry-ca",
		"configMap": map[string]any{
			"name": webhookCAConfig,
		},
	})
	return unstructured.SetNestedSlice(manager.Object, volumes, "spec", "template", "spec", "volumes")
}

// installRegistry runs the in-cluster TLS registry, teaches every node's containerd
// to trust it, and pushes the profile fixture image into it.
func (s *suite) installRegistry(t *testing.T, bundle certs.Bundle) {
	t.Helper()
	ctx := t.Context()

	registryNode := s.otherNode
	if err := s.ensureObject(ctx, newNamespace(registryNamespace)); err != nil {
		t.Fatalf("create the registry namespace: %v", err)
	}
	if err := s.ensureObject(ctx, newRegistryCertSecret(bundle.Cert, bundle.Key)); err != nil {
		t.Fatalf("create the registry TLS Secret: %v", err)
	}
	if err := s.ensureObject(ctx, newRegistryDeployment(registryNode)); err != nil {
		t.Fatalf("create the registry Deployment: %v", err)
	}
	if _, err := s.kubectl("-n", registryNamespace, "rollout", "status",
		"deployment/"+registryName, "--timeout=180s"); err != nil {
		s.dumpManifests(t, "install")
		t.Fatalf("the test registry did not become available: %v", err)
	}

	registryHost := fmt.Sprintf("%s:%d", s.nodeIPs[registryNode], registryPort)
	if registryHost == fmt.Sprintf(":%d", registryPort) {
		t.Fatalf("node %q has no InternalIP address", registryNode)
	}
	s.install.registryHost = registryHost
	s.install.registryNode = registryNode

	// kubelet pulls profile images through containerd, which needs the CA per
	// registry host. Pods reach the same host through the node IP.
	hostsDir := filepath.Join("/etc/containerd/certs.d", registryHost)
	hosts := fmt.Sprintf(`server = "https://%s"
[host."https://%s"]
  ca = "%s/ca.crt"
  capabilities = ["pull", "resolve", "push"]
`, registryHost, registryHost, hostsDir)
	for _, node := range s.nodes {
		if err := s.nodeWriteFile(node.Name, filepath.Join(hostsDir, "ca.crt"), bundle.CACert); err != nil {
			t.Fatalf("install the registry CA on node %q: %v", node.Name, err)
		}
		if err := s.nodeWriteFile(node.Name, filepath.Join(hostsDir, "hosts.toml"), []byte(hosts)); err != nil {
			t.Fatalf("install the containerd registry configuration on node %q: %v", node.Name, err)
		}
	}

	profileImage := registryHost + "/" + strings.TrimPrefix(s.images.profileFixture, "docker.io/")
	s.install.profileImage = profileImage
	s.pushProfileImage(t, registryNode, registryHost, profileImage, filepath.Join(hostsDir, "ca.crt"))
}

// pushProfileImage publishes the locally loaded profile fixture through the node's
// containerd, which already has the image and can reach the registry.
func (s *suite) pushProfileImage(t *testing.T, node, registryHost, profileImage, caPath string) {
	t.Helper()

	if _, err := s.nodeExec(node, "ctr", "-n", "k8s.io", "images", "tag", s.images.profileFixture, profileImage); err != nil {
		t.Fatalf("tag the profile fixture for the test registry: %v", err)
	}
	// --local is what makes ctr use --tlscacert; the registry's certificate is
	// signed by the suite's CA, which is not a system root.
	if out, err := s.nodeExec(node, "ctr", "-n", "k8s.io", "images", "push",
		"--local", "--platform", "linux/amd64", "--tlscacert", caPath, profileImage); err != nil {
		t.Fatalf("push %s to %s: %v", s.images.profileFixture, registryHost, err)
	} else {
		t.Logf("pushed the profile fixture to %s", strings.SplitN(profileImage, "@", 2)[0])
		_ = out
	}
}

// ensureObject creates an object, tolerating an object that a previous run left
// behind so that repeated suite runs do not require a fresh cluster.
func (s *suite) ensureObject(ctx context.Context, object client.Object) error {
	err := s.client.Create(ctx, object)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	return s.client.Update(ctx, object)
}

// kustomizeBuild renders a kustomization with kubectl's built-in kustomize.
func (s *suite) kustomizeBuild(path string) ([]*unstructured.Unstructured, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "kubectl", "--context", s.context, "kustomize", path)
	command.Dir = s.root
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("kubectl kustomize %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}

	decoder := apimachyaml.NewYAMLOrJSONDecoder(bytes.NewReader(stdout.Bytes()), 4096)
	var documents []*unstructured.Unstructured
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode kustomize output: %w", err)
		}
		if len(object.Object) == 0 {
			continue
		}
		documents = append(documents, &object)
	}
	return documents, nil
}

// findDeployment returns the named Deployment document from a rendered stream.
func findDeployment(documents []*unstructured.Unstructured, namespace, name string) (*unstructured.Unstructured, error) {
	for _, document := range documents {
		if document.GetKind() == "Deployment" && document.GetName() == name {
			return document, nil
		}
	}
	return nil, fmt.Errorf("rendered manifests contain no Deployment %s/%s", namespace, name)
}

// webhookServiceDNS is the in-cluster DNS name of the webhook Service.
func webhookServiceDNS(namespace string) string {
	return "vllm-vcr-operator-webhook-service." + namespace + ".svc"
}

// dumpManifests writes the rendered installation and the manager's diagnostics.
func (s *suite) dumpManifests(t *testing.T, step string) {
	t.Helper()
	dir := s.artifactDir(t, step)
	s.kubectlToFile(dir, "manager-describe.txt", "-n", operatorNamespace, "describe", "deployment/vllm-vcr-operator-controller-manager")
	s.kubectlToFile(dir, "manager-pods.txt", "-n", operatorNamespace, "get", "pods", "-o", "wide")
	s.kubectlToFile(dir, "manager-logs.txt", "-n", operatorNamespace, "logs", "-l", "control-plane=controller-manager", "--tail=500")
	s.kubectlToFile(dir, "manager-events.txt", "-n", operatorNamespace, "get", "events", "--sort-by=.lastTimestamp")
}

// kubectlToFile runs kubectl and writes combined output into dir/name.
func (s *suite) kubectlToFile(dir, name string, args ...string) {
	out, err := s.kubectl(args...)
	content := out
	if err != nil {
		content += "\n# error: " + err.Error() + "\n"
	}
	if writeErr := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); writeErr != nil {
		s.logf("write %s: %v", name, writeErr)
	}
}
