package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Action is what happens when a tenant has used its whole budget.
// +kubebuilder:validation:Enum=Warn;Sleep
type Action string

const (
	// ActionWarn only reports the overrun (status, event, metric).
	ActionWarn Action = "Warn"
	// ActionSleep puts the tenant to sleep until the next window.
	ActionSleep Action = "Sleep"
)

// BudgetState summarises where a tenant stands in the current window.
// +kubebuilder:validation:Enum=Within;Warning;Exceeded;Sleeping
type BudgetState string

const (
	StateWithin   BudgetState = "Within"
	StateWarning  BudgetState = "Warning"
	StateExceeded BudgetState = "Exceeded"
	// StateSleeping is Exceeded with onExceeded: Sleep, once the tenant is asleep.
	StateSleeping BudgetState = "Sleeping"
)

const (
	// ConditionMeasured is True when the last measurement succeeded.
	ConditionMeasured = "Measured"
	// ConditionAsleep is True while the tenant is asleep.
	ConditionAsleep = "Asleep"
)

// EnergyBudgetSpec is the energy a Capsule tenant may use per window.
type EnergyBudgetSpec struct {
	// Tenant is the name of the Capsule Tenant this budget applies to.
	// +kubebuilder:validation:MinLength=1
	Tenant string `json:"tenant"`

	// Budget is the energy allowed per window, in watt-hours (e.g. "5" or "500m").
	Budget resource.Quantity `json:"budget"`

	// Window is the length of a budget window. Windows are aligned to UTC:
	// a 24h window resets at 00:00 UTC.
	// +kubebuilder:default="24h"
	// +optional
	Window *metav1.Duration `json:"window,omitempty"`

	// WarnPercent is the share of the budget at which the state becomes Warning.
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	WarnPercent int32 `json:"warnPercent,omitempty"`

	// OnExceeded is what happens once the whole budget is used.
	// +kubebuilder:default=Warn
	// +optional
	OnExceeded Action `json:"onExceeded,omitempty"`

	// VirtualMachines are the tenant's worker VMs, as Kepler names them (vm_name).
	// Their whole energy is charged to the tenant.
	// +listType=set
	// +optional
	VirtualMachines []string `json:"virtualMachines,omitempty"`
}

// EnergyUsage is energy in watt-hours, as decimal strings (e.g. "1.234").
type EnergyUsage struct {
	// Hub is the tenant's share of the hub VM: the hub's energy times the
	// tenant namespaces' share of the hub's CPU time.
	Hub string `json:"hub,omitempty"`
	// VirtualMachines is the energy of the tenant's worker VMs.
	VirtualMachines string `json:"virtualMachines,omitempty"`
}

// EnergyBudgetStatus is the measured usage in the current window.
type EnergyBudgetStatus struct {
	// WindowStart is when the current window began.
	// +optional
	WindowStart *metav1.Time `json:"windowStart,omitempty"`
	// LastMeasured is when the usage was last measured.
	// +optional
	LastMeasured *metav1.Time `json:"lastMeasured,omitempty"`
	// Used is the energy used in the current window, in watt-hours.
	// +optional
	Used string `json:"used,omitempty"`
	// UsedPercent is Used as a share of the budget.
	// +optional
	UsedPercent int32 `json:"usedPercent,omitempty"`
	// AverageWatts is the tenant's average power since the window began.
	// +optional
	AverageWatts string `json:"averageWatts,omitempty"`
	// Breakdown splits Used by where the energy was spent.
	// +optional
	Breakdown EnergyUsage `json:"breakdown,omitempty"`
	// SleepingSince is when the tenant was put to sleep; empty while awake.
	// +optional
	SleepingSince *metav1.Time `json:"sleepingSince,omitempty"`
	// State is Within, Warning, Exceeded or Sleeping.
	// +optional
	State BudgetState `json:"state,omitempty"`
	// ObservedGeneration is the spec generation the status was computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Measured, Asleep.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EnergyBudget caps the energy a Capsule tenant may use per time window.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=eb
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
// +kubebuilder:printcolumn:name="Used (Wh)",type=string,JSONPath=`.status.used`
// +kubebuilder:printcolumn:name="Budget (Wh)",type=string,JSONPath=`.spec.budget`
// +kubebuilder:printcolumn:name="Used %",type=integer,JSONPath=`.status.usedPercent`
// +kubebuilder:printcolumn:name="Avg W",type=string,JSONPath=`.status.averageWatts`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.onExceeded`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EnergyBudget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EnergyBudgetSpec   `json:"spec,omitempty"`
	Status EnergyBudgetStatus `json:"status,omitempty"`
}

// EnergyBudgetList is a list of EnergyBudgets.
// +kubebuilder:object:root=true
type EnergyBudgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EnergyBudget `json:"items"`
}
