package intelligence

import (
	"context"
	"errors"
	"testing"

	courier "github.com/nokibsarkar/go-courier"
)

type reader struct {
	risk *courier.CustomerRisk
	err  error
}

func (r reader) GetCustomerRisk(context.Context, courier.CustomerRiskRequest) (*courier.CustomerRisk, error) {
	return r.risk, r.err
}

func ip(i int) *int { return &i }

func TestAggregatePreservesSourcesAndBoundsOverlap(t *testing.T) {
	a, err := New(WithSources(
		Source{"a", reader{risk: &courier.CustomerRisk{Source: "a", Counts: &courier.DeliveryCounts{Total: ip(10), Returned: ip(2)}}}},
		Source{"b", reader{risk: &courier.CustomerRisk{Source: "b", Counts: &courier.DeliveryCounts{Total: ip(4)}}}},
		Source{"c", reader{err: errors.New("down")}},
	))
	if err != nil {
		t.Fatal(err)
	}
	agg, err := a.GetCustomerRisk(context.Background(), courier.CustomerRiskRequest{Phone: "+8801700000000"})
	if err == nil {
		t.Fatal("per-source failure must surface in the aggregate error")
	}
	if len(agg.Results) != 3 || agg.Results[0].Source != "a" || agg.Results[2].Err == nil || agg.Results[0].Risk == nil {
		t.Fatalf("results: %+v", agg.Results)
	}
	if agg.ExactTotalsAvailable {
		t.Fatal("never exact under unknown overlap")
	}
	tot := agg.Bounds.Total
	if tot.Min != 10 || tot.Max == nil || *tot.Max != 14 {
		t.Fatalf("total bounds %+v", tot)
	}
	// Returned only reported by one source: lower bound only, never zero-filled.
	ret := agg.Bounds.Returned
	if ret.Min != 2 || ret.Max != nil {
		t.Fatalf("returned bounds %+v", ret)
	}
	if agg.Bounds.Cancelled != nil {
		t.Fatal("unreported count must be absent")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(); err == nil {
		t.Fatal("needs sources")
	}
	var nilReader *reader
	if _, err := New(WithSources(Source{"a", nilReader})); err == nil {
		t.Fatal("typed-nil reader")
	}
	if _, err := New(WithSources(Source{"a", reader{}}, Source{"a", reader{}})); err == nil {
		t.Fatal("duplicate id")
	}
}
