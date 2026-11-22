// Package controller reconciles EnergyBudgets.
package controller

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	"github.com/lachgar-Azzeddine/joule-budget/internal/sleep"
)

// TenantGVK is the Capsule Tenant, read as unstructured to avoid depending on Capsule's module.
var TenantGVK = sleep.TenantGVK

// Meter measures a tenant's energy, in joules, between two instants.
type Meter interface {
	Measure(ctx context.Context, t energy.Tenant, start, end time.Time) (energy.Usage, error)
}

// Power switches a tenant's virtual machines off and on.
type Power interface {
	// Off shuts down the tenant's running VMs among names; returns those it shut down.
	Off(ctx context.Context, tenant string, names []string) ([]string, error)
	// On starts the tenant's stopped VMs among names.
	On(ctx context.Context, tenant string, names []string) error
}

// Sleeper puts a tenant to sleep and wakes it up; both are idempotent.
type Sleeper interface {
	Sleep(ctx context.Context, tenant string, namespaces []string) (sleep.Summary, error)
	Wake(ctx context.Context, tenant string, namespaces []string) (sleep.Summary, error)
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
	Meter   Meter
	Sleeper Sleeper
	// Power is optional: without it, virtual machines keep running while asleep.
	Power    Power
	Recorder recorder.EventRecorder
	// Interval between two measurements of a budget.
	Interval time.Duration
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// +kubebuilder:rbac:groups=joule.azzhomelab.tech,resources=energybudgets,verbs=get;list;watch
// +kubebuilder:rbac:groups=joule.azzhomelab.tech,resources=energybudgets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=capsule.clastix.io,resources=tenants,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kamaji.clastix.io,resources=tenantcontrolplanes,verbs=get;list;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;patch
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

	if err := r.sleepOrWake(ctx, &eb, namespaces); err != nil {
		logger.Error(err, "sleep/wake", "tenant", eb.Spec.Tenant)
	}

	// Sleeping has its own events (TenantSleeping, TenantAwake)
	if eb.Status.State != previous && eb.Status.State != joulev1alpha1.StateWithin && eb.Status.State != joulev1alpha1.StateSleeping {
		r.event(&eb, corev1.EventTypeWarning, "Budget"+string(eb.Status.State),
			"tenant %s used %s of %s Wh (%d%%)", eb.Spec.Tenant, eb.Status.Used, eb.Spec.Budget.String(), eb.Status.UsedPercent)
	}

	usedWh.WithLabelValues(eb.Name, eb.Spec.Tenant).Set(used)
	budgetWh.WithLabelValues(eb.Name, eb.Spec.Tenant).Set(budget)

	return r.finish(ctx, &eb, before)
}

// sleepOrWake puts the tenant to sleep while its budget is exceeded and its
// action is Sleep, and wakes it up otherwise (new window, budget raised,
// action changed). A failure leaves the tenant as it is, to retry next time.
func (r *EnergyBudgetReconciler) sleepOrWake(ctx context.Context, eb *joulev1alpha1.EnergyBudget, namespaces []string) error {
	if r.Sleeper == nil {
		return nil
	}
	asleep := eb.Status.SleepingSince != nil
	if eb.Status.State == joulev1alpha1.StateExceeded && eb.Spec.OnExceeded == joulev1alpha1.ActionSleep {
		sum, err := r.Sleeper.Sleep(ctx, eb.Spec.Tenant, namespaces)
		if err != nil {
			r.setAsleep(eb, metav1.ConditionUnknown, "SleepFailed", err.Error())
			return err
		}
		eb.Status.State = joulev1alpha1.StateSleeping
		if r.Power != nil && len(eb.Spec.VirtualMachines) > 0 {
			off, err := r.Power.Off(ctx, eb.Spec.Tenant, eb.Spec.VirtualMachines)
			for _, name := range off {
				if !slices.Contains(eb.Status.PoweredOff, name) {
					eb.Status.PoweredOff = append(eb.Status.PoweredOff, name)
				}
			}
			if err != nil {
				r.event(eb, corev1.EventTypeWarning, "PowerOffFailed", "tenant %s: %s", eb.Spec.Tenant, err)
			} else if len(off) > 0 {
				r.event(eb, corev1.EventTypeNormal, "VirtualMachinesOff", "tenant %s: shut down %s", eb.Spec.Tenant, strings.Join(off, ", "))
			}
		}
		if !asleep {
			eb.Status.SleepingSince = eb.Status.LastMeasured
			r.event(eb, corev1.EventTypeWarning, "TenantSleeping", "tenant %s put to sleep until the next window: %s", eb.Spec.Tenant, sum)
		} else if sum.Changed() {
			r.event(eb, corev1.EventTypeWarning, "TenantKeptAsleep", "tenant %s woke something up while asleep, put back to sleep: %s", eb.Spec.Tenant, sum)
		}
		r.setAsleep(eb, metav1.ConditionTrue, "BudgetExceeded", fmt.Sprintf("since %s", eb.Status.SleepingSince.UTC().Format(time.RFC3339)))
		return nil
	}
	if !asleep {
		return nil
	}
	// Workers first, so they are back by the time the control plane is.
	// Only the VMs the budget shut down are started.
	if r.Power != nil && len(eb.Status.PoweredOff) > 0 {
		if err := r.Power.On(ctx, eb.Spec.Tenant, eb.Status.PoweredOff); err != nil {
			r.setAsleep(eb, metav1.ConditionUnknown, "WakeFailed", err.Error())
			return err
		}
		r.event(eb, corev1.EventTypeNormal, "VirtualMachinesOn", "tenant %s: started %s", eb.Spec.Tenant, strings.Join(eb.Status.PoweredOff, ", "))
		eb.Status.PoweredOff = nil
	}
	sum, err := r.Sleeper.Wake(ctx, eb.Spec.Tenant, namespaces)
	if err != nil {
		r.setAsleep(eb, metav1.ConditionUnknown, "WakeFailed", err.Error())
		return err
	}
	eb.Status.SleepingSince = nil
	r.setAsleep(eb, metav1.ConditionFalse, "WithinBudget", "awake")
	r.event(eb, corev1.EventTypeNormal, "TenantAwake", "tenant %s woken up: %s", eb.Spec.Tenant, sum)
	return nil
}

func (r *EnergyBudgetReconciler) event(eb *joulev1alpha1.EnergyBudget, eventType, reason, msg string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(eb, nil, eventType, reason, "Reconcile", msg, args...)
	}
}

func (r *EnergyBudgetReconciler) setAsleep(eb *joulev1alpha1.EnergyBudget, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&eb.Status.Conditions, metav1.Condition{
		Type:               joulev1alpha1.ConditionAsleep,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: eb.Generation,
	})
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
