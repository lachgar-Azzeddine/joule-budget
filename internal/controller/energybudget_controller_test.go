package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	joulev1alpha1 "github.com/lachgar-Azzeddine/joule-budget/api/v1alpha1"
	"github.com/lachgar-Azzeddine/joule-budget/internal/energy"
)

var k8s client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// envtest needs kube-apiserver and etcd: `make test` downloads them.
		os.Exit(0)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd"), "testdata"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = joulev1alpha1.AddToScheme(scheme)
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

type fakeMeter struct {
	usage  energy.Usage
	got    energy.Tenant
	start  time.Time
	called bool
}

func (f *fakeMeter) Measure(_ context.Context, t energy.Tenant, start, _ time.Time) (energy.Usage, error) {
	f.got, f.start, f.called = t, start, true
	return f.usage, nil
}

func tenant(t *testing.T, name string, namespaces ...string) {
	t.Helper()
	ctx := context.Background()
	tnt := &unstructured.Unstructured{}
	tnt.SetGroupVersionKind(TenantGVK)
	tnt.SetName(name)
	if err := k8s.Create(ctx, tnt); err != nil {
		t.Fatal(err)
	}
	ns := make([]any, len(namespaces))
	for i, n := range namespaces {
		ns[i] = n
	}
	_ = unstructured.SetNestedSlice(tnt.Object, ns, "status", "namespaces")
	if err := k8s.Status().Update(ctx, tnt); err != nil {
		t.Fatal(err)
	}
}

func budget(t *testing.T, name, tenantName, wh string, vms ...string) {
	t.Helper()
	eb := &joulev1alpha1.EnergyBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: joulev1alpha1.EnergyBudgetSpec{
			Tenant:          tenantName,
			Budget:          resource.MustParse(wh),
			VirtualMachines: vms,
		},
	}
	if err := k8s.Create(context.Background(), eb); err != nil {
		t.Fatal(err)
	}
}

func reconcile(t *testing.T, name string, m Meter, now time.Time) joulev1alpha1.EnergyBudget {
	t.Helper()
	r := &EnergyBudgetReconciler{Client: k8s, Meter: m, Now: func() time.Time { return now }}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != time.Minute {
		t.Errorf("RequeueAfter = %s, want 1m", res.RequeueAfter)
	}
	var eb joulev1alpha1.EnergyBudget
	if err := k8s.Get(context.Background(), types.NamespacedName{Name: name}, &eb); err != nil {
		t.Fatal(err)
	}
	return eb
}

func TestReconcileMeasuresTenant(t *testing.T) {
	tenant(t, "atlas", "atlas-k8s", "atlas-web")
	budget(t, "atlas", "atlas", "5", "atlas-worker-141")

	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	// 10 h into the window: 1 800 J on the hub + 5 400 J of VM = 2 Wh, 0.2 W.
	m := &fakeMeter{usage: energy.Usage{Hub: 1800, VirtualMachines: 5400}}
	eb := reconcile(t, "atlas", m, now)

	if len(m.got.Namespaces) != 2 || m.got.VirtualMachines[0] != "atlas-worker-141" {
		t.Errorf("measured tenant = %+v", m.got)
	}
	if !m.start.Equal(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("window start = %s", m.start)
	}
	s := eb.Status
	if s.Used != "2.000" || s.UsedPercent != 40 || s.AverageWatts != "0.200" || s.State != joulev1alpha1.StateWithin {
		t.Errorf("status = %+v", s)
	}
	if s.Breakdown.Hub != "0.500" || s.Breakdown.VirtualMachines != "1.500" {
		t.Errorf("breakdown = %+v", s.Breakdown)
	}
	if !meta.IsStatusConditionTrue(s.Conditions, joulev1alpha1.ConditionMeasured) {
		t.Errorf("conditions = %+v", s.Conditions)
	}
	if eb.Spec.Window == nil || eb.Spec.Window.Duration != 24*time.Hour || eb.Spec.WarnPercent != 80 || eb.Spec.OnExceeded != joulev1alpha1.ActionWarn {
		t.Errorf("CRD defaults not applied: %+v", eb.Spec)
	}
}

func TestReconcileStates(t *testing.T) {
	tenant(t, "orion", "orion-web")
	budget(t, "orion", "orion", "1")
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		joules float64
		want   joulev1alpha1.BudgetState
	}{
		{1800, joulev1alpha1.StateWithin},    // 0.5 Wh = 50 %
		{3000, joulev1alpha1.StateWarning},   // 0.83 Wh = 83 %
		{3600, joulev1alpha1.StateExceeded},  // 1 Wh = 100 %
		{36000, joulev1alpha1.StateExceeded}, // 10 Wh = 1000 %
	} {
		eb := reconcile(t, "orion", &fakeMeter{usage: energy.Usage{Hub: tc.joules}}, now)
		if eb.Status.State != tc.want {
			t.Errorf("%v J: state = %s, want %s", tc.joules, eb.Status.State, tc.want)
		}
	}
}

func TestReconcileMissingTenant(t *testing.T) {
	budget(t, "ghost", "ghost", "5")
	m := &fakeMeter{}
	eb := reconcile(t, "ghost", m, time.Now())
	if m.called {
		t.Error("must not measure a tenant that does not exist")
	}
	c := meta.FindStatusCondition(eb.Status.Conditions, joulev1alpha1.ConditionMeasured)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "TenantUnavailable" {
		t.Errorf("condition = %+v", c)
	}
}

func TestReconcileDeletedBudget(t *testing.T) {
	r := &EnergyBudgetReconciler{Client: k8s, Meter: &fakeMeter{}}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "gone"}})
	if err != nil || res.RequeueAfter != 0 {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

func TestState(t *testing.T) {
	for _, tc := range []struct {
		used, warn int32
		want       joulev1alpha1.BudgetState
	}{
		{0, 80, joulev1alpha1.StateWithin},
		{79, 80, joulev1alpha1.StateWithin},
		{80, 80, joulev1alpha1.StateWarning},
		{99, 80, joulev1alpha1.StateWarning},
		{100, 80, joulev1alpha1.StateExceeded},
		{100, 100, joulev1alpha1.StateExceeded},
	} {
		if got := State(tc.used, tc.warn); got != tc.want {
			t.Errorf("State(%d, %d) = %s, want %s", tc.used, tc.warn, got, tc.want)
		}
	}
}
