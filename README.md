# courier-go

A capability-based Go SDK for courier and delivery providers, built to the
*Universal Courier SDK — Software Requirements Specification v1.2*.

```bash
go get github.com/nokibsarkar/go-courier
```

Requires the Go version in `go.mod` (currently 1.27.3; kept on the latest release for security fixes). The only external dependency is
[`shopspring/decimal`](https://github.com/shopspring/decimal).

> **Status: pre-1.0.** The SRS gates are only partly met (see
> [PROVIDERS.md](PROVIDERS.md)): provider conformance is tested against
> fixtures, never live endpoints, and interface signatures are not yet frozen
> for a stable major release.

## Design in one minute

- **Capabilities, not a Provider.** The root package `courier` defines small,
  independent interfaces (`ShipmentTracker`, `ShipmentCreator`, `Quoter`,
  `BalanceReader`, `CustomerRiskReader`, …). There is no base class, registry or
  super-interface. A client implements what it can *faithfully* support, and
  adapters written outside this repository need only import `courier`.
- **Exact money.** `courier.Money` wraps `decimal.Decimal` + currency, defaults
  to **BDT**, serialises as `{"amount":"1250.75","currency":"BDT"}`, and refuses
  cross-currency arithmetic. Absent provider amounts are `nil`, never zero.
- **Strict credential modes.** Every adapter has two mutually exclusive
  constructors and never falls back between them:
  `New()` (global: typed options or `<PROVIDER>_*` env vars) and `NewTenant()`
  (credentials only from the request context). Violations fail *before* any
  outbound HTTP.
- **Honest errors.** One typed `*courier.Error` works with `errors.Is/As`
  (`courier.ErrCredentials`, `ErrRateLimit`, `ErrIndeterminate`, …). A timeout
  or 5xx on a state-changing call is **indeterminate** (it may have happened),
  mutations are never retried automatically, and an HTTP 200 carrying a business
  failure is an error.
- **No fabricated data.** Tracking results state history coverage
  (`full | partial | current_only | unknown`), preserve the exact provider
  status code and description as first-class data, leave missing timestamps
  `nil`, and canonicalize provider-specific statuses only toward the closest
  generic `Status`, never in the reverse direction, without discarding native
  values.
- **Quiet by default.** Optional `*slog.Logger` (no-op default, never
  `slog.Default()`). At info and above nothing but outcomes is logged. At
  **debug** level every HTTP exchange (and every received webhook) is logged
  with method, URL (query values masked), headers, status, duration and the
  request and response bodies, truncated at 8 KiB. Credentials, tokens,
  signatures and secret fields are masked at every level, but debug records
  contain customer names, phones and addresses: do not enable debug in
  production unless your log store may hold that data.

## Packages

| Package | Purpose |
|---|---|
| `courier` | Domain types, capability interfaces, errors, money |
| `courier/auth` | `Secret` (redacting), typed context credentials, global/tenant resolver, bounded token cache |
| `courier/httpx` | Shared transport: no redirects, bounded bodies/concurrency, protected headers, replay-safe-only retries |
| `courier/webhook` | Standard `http.Handler` plumbing + constant-time HMAC helpers |
| `courier/intelligence` | Fan-out over `CustomerRiskReader`s; per-source results, overlap-safe bounds, no merged scores |
| `courier/reporting` | Typed incident-report dispatcher: policy, mandatory authorizer, preflight, bounded fan-out |
| `courier/contracttest` | Public conformance helpers for independent adapters |
| `github.com/nokibsarkar/go-courier/provider/steadfast`, `github.com/nokibsarkar/go-courier/provider/ecourier`, `github.com/nokibsarkar/go-courier/provider/redx`, `github.com/nokibsarkar/go-courier/provider/carrybee`, `github.com/nokibsarkar/go-courier/provider/uber`, `github.com/nokibsarkar/go-courier/provider/pathao`, `github.com/nokibsarkar/go-courier/provider/lalamove` | Provider adapters ([capabilities and gaps](PROVIDERS.md)) |

## Verification states

Every adapter exports `Manifest()`, which reports each capability's state:
`verified_implemented`, `documented_not_implemented`, `unsupported` or
`unknown_unverified`, plus a list of behaviours that are still unverified.
"Verified" means implemented and tested against fixtures, never against a live
provider. [PROVIDERS.md](PROVIDERS.md) tabulates what each adapter implements
and its known gaps.

## Tracking only

```go
sf, err := steadfast.New() // STEADFAST_API_KEY / STEADFAST_SECRET_KEY
if err != nil { /* missing credentials fail here, with no network call */ }

var tracker courier.ShipmentTracker = sf
res, err := tracker.TrackShipment(ctx, courier.TrackingRequest{Code: "15BAEB8A"})
// res.ProviderStatus (raw), res.Status (canonical or StatusUnknown),
// res.Coverage (here: current_only), res.Events (none — never fabricated)
```

Multiple trackers can live in a plain `map[string]courier.ShipmentTracker`;
the SDK never probes or fans out across providers on its own.

## SaaS / multi-tenant

```go
c, _ := steadfast.NewTenant()          // holds no credentials at all
ctx = steadfast.ContextWithCredentials(ctx, steadfast.Credentials{
    APIKey: "…", SecretKey: "…", AccountID: "tenant-42",
})
sh, err := c.CreateShipment(ctx, req)
```

Credentials are snapshotted when attached to the context. Authorize
tenant → account use in your own code before attaching them. A global client
rejects context credentials (`courier.ErrAuthMode`) and a tenant client rejects
calls without them.

## Creating a shipment (and handling uncertainty)

```go
cod, _ := courier.ParseMoney("1250.50", courier.BDT)
sh, err := client.CreateShipment(ctx, courier.CreateShipmentRequest{
    MerchantOrderID: "ORD-1001",
    Recipient: courier.Party{
        Contact: courier.Contact{Name: "Rahim", Phone: "01711111111"},
        Address: courier.Address{Text: "Dhanmondi, Dhaka"},
    },
    COD: &cod,
})
if courier.IsIndeterminate(err) {
    // The call may have succeeded. Reconcile (e.g. TrackShipment by merchant
    // order ID) before replaying; the SDK will not retry for you.
}
```

Optional capabilities are discovered with ordinary type assertions:
`if b, ok := client.(courier.BatchShipmentCreator); ok { … }`.

## Webhooks

Adapters that support them implement `courier.WebhookHandlerProvider` and give
you a plain `http.Handler`; mount it anywhere:

```go
h, err := uberClient.WebhookHandler(courier.EventHandlerFunc(func(ctx context.Context, e courier.Event) error {
    // e.Trust == courier.TrustVerified; handlers must be idempotent.
    return nil
}))
mux.Handle("POST /webhooks/uber", h)
```

Signatures are verified over the original bytes before dispatch; an error from
your handler makes the HTTP response a 500 so the provider can redeliver. No
exactly-once delivery is promised.

## Reporting and risk data

`reporting.Dispatcher` fans a `CustomerIncidentReport` out to typed consumers
with a pluggable `Policy`, a **mandatory** `Authorizer` (registration alone never
authorizes disclosure), all-or-nothing preflight, bounded concurrency, per-consumer
receipts in order and an `errors.Join` aggregate. No reporting provider ships
because no documented submission API has been verified. `intelligence.Aggregator`
returns each source's score and counts untouched, plus min/max bounds that are
valid under unknown overlap — never a merged score, never an invented total.
Both packages treat phone numbers and incident text as personal data; you own
retention and legal basis (see SRS §10.3).

## Writing your own adapter

Implement only the interfaces you need and run the public conformance helpers:

```go
contracttest.RunShipmentTracker(t, contracttest.TrackerSuite{
    Tracker: myClient, ForbidCreation: true,
    Cases: []contracttest.TrackerCase{{Name: "delivered", Request: courier.TrackingRequest{Code: "X"}}},
})
```

Reuse `auth`, `httpx` and `webhook` if useful; none of it is `internal/`, and
none of it is required.

## Development

```bash
gofmt -l .                 # must print nothing
go vet ./...
go test -race ./...
govulncheck ./...          # also run in CI
```

`capability_api_test.go` freezes every published capability interface's method
set (SRS ARC-03 / QA-15). Add new functionality as a new optional interface.

Licensed under the terms in [LICENSE.md](LICENSE.md).
