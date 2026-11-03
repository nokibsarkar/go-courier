// Package ecourier is the eCourier (Bangladesh) merchant API adapter.
//
// Capabilities: ShipmentCreator, ShipmentTracker, ShipmentCanceller,
// ShipmentLabelReader and CustomerRiskReader, plus native methods for packages,
// cities, thanas, areas, hubs, payment status and child-parcel tracking.
//
// Global and tenant modes are exclusive and never fall back to each other:
//
//	global, err := ecourier.New(ecourier.WithProduction())       // ECOURIER_API_KEY / _API_SECRET / _USER_ID
//	tenant, err := ecourier.NewTenant(ecourier.WithProduction()) // ecourier.ContextWithCredentials(ctx, ...)
//
// The environment (sandbox https://staging.ecourier.com.bd/api or live
// https://backoffice.ecourier.com.bd/api) must be chosen explicitly so test and
// live credentials can never be mixed by inference.
//
// Provider behaviour handled here:
//   - Most failures are HTTP 200/4xx with "response_code"/"success" in the body;
//     they are returned as errors, never as success.
//   - Orders need a package code (from ListPackages), city, thana, area and zip;
//     the SDK never guesses locations (set Address.City, Address.Area, a
//     LocationRef{Kind:"thana"} and Address.PostalCode).
//   - eCourier rejects a duplicate product_id + recipient mobile, so MerchantOrderID
//     (sent as product_id) gives provider-documented duplicate protection. The SDK
//     still never replays a create: after an indeterminate create, call
//     TrackShipment by merchant order ID.
//   - Tracking is a POST that only reads; it is not auto-retried (TRK-13). Tracking
//     by product_id can match several parcels and then fails with ErrAmbiguous.
//   - The document does not state the time zone of event timestamps, so events carry
//     OccurredAt only if you opt in with WithEventLocation; the raw entry is kept.
//   - The fraud-status check may consume COD balance ("Insufficient COD Balance"
//     is a documented outcome): treat each call as billable.
//   - product_price is an integer taka amount; fractional amounts are rejected,
//     never rounded.
package ecourier
