package energy

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// Prometheus queries a Prometheus-compatible API (VictoriaMetrics here).
type Prometheus struct {
	api promv1.API
}

// NewPrometheus returns a Querier for the API at address (e.g. http://victoria-metrics.monitoring:8428).
func NewPrometheus(address string) (*Prometheus, error) {
	c, err := api.NewClient(api.Config{Address: address})
	if err != nil {
		return nil, err
	}
	return &Prometheus{api: promv1.NewAPI(c)}, nil
}

// Scalar runs query at the given time and returns its single value, 0 if empty.
func (p *Prometheus) Scalar(ctx context.Context, query string, at time.Time) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	v, _, err := p.api.Query(ctx, query, at)
	if err != nil {
		return 0, err
	}
	return single(v)
}

func single(v model.Value) (float64, error) {
	switch r := v.(type) {
	case model.Vector:
		switch len(r) {
		case 0:
			return 0, nil
		case 1:
			return finite(float64(r[0].Value)), nil
		default:
			return 0, fmt.Errorf("expected one series, got %d", len(r))
		}
	case *model.Scalar:
		return finite(float64(r.Value)), nil
	default:
		return 0, fmt.Errorf("unexpected result type %s", v.Type())
	}
}

// finite maps NaN and infinities (a 0/0 share when nothing ran) to 0.
func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}
