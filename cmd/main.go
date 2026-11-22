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
	"github.com/lachgar-Azzeddine/joule-budget/internal/credentials"
	"github.com/lachgar-Azzeddine/joule-budget/internal/energy"
	"github.com/lachgar-Azzeddine/joule-budget/internal/proxmox"
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

		proxmoxURL    string
		proxmoxCA     string
		openbaoURL    string
		openbaoRole   string
		openbaoSecret string
		spiffeSocket  string
	)
	flag.StringVar(&prometheusURL, "prometheus-url", "http://victoria-metrics.monitoring.svc:8428", "Prometheus-compatible API holding Kepler and cAdvisor metrics.")
	flag.StringVar(&hubVM, "hub-vm", "hub", "Kepler vm_name of the VM running this cluster.")
	flag.DurationVar(&interval, "interval", time.Minute, "Time between two measurements of a budget.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address of the metrics endpoint (0 disables it).")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address of the health probes.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election (several replicas).")
	flag.StringVar(&proxmoxURL, "proxmox-url", "", "Proxmox VE API (e.g. https://10.10.0.1:8006) to power tenant VMs off while asleep; empty disables it.")
	flag.StringVar(&proxmoxCA, "proxmox-ca", "/etc/joule-budget/proxmox-ca.pem", "CA certificate of the Proxmox API (pve-root-ca.pem).")
	flag.StringVar(&openbaoURL, "openbao-url", "http://openbao.openbao.svc:8200", "OpenBao holding the Proxmox API token.")
	flag.StringVar(&openbaoRole, "openbao-role", "joule-budget", "OpenBao jwt role bound to this controller's SPIFFE ID.")
	flag.StringVar(&openbaoSecret, "openbao-secret", "joule-system/proxmox", "KV v2 path (mount secret) with token_id and token_secret.")
	flag.StringVar(&spiffeSocket, "spiffe-socket", "unix:///spiffe-workload-api/spire-agent.sock", "SPIFFE Workload API socket.")
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

	var power controller.Power
	if proxmoxURL != "" {
		ca, err := os.ReadFile(proxmoxCA)
		if err != nil {
			setupLog.Error(err, "reading Proxmox CA")
			os.Exit(1)
		}
		httpClient, err := proxmox.NewHTTPClient(ca)
		if err != nil {
			setupLog.Error(err, "Proxmox CA")
			os.Exit(1)
		}
		// The Proxmox token lives in OpenBao; the controller logs in with its
		// SPIFFE identity, reads it, and keeps it in memory for 10 minutes.
		bao := &credentials.OpenBao{
			Address: openbaoURL, Role: openbaoRole, Audience: "openbao", Path: openbaoSecret,
			TTL: 10 * time.Minute, JWT: credentials.SPIFFE{Socket: spiffeSocket},
		}
		power = &proxmox.Power{
			Client:          &proxmox.Client{URL: proxmoxURL, Credentials: bao.Secret, HTTP: httpClient},
			ShutdownTimeout: 3 * time.Minute,
		}
		setupLog.Info("powering tenant VMs", "proxmox", proxmoxURL, "openbao", openbaoURL, "secret", openbaoSecret)
	}

	if err := (&controller.EnergyBudgetReconciler{
		Client:   mgr.GetClient(),
		Meter:    energy.Meter{Querier: prom, HubVM: hubVM, Step: time.Minute},
		Sleeper:  sleep.Sleeper{Client: mgr.GetClient()},
		Power:    power,
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
