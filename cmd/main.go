package main

import (
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/api/v1alpha1"
	cloudservice "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/internal/cloud"
	controller "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/internal/controller"
)

var scheme = runtime.NewScheme()

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(infrav1.AddToScheme(scheme))
}

func main() {
	var metricsAddress string
	var probeAddress string
	var leaderElection bool

	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "Address for the metrics endpoint")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8081", "Address for health probes")
	flag.BoolVar(&leaderElection, "leader-elect", true, "Enable leader election")
	zapOptions := zap.Options{Development: false}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))
	setupLog := ctrl.Log.WithName("setup")

	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddress},
		HealthProbeBindAddress: probeAddress,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "t-cloud-network-controller.infrastructure.otc.t-systems.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	reconciler := &controller.TCloudClusterNetworkReconciler{
		Client:    manager.GetClient(),
		APIReader: manager.GetAPIReader(),
		Scheme:    manager.GetScheme(),
		Factory:   cloudservice.TCloudFactory{},
		Recorder:  manager.GetEventRecorderFor("t-cloud-network-controller"),
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to register health check")
		os.Exit(1)
	}
	if err := manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to register readiness check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager stopped with an error")
		os.Exit(1)
	}
}
