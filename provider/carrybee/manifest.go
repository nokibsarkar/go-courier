package carrybee

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
			"create_shipment":  courier.VerifiedImplemented,
			"track_shipment":   courier.VerifiedImplemented,
			"cancel_shipment":  courier.VerifiedImplemented,
			"locations":        courier.VerifiedImplemented,      // paths only; entry fields assumed
			"stores":           courier.VerifiedImplemented,      // list only
			"webhooks":         courier.VerifiedImplemented,      // header secret and event names are assumed, not documented
			"create_shipments": courier.DocumentedNotImplemented, // POST /api/v2/orders-bulk {"orders":[...]}: response shape unknown
			"create_store":     courier.DocumentedNotImplemented, // POST /api/v2/stores: payload unknown
			"address_details":  courier.DocumentedNotImplemented, // POST /api/v2/address-details {"query"}
			"quote":            courier.UnknownUnverified,
			"labels":           courier.UnknownUnverified,
			"balance":          courier.UnknownUnverified,
		},
		Unverified: []string{
			"meaning of delivery_type and product_type codes (the SDK sends neither unless set); whether they are required",
			"item_weight unit (grams are assumed) and any other limits",
			"transfer_status vocabulary of the details endpoint (only webhook event names are evidenced)",
			"response shape of the location and store lists, and the store-create and bulk payloads",
			"whether cancellation needs a reason, and whether it takes effect immediately",
			"webhook: no timestamp or event ID in the payload, retry behaviour, and the acknowledgement body; the secret is a plain header value, not an HMAC",
			"error body shape (the adapter reads message, error_description, errors, error and status=false)",
			"token lifetime does not apply: credentials are static headers, and rate limits are undocumented",
		},
	}
}
