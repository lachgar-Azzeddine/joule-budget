// Package energy measures how much energy a tenant used, from Kepler and
// cAdvisor metrics stored in a Prometheus-compatible database.
package energy

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Querier runs an instant query and returns the single value of its result,
// or 0 when the result is empty.
type Querier interface {
	Scalar(ctx context.Context, query string, at time.Time) (float64, error)
}

// Tenant is what is charged to a tenant.
type Tenant struct {
	// Namespaces on the hub whose CPU share of the hub VM is charged.
	Namespaces []string
	// VirtualMachines whose whole energy is charged (Kepler vm_name).
	VirtualMachines []string
}

// Usage is energy in joules.
type Usage struct {
	Hub             float64
	VirtualMachines float64
}

// Total is the whole usage in joules.
func (u Usage) Total() float64 { return u.Hub + u.VirtualMachines }

// Meter turns metrics into joules.
type Meter struct {
	Querier Querier
	// HubVM is the Kepler vm_name of the hub VM.
	HubVM string
	// Step is the resolution of the hub share integration.
	Step time.Duration
}

// podCgroups selects whole-pod cgroups (not the containers in them, nor the
// pause containers) in cAdvisor's container_cpu_usage_seconds_total.
const podCgroups = `/kubepods(/[a-z]+)?/pod[^/]+`

// Measure returns the energy the tenant used between start and end.
func (m Meter) Measure(ctx context.Context, t Tenant, start, end time.Time) (Usage, error) {
	var u Usage
	rng := int64(end.Sub(start).Seconds())
	step := int64(m.Step.Seconds())
	if step <= 0 {
		step = 60
	}
	if rng < step {
		return u, nil // too early in the window to have samples
	}

	if q := HubQuery(m.HubVM, t.Namespaces, rng, step); q != "" {
		v, err := m.Querier.Scalar(ctx, q, end)
		if err != nil {
			return u, fmt.Errorf("hub share: %w", err)
		}
		u.Hub = v
	}
	if q := VMQuery(t.VirtualMachines, rng); q != "" {
		v, err := m.Querier.Scalar(ctx, q, end)
		if err != nil {
			return u, fmt.Errorf("virtual machines: %w", err)
		}
		u.VirtualMachines = v
	}
	return u, nil
}

// HubQuery integrates, over the last rng seconds, the hub VM's power times
// the share of the hub's CPU time used by the namespaces. The result is in joules.
func HubQuery(hubVM string, namespaces []string, rng, step int64) string {
	if len(namespaces) == 0 {
		return ""
	}
	share := fmt.Sprintf(
		`sum(rate(container_cpu_usage_seconds_total{namespace=~"%s",id=~"%s"}[2m]))`+
			` / sum(rate(container_cpu_usage_seconds_total{id="/"}[2m]))`,
		anyOf(namespaces), podCgroups)
	return fmt.Sprintf(
		`sum_over_time((sum(kepler_vm_cpu_watts{vm_name="%s",zone="package"}) * scalar(%s))[%ds:%ds]) * %d`,
		escape(hubVM), share, rng, step, step)
}

// VMQuery is the energy, in joules, of the VMs over the last rng seconds.
func VMQuery(vms []string, rng int64) string {
	if len(vms) == 0 {
		return ""
	}
	return fmt.Sprintf(`sum(increase(kepler_vm_cpu_joules_total{vm_name=~"%s",zone="package"}[%ds]))`,
		anyOf(vms), rng)
}

// anyOf is an anchored regex matching exactly one of the names, escaped for
// a double-quoted PromQL string.
func anyOf(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return escape("^(" + strings.Join(quoted, "|") + ")$")
}

// escape makes s safe inside a double-quoted PromQL string.
func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// WindowStart is the start of the window containing t. Windows are aligned
// to UTC, so a 24h window starts at 00:00 UTC.
func WindowStart(t time.Time, window time.Duration) time.Time {
	return t.UTC().Truncate(window)
}

// WattHours converts joules to watt-hours.
func WattHours(joules float64) float64 { return joules / 3600 }

// Format renders a value with three decimals ("1.234"); negative and NaN
// values (counter glitches) are shown as 0.
func Format(v float64) string {
	if math.IsNaN(v) || v < 0 {
		v = 0
	}
	return fmt.Sprintf("%.3f", v)
}
