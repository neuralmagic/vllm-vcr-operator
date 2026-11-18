package cluster

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	vcrv1alpha1 "github.com/opendatahub-io/vllm-vcr-operator/api/v1alpha1"
	"github.com/opendatahub-io/vllm-vcr-operator/internal/names"
)

// Names the suite creates. They are test-owned: the operator never creates
// workload namespaces, and the registry is suite infrastructure.
const (
	operatorNamespace = "vllm-vcr-operator-system"
	registryNamespace = "vcr-registry"
	registryName      = "vcr-registry"
	webhookSecretName = "vllm-vcr-operator-webhook-server-cert"
	webhookCAConfig   = "vcr-registry-ca"

	// registryPort is the in-cluster registry's TLS port, bound as a hostPort on
	// one node so both kubelet (containerd) and Pods can reach it by node IP.
	registryPort = 5000
)

// newNamespace returns a test-owned namespace.
func newNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "vcr-cluster-acceptance"},
		},
	}
}

// newServiceAccount returns the ServiceAccount the llm-d modelserver Pods run as
// (the same identity test/fixtures/llmd gives them).
func newServiceAccount(namespace, name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// newRegistryDeployment runs a real TLS OCI registry inside the cluster.
//
// The operator resolves profile metadata over HTTPS only (internal/registry is
// constructed without Options.Insecure), so the suite cannot use a plain-HTTP
// registry: it serves the registry with a certificate signed by a CA the suite
// also installs into the operator's trust store and into each node's containerd
// configuration. The hostPort makes the same address work for Pods (node IP) and
// for kubelet's image pulls.
func newRegistryDeployment(node string) *appsv1.Deployment {
	labels := map[string]string{"app": registryName}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: registryName, Namespace: registryNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// Pinned to one node so the registry address is stable for the
					// whole run; the node is the control-plane, which keeps the
					// simulation node's capacity for the workloads under test.
					NodeName: node,
					Tolerations: []corev1.Toleration{{
						Key:      "node-role.kubernetes.io/control-plane",
						Operator: corev1.TolerationOpExists,
						Effect:   corev1.TaintEffectNoSchedule,
					}},
					Containers: []corev1.Container{{
						Name:  "registry",
						Image: "registry:2",
						Env: []corev1.EnvVar{
							{Name: "REGISTRY_HTTP_ADDR", Value: fmt.Sprintf("0.0.0.0:%d", registryPort)},
							{Name: "REGISTRY_HTTP_TLS_CERTIFICATE", Value: "/certs/tls.crt"},
							{Name: "REGISTRY_HTTP_TLS_KEY", Value: "/certs/tls.key"},
							// Test infrastructure: the registry stores blobs on an
							// emptyDir and never persists them.
							{Name: "REGISTRY_STORAGE_DELETE_ENABLED", Value: "true"},
						},
						Ports: []corev1.ContainerPort{{
							Name:          "registry",
							ContainerPort: registryPort,
							HostPort:      registryPort,
							Protocol:      corev1.ProtocolTCP,
						}},
						VolumeMounts: []corev1.VolumeMount{{Name: "tls", MountPath: "/certs", ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{
						Name: "tls",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: "vcr-registry-tls"},
						},
					}},
				},
			},
		},
	}
}

// newWebhookCertSecret returns the serving certificate Secret the manager mounts,
// with the CA included so the suite can publish the same caBundle.
func newWebhookCertSecret(cert, key, ca []byte) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: webhookSecretName, Namespace: operatorNamespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       cert,
			corev1.TLSPrivateKeyKey: key,
			"ca.crt":                ca,
		},
	}
}

// newRegistryCertSecret returns the registry's serving certificate.
func newRegistryCertSecret(cert, key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vcr-registry-tls", Namespace: registryNamespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       cert,
			corev1.TLSPrivateKeyKey: key,
		},
	}
}

// newCATrustConfigMap publishes the registry CA for the manager container.
func newCATrustConfigMap(ca []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: webhookCAConfig, Namespace: operatorNamespace},
		Data:       map[string]string{"ca.crt": string(ca)},
	}
}

// newSimulationEnvironment returns the singleton configuration the suite applies.
//
// It is the shape of test/fixtures/environments/h200.yaml with two deliberate
// differences: the profile image is the fixture image the suite pushes to its own
// registry (a digest-pinned placeholder cannot be pulled by anything), and the
// runtime sizing is small so nine single-GPU Pods fit on one node and capacity
// exhaustion — not CPU pressure — is what leaves a Pod Pending.
func newSimulationEnvironment(profileImage string) *vcrv1alpha1.SimulationEnvironment {
	return &vcrv1alpha1.SimulationEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: vcrv1alpha1.SimulationEnvironmentNameDefault},
		Spec: vcrv1alpha1.SimulationEnvironmentSpec{
			NodeSelection: vcrv1alpha1.NodeSelection{
				NodeSelector: simulationNodeLabel(),
				Tolerations: []corev1.Toleration{{
					Key:      "vcr.opendatahub-io/simulation",
					Operator: corev1.TolerationOpExists,
					Effect:   corev1.TaintEffectNoSchedule,
				}},
			},
			DevicesPerNode: 8,
			GPUType:        "H200",
			Profiles:       []vcrv1alpha1.ProfileSource{{Image: profileImage}},
			LogLevel:       "info",
			Runtime: vcrv1alpha1.RuntimeSizing{
				Frontend: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
				Backend: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
				Provisioner: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("50m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			},
		},
	}
}

// workloadOptions varies the one unchanged modelserver workload the suite submits.
type workloadOptions struct {
	name     string
	replicas int32

	// runtimeClass asks for an NVIDIA runtime class, which admission must remove
	// because simulation nodes have no accelerator runtime.
	runtimeClass string

	// gpuNodeAffinity asks for accelerator-specific node affinity, which admission
	// must remove as well.
	gpuNodeAffinity bool

	// preservedNodeAffinity adds a non-accelerator constraint admission must keep.
	preservedNodeAffinity bool

	// unsatisfiableNodeAffinity adds a non-accelerator constraint no node in the
	// cluster satisfies. Admission must preserve it; scheduling then decides.
	unsatisfiableNodeAffinity bool

	// kvEvents configures --kv-events-config, which makes the operator configure
	// the simulator's KV-event publisher.
	kvEvents bool

	// shellWrapper submits the same vLLM invocation through `sh -c`, which is not
	// a resolvable invocation and must be denied by admission.
	shellWrapper bool
}

// modelserverDeployment returns the unchanged, GPU-requesting llm-d modelserver.
//
// It mirrors test/fixtures/llmd/mock-prefix-cache-deployment.yaml: the llm-d
// labels, container name, inference and KV-event ports, probes, model-cache volume,
// ServiceAccount, and the real `vllm serve` invocation are preserved. It carries no
// simulator opt-in label or annotation, so admission has to detect the GPU request.
func modelserverDeployment(namespace string, opts workloadOptions) *appsv1.Deployment {
	replicas := opts.replicas
	if replicas == 0 {
		replicas = 1
	}
	name := opts.name
	if name == "" {
		name = "mock-prefix-cache"
	}
	labels := map[string]string{
		"llm-d.ai/guide":              "precise-prefix-cache-routing",
		"llm-d.ai/model":              "qwen3-0-6b",
		"llm-d.ai/inference-serving":  "true",
		"app.kubernetes.io/component": "modelserver",
	}

	args := []string{
		"--served-model-name=Qwen/Qwen3-0.6B",
		"--tensor-parallel-size=1",
		"--max-model-len=32768",
		"--max-num-seqs=16",
		"--enable-prefix-caching",
	}
	if opts.kvEvents {
		args = append(args, `--kv-events-config={"enable_kv_cache_events":true,"publisher":"zmq","endpoint":"tcp://*:5556"}`)
	}

	command := []string{"vllm", "serve"}
	positionalModel := []string{"Qwen/Qwen3-0.6B"}
	if opts.shellWrapper {
		command = []string{"sh", "-c"}
		positionalModel = []string{"exec vllm serve Qwen/Qwen3-0.6B " + strings.Join(args, " ")}
		args = nil
	}

	container := corev1.Container{
		Name:            "modelserver",
		Image:           "vllm/vllm-openai:v0.33.0",
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         command,
		Args:            append(positionalModel, args...),
		Env: []corev1.EnvVar{
			{Name: "MODEL", Value: "Qwen/Qwen3-0.6B"},
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
			{Name: "HF_HOME", Value: "/models"},
			// A GPU-only variable the policy must remove: the simulation nodes have
			// no CUDA devices.
			{Name: "CUDA_VISIBLE_DEVICES", Value: "0"},
		},
		Ports: []corev1.ContainerPort{
			{Name: "modelserver", ContainerPort: 8000, Protocol: corev1.ProtocolTCP},
			{Name: "kv-events", ContainerPort: 5556, Protocol: corev1.ProtocolTCP},
		},
		LivenessProbe:  httpProbe("/health", "modelserver", 10, 5, 3),
		ReadinessProbe: httpProbe("/v1/models", "modelserver", 5, 2, 3),
		StartupProbe:   httpProbe("/v1/models", "modelserver", 10, 5, 60),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:                               resource.MustParse("100m"),
				corev1.ResourceMemory:                            resource.MustParse("128Mi"),
				corev1.ResourceName(names.SyntheticResourceName): resource.MustParse("1"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory:                            resource.MustParse("256Mi"),
				corev1.ResourceName(names.SyntheticResourceName): resource.MustParse("1"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "model-cache", MountPath: "/models"}},
	}

	podSpec := corev1.PodSpec{
		ServiceAccountName: "mock-pc-sa",
		Containers:         []corev1.Container{container},
		Volumes: []corev1.Volume{{
			Name: "model-cache",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptrQuantity(resource.MustParse("1Gi"))},
			},
		}},
	}
	if opts.runtimeClass != "" {
		podSpec.RuntimeClassName = ptr(opts.runtimeClass)
	}

	var requirements []corev1.NodeSelectorRequirement
	if opts.gpuNodeAffinity {
		requirements = append(requirements, corev1.NodeSelectorRequirement{
			Key:      "nvidia.com/gpu.product",
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"NVIDIA-H200"},
		})
	}
	if opts.preservedNodeAffinity {
		requirements = append(requirements, corev1.NodeSelectorRequirement{
			Key:      "kubernetes.io/os",
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"linux"},
		})
	}
	if opts.unsatisfiableNodeAffinity {
		requirements = append(requirements, corev1.NodeSelectorRequirement{
			Key:      "vcr.opendatahub-io/rack",
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"rack-that-does-not-exist"},
		})
	}
	if len(requirements) > 0 {
		podSpec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: requirements}},
			},
		}}
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"llm-d.ai/guide": "precise-prefix-cache-routing",
				"llm-d.ai/model": "qwen3-0-6b",
				"app":            name,
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"llm-d.ai/guide":             "precise-prefix-cache-routing",
					"llm-d.ai/model":             "qwen3-0-6b",
					"llm-d.ai/inference-serving": "true",
					"app":                        name,
				}},
				Spec: podSpec,
			},
		},
	}
}

// plainPod is a Pod with no GPU request. Admission must return it unchanged.
//
// It uses an already loaded image so the assertion does not depend on pulling
// anything: the suite's only external image is the registry.
func plainPod(namespace, name, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "sleep",
				Image:   image,
				Command: []string{"sleep", "60"},
			}},
		},
	}
}

func httpProbe(path, port string, period, timeout, failureThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: path,
			Port: intstr.FromString(port),
		}},
		PeriodSeconds:    period,
		TimeoutSeconds:   timeout,
		FailureThreshold: failureThreshold,
	}
}

func ptr[T any](value T) *T { return &value }

func ptrQuantity(value resource.Quantity) *resource.Quantity { return &value }
