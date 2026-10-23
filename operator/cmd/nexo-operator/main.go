// Command nexo-operator runs the Kubernetes operator of Nexo: it reconciles
// the NexoCluster resources of the cluster (or of the namespaces of
// WATCH_NAMESPACE, comma separated).
package main

import (
	"flag"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	nexov1 "github.com/r3n3r0/nexo/operator/api/v1alpha1"
	"github.com/r3n3r0/nexo/operator/internal/controller"
)

func main() {
	var metricsAddr, probeAddr string
	var leaderElect bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address of the metrics endpoint (0 disables it)")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address of the health probes")
	flag.BoolVar(&leaderElect, "leader-elect", true, "elect a leader among the replicas of the operator")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setup := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nexov1.AddToScheme(scheme))

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "nexo-operator.nexo.r3n3r0.github.io",
	}
	if ns := os.Getenv("WATCH_NAMESPACE"); ns != "" {
		namespaces := map[string]cache.Config{}
		for _, n := range strings.Split(ns, ",") {
			namespaces[strings.TrimSpace(n)] = cache.Config{}
		}
		mgrOpts.Cache = cache.Options{DefaultNamespaces: namespaces}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setup.Error(err, "unable to create the manager")
		os.Exit(1)
	}
	r := &controller.Reconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Health:   controller.HTTPHealth{Timeout: 5 * time.Second},
		Recorder: mgr.GetEventRecorder("nexo-operator"),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		setup.Error(err, "unable to create the controller")
		os.Exit(1)
	}
	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))
	setup.Info("starting the manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setup.Error(err, "manager stopped")
		os.Exit(1)
	}
}
