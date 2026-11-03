package courier

import (
	"errors"
	"fmt"
	"time"
)

// Kind classifies a failure in a provider-neutral way (ERR-01).
type Kind string

const (
	KindValidation    Kind = "validation"     // invalid/unsupported input; nothing was sent
	KindConfiguration Kind = "configuration"  // invalid or missing client configuration
	KindAuthMode      Kind = "auth_mode"      // global/tenant credential mode violation
	KindCredentials   Kind = "credentials"    // credentials rejected by the provider
	KindPermission    Kind = "permission"     // authenticated but not permitted
	KindUnsupported   Kind = "unsupported"    // capability/reference not supported or unverified
	KindNotFound      Kind = "not_found"      // resource does not exist
	KindAmbiguous     Kind = "ambiguous"      // input/result cannot be resolved uniquely
	KindConflict      Kind = "conflict"       // state conflict / duplicate
	KindRateLimit     Kind = "rate_limit"     // provider throttled the request
	KindProtocol      Kind = "protocol"       // malformed or nonconforming response
	KindResourceLimit Kind = "resource_limit" // SDK-side size/concurrency bound exceeded
	KindUnavailable   Kind = "unavailable"    // transport failure or provider outage on a safe operation
	KindIndeterminate Kind = "indeterminate"  // a mutation may or may not have happened (ERR-02)
	KindRejected      Kind = "rejected"       // provider confirmed rejection of the request
)

// Outcome says whether a failed mutation is known to have had no effect.
type Outcome string

const (
	// OutcomeUnknown is the zero value: not applicable (reads, local validation).
	OutcomeUnknown Outcome = ""
	// OutcomeRejected means the provider confirmed the request was not applied.
	OutcomeRejected Outcome = "rejected"
	// OutcomeIndeterminate means success cannot be excluded; callers must
	// reconcile before replaying (ERR-02, ERR-03).
	OutcomeIndeterminate Outcome = "indeterminate"
)

// Error is the single typed error returned by the SDK and maintained adapters.
type Error struct {
	Kind       Kind
	Op         string
	Provider   ProviderID
	Message    string
	Field      string
	StatusCode int
	// Retryable reports that the failure is transient. It does NOT mean a
	// mutation is safe to replay; see Outcome.
	Retryable  bool
	RetryAfter time.Duration
	Outcome    Outcome
	// ProviderCode is the provider-native error code, when one was supplied.
	ProviderCode string
	Err          error
}

// Sentinels for errors.Is(err, courier.ErrX); they match on Kind only.
var (
	ErrValidation    = &Error{Kind: KindValidation}
	ErrConfiguration = &Error{Kind: KindConfiguration}
	ErrAuthMode      = &Error{Kind: KindAuthMode}
	ErrCredentials   = &Error{Kind: KindCredentials}
	ErrPermission    = &Error{Kind: KindPermission}
	ErrUnsupported   = &Error{Kind: KindUnsupported}
	ErrNotFound      = &Error{Kind: KindNotFound}
	ErrAmbiguous     = &Error{Kind: KindAmbiguous}
	ErrConflict      = &Error{Kind: KindConflict}
	ErrRateLimit     = &Error{Kind: KindRateLimit}
	ErrProtocol      = &Error{Kind: KindProtocol}
	ErrResourceLimit = &Error{Kind: KindResourceLimit}
	ErrUnavailable   = &Error{Kind: KindUnavailable}
	ErrIndeterminate = &Error{Kind: KindIndeterminate}
	ErrRejected      = &Error{Kind: KindRejected}
)

// ErrCurrencyMismatch is wrapped by validation errors from Money arithmetic.
var ErrCurrencyMismatch = errors.New("currency mismatch")

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := string(e.Kind)
	if e.Provider != "" {
		s = string(e.Provider) + ": " + s
	}
	if e.Op != "" {
		s += " (" + e.Op + ")"
	}
	if e.Field != "" {
		s += fmt.Sprintf(" field %q", e.Field)
	}
	if e.StatusCode != 0 {
		s += fmt.Sprintf(" status %d", e.StatusCode)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is lets a bare-Kind sentinel match any *Error of the same kind.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || e == nil || t == nil {
		return false
	}
	bare := t.Op == "" && t.Provider == "" && t.Message == "" && t.Field == "" && t.StatusCode == 0 && t.Err == nil
	return bare && t.Kind == e.Kind
}

// IsKind reports whether err (or anything it wraps) is a *Error of kind.
func IsKind(err error, kind Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == kind
}

// IsIndeterminate reports whether err leaves a mutation's outcome unknown.
func IsIndeterminate(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Kind == KindIndeterminate || e.Outcome == OutcomeIndeterminate)
}

// Validation builds a validation error for a named field.
func Validation(field, message string) *Error {
	return &Error{Kind: KindValidation, Field: field, Message: message}
}

// Unsupported builds an unsupported-capability error.
func Unsupported(provider ProviderID, op, message string) *Error {
	return &Error{Kind: KindUnsupported, Provider: provider, Op: op, Message: message}
}

// Configuration builds a configuration error.
func Configuration(provider ProviderID, message string) *Error {
	return &Error{Kind: KindConfiguration, Provider: provider, Message: message}
}
