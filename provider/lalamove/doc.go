// Package lalamove is the Lalamove API v3 adapter (https://developers.lalamove.com/).
//
// Capabilities (see [Manifest] for verification state): Quoter, QuoteReader,
// ShipmentCreator (quotation-first), ShipmentReader, ShipmentCanceller and
// ShipmentTracker. Provider-specific extras live in separate methods
// (GetOrder, GetDriver, CreateShipmentWithOptions, QuoteWithOptions) and typed
// accessors (QuoteStops); nothing here needs a core change (LALA-01).
//
// Two mutually exclusive credential modes exist and never fall back to each other:
//
//	global, err := lalamove.New(lalamove.WithSandbox())       // WithCredentials or LALAMOVE_API_KEY / LALAMOVE_API_SECRET / LALAMOVE_MARKET
//	tenant, err := lalamove.NewTenant(lalamove.WithProduction()) // credentials only via lalamove.ContextWithCredentials
//
// Explicit choices (no silent defaults):
//   - The environment must be chosen with WithSandbox, WithProduction or WithBaseURL
//     (tests/proxies; the key-prefix check is then skipped). Sandbox keys are
//     pk_test_/sk_test_, production keys pk_prod_/sk_prod_; a mismatch fails locally
//     before any HTTP.
//   - Market is a required credential value (two upper-case letters, e.g. "SG").
//     There is NO default market. Bangladesh operational presence does not establish
//     API market support: the supported markets are JP, BR, HK, ID, MY, MX, PH, SG,
//     TW, TH, VN, AE, SA and not BD. Nothing in this package implies Bangladesh
//     availability (LALA-06).
//
// Request signing (LALA-03): every call carries a fresh
//
//	Authorization: hmac <key>:<unix-ms>:<hex HMAC-SHA256(secret, ts+"\r\n"+METHOD+"\r\n"+PATH+"\r\n\r\n"+BODY)>
//
// where PATH includes the "/v3" prefix and BODY is exactly the bytes sent. A fresh
// random UUIDv4 Request-ID is generated per attempt, including retries of reads
// (the adapter runs its own retry loop so signature and nonce are never reused).
//
// Quotation-first (LALA-02): CreateShipment requires CreateShipmentRequest.QuoteID.
// It fetches the quotation, rejects an expired one locally (injectable WithClock),
// requires exactly one pickup and one recipient stop (multi-stop returns
// courier.ErrUnsupported), preserves stop identity and order, and never reprices
// or re-quotes. Order creation is never retried; timeouts, 5xx and unusable 200
// responses are indeterminate. Lalamove documents no idempotency key, so a
// non-empty IdempotencyKey is refused rather than silently ignored; reconcile with
// the order metadata "merchantOrderId" (set from MerchantOrderID) and GetOrder.
//
// Webhooks (LALA-05) are intentionally not implemented: neither
// courier.WebhookHandlerProvider nor courier.WebhookDecoder. Inbound webhooks
// stay disabled until signature authenticity and tenant attribution are proven
// for the configured account architecture.
package lalamove
