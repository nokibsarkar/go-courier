// Package redx is the RedX (Bangladesh) merchant API adapter (v1.0.0-beta).
//
// Capabilities: ShipmentCreator, ShipmentTracker, ShipmentCanceller,
// WebhookHandlerProvider and WebhookDecoder, plus native methods for areas,
// pickup stores and the charge calculator. There is no quote ID, so the
// charge calculator is a native method, not a Quoter.
//
// Global and tenant modes are exclusive and never fall back to each other:
//
//	global, err := redx.New(redx.WithProduction())       // REDX_API_TOKEN
//	tenant, err := redx.NewTenant(redx.WithProduction()) // redx.ContextWithCredentials(ctx, ...)
//
// The environment must be chosen explicitly (WithSandbox, WithProduction,
// WithBaseURL): sandbox and production tokens differ and are never inferred.
//
// Provider behaviour handled here:
//   - Parcels need a RedX delivery area ID (LocationRef Kind "area" on the
//     recipient address, from ListAreas), the area name, weight in grams and a
//     declared value. Nothing is guessed or defaulted.
//   - RedX documents no idempotency or duplicate protection. A create that
//     times out, fails with 5xx or returns an unusable 200 is indeterminate
//     and is never replayed; reconcile with TrackShipment using the tracking
//     ID if you have it, otherwise check the merchant panel.
//   - Tracking combines GET /parcel/info (current status, mapped) with GET
//     /parcel/track (message history; entries have no status codes, so event
//     statuses are unknown and the raw entry is kept).
//   - Cancel returns "Request Accepted": acceptance, not confirmation.
//   - Webhooks have no signature; authentication is a token in the callback
//     URL query string. See the note in webhook.go before exposing one.
package redx
