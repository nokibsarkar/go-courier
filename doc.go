// Package courier defines the canonical domain types and small, independent
// capability interfaces of the Universal Courier SDK.
//
// The package is capability-first: there is no mandatory Provider
// super-interface, registry or base implementation (SCP-01). A client
// implements only the interfaces it can faithfully support, for example a
// tracking-only adapter implements just [ShipmentTracker]. Interface
// satisfaction is structural, so adapters written outside this repository need only import this
// package.
//
// Money uses exact decimals (see [Money]); identifiers are opaque named string
// types; optional values are pointers so "absent" never collapses into zero.
// All failures are returned as *[Error] values compatible with errors.Is and
// errors.As.
package courier

// Version is the SDK version used in the default User-Agent.
const Version = "0.2.0"
