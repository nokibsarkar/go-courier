// Package carrybee is the Carrybee courier adapter (Bangladesh).
//
// Capabilities: [courier.ShipmentCreator], [courier.ShipmentTracker] (current
// state from the details endpoint), [courier.ShipmentCanceller], webhooks, and
// native ListCities, ListZones, ListAreas, AreaSuggestion and ListStores.
// Not implemented: bulk creation, store creation, address details, quotes.
//
// Credentials are three static values sent as the headers Client-ID,
// Client-Secret and Client-Context; there is no token exchange. Two mutually
// exclusive modes exist and never fall back:
//
//	global, err := carrybee.New(carrybee.WithSandbox())  // WithCredentials or CARRYBEE_* env
//	tenant, err := carrybee.NewTenant(carrybee.WithSandbox())
//
// The environment is always an explicit choice and is never inferred.
//
// Adapter-specific behaviour (PROV-02):
//   - A 2xx body that states a failure (error set, status false, non-empty
//     errors) is a rejection, never a success.
//   - Store, city and zone must be explicit [courier.LocationRef] entries
//     (Provider "carrybee", Kind "store" on the pickup address, "city" and
//     "zone" on the recipient, "area" optional). The adapter never matches free
//     text to a location; use the List* and AreaSuggestion helpers for IDs.
//   - Weight (grams) and quantity are required and never defaulted; COD is
//     whole taka and fractions are rejected, never rounded.
//   - A create that times out, returns 5xx or unusable JSON is indeterminate
//     and is never replayed.
//   - Webhooks use a shared secret in a header, not an HMAC: they authenticate
//     the sender but give no body integrity. Configure one secret per account
//     with [WithWebhookSecrets]; with none the handler is not offered.
package carrybee
