// Package pathao is the Pathao Courier merchant API adapter (Bangladesh).
//
// Capabilities: [courier.ShipmentCreator], [courier.ShipmentTracker] (via the
// order info endpoint, current status only) and [courier.Quoter] (price plan),
// plus provider-native ListStores, ListCities, ListZones and ListAreas.
// Not implemented: bulk creation, cancellation, reading, labels, returns and
// webhooks (webhook signature/account attribution is unverified, so no
// WebhookHandlerProvider is advertised).
//
// Two mutually exclusive credential modes exist and never fall back:
//
//	global, err := pathao.New(pathao.WithSandbox())   // WithCredentials or PATHAO_* env
//	tenant, err := pathao.NewTenant()                  // pathao.ContextWithCredentials(ctx, ...)
//
// The environment (production or sandbox) is always an explicit choice: the
// default is production, [WithSandbox] selects the sandbox, and it is never
// inferred from credentials. There are no default credentials.
//
// Adapter-specific behaviour (PROV-02):
//   - Authentication is an OAuth password grant; the bearer token is cached per
//     credential identity (all credential fields plus base URL) and refreshed
//     with the refresh token. A 401 invalidates exactly the rejected token and
//     re-issues once; the call is then retried once (401 means not processed).
//   - Responses are enveloped; an HTTP 200 with type "error" or a code other
//     than 200/201/202 is an error, never a success.
//   - City, zone and area must be explicit [courier.LocationRef] entries on the
//     recipient address (Provider "pathao", Kind "city"/"zone"/"area", numeric
//     ID). The adapter never matches free text to a location; use ListCities,
//     ListZones and ListAreas to obtain IDs.
//   - The price plan call is a POST read: Mutation=false but not replay-safe.
//   - A create that times out, returns 5xx or returns unusable JSON is
//     indeterminate. Reconcile before replaying; no merchant-order lookup
//     endpoint is verified, so tracking by merchant order ID is unsupported.
package pathao
