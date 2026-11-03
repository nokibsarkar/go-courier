// Package uber is the Uber Direct (DaaS) adapter.
//
// Capabilities (see [Manifest]): Quoter, ShipmentCreator, ShipmentReader,
// ShipmentTracker, ShipmentCanceller, WebhookHandlerProvider and
// WebhookDecoder. Batch creation, proof of delivery, labels, returns,
// balances and incident reporting are not implemented.
//
// Two mutually exclusive modes exist and never fall back to each other:
//
//	global, err := uber.New()       // UBER_DIRECT_CLIENT_ID / _CLIENT_SECRET / _CUSTOMER_ID or WithCredentials
//	tenant, err := uber.NewTenant() // credentials only via uber.ContextWithCredentials(ctx, ...)
//
// Adapter-specific behaviour (PROV-02):
//   - OAuth2 client-credentials tokens (scope eats.deliveries) last about 30
//     days, cannot be refreshed and token requests are limited to 100/hour, so
//     they are cached per credential identity. An API 401 invalidates exactly
//     that token and re-issues once (safe: 401 means nothing was processed).
//   - The customer ID is part of [Credentials] and of every API path; one
//     client ID may serve several customer IDs.
//   - Money fields are integer minor units and are converted with an explicit
//     currency-exponent table; the currency always comes from the provider.
//   - Quote never books. CreateShipment books only when called; with an empty
//     QuoteID the delivery is created without a quote_id.
//   - Uber exposes only the current delivery status, so tracking declares
//     CoverageCurrentOnly with no events.
//   - Webhooks are verified (HMAC-SHA256 over the raw body) against explicitly
//     configured per-account signing keys; an event is attributed to an
//     account only when exactly one key verifies. Verification needs no
//     outbound credentials, so a tenant-mode client suffices as a receiver.
//   - Provider error message text is never copied into errors (it may echo
//     addresses or phone numbers); only the provider error code is kept.
package uber
