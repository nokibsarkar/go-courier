// Package steadfast is the Steadfast (packzy) merchant API adapter.
//
// Capabilities (see [Manifest] for verification state):
//
//   - ShipmentCreator, BatchShipmentCreator (max 500; the request carries the
//     orders as a JSON array), ShipmentTracker, BalanceReader and webhooks.
//   - Native methods: GetCustomerScore, TrackingHistory, TrackWithReturnStatus,
//     CreatePickupRequest, CreateReturnRequest, ListPayments, GetPayment and
//     ListPoliceStations. ListPoliceStations returns districts with nested
//     police stations.
//   - The single and list return-request bodies are not described by the
//     provider and are decoded permissively. A single payment arrives under
//     "payment", like a single order under "consignment".
//   - Not offered by the provider API: cancel. Extended bulk booking is not
//     used.
//
// Two mutually exclusive modes exist and never fall back to each other:
//
//	global, err := steadfast.New()       // STEADFAST_API_KEY / STEADFAST_SECRET_KEY or WithCredentials
//	tenant, err := steadfast.NewTenant() // credentials only via steadfast.ContextWithCredentials(ctx, ...)
//
// Adapter-specific behaviour handled here (PROV-02), not generalised:
//   - HTTP 200 responses can embed a failure in the JSON "status" field; they are
//     returned as errors, never as success.
//   - "*_approval_pending" statuses are the rider's word and not yet confirmed:
//     they map to StatusUnknown with the raw code kept, so do not settle money on
//     them; wait for delivered, partial_delivered or cancelled. partial_delivered
//     has no canonical phase and also maps to StatusUnknown.
//   - The status endpoint only exposes the current status, so tracking results
//     declare [courier.CoverageCurrentOnly] and carry no events.
//   - Repeated rejected credentials may lock an account upstream, so credential
//     rejections are never retried.
//   - Merchant invoice IDs are unique per merchant and duplicates are refused, so
//     the provider documents that retrying a create with the same invoice cannot
//     double-book. The SDK still does not replay for you: after an indeterminate
//     create, call TrackShipment by merchant order ID (or re-submit the same
//     invoice yourself; a duplicate refusal means the first attempt succeeded).
//   - Status answers are cached upstream for 60 seconds; polling faster tells you
//     nothing new. Prefer webhooks (see [Client.WebhookHandler]).
//   - Over-long fields are truncated upstream; this adapter rejects them locally
//     (invoice 100, name 100, address 490, note 480 characters).
//   - Keys belong to one business profile; use one credential set per profile.
//   - A key that gets repeated 401s is locked out with 429 for 60 minutes, so a
//     401 is never retried.
//   - Text fields have these characters replaced by a space before storage:
//     { } ; < > $ . What you send is not always what you get back.
//   - A customer score value of nil means "not known", never zero.
package steadfast
