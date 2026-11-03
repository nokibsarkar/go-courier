package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	courier "github.com/nokibsarkar/go-courier"
)

func decoder(key string) Decoder {
	return func(_ context.Context, h http.Header, body []byte) ([]courier.Event, error) {
		if !VerifyHMACSHA256([]byte(key), body, h.Get("X-Sig")) {
			return nil, ErrInvalidSignature("p")
		}
		return []courier.Event{{Provider: "p", Type: "a", Trust: courier.TrustVerified}, {Provider: "p", Type: "b", Trust: courier.TrustVerified}}, nil
	}
}

func do(h http.Handler, method, body, sig string) int {
	req := httptest.NewRequest(method, "/webhooks/p", strings.NewReader(body))
	req.Header.Set("X-Sig", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestHandlerVerifiesBeforeDispatch(t *testing.T) {
	var got []string
	h, err := NewHandler(Config{Provider: "p", Decode: decoder("k"), MaxBody: 64,
		Handler: courier.EventHandlerFunc(func(_ context.Context, e courier.Event) error { got = append(got, e.Type); return nil })})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"x":1}`
	if c := do(h, "POST", body, "deadbeef"); c != 401 || len(got) != 0 {
		t.Fatalf("bad sig: %d %v", c, got)
	}
	if c := do(h, "GET", body, ""); c != 405 {
		t.Fatalf("method: %d", c)
	}
	if c := do(h, "POST", strings.Repeat("a", 100), SignHMACSHA256([]byte("k"), []byte(body))); c != 413 {
		t.Fatalf("size: %d", c)
	}
	if c := do(h, "POST", body, SignHMACSHA256([]byte("k"), []byte(body))); c != 200 || len(got) != 2 {
		t.Fatalf("ok: %d %v", c, got)
	}
}

func TestHandlerFailsRequestWhenApplicationRejects(t *testing.T) {
	n := 0
	h, _ := NewHandler(Config{Provider: "p", Decode: decoder("k"),
		Handler: courier.EventHandlerFunc(func(context.Context, courier.Event) error {
			n++
			if n == 1 {
				return context.Canceled
			}
			return nil
		})})
	body := `{}`
	if c := do(h, "POST", body, SignHMACSHA256([]byte("k"), []byte(body))); c != 500 || n != 2 {
		t.Fatalf("code=%d n=%d (all events must still be attempted)", c, n)
	}
}

func TestNewHandlerRequiresParts(t *testing.T) {
	if _, err := NewHandler(Config{}); err == nil {
		t.Fatal("want error")
	}
	if VerifyHMACSHA256(nil, []byte("x"), "00") || VerifyHMACSHA256([]byte("k"), []byte("x"), "zz") {
		t.Fatal("must reject empty key / bad hex")
	}
}
