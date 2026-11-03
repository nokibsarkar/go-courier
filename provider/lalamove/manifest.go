package lalamove

import (
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// Manifest reports this adapter's capability and verification state.
func Manifest() courier.SourceManifest {
	return courier.SourceManifest{
		Provider:   ProviderID,
		Version:    "v3",
		VerifiedOn: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		Capabilities: map[string]courier.Verification{
			"quote":                  courier.VerifiedImplemented,
			"get_quote":              courier.VerifiedImplemented,
			"create_shipment":        courier.VerifiedImplemented,
			"get_shipment":           courier.VerifiedImplemented,
			"cancel_shipment":        courier.VerifiedImplemented,
			"track_shipment":         courier.VerifiedImplemented,
			"driver_details":         courier.VerifiedImplemented,
			"pod_data_in_order":      courier.VerifiedImplemented,
			"proof_of_delivery_read": courier.DocumentedNotImplemented,
			"multi_stop_orders":      courier.DocumentedNotImplemented,
			"edit_order":             courier.DocumentedNotImplemented,
			"change_driver":          courier.DocumentedNotImplemented,
			"priority_fee":           courier.DocumentedNotImplemented,
			"webhooks":               courier.DocumentedNotImplemented,
			"batch_create":           courier.UnknownUnverified,
			"idempotency_key":        courier.UnknownUnverified,
			"tracking_history":       courier.UnknownUnverified,
			"market_BD":              courier.UnknownUnverified,
			"availability_BD":        courier.UnknownUnverified,
		},
		Unverified: []string{
			"Bangladesh (BD) market code, service types, production credentials, order acceptance and country limits",
			"error response body shape (only errors[].id is read)",
			"stop-count limit (docs say 2-16, 2-17 and 1+15)",
			"ON_GOING, REJECTED and EXPIRED status semantics (mapped to unknown)",
			"order response timestamps (none mapped)",
			"quotation item/weight/category fields (not sent)",
			"webhook signature authenticity and tenant attribution",
		},
		Notes: "Bangladesh operational presence does not establish API market support: the supported market list is JP, BR, HK, ID, MY, MX, PH, SG, TW, TH, VN, AE and SA, and not BD. " +
			"Capabilities for Bangladesh are unknown_unverified; Market is a required explicit credential with no default. " +
			"Webhooks are disabled (fail closed) until signature authenticity and tenant attribution are proven (LALA-05). " +
			"Quotations are honoured for about five minutes; creation is quotation-first and single-recipient only.",
	}
}
