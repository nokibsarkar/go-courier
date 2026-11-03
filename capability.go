package courier

import (
	"context"
	"net/http"
)

// Each interface below is independent and optional (SCP-01, ARC-03). Published
// interfaces never gain methods within a major version; new functionality is
// added as new interfaces. Method semantics are documented per interface.

// ShipmentCreator creates one shipment. It is a state-changing operation:
// callers must treat ErrIndeterminate as "may have been created" and reconcile
// before retrying unless the provider documents idempotency.
type ShipmentCreator interface {
	CreateShipment(context.Context, CreateShipmentRequest) (*Shipment, error)
}

// BatchShipmentCreator creates several shipments, retaining per-item results.
type BatchShipmentCreator interface {
	CreateShipments(context.Context, []CreateShipmentRequest) (*BatchResult, error)
}

// ShipmentReader fetches a shipment by reference.
type ShipmentReader interface {
	GetShipment(context.Context, ShipmentRef) (*Shipment, error)
}

// ShipmentCanceller cancels a shipment. Cancellation is distinct from return
// and exchange.
type ShipmentCanceller interface {
	CancelShipment(context.Context, ShipmentRef) error
}

// ShipmentTracker is the narrow tracking contract (TRK-01). It needs no
// shipment creation, retrieval, reporting, webhook or registry capability.
// Implementations must not fabricate events, timestamps, locations or
// delivery confirmation, and must return unknown rather than guess.
type ShipmentTracker interface {
	TrackShipment(context.Context, TrackingRequest) (*TrackingResult, error)
}

// BulkShipmentTracker is an optional bulk tracking capability (TRK-09). The
// result slice is index-aligned with the requests.
type BulkShipmentTracker interface {
	TrackShipments(context.Context, []TrackingRequest) ([]TrackingOutcome, error)
}

// TrackingOutcome is one bulk tracking item.
type TrackingOutcome struct {
	Result *TrackingResult
	Err    error
}

// ProofOfDeliveryReader streams or references proof of delivery.
type ProofOfDeliveryReader interface {
	GetProofOfDelivery(context.Context, ShipmentRef) (*Document, error)
}

// ShipmentLabelReader streams or references a shipment label.
type ShipmentLabelReader interface {
	GetShipmentLabel(context.Context, ShipmentRef) (*Document, error)
}

// Quoter obtains priced quotes; it never books anything (CAP-02).
type Quoter interface {
	Quote(context.Context, QuoteRequest) (*Quote, error)
}

// QuoteReader retrieves a previously issued quote.
type QuoteReader interface {
	GetQuote(context.Context, QuoteID) (*Quote, error)
}

// ServiceabilityChecker reports route serviceability.
type ServiceabilityChecker interface {
	CheckServiceability(context.Context, ServiceabilityRequest) (*Serviceability, error)
}

// PickupRequester explicitly requests a pickup.
type PickupRequester interface {
	RequestPickup(context.Context, PickupRequest) (*Pickup, error)
}

// ReturnRequester requests a return.
type ReturnRequester interface {
	RequestReturn(context.Context, ReturnRequest) (*ReturnReceipt, error)
}

// BalanceReader reads an account balance.
type BalanceReader interface {
	GetBalance(context.Context) (*Balance, error)
}

// CustomerRiskReader is an independent risk-data source (RISK-01). It needs no
// shipment capability.
type CustomerRiskReader interface {
	GetCustomerRisk(context.Context, CustomerRiskRequest) (*CustomerRisk, error)
}

// CustomerIncidentReporter submits an incident report. It is only ever invoked
// by an explicit, authorized action (RPT-09, PROV-03).
type CustomerIncidentReporter interface {
	ReportCustomerIncident(context.Context, CustomerIncidentReport) (*IncidentReceipt, error)
}

// WebhookHandlerProvider exposes one standard http.Handler per provider
// (HOOK-01). The application mounts it at a provider-specific path; no router
// is required or imported.
type WebhookHandlerProvider interface {
	WebhookHandler(EventHandler) (http.Handler, error)
}

// WebhookDecoder decodes and verifies webhook requests without HTTP, for
// queue/serverless flows (HOOK-06). Decoding alone claims authenticity only
// for events with Trust == TrustVerified.
type WebhookDecoder interface {
	DecodeWebhook(ctx context.Context, header http.Header, body []byte) ([]Event, error)
}
