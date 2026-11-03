// Package auth provides reusable credential utilities for adapters: redacting
// secrets, typed per-adapter context credentials, the global/tenant mode
// resolver and a bounded, concurrency-safe token cache.
//
// Adapters are not required to use this package (AUTH-08); it exists so that
// the exclusive global-or-tenant rule is implemented once and correctly.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"

	courier "github.com/nokibsarkar/go-courier"
)

// Mode is the immutable credential mode of a client (AUTH-01).
type Mode int

const (
	// ModeGlobal clients use credentials fixed at construction (typed options
	// or provider-prefixed environment variables). They reject context credentials.
	ModeGlobal Mode = iota + 1
	// ModeTenant clients use only credentials carried by each call's context.
	ModeTenant
)

func (m Mode) String() string {
	switch m {
	case ModeGlobal:
		return "global"
	case ModeTenant:
		return "tenant"
	}
	return "invalid"
}

// Secret is a string that never prints itself.
type Secret string

// Reveal returns the secret value. Use only when attaching to a request.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is empty (after trimming).
func (s Secret) IsZero() bool { return strings.TrimSpace(string(s)) == "" }

func (Secret) String() string               { return "[REDACTED]" }
func (Secret) GoString() string             { return `"[REDACTED]"` }
func (Secret) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }
func (Secret) LogValue() slog.Value         { return slog.StringValue("[REDACTED]") }

// Identity derives a stable, non-reversible credential identity from parts
// (for token-cache keys and diagnostics). Parts are length-prefixed so
// ("ab","c") and ("a","bc") differ.
func Identity(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var n [8]byte
		l := uint64(len(p))
		for i := 0; i < 8; i++ {
			n[i] = byte(l >> (8 * i))
		}
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Cloner is implemented by credential structs so that mutable contents
// (maps, slices) are snapshotted before SDK ownership (AUTH-05).
type Cloner[T any] interface{ Clone() T }

type ctxKey struct{ k *byte }

// Key is a typed context key for one adapter's credential type.
type Key[T Cloner[T]] struct{ id *byte }

// NewKey returns a new unique key. Create one per adapter at package level.
func NewKey[T Cloner[T]]() Key[T] { return Key[T]{id: new(byte)} }

// With stores a snapshot of v in ctx.
func (k Key[T]) With(ctx context.Context, v T) context.Context {
	return context.WithValue(ctx, ctxKey{k.id}, v.Clone())
}

// From returns a snapshot of the credentials in ctx.
func (k Key[T]) From(ctx context.Context) (T, bool) {
	v, ok := ctx.Value(ctxKey{k.id}).(T)
	if !ok {
		var zero T
		return zero, false
	}
	return v.Clone(), true
}

// Resolver enforces the exclusive mode rule for one client.
type Resolver[T Cloner[T]] struct {
	provider courier.ProviderID
	mode     Mode
	global   T
	key      Key[T]
}

// NewGlobalResolver builds a global-mode resolver holding a snapshot of creds.
func NewGlobalResolver[T Cloner[T]](p courier.ProviderID, key Key[T], creds T) *Resolver[T] {
	return &Resolver[T]{provider: p, mode: ModeGlobal, global: creds.Clone(), key: key}
}

// NewTenantResolver builds a tenant-mode resolver; it holds no credentials.
func NewTenantResolver[T Cloner[T]](p courier.ProviderID, key Key[T]) *Resolver[T] {
	return &Resolver[T]{provider: p, mode: ModeTenant, key: key}
}

// Mode returns the immutable mode.
func (r *Resolver[T]) Mode() Mode { return r.mode }

// Resolve returns the credentials for this call, or an auth-mode error before
// any outbound dispatch (AUTH-02, QA-04). There is no fallback in either
// direction.
func (r *Resolver[T]) Resolve(ctx context.Context) (T, error) {
	var zero T
	ctxCreds, has := r.key.From(ctx)
	switch r.mode {
	case ModeGlobal:
		if has {
			return zero, &courier.Error{Kind: courier.KindAuthMode, Provider: r.provider,
				Message: "global-mode client rejects request-context credentials; use the tenant constructor"}
		}
		return r.global.Clone(), nil
	case ModeTenant:
		if !has {
			return zero, &courier.Error{Kind: courier.KindAuthMode, Provider: r.provider,
				Message: "tenant-mode client requires request-context credentials"}
		}
		return ctxCreds, nil
	}
	return zero, courier.Configuration(r.provider, "invalid credential mode")
}
