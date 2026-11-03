package courier

import (
	"context"
	"encoding/json"
	"time"
)

// Trust states whether an event's authenticity and account attribution were
// cryptographically verified (HOOK-03, HOOK-05).
type Trust string

const (
	// TrustVerified: signature verified and account attribution established.
	TrustVerified Trust = "verified"
	// TrustUntrusted: authenticity or account could not be established; the
	// application must corroborate (for example by re-fetching) before acting.
	TrustUntrusted Trust = "untrusted"
)

// Event is a provider webhook event with raw type and payload preserved.
type Event struct {
	Provider ProviderID
	// Account is empty when attribution was not established.
	Account    AccountID
	Trust      Trust
	ID         string
	Type       string // provider-native event type
	OccurredAt *time.Time
	// Tracking is set when the event carries a status update the adapter could
	// map; its Raw field and Payload retain the original data.
	Tracking *TrackingResult
	Payload  json.RawMessage
}

// EventHandler receives verified events. Returning an error makes the HTTP
// handler fail the request so the provider may redeliver. Delivery is
// at-least-once at best; handlers must be idempotent (HOOK-04).
type EventHandler interface {
	HandleEvent(context.Context, Event) error
}

// EventHandlerFunc adapts a function to EventHandler.
type EventHandlerFunc func(context.Context, Event) error

// HandleEvent implements EventHandler.
func (f EventHandlerFunc) HandleEvent(ctx context.Context, e Event) error { return f(ctx, e) }
