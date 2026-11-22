package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	joulev1alpha1 "github.com/lachgar-Azzeddine/joule-budget/api/v1alpha1"
	"github.com/lachgar-Azzeddine/joule-budget/internal/energy"
	"github.com/lachgar-Azzeddine/joule-budget/internal/sleep"
)

func create(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8s.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
}

func namespace(t *testing.T, name string) {
	t.Helper()
	create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
}

func deployment(t *testing.T, ns, name string, replicas int32, owner *metav1.OwnerReference) {
	t.Helper()
	labels := map[string]string{"app": name}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
	}
	if owner != nil {
		d.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	create(t, d)
}

func statefulSet(t *testing.T, ns, name string, replicas int32) {
	t.Helper()
	labels := map[string]string{"app": name}
	create(t, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
	})
}

func controlPlane(t *testing.T, ns, name string, replicas *int64) {
	t.Helper()
	tcp := &unstructured.Unstructured{}
	tcp.SetGroupVersionKind(sleep.ControlPlaneGVK)
	tcp.SetNamespace(ns)
	tcp.SetName(name)
	_ = unstructured.SetNestedField(tcp.Object, "v1.36.3", "spec", "kubernetes", "version")
	if replicas != nil {
		_ = unstructured.SetNestedField(tcp.Object, *replicas, "spec", "controlPlane", "deployment", "replicas")
	}
	create(t, tcp)
}

func replicasOf(t *testing.T, obj client.Object, ns, name string) (int32, string, bool) {
	t.Helper()
	if err := k8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatal(err)
	}
	saved, ok := obj.GetAnnotations()[sleep.ReplicasAnnotation]
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return ptr.Deref(o.Spec.Replicas, -1), saved, ok
	case *appsv1.StatefulSet:
		return ptr.Deref(o.Spec.Replicas, -1), saved, ok
	}
	t.Fatalf("unexpected %T", obj)
	return 0, "", false
}

func tcpReplicas(t *testing.T, ns, name string) (int64, bool, string, bool) {
	t.Helper()
	tcp := &unstructured.Unstructured{}
	tcp.SetGroupVersionKind(sleep.ControlPlaneGVK)
	if err := k8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, tcp); err != nil {
		t.Fatal(err)
	}
	r, set, _ := unstructured.NestedInt64(tcp.Object, "spec", "controlPlane", "deployment", "replicas")
	saved, ok := tcp.GetAnnotations()[sleep.ReplicasAnnotation]
	return r, set, saved, ok
}

func tenantCordon(t *testing.T, name string) (bool, bool) {
	t.Helper()
	tnt := &unstructured.Unstructured{}
	tnt.SetGroupVersionKind(TenantGVK)
	if err := k8s.Get(context.Background(), types.NamespacedName{Name: name}, tnt); err != nil {
		t.Fatal(err)
	}
	cordoned, _, _ := unstructured.NestedBool(tnt.Object, "spec", "cordoned")
	_, ours := tnt.GetAnnotations()[sleep.CordonedAnnotation]
	return cordoned, ours
}

func reconcileSleeping(t *testing.T, name string, joules float64, now time.Time) joulev1alpha1.EnergyBudget {
	t.Helper()
	r := &EnergyBudgetReconciler{Client: k8s, Meter: &fakeMeter{usage: energy.Usage{Hub: joules}}, Sleeper: sleep.Sleeper{Client: k8s}, Now: func() time.Time { return now }}
	if _, err := r.Reconcile(context.Background(), reqFor(name)); err != nil {
		t.Fatal(err)
	}
	var eb joulev1alpha1.EnergyBudget
	if err := k8s.Get(context.Background(), types.NamespacedName{Name: name}, &eb); err != nil {
		t.Fatal(err)
	}
	return eb
}

func TestSleepAndWake(t *testing.T) {
	namespace(t, "vega-web")
	namespace(t, "vega-k8s")
	tenant(t, "vega", "vega-web", "vega-k8s")
	deployment(t, "vega-web", "web", 3, nil)
	statefulSet(t, "vega-web", "db", 1)
	// the control plane Deployment is Kamaji's: scaled through its TenantControlPlane only
	deployment(t, "vega-k8s", "vega", 1, &metav1.OwnerReference{
		APIVersion: "kamaji.clastix.io/v1alpha1", Kind: "TenantControlPlane", Name: "vega", UID: "u", Controller: ptr.To(true),
	})
	controlPlane(t, "vega-k8s", "vega", ptr.To(int64(1)))
	controlPlane(t, "vega-k8s", "defaults", nil)
	create(t, &joulev1alpha1.EnergyBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "vega"},
		Spec:       joulev1alpha1.EnergyBudgetSpec{Tenant: "vega", Budget: resourceWh("1"), OnExceeded: joulev1alpha1.ActionSleep},
	})
	day := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	// 1. Over budget: asleep
	eb := reconcileSleeping(t, "vega", 4000, day)
	if eb.Status.State != joulev1alpha1.StateSleeping || eb.Status.SleepingSince == nil ||
		!meta.IsStatusConditionTrue(eb.Status.Conditions, joulev1alpha1.ConditionAsleep) {
		t.Fatalf("not asleep: %+v", eb.Status)
	}
	if r, saved, _ := replicasOf(t, &appsv1.Deployment{}, "vega-web", "web"); r != 0 || saved != "3" {
		t.Errorf("web: replicas %d saved %q", r, saved)
	}
	if r, saved, _ := replicasOf(t, &appsv1.StatefulSet{}, "vega-web", "db"); r != 0 || saved != "1" {
		t.Errorf("db: replicas %d saved %q", r, saved)
	}
	if r, _, ok := replicasOf(t, &appsv1.Deployment{}, "vega-k8s", "vega"); r != 1 || ok {
		t.Errorf("owned deployment touched: replicas %d annotated %t", r, ok)
	}
	if r, set, saved, _ := tcpReplicas(t, "vega-k8s", "vega"); !set || r != 0 || saved != "1" {
		t.Errorf("tcp vega: replicas %d set %t saved %q", r, set, saved)
	}
	if r, set, saved, ok := tcpReplicas(t, "vega-k8s", "defaults"); !set || r != 0 || !ok || saved != "" {
		t.Errorf("tcp defaults: replicas %d set %t saved %q", r, set, saved)
	}
	if cordoned, ours := tenantCordon(t, "vega"); !cordoned || !ours {
		t.Errorf("tenant cordoned %t ours %t", cordoned, ours)
	}
	since := eb.Status.SleepingSince.Time

	// 2. The owner scales web back up (cordon does not cover /scale): put back to sleep,
	//    the saved replicas are still the ones from before the sleep.
	web := &appsv1.Deployment{}
	_, _, _ = replicasOf(t, web, "vega-web", "web")
	web.Spec.Replicas = ptr.To(int32(2))
	if err := k8s.Update(context.Background(), web); err != nil {
		t.Fatal(err)
	}
	eb = reconcileSleeping(t, "vega", 4100, day.Add(time.Minute))
	if r, saved, _ := replicasOf(t, &appsv1.Deployment{}, "vega-web", "web"); r != 0 || saved != "3" {
		t.Errorf("web after scale-up: replicas %d saved %q", r, saved)
	}
	if !eb.Status.SleepingSince.Time.Equal(since) {
		t.Errorf("sleepingSince moved: %s -> %s", since, eb.Status.SleepingSince.Time)
	}

	// 3. Next window: awake, everything as it was
	eb = reconcileSleeping(t, "vega", 0, day.Add(24*time.Hour))
	if eb.Status.State != joulev1alpha1.StateWithin || eb.Status.SleepingSince != nil ||
		meta.IsStatusConditionTrue(eb.Status.Conditions, joulev1alpha1.ConditionAsleep) {
		t.Fatalf("not awake: %+v", eb.Status)
	}
	if r, _, ok := replicasOf(t, &appsv1.Deployment{}, "vega-web", "web"); r != 3 || ok {
		t.Errorf("web: replicas %d annotated %t", r, ok)
	}
	if r, _, ok := replicasOf(t, &appsv1.StatefulSet{}, "vega-web", "db"); r != 1 || ok {
		t.Errorf("db: replicas %d annotated %t", r, ok)
	}
	if r, set, _, ok := tcpReplicas(t, "vega-k8s", "vega"); !set || r != 1 || ok {
		t.Errorf("tcp vega: replicas %d set %t annotated %t", r, set, ok)
	}
	if _, set, _, ok := tcpReplicas(t, "vega-k8s", "defaults"); set || ok {
		t.Errorf("tcp defaults: replicas set %t annotated %t", set, ok)
	}
	if cordoned, ours := tenantCordon(t, "vega"); cordoned || ours {
		t.Errorf("tenant cordoned %t ours %t", cordoned, ours)
	}
}

func TestWakeKeepsAnAdministratorsCordon(t *testing.T) {
	namespace(t, "lyra-web")
	tenant(t, "lyra", "lyra-web")
	tnt := &unstructured.Unstructured{}
	tnt.SetGroupVersionKind(TenantGVK)
	_ = k8s.Get(context.Background(), types.NamespacedName{Name: "lyra"}, tnt)
	_ = unstructured.SetNestedField(tnt.Object, true, "spec", "cordoned")
	if err := k8s.Update(context.Background(), tnt); err != nil {
		t.Fatal(err)
	}
	create(t, &joulev1alpha1.EnergyBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "lyra"},
		Spec:       joulev1alpha1.EnergyBudgetSpec{Tenant: "lyra", Budget: resourceWh("1"), OnExceeded: joulev1alpha1.ActionSleep},
	})
	day := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	reconcileSleeping(t, "lyra", 4000, day)
	if cordoned, ours := tenantCordon(t, "lyra"); !cordoned || ours {
		t.Errorf("asleep: cordoned %t ours %t", cordoned, ours)
	}
	reconcileSleeping(t, "lyra", 0, day.Add(24*time.Hour))
	if cordoned, _ := tenantCordon(t, "lyra"); !cordoned {
		t.Error("waking removed the administrator's cordon")
	}
}

func TestWarnDoesNotSleep(t *testing.T) {
	namespace(t, "nova-web")
	tenant(t, "nova", "nova-web")
	deployment(t, "nova-web", "web", 2, nil)
	budget(t, "nova", "nova", "1") // onExceeded defaults to Warn

	eb := reconcileSleeping(t, "nova", 4000, time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC))
	if eb.Status.State != joulev1alpha1.StateExceeded || eb.Status.SleepingSince != nil {
		t.Errorf("status = %+v", eb.Status)
	}
	if r, _, ok := replicasOf(t, &appsv1.Deployment{}, "nova-web", "web"); r != 2 || ok {
		t.Errorf("web: replicas %d annotated %t", r, ok)
	}
}

func TestRaisingTheBudgetWakesTheTenant(t *testing.T) {
	namespace(t, "rigel-web")
	tenant(t, "rigel", "rigel-web")
	deployment(t, "rigel-web", "web", 2, nil)
	create(t, &joulev1alpha1.EnergyBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "rigel"},
		Spec:       joulev1alpha1.EnergyBudgetSpec{Tenant: "rigel", Budget: resourceWh("1"), OnExceeded: joulev1alpha1.ActionSleep},
	})
	day := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	reconcileSleeping(t, "rigel", 4000, day)

	var eb joulev1alpha1.EnergyBudget
	_ = k8s.Get(context.Background(), types.NamespacedName{Name: "rigel"}, &eb)
	eb.Spec.Budget = resourceWh("10")
	if err := k8s.Update(context.Background(), &eb); err != nil {
		t.Fatal(err)
	}
	eb = reconcileSleeping(t, "rigel", 4000, day.Add(time.Minute))
	if eb.Status.State != joulev1alpha1.StateWithin || eb.Status.SleepingSince != nil {
		t.Errorf("status = %+v", eb.Status)
	}
	if r, _, ok := replicasOf(t, &appsv1.Deployment{}, "rigel-web", "web"); r != 2 || ok {
		t.Errorf("web: replicas %d annotated %t", r, ok)
	}
}

func resourceWh(s string) resource.Quantity { return resource.MustParse(s) }

func reqFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
}
