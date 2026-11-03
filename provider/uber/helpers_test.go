package uber

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	courier "github.com/nokibsarkar/go-courier"
)

// All fixtures in this package are derived from public docs, not captured
// from production.

var creds = Credentials{ClientID: "cid-1", ClientSecret: "sec-1", CustomerID: "cust-1"}

// fake is an httptest Uber: /oauth/v2/token plus /v1/customers/{id}/...
type fake struct {
	srv        *httptest.Server
	tokenHits  atomic.Int32
	apiHits    atomic.Int32
	mu         sync.Mutex
	violations []string
	// rejectTokens: first N API calls answer 401.
	reject401 atomic.Int32
	api       func(w http.ResponseWriter, r *http.Request, body string)
	tokenSeq  atomic.Int32
	// tokenFor derives the token handed to a client id.
	lastForm map[string]string
}

func (f *fake) violate(format string, a ...any) {
	f.mu.Lock()
	f.violations = append(f.violations, fmt.Sprintf(format, a...))
	f.mu.Unlock()
}

func newFake(t *testing.T, api func(w http.ResponseWriter, r *http.Request, body string)) *fake {
	t.Helper()
	f := &fake{api: api}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenHits.Add(1)
		_ = r.ParseForm()
		f.mu.Lock()
		f.lastForm = map[string]string{}
		for k := range r.PostForm {
			f.lastForm[k] = r.PostForm.Get(k)
		}
		f.mu.Unlock()
		if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			f.violate("token request method/content-type")
		}
		if r.PostForm.Get("client_secret") != "sec-"+strings.TrimPrefix(r.PostForm.Get("client_id"), "cid-") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		n := f.tokenSeq.Add(1)
		_, _ = fmt.Fprintf(w, `{"access_token":"tok-%s-%d","expires_in":2592000,"token_type":"Bearer","scope":"eats.deliveries"}`, r.PostForm.Get("client_id"), n)
	})
	mux.HandleFunc("/v1/customers/", func(w http.ResponseWriter, r *http.Request) {
		f.apiHits.Add(1)
		if f.reject401.Load() > 0 && f.reject401.Add(-1) >= 0 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		f.api(w, r, string(raw))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) opts(extra ...Option) []Option {
	return append([]Option{WithBaseURL(f.srv.URL + "/v1"), WithAuthURL(f.srv.URL + "/oauth/v2/token")}, extra...)
}

func (f *fake) global(t *testing.T, extra ...Option) *Client {
	t.Helper()
	c, err := New(f.opts(append([]Option{WithCredentials(creds)}, extra...)...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fake) tenant(t *testing.T, extra ...Option) *Client {
	t.Helper()
	c, err := NewTenant(f.opts(extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func fastRetry() Option { return WithRetry(httpxRetry(3)) }

func structuredParty(name, phone string) courier.Party {
	return courier.Party{
		Contact: courier.Contact{Name: name, Phone: phone},
		Address: courier.Address{Line1: "100 Maiden Ln", City: "New York", State: "NY", PostalCode: "10023", Country: "US"},
	}
}

func createReq() courier.CreateShipmentRequest {
	return courier.CreateShipmentRequest{
		MerchantOrderID: "ORD-1",
		Pickup:          structuredParty("Store", "+1 415 555 0100"),
		Recipient:       structuredParty("Ana", "+14155550123"),
		Package:         courier.Package{Description: "Pizza", Quantity: 2},
	}
}

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad json %q: %v", s, err)
	}
	return m
}
