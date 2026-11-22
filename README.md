# joule-budget

An energy budget for each tenant of a multi-tenant Kubernetes platform.

Capsule caps how much **capacity** a tenant can hold (CPU, memory, pods). It
does not see **energy over time**: a cluster left running all night costs the
tenant nothing. `joule-budget` gives each [Capsule](https://projectcapsule.dev)
tenant a budget in watt-hours per window, measures what it really uses from
[Kepler](https://sustainable-computing.io) metrics, and reports it.

```console
$ kubectl get energybudgets
NAME    TENANT   USED (WH)   BUDGET (WH)   USED %   AVG W   STATE    ACTION
atlas   atlas    1.912       5             38       0.152   Within   Sleep
```

## How a tenant's energy is measured

A tenant uses energy in two places:

- **On the hub cluster**, in its namespaces (including the Kamaji control
  plane of its own Kubernetes cluster). Kepler sees the hub as one VM, so the
  tenant is charged the hub VM's power times its namespaces' share of the
  hub's CPU time (cAdvisor), integrated every minute over the window.
- **On its own worker VMs**, listed in the budget. Their whole energy, from
  Kepler's per-VM counters, is charged to the tenant.

Windows are aligned to UTC: a `24h` window resets at 00:00 UTC.

```yaml
apiVersion: joule.azzhomelab.tech/v1alpha1
kind: EnergyBudget
metadata:
  name: atlas
spec:
  tenant: atlas            # Capsule Tenant
  budget: "5"              # watt-hours per window
  window: 24h
  warnPercent: 80
  onExceeded: Sleep        # or Warn
  virtualMachines:         # Kepler vm_name of the tenant's workers
    - atlas-worker-141
status:
  used: "1.912"
  usedPercent: 38
  averageWatts: "0.152"
  breakdown:
    hub: "0.441"
    virtualMachines: "1.471"
  state: Within            # Within | Warning | Exceeded
```

EnergyBudgets are cluster-scoped: the platform administrator sets them, not
the tenant.

The controller also exports `joule_budget_used_watt_hours` and
`joule_budget_watt_hours` per budget and tenant for dashboards and alerts.

## Status

| Step | |
|---|---|
| Measure and report usage | done |
| Sleep and wake: Kamaji control plane and hub workloads | next |
| Power off the worker VMs (Proxmox API, token from OpenBao via SPIFFE) | planned |
| Signed images, Helm chart | planned |

## Requirements

- Capsule (reads `Tenant.status.namespaces`)
- Kepler, with VM metrics (`kepler_vm_cpu_watts`, `kepler_vm_cpu_joules_total`)
- kubelet cAdvisor metrics (`container_cpu_usage_seconds_total`, pod cgroups)
- a Prometheus-compatible API holding both (VictoriaMetrics, Prometheus)

## Development

```sh
make test     # unit tests + envtest
make lint
make run      # against the current kubeconfig; PROMETHEUS_URL=http://...
```

## License

Apache 2.0
