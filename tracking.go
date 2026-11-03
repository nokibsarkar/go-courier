package courier

import (
	"strings"
	"time"
)

// Status is the canonical shipment phase. StatusUnknown is explicit and is
// never silently mapped to success (DOM-07, TRK-07).
type Status string

const (
	StatusUnknown        Status = "unknown"
	StatusCreated        Status = "created"
	StatusPickupPending  Status = "pickup_pending"
	StatusPickedUp       Status = "picked_up"
	StatusInTransit      Status = "in_transit"
	StatusOutForDelivery Status = "out_for_delivery"
	StatusDelivered      Status = "delivered"
	StatusDeliveryFailed Status = "delivery_failed"
	StatusOnHold         Status = "on_hold"
	StatusCancelled      Status = "cancelled"
	StatusReturning      Status = "returning"
	StatusReturned       Status = "returned"
)

// ProviderStatus preserves the provider-native status (TRK-05).
type ProviderStatus struct {
	Code        string
	Description string
}

// IsZero reports whether no provider status was supplied.
func (p ProviderStatus) IsZero() bool { return p == ProviderStatus{} }

// HistoryCoverage states how complete the returned event history is (TRK-06).
type HistoryCoverage string

const (
	CoverageUnknown     HistoryCoverage = "unknown"      // history completeness cannot be determined
	CoverageFull        HistoryCoverage = "full"         // full as exposed by the provider
	CoveragePartial     HistoryCoverage = "partial"      // provider exposes only some events
	CoverageCurrentOnly HistoryCoverage = "current_only" // only the current state is exposed
)

// LookupType names an identifier kind in a tracking request.
type LookupType string

const (
	LookupProviderShipmentID LookupType = "provider_shipment_id"
	LookupMerchantOrderID    LookupType = "merchant_order_id"
	LookupTrackingCode       LookupType = "tracking_code"
	LookupPhone              LookupType = "phone"
)

// Lookup is an additional identifier, usable for shipments created outside
// the SDK (TRK-03).
type Lookup struct {
	Type  LookupType
	Value string
}

// TrackingRequest asks for tracking by provider-issued code and/or additional
// identifiers. It never requires a locally persisted shipment (TRK-03).
type TrackingRequest struct {
	Code    TrackingCode
	Lookups []Lookup
	// Account scopes the lookup where the provider requires it (TRK-04).
	// Adapters never infer it from overlapping codes.
	Account AccountID
}

// Validate checks that at least one non-blank identifier exists.
func (r TrackingRequest) Validate() error {
	if strings.TrimSpace(string(r.Code)) != "" {
		return nil
	}
	for _, l := range r.Lookups {
		if strings.TrimSpace(l.Value) != "" && l.Type != "" {
			return nil
		}
	}
	return Validation("code", "a tracking code or lookup identifier is required")
}

// Find returns the value of the first Lookup of type t.
func (r TrackingRequest) Find(t LookupType) (string, bool) {
	for _, l := range r.Lookups {
		if l.Type == t && strings.TrimSpace(l.Value) != "" {
			return strings.TrimSpace(l.Value), true
		}
	}
	return "", false
}

// Location is a genuine source-provided event location.
type Location struct {
	Name        string
	Address     *Address
	Coordinates *Coordinates
}

// TrackingEvent is one provider event. OccurredAt is nil when the provider
// supplied no timestamp; it is never substituted (TRK-07, DOM-08).
type TrackingEvent struct {
	OccurredAt     *time.Time
	ProviderStatus ProviderStatus
	Status         Status
	Description    string
	Location       *Location
	Raw            []byte
}

// DeliveryConfirmation is present only when the provider confirms delivery.
type DeliveryConfirmation struct {
	DeliveredAt *time.Time
	ReceivedBy  string
}

// CODState reports COD collection, kept separate from delivery phase (DOM-07).
type CODState string

const (
	CODUnknown   CODState = "unknown"
	CODNone      CODState = "none"
	CODPending   CODState = "pending"
	CODCollected CODState = "collected"
	CODSettled   CODState = "settled"
)

// TrackingResult is the outcome of a tracking lookup.
type TrackingResult struct {
	Ref          ShipmentRef
	TrackingCode TrackingCode
	// ProviderStatus is the raw current provider status; Status is the
	// inferred canonical phase (StatusUnknown when ambiguous).
	ProviderStatus ProviderStatus
	Status         Status
	Coverage       HistoryCoverage
	// Events preserve provider order; they are never re-sorted.
	Events   []TrackingEvent
	Delivery *DeliveryConfirmation
	COD      CODState
	// ObservedAt is when the SDK retrieved this data (not event time).
	ObservedAt time.Time
	// Partial is true with Diagnostics when records were skipped or malformed
	// in a declared-partial result (TRK-08).
	Partial     bool
	Diagnostics []string
	Raw         []byte
}
