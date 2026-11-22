package energy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
)

type fakeQuerier struct {
	answers map[string]float64 // keyed by a substring of the query
	err     error
	queries []string
}

func (f *fakeQuerier) Scalar(_ context.Context, q string, _ time.Time) (float64, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return 0, f.err
	}
	for k, v := range f.answers {
		if strings.Contains(q, k) {
			return v, nil
		}
	}
	return 0, nil
}

func TestHubQuery(t *testing.T) {
	got := HubQuery("hub", []string{"atlas-k8s", "atlas-web"}, 3600, 60)
	want := `sum_over_time((sum(kepler_vm_cpu_watts{vm_name="hub",zone="package"}) * scalar(` +
		`sum(rate(container_cpu_usage_seconds_total{namespace=~"^(atlas-k8s|atlas-web)$",id=~"/kubepods(/[a-z]+)?/pod[^/]+"}[2m]))` +
		` / sum(rate(container_cpu_usage_seconds_total{id="/"}[2m]))))[3600s:60s]) * 60`
	if got != want {
		t.Errorf("HubQuery:\n got %s\nwant %s", got, want)
	}
	if HubQuery("hub", nil, 3600, 60) != "" {
		t.Error("no namespaces must give no query")
	}
}

func TestVMQuery(t *testing.T) {
	got := VMQuery([]string{"atlas-worker-141"}, 120)
	want := `sum(increase(kepler_vm_cpu_joules_total{vm_name=~"^(atlas-worker-141)$",zone="package"}[120s]))`
	if got != want {
		t.Errorf("VMQuery:\n got %s\nwant %s", got, want)
	}
	if VMQuery(nil, 120) != "" {
		t.Error("no VMs must give no query")
	}
}

func TestNamesAreEscaped(t *testing.T) {
	// A dot must match only a dot, and a quote must not end the PromQL string.
	if got := anyOf([]string{`a.b`, `c"d`}); got != `^(a\\.b|c\"d)$` {
		t.Errorf("anyOf = %s", got)
	}
}

func TestMeasure(t *testing.T) {
	start := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	tenant := Tenant{Namespaces: []string{"atlas-k8s"}, VirtualMachines: []string{"atlas-worker-141"}}

	t.Run("sums hub share and VMs", func(t *testing.T) {
		q := &fakeQuerier{answers: map[string]float64{"kepler_vm_cpu_watts": 126, "kepler_vm_cpu_joules_total": 426}}
		u, err := Meter{Querier: q, HubVM: "hub", Step: time.Minute}.Measure(context.Background(), tenant, start, start.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if u.Hub != 126 || u.VirtualMachines != 426 || u.Total() != 552 {
			t.Errorf("usage = %+v", u)
		}
		if len(q.queries) != 2 || !strings.Contains(q.queries[0], "[3600s:60s]") || !strings.Contains(q.queries[1], "[3600s]") {
			t.Errorf("queries = %v", q.queries)
		}
	})

	t.Run("nothing to measure before one step", func(t *testing.T) {
		q := &fakeQuerier{}
		u, err := Meter{Querier: q, HubVM: "hub", Step: time.Minute}.Measure(context.Background(), tenant, start, start.Add(30*time.Second))
		if err != nil || u.Total() != 0 || len(q.queries) != 0 {
			t.Errorf("usage = %+v, err = %v, queries = %v", u, err, q.queries)
		}
	})

	t.Run("tenant without VMs only queries the hub", func(t *testing.T) {
		q := &fakeQuerier{}
		_, err := Meter{Querier: q, HubVM: "hub"}.Measure(context.Background(), Tenant{Namespaces: []string{"x"}}, start, start.Add(time.Hour))
		if err != nil || len(q.queries) != 1 {
			t.Errorf("err = %v, queries = %v", err, q.queries)
		}
	})

	t.Run("query errors are returned", func(t *testing.T) {
		q := &fakeQuerier{err: errors.New("down")}
		if _, err := (Meter{Querier: q, HubVM: "hub"}).Measure(context.Background(), tenant, start, start.Add(time.Hour)); err == nil {
			t.Error("expected an error")
		}
	})
}

func TestWindowStart(t *testing.T) {
	at := time.Date(2026, 11, 22, 14, 37, 5, 0, time.FixedZone("Casablanca", 3600))
	if got := WindowStart(at, 24*time.Hour); !got.Equal(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("24h window start = %s", got)
	}
	if got := WindowStart(at, time.Hour); !got.Equal(time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("1h window start = %s", got)
	}
}

func TestFormat(t *testing.T) {
	for in, want := range map[float64]string{1.23456: "1.235", 0: "0.000", -2: "0.000"} {
		if got := Format(in); got != want {
			t.Errorf("Format(%v) = %s, want %s", in, got, want)
		}
	}
}

func TestSingle(t *testing.T) {
	if v, err := single(model.Vector{}); v != 0 || err != nil {
		t.Errorf("empty vector = %v, %v", v, err)
	}
	if v, _ := single(model.Vector{{Value: model.SampleValue(2.5)}}); v != 2.5 {
		t.Errorf("one sample = %v", v)
	}
	if _, err := single(model.Vector{{}, {}}); err == nil {
		t.Error("two series must be an error")
	}
}
