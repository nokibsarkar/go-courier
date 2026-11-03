package pathao

// Tests only talk to httptest servers on loopback.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/httpx"
)

var creds = Credentials{ClientID: "cid-1", ClientSecret: "csec-1", Username: "u1@example.test", Password: "pw-1", AccountID: "acct-1", DefaultStoreID: "777"}

func httpxRetry(n int) httpx.RetryPolicy {
	return httpx.RetryPolicy{MaxAttempts: n, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
}

// fake is a Pathao fixture server: it issues tokens and delegates the rest.
type fake struct {
	srv       *httptest.Server
	mu        sync.Mutex
	grants    []string // grant_type per token request
	tokenReqs []tokenRequest
	apiHits   atomic.Int32
	allHits   atomic.Int32
	expires   int
	// issue returns the access token for the n-th (1-based) token request.
	issue func(n int, tr tokenRequest) string
	api   func(w http.ResponseWriter, r *http.Request)
}

func newFake(t *testing.T, api func(w http.ResponseWriter, r *http.Request)) *fake {
	t.Helper()
	f := &fake{expires: 3600, api: api}
	f.issue = func(n int, _ tokenRequest) string { return "tok-" + string(rune('0'+n)) }
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.allHits.Add(1)
		if r.URL.Path == "/aladdin/api/v1/issue-token" {
			var tr tokenRequest
			_ = json.NewDecoder(r.Body).Decode(&tr)
			f.mu.Lock()
			f.grants = append(f.grants, tr.GrantType)
			f.tokenReqs = append(f.tokenReqs, tr)
			n := len(f.grants)
			exp := f.expires
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			b, _ := json.Marshal(map[string]any{"token_type": "Bearer", "expires_in": exp,
				"access_token": f.issue(n, tr), "refresh_token": "rt-" + tr.ClientID})
			_, _ = w.Write(b)
			return
		}
		f.apiHits.Add(1)
		f.api(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) grantList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.grants...)
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func readBody(r *http.Request) string { b, _ := io.ReadAll(r.Body); return string(b) }

func global(t *testing.T, url string, opts ...Option) *Client {
	t.Helper()
	c, err := New(append([]Option{WithCredentials(creds), WithBaseURL(url)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ref(kind, id string) courier.LocationRef {
	return courier.LocationRef{Provider: ProviderID, Kind: kind, ID: id}
}

func weight(g uint32) *uint32 { return &g }

func createReq(order, cod string) courier.CreateShipmentRequest {
	r := courier.CreateShipmentRequest{
		MerchantOrderID: courier.MerchantOrderID(order),
		Recipient: courier.Party{
			Contact: courier.Contact{Name: "Noki Rahman", Phone: "01711-111111"},
			Address: courier.Address{Text: "House 1, Road 2, Dhanmondi, Dhaka",
				Refs: []courier.LocationRef{ref("city", "1"), ref("zone", "298"), ref("area", "2104")}},
		},
		Package: courier.Package{Description: "book", Quantity: 2, WeightGrams: weight(1250)},
	}
	if cod != "" {
		m, _ := courier.ParseMoney(cod, courier.BDT)
		r.COD = &m
	}
	return r
}

const createOK = `{"message":"Order Created Successfully","type":"success","code":200,"data":{"consignment_id":"DL010224AB12CD","merchant_order_id":"ORD-1","order_status":"Pending","delivery_fee":60}}`
