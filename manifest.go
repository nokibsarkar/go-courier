package courier

import "time"

// Verification is the evidence state of a provider capability (PROV-01).
type Verification string

const (
	VerifiedImplemented      Verification = "verified_implemented"
	DocumentedNotImplemented Verification = "documented_not_implemented"
	ProviderUnsupported      Verification = "unsupported"
	UnknownUnverified        Verification = "unknown_unverified"
)

// SourceManifest reports an adapter's capability and verification state
// (ARC-05). Maintained adapters export one via a Manifest function.
type SourceManifest struct {
	Provider ProviderID
	// Document, URL and Fingerprint are optional descriptive metadata.
	Document string
	URL      string
	// Version is the upstream protocol/API version when published.
	Version    string
	VerifiedOn time.Time
	// Fingerprint identifies the schema/fixture set used (e.g. fixture names).
	Fingerprint string
	// Capabilities maps capability name to its verification state.
	Capabilities map[string]Verification
	// Unverified lists upstream methods that are unsupported or unverified.
	Unverified []string
	Notes      string
}
