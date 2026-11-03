package ecourier

import (
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

// Manifest reports this adapter's capability and verification state.
func Manifest() courier.SourceManifest {
	return courier.SourceManifest{
		Provider:   ProviderID,
		Version:    "5.5",
		VerifiedOn: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		Capabilities: map[string]courier.Verification{
			"place_order":        courier.VerifiedImplemented,      // POST /order-place
			"track":              courier.VerifiedImplemented,      // POST /track
			"track_child":        courier.VerifiedImplemented,      // POST /track-child
			"cancel_order":       courier.VerifiedImplemented,      // POST /cancel-order
			"cancel_child_order": courier.DocumentedNotImplemented, // POST /cancel-order-child
			"label_print":        courier.VerifiedImplemented,      // POST /label-print (returns a URL)
			"fraud_status_check": courier.VerifiedImplemented,      // POST /fraud-status-check
			"packages":           courier.VerifiedImplemented,
			"city_list":          courier.VerifiedImplemented,
			"thana_list":         courier.VerifiedImplemented,
			"area_by_district":   courier.VerifiedImplemented,
			"hub_info_by_area":   courier.VerifiedImplemented,
			"payment_status":     courier.VerifiedImplemented,
			"area_by_post_code":  courier.DocumentedNotImplemented, // section 2.8
			"post_code_list":     courier.DocumentedNotImplemented, // section 2.9
			"branch_list":        courier.DocumentedNotImplemented, // section 2.10
			"booster_sms":        courier.ProviderUnsupported,      // messaging/billing, out of SDK scope
			"otp_sms":            courier.ProviderUnsupported,
			"webhooks":           courier.UnknownUnverified, // not offered by the provider API
			"batch_create":       courier.ProviderUnsupported,
			"quote":              courier.ProviderUnsupported, // ListPackages exposes shipping_charge only
			"event_timestamp_tz": courier.UnknownUnverified,
		},
		Unverified: []string{
			"time zone of tracking event timestamps",
			"recipient_address length (limit stated as 40 characters, but longer values occur): not enforced locally",
			"whether tracking POSTs are safe to replay (not auto-retried)",
			"pages 14-21 (area by post code, post code list, branch list, booster SMS) were not reviewed",
		},
		Notes: "Sandbox: https://staging.ecourier.com.bd/api. Live: https://backoffice.ecourier.com.bd/api.",
	}
}
