package pathao

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
			// Implemented and exercised against assumption-based fixtures only.
			"create_shipment": courier.VerifiedImplemented,
			"track_shipment":  courier.VerifiedImplemented,
			"quote":           courier.VerifiedImplemented,
			"locations":       courier.VerifiedImplemented,
			"stores":          courier.VerifiedImplemented,
			// Not implemented.
			"create_shipments":    courier.DocumentedNotImplemented, // asynchronous bulk (202) is assumed, not documented
			"webhooks":            courier.DocumentedNotImplemented, // events, signature and attribution are unverified
			"create_store":        courier.DocumentedNotImplemented,
			"merchant_order_find": courier.UnknownUnverified,
			"cancel_shipment":     courier.UnknownUnverified,
			"read_shipment":       courier.UnknownUnverified,
			"labels":              courier.UnknownUnverified,
			"balance":             courier.UnknownUnverified,
		},
		Unverified: []string{
			"endpoint URL paths (assumed /aladdin/api/v1/...)", "response envelope shape {type, code, message, data}",
			"token endpoint and refresh grant details", "status vocabulary completeness",
			"order info timestamps and event history", "rate limits and token lifetime",
			"amount_to_collect numeric format (integer vs decimal)", "request field length limits",
		},
	}
}
