package redx

import (
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// Manifest reports this adapter's capability and verification state.
func Manifest() courier.SourceManifest {
	return courier.SourceManifest{
		Provider:   ProviderID,
		Version:    "v1.0.0-beta",
		VerifiedOn: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		Capabilities: map[string]courier.Verification{
			"auth_api_access_token": courier.VerifiedImplemented, // API-ACCESS-TOKEN: Bearer <jwt>
			"create_parcel":         courier.VerifiedImplemented, // POST /parcel
			"track_parcel":          courier.VerifiedImplemented, // GET /parcel/track/{id}
			"parcel_info":           courier.VerifiedImplemented, // GET /parcel/info/{id}
			"cancel_parcel":         courier.VerifiedImplemented, // PATCH /parcels, accepted asynchronously
			"update_other_props":    courier.DocumentedNotImplemented,
			"areas":                 courier.VerifiedImplemented, // GET /areas (+post_code, district_name)
			"pickup_store_create":   courier.VerifiedImplemented,
			"pickup_store_list":     courier.VerifiedImplemented,
			"pickup_store_get":      courier.VerifiedImplemented,
			"charge_calculator":     courier.VerifiedImplemented, // native CalculateCharge; not a Quoter
			"webhook_status":        courier.VerifiedImplemented, // token in URL query, no signature
			"batch_create":          courier.ProviderUnsupported,
			"quote":                 courier.ProviderUnsupported, // no quote ID is issued
			"merchant_id_tracking":  courier.ProviderUnsupported,
			"idempotency":           courier.ProviderUnsupported,
		},
		Unverified: []string{
			"base URL scheme (https assumed)",
			"error response body shape and HTTP status mapping",
			"JSON types for create: parcel_weight and value are sent as numbers (weight in integer grams); string forms are not tested",
			"whether fractional taka amounts are accepted (the SDK rejects fractions)",
			"cash_collection_amount \"0\" as the way to express no COD",
			"parcel_weight unit on create (kg or g; grams are sent)",
			"meaning of codCharge relative to deliveryCharge (no combined quote is produced)",
			"tracking history timestamp zone: Z-suffixed ISO 8601 values are parsed",
			"webhook timestamp format and delivery retry behaviour (placeholders only)",
			"whether a cancel request that was accepted is later rejected",
		},
		Notes: "Sandbox and production tokens differ; the environment must be chosen explicitly. Webhooks have no signature: a URL token is the only authentication.",
	}
}
