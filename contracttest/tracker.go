// Package contracttest holds public conformance helpers that independent
// adapters can run against their own fixtures without importing any SDK
// internals (QA-02).
package contracttest

import (
	"context"
	"errors"
	"testing"

	courier "github.com/nokibsarkar/go-courier"
)

// TrackerCase is one fixture-backed tracking expectation.
type TrackerCase struct {
	Name    string
	Request courier.TrackingRequest
	// Check performs adapter-specific assertions on the result.
	Check func(t *testing.T, res *courier.TrackingResult)
}

// TrackerSuite describes a tracking-only conformance run. Tracker must be
// wired to fixtures/mocks (never live endpoints).
type TrackerSuite struct {
	Tracker courier.ShipmentTracker
	Cases   []TrackerCase
	// ForbidCreation asserts the client does not expose creation methods
	// (for tracking-only adapters).
	ForbidCreation bool
	// MalformedRequest, when set, must make the tracker return an error
	// (e.g. a request whose fixture response is malformed).
	MalformedRequest *courier.TrackingRequest
}

// RunShipmentTracker runs the generic TRK conformance checks.
func RunShipmentTracker(t *testing.T, s TrackerSuite) {
	t.Helper()
	if s.Tracker == nil {
		t.Fatal("contracttest: nil Tracker")
	}
	if s.ForbidCreation {
		if _, ok := s.Tracker.(courier.ShipmentCreator); ok {
			t.Error("tracking-only client must not implement ShipmentCreator")
		}
	}
	t.Run("empty request is a validation error", func(t *testing.T) {
		_, err := s.Tracker.TrackShipment(context.Background(), courier.TrackingRequest{})
		if !errors.Is(err, courier.ErrValidation) {
			t.Fatalf("want validation error, got %v", err)
		}
	})
	t.Run("caller cancellation is honored", func(t *testing.T) {
		if len(s.Cases) == 0 {
			t.Skip("no cases")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res, err := s.Tracker.TrackShipment(ctx, s.Cases[0].Request)
		if err == nil || res != nil {
			t.Fatalf("want error and nil result on canceled context, got %v %v", res, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error should wrap context.Canceled, got %v", err)
		}
	})
	for _, c := range s.Cases {
		t.Run(c.Name, func(t *testing.T) {
			res, err := s.Tracker.TrackShipment(context.Background(), c.Request)
			if err != nil {
				t.Fatalf("TrackShipment: %v", err)
			}
			CheckTrackingResult(t, res)
			if c.Check != nil {
				c.Check(t, res)
			}
		})
	}
	if s.MalformedRequest != nil {
		t.Run("malformed record is an explicit error", func(t *testing.T) {
			res, err := s.Tracker.TrackShipment(context.Background(), *s.MalformedRequest)
			if err == nil && !(res != nil && res.Partial && len(res.Diagnostics) > 0) {
				t.Fatal("malformed record must yield an error or a declared partial result with diagnostics")
			}
		})
	}
}

// CheckTrackingResult asserts invariants every TrackingResult must satisfy.
func CheckTrackingResult(t *testing.T, r *courier.TrackingResult) {
	t.Helper()
	if r == nil {
		t.Fatal("nil result")
	}
	if r.Status == "" {
		t.Error("Status must be set (use StatusUnknown)")
	}
	switch r.Coverage {
	case courier.CoverageFull, courier.CoveragePartial, courier.CoverageCurrentOnly, courier.CoverageUnknown:
	default:
		t.Errorf("invalid coverage %q", r.Coverage)
	}
	if r.ObservedAt.IsZero() {
		t.Error("ObservedAt (retrieval time) must be set")
	}
	if r.Coverage == courier.CoverageCurrentOnly && len(r.Events) > 0 {
		t.Error("current-only coverage must not carry fabricated history events")
	}
	if r.Status == courier.StatusDelivered && r.Delivery == nil && len(r.Events) == 0 && r.ProviderStatus.IsZero() {
		t.Error("delivered status without any provider evidence")
	}
	if r.Partial && len(r.Diagnostics) == 0 {
		t.Error("partial results must include diagnostics")
	}
	if r.Delivery != nil && r.Status != courier.StatusDelivered {
		t.Error("delivery confirmation present but status is not delivered")
	}
	for i, e := range r.Events {
		if e.Status == "" {
			t.Errorf("event %d: Status must be set", i)
		}
		if e.OccurredAt != nil && e.OccurredAt.IsZero() {
			t.Errorf("event %d: zero OccurredAt must be nil", i)
		}
	}
}
