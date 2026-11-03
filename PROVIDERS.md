# Provider coverage

Per SRS PROV-01. "Verified" here means *implemented and tested
against fixtures*, **not** tested against live provider endpoints: no adapter has
been exercised with real credentials (SRS release gate D is not claimed). Each
adapter's `Manifest()` is the machine-readable version of this table, including
per-capability states: `verified_implemented`, `documented_not_implemented`,
`unsupported`, `unknown_unverified`.

| Provider | Package | Implemented capabilities | Notable gaps |
|---|---|---|---|
| Steadfast | `provider/steadfast` | create, batch create (≤500, array body), track (current status), tracking history, return-status lookup, balance, customer score, pickup requests, return-request creation, payments (list and single), webhooks (bearer token plus HMAC-SHA256 signature, Idempotency-Key, all event types) | police-station and return-request read bodies are not described (decoded permissively, kept raw); cancel is not offered; extended bulk not used; timestamps without a timezone are kept as text |
| Uber Direct | `provider/uber` | quote, create (idempotency key forwarded, optional quote), get, track (current status + real waypoint outcomes), cancel, list deliveries, proof-of-delivery (native), webhook handler + decoder | all five webhook kinds are implemented (`event.delivery_status`, `event.courier_update`, `event.refund_request` via `ParseRefundRequest`, `event.shopping_progress` via `ParseShoppingProgress`, `direct.organization` via `ParseBillingPolicyUpdate`; courier coordinates stay in the raw payload); update delivery; find stores; free-text addresses (spec requires structured JSON-string address); COD |
| Pathao | `provider/pathao` | create, track (current only), quote, native store/city/zone/area listing | all URL paths and the response envelope unverified; webhooks, bulk, cancel, merchant-order lookup; normal-delivery parcels only |
| Lalamove | `provider/lalamove` | quote, get quote, create (quotation-first, one recipient), get, track (current only), cancel | **Bangladesh market support unverified** (BD is not in the supported market list); webhooks disabled (LALA-05); idempotency, history, batch unverified |
| eCourier | `provider/ecourier` | create order, track (full event history), cancel, label (URL), fraud-status check, packages, cities, thanas, areas, hub, payment status, child tracking | event timestamp time zone unknown (OccurredAt nil unless opted in); no webhooks; area-by-post-code, post-code list, branch list, child cancel not implemented; booster/OTP SMS out of scope |
| RedX | `provider/redx` | create parcel, track (info status + message history), cancel (accepted asynchronously), areas, pickup stores (create/list/get), charge calculator (native, not a Quoter), webhook handler + decoder (token in URL query, no signature) | error body shape; create field JSON types (table and curl sample disagree); weight unit; batch; quote; merchant-order tracking; other-property updates; no idempotency |
| Carrybee | `provider/carrybee` | create order, track (current status), cancel, location and store lists, webhooks (shared-secret header) | everything is inferred: delivery/product type codes, weight unit, `transfer_status` vocabulary, list and store-create shapes, bulk, webhook retry and ack; static-header auth, no token exchange |
| Sundarban | — | — | not implemented; real fixtures and authorization needed first |
| Fraud BD / REDX / Paperfly | — | — | not implemented; no incident-report API is verified (PROV-03) |

## Rules every adapter follows

- Global (`New`) and tenant (`NewTenant`) modes are exclusive; violations fail
  before any HTTP call.
- Mutations are never retried automatically. A timeout, 5xx or unusable 200 on a
  create or cancel is `courier.ErrIndeterminate`; reconcile before replaying.
- Unknown or ambiguous statuses become `StatusUnknown` with the raw value kept;
  missing timestamps stay `nil`; tracking declares its history coverage.
- Provider message text that may echo phone numbers or addresses is not copied
  into errors or logs.

## Before production use

1. Replace the sample-based fixtures with real, sanitised response fixtures and
   re-run the conformance tests.
2. Confirm account eligibility, market and service support (notably Lalamove in
   Bangladesh) and exercise the adapter in the provider's sandbox.
