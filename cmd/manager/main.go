// Command manager runs the vllm-vcr-operator: configuration and device-plugin
// reconciliation, profile metadata reconciliation, and Pod admission.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	vcrv1alpha1 "github.com/neuralmagic/vllm-vcr-operator/api/v1alpha1"
	"github.com/neuralmagic/vllm-vcr-operator/internal/controller"
	operatorwebhook "github.com/neuralmagic/vllm-vcr-operator/internal/webhook"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(vcrv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr    string
		probeAddr      string
		operatorNS     string
		leaderElection bool
		webhookPort    int
		webhookCertDir string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Metrics endpoint address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health probe address.")
	flag.StringVar(&operatorNS, "operator-namespace", envOr("OPERATOR_NAMESPACE", "vllm-vcr-operator-system"),
		"Namespace holding operator-owned resource and Secret references.")
	flag.BoolVar(&leaderElection, "leader-elect", false, "Enable leader election.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Admission webhook port.")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "Webhook certificate directory.")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "vllm-vcr-operator.vcr.opendatahub-io",
		WebhookServer:          webhook.NewServer(webhook.Options{Port: webhookPort, CertDir: webhookCertDir}),
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// With more than one replica the admission Service load-balances across them,
	// so a replica must not report Ready until its webhook server is serving.
	if err := mgr.AddReadyzCheck("webhook-server", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to set up webhook readiness check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	environmentReconciler, err := controller.NewVcrGpuClusterReconciler(controller.Options{
		Client:            mgr.GetClient(),
		OperatorNamespace: operatorNS,
		DevicePluginImage: controller.DevicePluginImageFromEnv(os.Getenv),
		Recorder:          mgr.GetEventRecorderFor("vcrgpucluster"),
	})
	if err != nil {
		setupLog.Error(err, "unable to create the simulation environment reconciler")
		os.Exit(1)
	}
	if err := environmentReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register the simulation environment reconciler")
		os.Exit(1)
	}

	// A namespace's profile selection is resolved against the cluster's catalog so that a
	// wrong identity is reported on the object rather than at Pod creation.
	if err := controller.NewVcrGpuProfileReconciler(mgr.GetClient()).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register the GPU profile selection reconciler")
		os.Exit(1)
	}

	// Admission writes the projected pull Secret, and this sweep keeps it right:
	// rotated sources reach every copy and copies whose Pods are gone disappear.
	if err := mgr.Add(&controller.ProjectedSecretReconciler{
		Client:            mgr.GetClient(),
		Reader:            mgr.GetAPIReader(),
		OperatorNamespace: operatorNS,
		Log:               ctrl.Log.WithName("projected-secrets"),
	}); err != nil {
		setupLog.Error(err, "unable to register the projected pull-secret reconciler")
		os.Exit(1)
	}

	if err := operatorwebhook.SetupWithManager(mgr, operatorwebhook.Options{
		Client:            mgr.GetClient(),
		Reader:            mgr.GetAPIReader(),
		OperatorNamespace: operatorNS,
		FrontendImage:     operatorwebhook.FrontendImageFromEnv(os.Getenv),
		BackendImage:      operatorwebhook.BackendImageFromEnv(os.Getenv),
		Recorder:          mgr.GetEventRecorderFor("pod-admission"),
	}); err != nil {
		setupLog.Error(err, "unable to register the admission webhooks")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "operatorNamespace", operatorNS)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
