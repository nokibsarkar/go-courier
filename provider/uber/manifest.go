package uber

import (
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// Manifest reports this adapter's capability and verification state.
func Manifest() courier.SourceManifest {
	return courier.SourceManifest{
		Provider:   ProviderID,
		Version:    "1.0.1",
		VerifiedOn: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		Capabilities: map[string]courier.Verification{
			"auth_client_credentials":   courier.VerifiedImplemented,
			"quote":                     courier.VerifiedImplemented,
			"create_shipment":           courier.VerifiedImplemented,
			"get_shipment":              courier.VerifiedImplemented,
			"track_shipment":            courier.VerifiedImplemented,
			"cancel_shipment":           courier.VerifiedImplemented,
			"list_deliveries":           courier.VerifiedImplemented,
			"proof_of_delivery":         courier.VerifiedImplemented, // native GetProofOfDeliveryDocument
			"idempotency_key":           courier.VerifiedImplemented, // documented: 60-minute default window
			"unquoted_create":           courier.VerifiedImplemented, // quote_id is optional in DeliveryReq
			"webhook_signature":         courier.VerifiedImplemented,
			"webhook_delivery_status":   courier.VerifiedImplemented, // delivery_status payload
			"webhook_courier_update":    courier.VerifiedImplemented, // current status only; courier coordinates stay in the raw payload
			"webhook_refund_request":    courier.VerifiedImplemented, // decoded generically; typed via ParseRefundRequest
			"webhook_shopping_progress": courier.VerifiedImplemented, // typed via ParseShoppingProgress
			"webhook_billing_policy":    courier.VerifiedImplemented, // direct.organization, typed via ParseBillingPolicyUpdate
			"webhook_batched_payloads":  courier.UnknownUnverified,
			"update_delivery":           courier.DocumentedNotImplemented, // POST deliveries/{id}
			"find_stores":               courier.DocumentedNotImplemented, // GET /direct/organizations/{id}/stores (organization scope)
			"create_shipments":          courier.ProviderUnsupported,
			"cod":                       courier.ProviderUnsupported,
			"unstructured_address":      courier.ProviderUnsupported, // the API requires a JSON-string address
		},
		Unverified: []string{
			"webhook batch/array bodies (accepted defensively)",
			"minor-unit exponent for zero/three-decimal currencies (only \"cents, 1/100\" is stated)",
			"Update Delivery and Find Stores (not implemented)",
		},
		Notes: "Token requests are limited to 100/hour; tokens are cached. Fees are integer cents. Find Stores lives under /direct/organizations and may need the direct.organizations scope.",
	}
}
