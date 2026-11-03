package ecourier

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/httpx"
)

// ProviderID is the canonical provider identity.
const ProviderID courier.ProviderID = "ecourier"

// Documented base URLs (v5.5, sections 4.0 and 5.0).
const (
	SandboxURL    = "https://staging.ecourier.com.bd/api"
	ProductionURL = "https://backoffice.ecourier.com.bd/api"
)

// Credentials are the typed eCourier credentials: API-KEY, API-SECRET and USER-ID
// headers. AccountID is an optional application label for ShipmentRef.Account.
type Credentials struct {
	APIKey    auth.Secret
	APISecret auth.Secret
	UserID    string
	AccountID courier.AccountID
}

// Clone implements auth.Cloner.
func (c Credentials) Clone() Credentials { return c }

func (c Credentials) validate() error {
	if c.APIKey.IsZero() || c.APISecret.IsZero() || strings.TrimSpace(c.UserID) == "" {
		return courier.Configuration(ProviderID, "api key, api secret and user id are required")
	}
	if strings.ContainsAny(c.UserID, "\r\n") {
		return courier.Configuration(ProviderID, "invalid user id")
	}
	return nil
}

func (c Credentials) account() courier.AccountID {
	if c.AccountID != "" {
		return c.AccountID
	}
	return courier.AccountID(strings.TrimSpace(c.UserID))
}

var credKey = auth.NewKey[Credentials]()

// ContextWithCredentials attaches tenant credentials to ctx (tenant mode only).
func ContextWithCredentials(ctx context.Context, c Credentials) context.Context {
	return credKey.With(ctx, c)
}

type settings struct {
	creds     *Credentials
	baseURL   string
	envSet    bool
	hc        *http.Client
	logger    *slog.Logger
	userAgent string
	maxBody   int64
	maxConc   int
	retry     *httpx.RetryPolicy
	loc       *time.Location
	cancelMsg string
}

// Option configures New / NewTenant.
type Option func(*settings)

// WithCredentials supplies explicit global credentials (global mode only).
func WithCredentials(c Credentials) Option { return func(s *settings) { s.creds = &c } }

// WithSandbox selects https://staging.ecourier.com.bd/api.
func WithSandbox() Option { return func(s *settings) { s.baseURL, s.envSet = SandboxURL, true } }

// WithProduction selects https://backoffice.ecourier.com.bd/api.
func WithProduction() Option { return func(s *settings) { s.baseURL, s.envSet = ProductionURL, true } }

// WithBaseURL sets a custom base URL (tests, proxies); counts as an explicit environment.
func WithBaseURL(u string) Option {
	return func(s *settings) { s.baseURL, s.envSet = strings.TrimRight(strings.TrimSpace(u), "/"), true }
}

// WithHTTPClient injects a trusted HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(s *settings) { s.hc = c } }

// WithLogger sets an optional logger (default no-op).
func WithLogger(l *slog.Logger) Option { return func(s *settings) { s.logger = l } }

// WithUserAgent overrides the User-Agent.
func WithUserAgent(ua string) Option { return func(s *settings) { s.userAgent = ua } }

// WithMaxResponseBytes bounds response bodies.
func WithMaxResponseBytes(n int64) Option { return func(s *settings) { s.maxBody = n } }

// WithMaxConcurrent bounds in-flight requests.
func WithMaxConcurrent(n int) Option { return func(s *settings) { s.maxConc = n } }

// WithRetry sets the retry policy for replay-safe requests. eCourier lookups are
// POSTs and are never treated as replay-safe, so this only affects future GETs.
func WithRetry(p httpx.RetryPolicy) Option { return func(s *settings) { s.retry = &p } }

// WithEventLocation overrides the default Asia/Dhaka location for tracking
// event timestamps that do not include a timezone.
func WithEventLocation(l *time.Location) Option { return func(s *settings) { s.loc = l } }

// WithCancelComment sets the mandatory cancel reason used by CancelShipment
// (default "Cancelled by merchant"). Use CancelOrder to give a per-call reason.
func WithCancelComment(c string) Option { return func(s *settings) { s.cancelMsg = c } }

// Client is the eCourier adapter, safe for concurrent use.
type Client struct {
	http      *httpx.Client
	creds     *auth.Resolver[Credentials]
	baseURL   string
	loc       *time.Location
	cancelMsg string
}

var (
	_ courier.ShipmentCreator     = (*Client)(nil)
	_ courier.ShipmentTracker     = (*Client)(nil)
	_ courier.ShipmentCanceller   = (*Client)(nil)
	_ courier.ShipmentLabelReader = (*Client)(nil)
	_ courier.CustomerRiskReader  = (*Client)(nil)
)

func build(mode auth.Mode, opts []Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if !s.envSet {
		return nil, courier.Configuration(ProviderID, "choose the environment explicitly: WithSandbox, WithProduction or WithBaseURL")
	}
	if strings.TrimSpace(s.cancelMsg) == "" {
		s.cancelMsg = "Cancelled by merchant"
	}
	if len(s.cancelMsg) > 255 {
		return nil, courier.Configuration(ProviderID, "cancel comment must be at most 255 characters")
	}
	hc, err := httpx.New(httpx.Config{Provider: ProviderID, HTTPClient: s.hc, UserAgent: s.userAgent, MaxResponseBytes: s.maxBody,
		MaxConcurrent: s.maxConc, Retry: derefRetry(s.retry), Logger: s.logger})
	if err != nil {
		return nil, err
	}
	if s.loc == nil {
		s.loc = asiaDhaka
	}
	c := &Client{http: hc, baseURL: strings.TrimRight(s.baseURL, "/"), loc: s.loc, cancelMsg: s.cancelMsg}
	switch mode {
	case auth.ModeGlobal:
		var creds Credentials
		if s.creds != nil {
			creds = *s.creds
		} else {
			creds = Credentials{
				APIKey:    auth.Secret(strings.TrimSpace(os.Getenv("ECOURIER_API_KEY"))),
				APISecret: auth.Secret(strings.TrimSpace(os.Getenv("ECOURIER_API_SECRET"))),
				UserID:    strings.TrimSpace(os.Getenv("ECOURIER_USER_ID")),
				AccountID: courier.AccountID(os.Getenv("ECOURIER_ACCOUNT_ID")),
			}
		}
		if err := creds.validate(); err != nil {
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

func derefRetry(p *httpx.RetryPolicy) httpx.RetryPolicy {
	if p == nil {
		return httpx.RetryPolicy{}
	}
	return *p
}

// New creates a global-mode client (explicit credentials or ECOURIER_* env vars;
// construction fails if missing; no network call).
func New(opts ...Option) (*Client, error) { return build(auth.ModeGlobal, opts) }

// NewTenant creates a tenant-mode client that holds no credentials.
func NewTenant(opts ...Option) (*Client, error) { return build(auth.ModeTenant, opts) }

// Mode reports the immutable credential mode.
func (c *Client) Mode() auth.Mode { return c.creds.Mode() }

func (c *Client) call(ctx context.Context, op, path string, payload any, mutation bool) (*httpx.Response, Credentials, error) {
	creds, err := c.creds.Resolve(ctx)
	if err != nil {
		return nil, creds, err
	}
	if err := creds.validate(); err != nil {
		return nil, creds, err
	}
	var body []byte
	if payload != nil {
		if body, err = json.Marshal(payload); err != nil {
			return nil, creds, courier.Validation("body", "cannot encode request")
		}
	} else {
		body = []byte("{}")
	}
	resp, err := c.http.Do(ctx, httpx.Request{Op: op, Method: http.MethodPost, URL: c.baseURL + path, Body: body,
		Mutation: mutation, ErrorMessage: errorText,
		Auth: http.Header{"Api-Key": {creds.APIKey.Reveal()}, "Api-Secret": {creds.APISecret.Reveal()}, "User-Id": {strings.TrimSpace(creds.UserID)}}})
	var ce *courier.Error
	if errors.As(err, &ce) && ce.Kind == courier.KindValidation && strings.Contains(strings.ToLower(ce.Message), "duplicate") {
		ce.Kind = courier.KindConflict // documented: duplicate product id and mobile
	}
	return resp, creds, err
}
