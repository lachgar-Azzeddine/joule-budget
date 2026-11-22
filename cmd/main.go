// Command joule-budget runs the EnergyBudget controller.
package main

import (
	"flag"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	joulev1alpha1 "github.com/lachgar-Azzeddine/joule-budget/api/v1alpha1"
	"github.com/lachgar-Azzeddine/joule-budget/internal/controller"
	"github.com/lachgar-Azzeddine/joule-budget/internal/energy"
	"github.com/lachgar-Azzeddine/joule-budget/internal/sleep"
)

func main() {
	var (
		prometheusURL string
		hubVM         string
		interval      time.Duration
		metricsAddr   string
		probeAddr     string
		leaderElect   bool
	)
	flag.StringVar(&prometheusURL, "prometheus-url", "http://victoria-metrics.monitoring.svc:8428", "Prometheus-compatible API holding Kepler and cAdvisor metrics.")
	flag.StringVar(&hubVM, "hub-vm", "hub", "Kepler vm_name of the VM running this cluster.")
	flag.DurationVar(&interval, "interval", time.Minute, "Time between two measurements of a budget.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address of the metrics endpoint (0 disables it).")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address of the health probes.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election (several replicas).")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(joulev1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "joule-budget.joule.azzhomelab.tech",
		// Workloads are read only while sleeping or waking a tenant: read them
		// directly instead of caching (and watching) every one in the cluster.
		// Tenants and TenantControlPlanes are unstructured, never cached.
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor: []client.Object{&appsv1.Deployment{}, &appsv1.StatefulSet{}},
		}},
	})
	if err != nil {
		setupLog.Error(err, "creating manager")
		os.Exit(1)
	}

	prom, err := energy.NewPrometheus(prometheusURL)
	if err != nil {
		setupLog.Error(err, "creating Prometheus client")
		os.Exit(1)
	}

	if err := (&controller.EnergyBudgetReconciler{
		Client:   mgr.GetClient(),
		Meter:    energy.Meter{Querier: prom, HubVM: hubVM, Step: time.Minute},
		Sleeper:  sleep.Sleeper{Client: mgr.GetClient()},
		Recorder: mgr.GetEventRecorder("joule-budget"),
		Interval: interval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "setting up controller")
		os.Exit(1)
	}

	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	setupLog.Info("starting", "prometheus", prometheusURL, "hubVM", hubVM, "interval", interval)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "running manager")
		os.Exit(1)
	}
}
