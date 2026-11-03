package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

type creds struct {
	Key     Secret
	Headers map[string]string
}

func (c creds) Clone() creds {
	h := make(map[string]string, len(c.Headers))
	for k, v := range c.Headers {
		h[k] = v
	}
	return creds{Key: c.Key, Headers: h}
}

var testKey = NewKey[creds]()

func TestSecretNeverPrints(t *testing.T) {
	s := Secret("hunter2")
	var sb strings.Builder
	l := slog.New(slog.NewTextHandler(&sb, nil))
	l.Info("x", "s", s, "c", creds{Key: s})
	out := fmt.Sprintf("%v %+v %#v %s", s, creds{Key: s}, s, s) + sb.String()
	if strings.Contains(out, "hunter2") {
		t.Fatalf("secret leaked: %s", out)
	}
}

func TestResolverModesAreExclusive(t *testing.T) {
	global := NewGlobalResolver("p", testKey, creds{Key: "g"})
	tenant := NewTenantResolver("p", testKey)

	if c, err := global.Resolve(context.Background()); err != nil || c.Key != "g" {
		t.Fatal(c, err)
	}
	tctx := testKey.With(context.Background(), creds{Key: "t"})
	if _, err := global.Resolve(tctx); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("global must reject ctx creds: %v", err)
	}
	if _, err := tenant.Resolve(context.Background()); !errors.Is(err, courier.ErrAuthMode) {
		t.Fatalf("tenant must require ctx creds: %v", err)
	}
	if c, err := tenant.Resolve(tctx); err != nil || c.Key != "t" {
		t.Fatal(c, err)
	}
}

func TestContextCredentialsAreSnapshotted(t *testing.T) {
	in := creds{Key: "k", Headers: map[string]string{"A": "1"}}
	ctx := testKey.With(context.Background(), in)
	in.Headers["A"] = "mutated"
	got, _ := testKey.From(ctx)
	if got.Headers["A"] != "1" {
		t.Fatal("caller mutation leaked into SDK")
	}
	got.Headers["A"] = "x"
	again, _ := testKey.From(ctx)
	if again.Headers["A"] != "1" {
		t.Fatal("returned value shares state")
	}
}

func TestIdentityIsUnambiguous(t *testing.T) {
	if Identity("ab", "c") == Identity("a", "bc") {
		t.Fatal("collision")
	}
}

func TestTokenCacheSingleFlightPerIdentity(t *testing.T) {
	c := NewTokenCache()
	var calls atomic.Int32
	fetch := func(context.Context) (Token, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return Token{Value: "tok", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := c.Get(context.Background(), "id", fetch); err != nil || v != "tok" {
				t.Error(v, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fetch called %d times", calls.Load())
	}
}

func TestTokenCacheIsolationExpiryAndBound(t *testing.T) {
	now := time.Now()
	c := NewTokenCache(WithMaxEntries(2), WithClock(func() time.Time { return now }))
	get := func(id, val string) Secret {
		v, err := c.Get(context.Background(), id, func(context.Context) (Token, error) {
			return Token{Value: Secret(val), ExpiresAt: now.Add(10 * time.Minute)}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if get("a", "A") != "A" || get("b", "B") != "B" || get("a", "X") != "A" {
		t.Fatal("identities must not share tokens")
	}
	get("c", "C")
	if c.Len() > 2 {
		t.Fatalf("unbounded: %d", c.Len())
	}
	now = now.Add(20 * time.Minute)
	if get("a", "A2") != "A2" {
		t.Fatal("expired token reused")
	}
	// failed fetch not cached
	_, err := c.Get(context.Background(), "z", func(context.Context) (Token, error) { return Token{}, errors.New("boom") })
	if err == nil {
		t.Fatal("want error")
	}
	// invalidate only drops the rejected token
	c.Invalidate("a", "other")
	if get("a", "A3") != "A2" {
		t.Fatal("invalidate dropped a different token")
	}
	c.Invalidate("a", "A2")
	if get("a", "A3") != "A3" {
		t.Fatal("rejected token not dropped")
	}
}
