package contracttest_test

import (
	"context"
	"testing"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/contracttest"
)

// fixtureTracker is an independently authored tracking-only adapter (QA-02,
// SRS 8): it imports only the public root package and contracttest, registers
// nothing globally and implements no creation/reading/reporting method.
type fixtureTracker struct{}

var _ courier.ShipmentTracker = fixtureTracker{}

func (fixtureTracker) TrackShipment(ctx context.Context, r courier.TrackingRequest) (*courier.TrackingResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, &courier.Error{Kind: courier.KindUnavailable, Err: err}
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	switch r.Code {
	case "MALFORMED":
		return nil, &courier.Error{Kind: courier.KindProtocol, Message: "bad record"}
	case "UNMAPPED":
		// unknown provider status is preserved raw and stays unknown
		return &courier.TrackingResult{ProviderStatus: courier.ProviderStatus{Code: "WEIRD_42"}, Status: courier.StatusUnknown,
			Coverage: courier.CoverageCurrentOnly, COD: courier.CODUnknown, ObservedAt: time.Now()}, nil
	}
	return &courier.TrackingResult{
		TrackingCode: r.Code, ProviderStatus: courier.ProviderStatus{Code: "DLV", Description: "Delivered"},
		Status: courier.StatusDelivered, Coverage: courier.CoverageFull, Delivery: &courier.DeliveryConfirmation{},
		Events: []courier.TrackingEvent{
			{ProviderStatus: courier.ProviderStatus{Code: "PU"}, Status: courier.StatusPickedUp}, // no timestamp: stays nil
			{ProviderStatus: courier.ProviderStatus{Code: "DLV"}, Status: courier.StatusDelivered},
		},
		ObservedAt: time.Now(),
	}, nil
}

func TestIndependentTrackingOnlyAdapter(t *testing.T) {
	bad := courier.TrackingRequest{Code: "MALFORMED"}
	contracttest.RunShipmentTracker(t, contracttest.TrackerSuite{
		Tracker:          fixtureTracker{},
		ForbidCreation:   true,
		MalformedRequest: &bad,
		Cases: []contracttest.TrackerCase{
			{Name: "full history, missing timestamps stay absent", Request: courier.TrackingRequest{Code: "OK"}, Check: func(t *testing.T, r *courier.TrackingResult) {
				for _, e := range r.Events {
					if e.OccurredAt != nil {
						t.Error("timestamp fabricated")
					}
				}
			}},
			{Name: "unknown status preserved raw", Request: courier.TrackingRequest{Code: "UNMAPPED"}, Check: func(t *testing.T, r *courier.TrackingResult) {
				if r.Status != courier.StatusUnknown || r.ProviderStatus.Code != "WEIRD_42" {
					t.Errorf("%+v", r)
				}
			}},
		},
	})
}
