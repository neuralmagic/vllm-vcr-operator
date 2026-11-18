// Package e2e is the operator's Kubernetes-interface acceptance suite.
//
// It runs a real kube-apiserver and etcd through envtest, installs the CRDs and
// the admission configurations from config/, and runs the real reconcilers and
// webhooks in-process. Every assertion is made through Kubernetes interfaces:
// objects that were created, statuses that were published, admission decisions
// that the API server enforced. No private controller or plan internals are
// touched.
//
// What this suite cannot prove, because envtest runs neither a scheduler, nor a
// kubelet, nor the Kubernetes controllers:
//
//   - Scheduling. A Pod admitted here is Pending forever: there is no
//     kube-scheduler and no device plugin, so neither extended-resource
//     accounting nor capacity exhaustion is observable. The device-plugin
//     DaemonSet is rendered and asserted, but its Pods never run and kubelet
//     never registers the plugin. Real allocation and advertisement are covered
//     by the device plugin's gRPC unit tests (internal/deviceplugin) and by the
//     kind-based cluster suite in test/cluster.
//   - Runtime behavior. Nothing executes: the frontend, the simulator, and the
//     profile provisioner never start, so readiness handshakes, token emission,
//     KV events, and lifecycle supervision are unobservable here. test/spike is
//     the executable evidence for the runtime layout, and test/cluster for the
//     lifecycle.
//   - Controller-created Pods, literally. There is no ReplicaSet controller, so
//     a Deployment never produces a Pod. The suite submits the Pod a ReplicaSet
//     would (the fixture Deployment's PodTemplate, with generateName and an
//     owner reference) to exercise the same admission path.
//   - Kubelet-reported DaemonSet readiness. The suite stands in for kubelet by
//     writing the DaemonSet status the environment reconciler reports on.
//
// The suite skips cleanly when KUBEBUILDER_ASSETS is unset; see
// test/e2e/README.md.
package e2e
