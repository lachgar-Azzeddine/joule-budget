// Package sleep puts a tenant's workloads on the hub to sleep and wakes them up.
//
// Sleeping a tenant:
//   - scales its Kamaji TenantControlPlanes to 0 (Kamaji reports them Sleeping),
//   - scales its Deployments and StatefulSets to 0, except those owned by a
//     controller (e.g. the control plane Deployment owned by its TenantControlPlane,
//     which would only be scaled back up),
//   - cordons the Capsule Tenant, so its owners cannot change or create anything.
//
// Each object remembers its replicas in an annotation, so waking restores
// exactly what was there. Both operations are idempotent: Sleep runs again on
// every measurement while the tenant is asleep and scales back down anything
// scaled up in the meantime (cordoning does not cover the scale subresource).
package sleep

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ReplicasAnnotation holds an object's replicas from before the sleep.
	// An empty value means the field was unset (the API default applied).
	ReplicasAnnotation = "joule.azzhomelab.tech/replicas-before-sleep"
	// CordonedAnnotation marks a Tenant cordoned by the budget, so waking
	// never uncordons a tenant an administrator cordoned by hand.
	CordonedAnnotation = "joule.azzhomelab.tech/cordoned-by-budget"
)

var (
	// TenantGVK is the Capsule Tenant.
	TenantGVK = schema.GroupVersionKind{Group: "capsule.clastix.io", Version: "v1beta2", Kind: "Tenant"}
	// ControlPlaneGVK is the Kamaji TenantControlPlane.
	ControlPlaneGVK = schema.GroupVersionKind{Group: "kamaji.clastix.io", Version: "v1alpha1", Kind: "TenantControlPlane"}

	controlPlaneReplicas = []string{"spec", "controlPlane", "deployment", "replicas"}
)

// Summary counts what an operation changed.
type Summary struct {
	ControlPlanes int
	Workloads     int
	Tenant        bool
}

// Changed reports whether anything was changed.
func (s Summary) Changed() bool { return s.ControlPlanes > 0 || s.Workloads > 0 || s.Tenant }

func (s Summary) String() string {
	return fmt.Sprintf("%d control planes, %d workloads, tenant cordon changed: %t", s.ControlPlanes, s.Workloads, s.Tenant)
}

// Sleeper sleeps and wakes tenants.
type Sleeper struct {
	Client client.Client
}

// Sleep scales the tenant's workloads to 0 and cordons it.
func (s Sleeper) Sleep(ctx context.Context, tenant string, namespaces []string) (Summary, error) {
	var sum Summary
	for _, ns := range namespaces {
		n, err := s.sleepControlPlanes(ctx, ns)
		sum.ControlPlanes += n
		if err != nil {
			return sum, err
		}
		n, err = s.scaleWorkloads(ctx, ns, true)
		sum.Workloads += n
		if err != nil {
			return sum, err
		}
	}
	changed, err := s.cordon(ctx, tenant, true)
	sum.Tenant = changed
	return sum, err
}

// Wake uncordons the tenant and restores the replicas saved by Sleep.
func (s Sleeper) Wake(ctx context.Context, tenant string, namespaces []string) (Summary, error) {
	var sum Summary
	changed, err := s.cordon(ctx, tenant, false)
	sum.Tenant = changed
	if err != nil {
		return sum, err
	}
	for _, ns := range namespaces {
		n, err := s.wakeControlPlanes(ctx, ns)
		sum.ControlPlanes += n
		if err != nil {
			return sum, err
		}
		n, err = s.scaleWorkloads(ctx, ns, false)
		sum.Workloads += n
		if err != nil {
			return sum, err
		}
	}
	return sum, nil
}

func (s Sleeper) controlPlanes(ctx context.Context, ns string) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ControlPlaneGVK.GroupVersion().WithKind(ControlPlaneGVK.Kind + "List"))
	if err := s.Client.List(ctx, list, client.InNamespace(ns)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil // Kamaji is not installed
		}
		return nil, fmt.Errorf("listing control planes in %s: %w", ns, err)
	}
	return list.Items, nil
}

func (s Sleeper) sleepControlPlanes(ctx context.Context, ns string) (int, error) {
	items, err := s.controlPlanes(ctx, ns)
	if err != nil {
		return 0, err
	}
	changed := 0
	for i := range items {
		tcp := &items[i]
		replicas, set, _ := unstructured.NestedInt64(tcp.Object, controlPlaneReplicas...)
		if set && replicas == 0 {
			continue
		}
		before := tcp.DeepCopy()
		if _, saved := tcp.GetAnnotations()[ReplicasAnnotation]; !saved {
			saveReplicas(tcp, set, replicas)
		}
		if err := unstructured.SetNestedField(tcp.Object, int64(0), controlPlaneReplicas...); err != nil {
			return changed, err
		}
		if err := s.Client.Patch(ctx, tcp, client.MergeFrom(before)); err != nil {
			return changed, fmt.Errorf("sleeping control plane %s/%s: %w", ns, tcp.GetName(), err)
		}
		changed++
	}
	return changed, nil
}

func (s Sleeper) wakeControlPlanes(ctx context.Context, ns string) (int, error) {
	items, err := s.controlPlanes(ctx, ns)
	if err != nil {
		return 0, err
	}
	changed := 0
	for i := range items {
		tcp := &items[i]
		saved, ok := tcp.GetAnnotations()[ReplicasAnnotation]
		if !ok {
			continue
		}
		before := tcp.DeepCopy()
		if saved == "" {
			unstructured.RemoveNestedField(tcp.Object, controlPlaneReplicas...)
		} else {
			n, err := strconv.ParseInt(saved, 10, 32)
			if err != nil {
				return changed, fmt.Errorf("control plane %s/%s: bad %s %q", ns, tcp.GetName(), ReplicasAnnotation, saved)
			}
			if err := unstructured.SetNestedField(tcp.Object, n, controlPlaneReplicas...); err != nil {
				return changed, err
			}
		}
		dropAnnotation(tcp, ReplicasAnnotation)
		if err := s.Client.Patch(ctx, tcp, client.MergeFrom(before)); err != nil {
			return changed, fmt.Errorf("waking control plane %s/%s: %w", ns, tcp.GetName(), err)
		}
		changed++
	}
	return changed, nil
}

// scaleWorkloads sleeps (down) or wakes (!down) the Deployments and
// StatefulSets of a namespace that no controller owns.
func (s Sleeper) scaleWorkloads(ctx context.Context, ns string, down bool) (int, error) {
	var deployments appsv1.DeploymentList
	if err := s.Client.List(ctx, &deployments, client.InNamespace(ns)); err != nil {
		return 0, fmt.Errorf("listing deployments in %s: %w", ns, err)
	}
	var statefulSets appsv1.StatefulSetList
	if err := s.Client.List(ctx, &statefulSets, client.InNamespace(ns)); err != nil {
		return 0, fmt.Errorf("listing statefulsets in %s: %w", ns, err)
	}
	type workload struct {
		obj      client.Object
		replicas **int32
	}
	var all []workload
	for i := range deployments.Items {
		all = append(all, workload{&deployments.Items[i], &deployments.Items[i].Spec.Replicas})
	}
	for i := range statefulSets.Items {
		all = append(all, workload{&statefulSets.Items[i], &statefulSets.Items[i].Spec.Replicas})
	}

	changed := 0
	for _, w := range all {
		if metaOwnedByController(w.obj) {
			continue
		}
		var (
			did bool
			err error
		)
		if down {
			did, err = s.sleepWorkload(ctx, w.obj, w.replicas)
		} else {
			did, err = s.wakeWorkload(ctx, w.obj, w.replicas)
		}
		if err != nil {
			return changed, err
		}
		if did {
			changed++
		}
	}
	return changed, nil
}

func (s Sleeper) sleepWorkload(ctx context.Context, obj client.Object, replicas **int32) (bool, error) {
	if *replicas != nil && **replicas == 0 {
		return false, nil
	}
	before := obj.DeepCopyObject().(client.Object)
	if _, saved := obj.GetAnnotations()[ReplicasAnnotation]; !saved {
		saved := ""
		if *replicas != nil {
			saved = strconv.Itoa(int(**replicas))
		}
		setAnnotation(obj, ReplicasAnnotation, saved)
	}
	zero := int32(0)
	*replicas = &zero
	if err := s.Client.Patch(ctx, obj, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("sleeping %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	return true, nil
}

func (s Sleeper) wakeWorkload(ctx context.Context, obj client.Object, replicas **int32) (bool, error) {
	saved, ok := obj.GetAnnotations()[ReplicasAnnotation]
	if !ok {
		return false, nil
	}
	before := obj.DeepCopyObject().(client.Object)
	if saved == "" {
		*replicas = nil
	} else {
		n, err := strconv.ParseInt(saved, 10, 32)
		if err != nil {
			return false, fmt.Errorf("%s/%s: bad %s %q", obj.GetNamespace(), obj.GetName(), ReplicasAnnotation, saved)
		}
		r := int32(n)
		*replicas = &r
	}
	dropAnnotation(obj, ReplicasAnnotation)
	if err := s.Client.Patch(ctx, obj, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("waking %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	return true, nil
}

// cordon sets the Tenant's spec.cordoned. Uncordoning only undoes a cordon
// the budget set.
func (s Sleeper) cordon(ctx context.Context, name string, on bool) (bool, error) {
	tnt := &unstructured.Unstructured{}
	tnt.SetGroupVersionKind(TenantGVK)
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name}, tnt); err != nil {
		return false, fmt.Errorf("tenant %s: %w", name, err)
	}
	cordoned, _, _ := unstructured.NestedBool(tnt.Object, "spec", "cordoned")
	_, ours := tnt.GetAnnotations()[CordonedAnnotation]
	before := tnt.DeepCopy()
	switch {
	case on && !cordoned:
		setAnnotation(tnt, CordonedAnnotation, "true")
	case !on && ours:
		dropAnnotation(tnt, CordonedAnnotation)
		if !cordoned {
			// already uncordoned by someone else: only drop our marker
			return true, s.Client.Patch(ctx, tnt, client.MergeFrom(before))
		}
	default:
		return false, nil
	}
	if err := unstructured.SetNestedField(tnt.Object, on, "spec", "cordoned"); err != nil {
		return false, err
	}
	if err := s.Client.Patch(ctx, tnt, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("cordoning tenant %s: %w", name, err)
	}
	return true, nil
}

func saveReplicas(obj client.Object, set bool, replicas int64) {
	saved := ""
	if set {
		saved = strconv.FormatInt(replicas, 10)
	}
	setAnnotation(obj, ReplicasAnnotation, saved)
}

func setAnnotation(obj client.Object, key, value string) {
	a := obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[key] = value
	obj.SetAnnotations(a)
}

func dropAnnotation(obj client.Object, key string) {
	a := obj.GetAnnotations()
	delete(a, key)
	obj.SetAnnotations(a)
}

func metaOwnedByController(obj client.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}
