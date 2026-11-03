package lalamove

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/httpx"
)

// ProviderID is the canonical provider identity.
const ProviderID courier.ProviderID = "lalamove"

// Documented base URLs (verified 2026-11-03).
const (
	SandboxBaseURL    = "https://rest.sandbox.lalamove.com/v3"
	ProductionBaseURL = "https://rest.lalamove.com/v3"
)

type environment string

const (
	envSandbox    environment = "sandbox"
	envProduction environment = "production"
	envCustom     environment = "custom" // WithBaseURL: key-prefix check skipped
)

var (
	marketPattern = regexp.MustCompile(`^[A-Z]{2}$`)
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	codePattern   = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)
)

// Credentials are the typed Lalamove credentials. Market is REQUIRED and has no
// default: it is the two-letter upper-case Lalamove market code (for example
// "SG"). AccountID is an optional application-chosen opaque label used in
// ShipmentRef.Account; it is never sent to Lalamove.
type Credentials struct {
	APIKey    string
	APISecret auth.Secret
	Market    string
	AccountID courier.AccountID
}

// Clone implements auth.Cloner.
func (c Credentials) Clone() Credentials { return c }

func (c Credentials) validate(env environment) error {
	if strings.TrimSpace(c.APIKey) == "" || c.APISecret.IsZero() {
		return courier.Configuration(ProviderID, "api key and api secret are required")
	}
	if strings.ContainsAny(c.APIKey, ": \t\r\n") || strings.ContainsAny(c.APISecret.Reveal(), "\r\n") {
		return courier.Configuration(ProviderID, "api key or secret contains invalid characters")
	}
	if !marketPattern.MatchString(c.Market) {
		return courier.Configuration(ProviderID, "market is required and must be a two-letter upper-case code; there is no default")
	}
	switch env {
	case envSandbox:
		if !strings.HasPrefix(c.APIKey, "pk_test_") || !strings.HasPrefix(c.APISecret.Reveal(), "sk_test_") {
			return courier.Configuration(ProviderID, "credentials do not belong to the sandbox environment (expected pk_test_/sk_test_ prefixes)")
		}
	case envProduction:
		if !strings.HasPrefix(c.APIKey, "pk_prod_") || !strings.HasPrefix(c.APISecret.Reveal(), "sk_prod_") {
			return courier.Configuration(ProviderID, "credentials do not belong to the production environment (expected pk_prod_/sk_prod_ prefixes)")
		}
	}
	return nil
}

var credKey = auth.NewKey[Credentials]()

// ContextWithCredentials attaches tenant credentials to ctx for a tenant-mode
// client. A global-mode client rejects them (AUTH-02).
func ContextWithCredentials(ctx context.Context, c Credentials) context.Context {
	return credKey.With(ctx, c)
}

type settings struct {
	creds      *Credentials
	sandbox    bool
	production bool
	baseURL    string
	hc         *http.Client
	logger     *slog.Logger
	userAgent  string
	maxBody    int64
	maxConc    int
	retry      *httpx.RetryPolicy
	clock      func() time.Time
}

// Option configures New / NewTenant.
type Option func(*settings)

// WithCredentials supplies explicit global credentials (global mode only).
func WithCredentials(c Credentials) Option { return func(s *settings) { s.creds = &c } }

// WithSandbox selects the sandbox environment (sandbox keys required).
func WithSandbox() Option { return func(s *settings) { s.sandbox = true } }

// WithProduction selects the production environment (production keys required).
func WithProduction() Option { return func(s *settings) { s.production = true } }

// WithBaseURL overrides the API base URL including the "/v3" prefix (tests,
// proxies). It counts as an explicit environment choice; the key-prefix check is
// skipped. The path of this URL is what gets signed.
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

// WithHTTPClient injects a trusted HTTP client (explicit extension boundary).
func WithHTTPClient(c *http.Client) Option { return func(s *settings) { s.hc = c } }

// WithLogger sets an optional logger; the default is no-op.
func WithLogger(l *slog.Logger) Option { return func(s *settings) { s.logger = l } }

// WithUserAgent overrides the User-Agent.
func WithUserAgent(ua string) Option { return func(s *settings) { s.userAgent = ua } }

// WithMaxResponseBytes bounds response bodies.
func WithMaxResponseBytes(n int64) Option { return func(s *settings) { s.maxBody = n } }

// WithMaxConcurrent bounds in-flight requests.
func WithMaxConcurrent(n int) Option { return func(s *settings) { s.maxConc = n } }

// WithRetry sets the retry policy for replay-safe GETs. Mutations (order create,
// cancel) and quotation creation are never retried automatically.
func WithRetry(p httpx.RetryPolicy) Option { return func(s *settings) { s.retry = &p } }

// WithClock injects the time source used for signature timestamps and quote
// expiry checks (tests).
func WithClock(f func() time.Time) Option { return func(s *settings) { s.clock = f } }

// Client is the Lalamove adapter. It is safe for concurrent use.
type Client struct {
	http     *httpx.Client
	creds    *auth.Resolver[Credentials]
	baseURL  string
	basePath string
	env      environment
	retry    httpx.RetryPolicy
	now      func() time.Time
}

var (
	_ courier.Quoter            = (*Client)(nil)
	_ courier.QuoteReader       = (*Client)(nil)
	_ courier.ShipmentCreator   = (*Client)(nil)
	_ courier.ShipmentReader    = (*Client)(nil)
	_ courier.ShipmentCanceller = (*Client)(nil)
	_ courier.ShipmentTracker   = (*Client)(nil)
)

func build(mode auth.Mode, opts []Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if s.sandbox && s.production {
		return nil, courier.Configuration(ProviderID, "WithSandbox and WithProduction are mutually exclusive")
	}
	c := &Client{now: s.clock}
	if c.now == nil {
		c.now = time.Now
	}
	switch {
	case strings.TrimSpace(s.baseURL) != "":
		c.baseURL, c.env = strings.TrimRight(strings.TrimSpace(s.baseURL), "/"), envCustom
	case s.sandbox:
		c.baseURL, c.env = SandboxBaseURL, envSandbox
	case s.production:
		c.baseURL, c.env = ProductionBaseURL, envProduction
	default:
		return nil, courier.Configuration(ProviderID, "an explicit environment is required: WithSandbox, WithProduction or WithBaseURL")
	}
	u, err := url.Parse(c.baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" {
		return nil, courier.Configuration(ProviderID, "invalid base URL")
	}
	c.basePath = strings.TrimRight(u.Path, "/")
	c.retry = httpx.RetryPolicy{MaxAttempts: 3}
	if s.retry != nil {
		c.retry = *s.retry
	}
	if c.retry.BaseDelay <= 0 {
		c.retry.BaseDelay = 200 * time.Millisecond
	}
	if c.retry.MaxDelay <= 0 {
		c.retry.MaxDelay = 5 * time.Second
	}
	// httpx retries are disabled on purpose: they would replay the same
	// signature and Request-ID. The adapter retries GETs itself.
	hc, err := httpx.New(httpx.Config{Provider: ProviderID, HTTPClient: s.hc, UserAgent: s.userAgent,
		MaxResponseBytes: s.maxBody, MaxConcurrent: s.maxConc, Logger: s.logger})
	if err != nil {
		return nil, err
	}
	c.http = hc
	switch mode {
	case auth.ModeGlobal:
		var creds Credentials
		if s.creds != nil {
			creds = *s.creds
		} else {
			creds = Credentials{
				APIKey:    strings.TrimSpace(os.Getenv("LALAMOVE_API_KEY")),
				APISecret: auth.Secret(strings.TrimSpace(os.Getenv("LALAMOVE_API_SECRET"))),
				Market:    strings.TrimSpace(os.Getenv("LALAMOVE_MARKET")),
				AccountID: courier.AccountID(os.Getenv("LALAMOVE_ACCOUNT_ID")),
			}
		}
		if err := creds.validate(c.env); err != nil { // AUTH-07, LALA-06: fail locally
			return nil, err
		}
		c.creds = auth.NewGlobalResolver(ProviderID, credKey, creds)
	case auth.ModeTenant:
		if s.creds != nil {
			return nil, courier.Configuration(ProviderID, "tenant-mode clients take credentials from the request context only")
		}
		c.creds = auth.NewTenantResolver(ProviderID, credKey)
	}
	return c, nil
}

// New creates a global-mode client. Credentials come from WithCredentials or the
// LALAMOVE_API_KEY / LALAMOVE_API_SECRET / LALAMOVE_MARKET environment variables;
// construction fails if any is missing. An environment option is required. No
// network call is made.
func New(opts ...Option) (*Client, error) { return build(auth.ModeGlobal, opts) }

// NewTenant creates a tenant-mode client that never reads environment or global
// credentials; every call needs ContextWithCredentials.
func NewTenant(opts ...Option) (*Client, error) { return build(auth.ModeTenant, opts) }

// Mode reports the client's immutable credential mode.
func (c *Client) Mode() auth.Mode { return c.creds.Mode() }

// errMsg extracts only the provider error ID. Provider message text is dropped
// because it can echo personal data (PRIV-01).
func errMsg(_ int, body []byte) (string, string) {
	var e errorEnvelope
	if json.Unmarshal(body, &e) != nil || len(e.Errors) == 0 {
		return "", ""
	}
	id := strings.TrimSpace(string(e.Errors[0].ID))
	if !codePattern.MatchString(id) {
		return "", ""
	}
	return id, ""
}

func (c *Client) attempt(ctx context.Context, creds Credentials, op, method, endpoint string, body []byte, mutation bool) (*httpx.Response, error) {
	id, err := newRequestID()
	if err != nil {
		return nil, &courier.Error{Kind: courier.KindConfiguration, Provider: ProviderID, Op: op, Message: "cannot generate request id"}
	}
	ts := strconv.FormatInt(c.now().UnixMilli(), 10)
	sig := sign(creds.APISecret.Reveal(), ts, method, c.basePath+endpoint, body)
	return c.http.Do(ctx, httpx.Request{
		Op: op, Method: method, URL: c.baseURL + endpoint, Body: body, Mutation: mutation, ErrorMessage: errMsg,
		Auth: http.Header{
			"Authorization": {"hmac " + creds.APIKey + ":" + ts + ":" + sig},
			"Market":        {creds.Market},
			"Request-Id":    {id},
		},
	})
}

// call resolves and validates credentials before any dispatch, serialises the
// body exactly once (the same bytes are signed and sent) and performs the
// request. Only GETs are retried, each attempt with a fresh signature and nonce.
func (c *Client) call(ctx context.Context, op, method, endpoint string, payload any, mutation bool) (*httpx.Response, Credentials, error) {
	creds, err := c.creds.Resolve(ctx)
	if err != nil {
		return nil, creds, err
	}
	if err := creds.validate(c.env); err != nil {
		return nil, creds, err
	}
	var body []byte
	if payload != nil {
		if body, err = json.Marshal(payload); err != nil {
			return nil, creds, courier.Validation("body", "cannot encode request")
		}
	}
	attempts := 1
	if method == http.MethodGet && !mutation && c.retry.MaxAttempts > 1 {
		attempts = c.retry.MaxAttempts
	}
	var resp *httpx.Response
	for i := 0; i < attempts; i++ {
		if i > 0 {
			t := time.NewTimer(c.backoff(i, err))
			select {
			case <-ctx.Done():
				t.Stop()
				return resp, creds, err
			case <-t.C:
			}
		}
		resp, err = c.attempt(ctx, creds, op, method, endpoint, body, mutation)
		if err == nil {
			return resp, creds, nil
		}
		var ce *courier.Error
		if !errors.As(err, &ce) || !ce.Retryable || ctx.Err() != nil {
			break
		}
	}
	return resp, creds, err
}

func (c *Client) backoff(attempt int, last error) time.Duration {
	var ce *courier.Error
	if errors.As(last, &ce) && ce.RetryAfter > 0 {
		return min(ce.RetryAfter, c.retry.MaxDelay)
	}
	d := c.retry.BaseDelay << (attempt - 1)
	if d <= 0 || d > c.retry.MaxDelay {
		d = c.retry.MaxDelay
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func asIndeterminate(err error) error {
	return &courier.Error{Kind: courier.KindIndeterminate, Provider: ProviderID, Outcome: courier.OutcomeIndeterminate,
		Message: "unusable response to a state-changing request; the operation may have been applied", Err: err}
}

func protocol(op, msg string) error {
	return &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: op, Message: msg}
}

func checkID(field, v string) error {
	if !idPattern.MatchString(v) {
		return courier.Validation(field, "invalid identifier")
	}
	return nil
}

// QuoteOptions are the Lalamove-specific quotation inputs that the canonical
// QuoteRequest cannot carry. ServiceType (for example "MOTORCYCLE") and Language
// are required and never defaulted.
type QuoteOptions struct {
	ServiceType     string
	Language        string
	SpecialRequests []string
}

type quoteOptKey struct{}

// ContextWithQuoteOptions passes QuoteOptions through the canonical
// courier.Quoter interface.
func ContextWithQuoteOptions(ctx context.Context, o QuoteOptions) context.Context {
	o.SpecialRequests = append([]string(nil), o.SpecialRequests...)
	return context.WithValue(ctx, quoteOptKey{}, o)
}

// Quote implements courier.Quoter. QuoteOptions must be in ctx.
func (c *Client) Quote(ctx context.Context, req courier.QuoteRequest) (*courier.Quote, error) {
	o, ok := ctx.Value(quoteOptKey{}).(QuoteOptions)
	if !ok {
		return nil, courier.Validation("quote_options", "service type and language are required; use ContextWithQuoteOptions or QuoteWithOptions")
	}
	return c.QuoteWithOptions(ctx, req, o)
}

// QuoteWithOptions requests a quotation (POST /v3/quotations). It books nothing.
func (c *Client) QuoteWithOptions(ctx context.Context, req courier.QuoteRequest, o QuoteOptions) (*courier.Quote, error) {
	body, err := buildQuote(req, o)
	if err != nil {
		return nil, err
	}
	// A quotation is not a booking: not an indeterminate-outcome mutation, but still never replayed.
	resp, creds, err := c.call(ctx, "quote.create", http.MethodPost, "/quotations", body, false)
	if err != nil {
		return nil, err
	}
	q, _, err := mapQuote("quote.create", creds.AccountID, resp.Body)
	return q, err
}

// GetQuote implements courier.QuoteReader (GET /v3/quotations/{id}).
func (c *Client) GetQuote(ctx context.Context, id courier.QuoteID) (*courier.Quote, error) {
	q, _, _, err := c.fetchQuote(ctx, id)
	return q, err
}

func (c *Client) fetchQuote(ctx context.Context, id courier.QuoteID) (*courier.Quote, *quotationData, Credentials, error) {
	if err := checkID("quote_id", string(id)); err != nil {
		return nil, nil, Credentials{}, err
	}
	resp, creds, err := c.call(ctx, "quote.get", http.MethodGet, "/quotations/"+string(id), nil, false)
	if err != nil {
		return nil, nil, creds, err
	}
	q, d, err := mapQuote("quote.get", creds.AccountID, resp.Body)
	if err == nil && string(d.QuotationID) != string(id) {
		return nil, nil, creds, protocol("quote.get", "response quotation id does not match the request")
	}
	return q, d, creds, err
}

// CreateOptions are Lalamove-specific order options.
type CreateOptions struct {
	// EnablePOD asks Lalamove to collect proof of delivery (isPODEnabled).
	EnablePOD bool
}

// CreateShipment implements courier.ShipmentCreator. It is quotation-first:
// req.QuoteID is required, and nothing is quoted or repriced on the caller's behalf.
func (c *Client) CreateShipment(ctx context.Context, req courier.CreateShipmentRequest) (*courier.Shipment, error) {
	return c.CreateShipmentWithOptions(ctx, req, CreateOptions{})
}

// CreateShipmentWithOptions is CreateShipment with Lalamove-specific options.
func (c *Client) CreateShipmentWithOptions(ctx context.Context, req courier.CreateShipmentRequest, opt CreateOptions) (*courier.Shipment, error) {
	p, err := validateCreate(req)
	if err != nil {
		return nil, err
	}
	// Credentials are resolved by fetchQuote; failures here happen before any dispatch.
	_, qd, _, err := c.fetchQuote(ctx, req.QuoteID)
	if err != nil {
		return nil, err
	}
	exp := parseTime(qd.ExpiresAt)
	if exp == nil {
		return nil, protocol("shipment.create", "quotation lacks a usable expiresAt; refusing to order")
	}
	if !c.now().Before(*exp) {
		return nil, courier.Validation("quote_id", "quotation has expired; request a new quotation")
	}
	stops, err := checkStops(req, qd)
	if err != nil {
		return nil, err
	}
	body := orderBody{Data: orderReq{
		QuotationID: string(req.QuoteID),
		Sender:      personReq{StopID: stops[0], Name: p.senderName, Phone: p.senderPhone},
		Recipients:  []personReq{{StopID: stops[1], Name: p.recipName, Phone: p.recipPhone, Remarks: p.remarks}},
	}}
	if opt.EnablePOD {
		t := true
		body.Data.IsPODEnabled = &t
	}
	if m := strings.TrimSpace(string(req.MerchantOrderID)); m != "" {
		body.Data.Metadata = map[string]string{"merchantOrderId": m}
	}
	resp, creds, err := c.call(ctx, "shipment.create", http.MethodPost, "/orders", body, true)
	if err != nil {
		return nil, err
	}
	od, err := decodeOrder("shipment.create", resp.Body)
	if err != nil {
		return nil, asIndeterminate(err)
	}
	sh, err := mapOrder(creds.AccountID, od, resp.Body)
	if err != nil {
		return nil, asIndeterminate(err)
	}
	sh.MerchantOrderID = req.MerchantOrderID
	return sh, nil
}

// refID validates a ShipmentRef against the resolved credentials.
func refID(creds Credentials, ref courier.ShipmentRef) (string, error) {
	if ref.Provider != "" && ref.Provider != ProviderID {
		return "", courier.Validation("ref.provider", "shipment reference belongs to another provider")
	}
	if ref.Account != "" && creds.AccountID != "" && ref.Account != creds.AccountID {
		return "", courier.Validation("ref.account", "shipment reference belongs to another account")
	}
	if err := checkID("ref.id", string(ref.ID)); err != nil {
		return "", err
	}
	return string(ref.ID), nil
}

func (c *Client) fetchOrder(ctx context.Context, op string, check func(Credentials) (string, error)) (*orderData, []byte, Credentials, error) {
	creds, err := c.creds.Resolve(ctx)
	if err != nil {
		return nil, nil, creds, err
	}
	id, err := check(creds)
	if err != nil {
		return nil, nil, creds, err
	}
	resp, creds, err := c.call(ctx, op, http.MethodGet, "/orders/"+id, nil, false)
	if err != nil {
		return nil, nil, creds, err
	}
	od, err := decodeOrder(op, resp.Body)
	if err != nil {
		return nil, nil, creds, err
	}
	if string(od.OrderID) != id {
		return nil, nil, creds, protocol(op, "response order id does not match the request")
	}
	return od, resp.Body, creds, nil
}

// GetShipment implements courier.ShipmentReader.
func (c *Client) GetShipment(ctx context.Context, ref courier.ShipmentRef) (*courier.Shipment, error) {
	od, raw, creds, err := c.fetchOrder(ctx, "shipment.get", func(cr Credentials) (string, error) { return refID(cr, ref) })
	if err != nil {
		return nil, err
	}
	return mapOrder(creds.AccountID, od, raw)
}

// GetOrder returns the typed Lalamove order, including proof-of-pickup/delivery
// data per stop when the provider supplied it.
func (c *Client) GetOrder(ctx context.Context, ref courier.ShipmentRef) (*Order, error) {
	od, raw, creds, err := c.fetchOrder(ctx, "order.get", func(cr Credentials) (string, error) { return refID(cr, ref) })
	if err != nil {
		return nil, err
	}
	return toOrder(creds.AccountID, od, raw)
}

// CancelShipment implements courier.ShipmentCanceller (DELETE /v3/orders/{id}).
// A 409 ERR_CANCELLATION_FORBIDDEN is a confirmed rejection (ErrConflict with
// the provider code preserved); timeouts and 5xx are indeterminate.
func (c *Client) CancelShipment(ctx context.Context, ref courier.ShipmentRef) error {
	creds, err := c.creds.Resolve(ctx)
	if err != nil {
		return err
	}
	id, err := refID(creds, ref)
	if err != nil {
		return err
	}
	_, _, err = c.call(ctx, "shipment.cancel", http.MethodDelete, "/orders/"+id, nil, true)
	return err
}

// TrackShipment implements courier.ShipmentTracker. The order ID is taken from
// a LookupProviderShipmentID lookup or, failing that, from req.Code (Lalamove
// has no separate tracking code). Only the current status is exposed, so the
// result is CoverageCurrentOnly with no events.
func (c *Client) TrackShipment(ctx context.Context, req courier.TrackingRequest) (*courier.TrackingResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(req.Code))
	if v, ok := req.Find(courier.LookupProviderShipmentID); ok {
		id = v
	}
	if id == "" {
		return nil, courier.Unsupported(ProviderID, "tracking.get", "supported lookups: order ID (provider shipment ID or code)")
	}
	od, raw, creds, err := c.fetchOrder(ctx, "tracking.get", func(cr Credentials) (string, error) {
		if req.Account != "" && cr.AccountID != "" && req.Account != cr.AccountID {
			return "", courier.Validation("account", "tracking account does not match the credentials")
		}
		return id, checkID("code", id)
	})
	if err != nil {
		return nil, err
	}
	return mapTracking(creds.AccountID, req, od, raw, c.now().UTC()), nil
}

// GetDriver returns driver details (GET /v3/orders/{orderId}/drivers/{driverId}).
// Lalamove makes them available only shortly before pickup; other times return a
// provider 403. The result is personal data and is never logged.
func (c *Client) GetDriver(ctx context.Context, ref courier.ShipmentRef, driverID string) (*Driver, error) {
	creds, err := c.creds.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	id, err := refID(creds, ref)
	if err != nil {
		return nil, err
	}
	if err := checkID("driver_id", driverID); err != nil {
		return nil, err
	}
	resp, _, err := c.call(ctx, "driver.get", http.MethodGet, "/orders/"+id+"/drivers/"+driverID, nil, false)
	if err != nil {
		return nil, err
	}
	var env driverEnvelope
	if err := httpx.DecodeJSON(ProviderID, "driver.get", resp.Body, &env); err != nil {
		return nil, err
	}
	if env.Data == nil || strings.TrimSpace(string(env.Data.DriverID)) == "" {
		return nil, protocol("driver.get", "response lacks a driver")
	}
	return toDriver(env.Data, resp.Body), nil
}
