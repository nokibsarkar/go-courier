package steadfast

import (
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// Manifest reports this adapter's capability and verification state.
func Manifest() courier.SourceManifest {
	return courier.SourceManifest{
		Provider:   ProviderID,
		VerifiedOn: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		Capabilities: map[string]courier.Verification{
			"create_shipment":   courier.VerifiedImplemented,
			"create_shipments":  courier.VerifiedImplemented, // POST /create_order/bulk-order, data as an array
			"track_shipment":    courier.VerifiedImplemented,
			"get_balance":       courier.VerifiedImplemented,
			"ping":              courier.VerifiedImplemented,
			"tracking_history":  courier.VerifiedImplemented,      // native TrackingHistory: GET /trackings_by_invoice/{invoice}
			"return_status":     courier.VerifiedImplemented,      // native TrackWithReturnStatus
			"pickup_request":    courier.VerifiedImplemented,      // native CreatePickupRequest
			"customer_risk":     courier.VerifiedImplemented,      // native GetCustomerScore: /fraud_check/score/{phone}
			"payments":          courier.VerifiedImplemented,      // list under "payments", single under "payment"
			"returns":           courier.VerifiedImplemented,      // creation; the single and list reads are not described
			"webhooks":          courier.VerifiedImplemented,      // bearer token plus HMAC-SHA256 X-Signature, Idempotency-Key, all event types
			"police_stations":   courier.UnknownUnverified,        // GET /police_stations: no field is described
			"bulk_extended":     courier.DocumentedNotImplemented, // POST /create_order/bulk-order/extended
			"cancel_shipment":   courier.ProviderUnsupported,
			"proof_of_delivery": courier.ProviderUnsupported,
		},
		Notes: "Status answers are cached for 60 seconds; field lengths are truncated upstream and rejected locally; keys belong to one business profile. Request limits are 1,000 a minute (6,000 for booking); ten failed authentications in five minutes lock the client out for an hour.",
		Unverified: []string{
			"police stations: response fields",
			"the single and list return-request bodies; the parcels settled by a payment are kept raw",
			"the notification types other than delivery_status and tracking_update are passed through raw: their fields are only named, not typed",
			"webhook updated_at, payment timestamps and similar fields carry no timezone, so they are kept as text and never converted to an instant",
			"extended bulk booking is not used",
			"body shapes of non-400 errors",
		},
	}
}
