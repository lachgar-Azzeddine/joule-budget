// Package controller reconciles EnergyBudgets.
package controller

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	joulev1alpha1 "github.com/lachgar-Azzeddine/joule-budget/api/v1alpha1"
	"github.com/lachgar-Azzeddine/joule-budget/internal/energy"
)

// TenantGVK is the Capsule Tenant, read as unstructured to avoid depending on Capsule's module.
var TenantGVK = schema.GroupVersionKind{Group: "capsule.clastix.io", Version: "v1beta2", Kind: "Tenant"}

// Meter measures a tenant's energy, in joules, between two instants.
type Meter interface {
	Measure(ctx context.Context, t energy.Tenant, start, end time.Time) (energy.Usage, error)
}

var (
	usedWh = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "joule_budget_used_watt_hours",
		Help: "Energy used by the tenant in the current window, in watt-hours.",
	}, []string{"budget", "tenant"})
	budgetWh = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "joule_budget_watt_hours",
		Help: "Energy budget of the tenant per window, in watt-hours.",
	}, []string{"budget", "tenant"})
)

func init() {
	metrics.Registry.MustRegister(usedWh, budgetWh)
}

// EnergyBudgetReconciler measures each tenant's energy and reports it in the budget's status.
type EnergyBudgetReconciler struct {
	client.Client
	Meter    Meter
	Recorder recorder.EventRecorder
	// Interval between two measurements of a budget.
	Interval time.Duration
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// +kubebuilder:rbac:groups=joule.azzhomelab.tech,resources=energybudgets,verbs=get;list;watch
// +kubebuilder:rbac:groups=joule.azzhomelab.tech,resources=energybudgets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=capsule.clastix.io,resources=tenants,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile measures one budget and updates its status.
func (r *EnergyBudgetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var eb joulev1alpha1.EnergyBudget
	if err := r.Get(ctx, req.NamespacedName, &eb); err != nil {
		if apierrors.IsNotFound(err) {
			usedWh.DeletePartialMatch(prometheus.Labels{"budget": req.Name})
			budgetWh.DeletePartialMatch(prometheus.Labels{"budget": req.Name})
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	before := eb.DeepCopy()

	now := r.now()
	window := 24 * time.Hour
	if eb.Spec.Window != nil && eb.Spec.Window.Duration >= time.Minute {
		window = eb.Spec.Window.Duration
	}
	start := energy.WindowStart(now, window)

	namespaces, err := r.tenantNamespaces(ctx, eb.Spec.Tenant)
	if err != nil {
		r.setMeasured(&eb, metav1.ConditionFalse, "TenantUnavailable", err.Error())
		return r.finish(ctx, &eb, before)
	}

	usage, err := r.Meter.Measure(ctx, energy.Tenant{Namespaces: namespaces, VirtualMachines: eb.Spec.VirtualMachines}, start, now)
	if err != nil {
		logger.Error(err, "measuring energy", "tenant", eb.Spec.Tenant)
		r.setMeasured(&eb, metav1.ConditionFalse, "QueryFailed", err.Error())
		return r.finish(ctx, &eb, before)
	}

	budget := eb.Spec.Budget.AsApproximateFloat64()
	used := energy.WattHours(usage.Total())
	previous := eb.Status.State
	if eb.Status.WindowStart == nil || !eb.Status.WindowStart.Time.Equal(start) {
		previous = "" // a new window starts from a clean slate
	}

	eb.Status.WindowStart = &metav1.Time{Time: start}
	eb.Status.LastMeasured = &metav1.Time{Time: now}
	eb.Status.Used = energy.Format(used)
	eb.Status.UsedPercent = percent(used, budget)
	eb.Status.AverageWatts = energy.Format(usage.Total() / math.Max(now.Sub(start).Seconds(), 1))
	eb.Status.Breakdown = joulev1alpha1.EnergyUsage{
		Hub:             energy.Format(energy.WattHours(usage.Hub)),
		VirtualMachines: energy.Format(energy.WattHours(usage.VirtualMachines)),
	}
	eb.Status.State = State(eb.Status.UsedPercent, warnPercent(eb.Spec.WarnPercent))
	eb.Status.ObservedGeneration = eb.Generation
	r.setMeasured(&eb, metav1.ConditionTrue, "Measured",
		fmt.Sprintf("%d namespaces, %d virtual machines", len(namespaces), len(eb.Spec.VirtualMachines)))

	if eb.Status.State != previous && eb.Status.State != joulev1alpha1.StateWithin && r.Recorder != nil {
		r.Recorder.Eventf(&eb, nil, corev1.EventTypeWarning, "Budget"+string(eb.Status.State), "Measure",
			"tenant %s used %s of %s Wh (%d%%)", eb.Spec.Tenant, eb.Status.Used, eb.Spec.Budget.String(), eb.Status.UsedPercent)
	}

	usedWh.WithLabelValues(eb.Name, eb.Spec.Tenant).Set(used)
	budgetWh.WithLabelValues(eb.Name, eb.Spec.Tenant).Set(budget)

	return r.finish(ctx, &eb, before)
}

func (r *EnergyBudgetReconciler) finish(ctx context.Context, eb, before *joulev1alpha1.EnergyBudget) (ctrl.Result, error) {
	if err := r.Status().Patch(ctx, eb, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.interval()}, nil
}

// tenantNamespaces lists the namespaces of a Capsule Tenant, from its status.
func (r *EnergyBudgetReconciler) tenantNamespaces(ctx context.Context, name string) ([]string, error) {
	tnt := &unstructured.Unstructured{}
	tnt.SetGroupVersionKind(TenantGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: name}, tnt); err != nil {
		return nil, fmt.Errorf("tenant %s: %w", name, err)
	}
	ns, _, err := unstructured.NestedStringSlice(tnt.Object, "status", "namespaces")
	if err != nil {
		return nil, fmt.Errorf("tenant %s status: %w", name, err)
	}
	return ns, nil
}

func (r *EnergyBudgetReconciler) setMeasured(eb *joulev1alpha1.EnergyBudget, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&eb.Status.Conditions, metav1.Condition{
		Type:               joulev1alpha1.ConditionMeasured,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: eb.Generation,
	})
}

func (r *EnergyBudgetReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *EnergyBudgetReconciler) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return time.Minute
}

// State is the budget state for a usage percentage.
func State(usedPercent, warnAt int32) joulev1alpha1.BudgetState {
	switch {
	case usedPercent >= 100:
		return joulev1alpha1.StateExceeded
	case usedPercent >= warnAt:
		return joulev1alpha1.StateWarning
	default:
		return joulev1alpha1.StateWithin
	}
}

func percent(used, budget float64) int32 {
	if budget <= 0 {
		return 100
	}
	return int32(math.Min(math.Floor(used/budget*100), math.MaxInt32))
}

func warnPercent(p int32) int32 {
	if p <= 0 {
		return 80
	}
	return p
}

// SetupWithManager registers the reconciler.
func (r *EnergyBudgetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Spec changes only: the status patch after each measurement must not trigger another one.
		For(&joulev1alpha1.EnergyBudget{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("energybudget").
		Complete(r)
}
