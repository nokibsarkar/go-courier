package carrybee

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/httpx"
)

// ProviderID is the canonical provider identity.
const ProviderID courier.ProviderID = "carrybee"

const (
	SandboxURL    = "https://stage-sandbox.carrybee.com"
	ProductionURL = "https://developers.carrybee.com"
)

const apiPrefix = "/api/v2"

// Credentials are the three static Carrybee credentials, sent on every request
// as the headers Client-ID, Client-Secret and Client-Context (there is no
// token exchange). AccountID is an application label used for ShipmentRef.Account
// and webhook attribution.
type Credentials struct {
	ClientID      string
	ClientSecret  auth.Secret
	ClientContext auth.Secret
	AccountID     courier.AccountID
}

// Clone implements auth.Cloner.
func (c Credentials) Clone() Credentials { return c }

func (c Credentials) validate() error {
	if strings.TrimSpace(c.ClientID) == "" || strings.TrimSpace(c.ClientSecret.Reveal()) == "" || strings.TrimSpace(c.ClientContext.Reveal()) == "" {
		return courier.Configuration(ProviderID, "client ID, client secret and client context are all required")
	}
	for _, v := range []string{c.ClientID, c.ClientSecret.Reveal(), c.ClientContext.Reveal()} {
		if strings.ContainsAny(v, "\r\n") {
			return courier.Configuration(ProviderID, "invalid credential")
		}
	}
	return nil
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
	hookSec   map[courier.AccountID]auth.Secret
}

// Option configures New / NewTenant.
type Option func(*settings)

// WithCredentials supplies explicit global credentials (global mode only).
func WithCredentials(c Credentials) Option { return func(s *settings) { s.creds = &c } }

// WithSandbox selects the sandbox base URL.
func WithSandbox() Option { return func(s *settings) { s.baseURL, s.envSet = SandboxURL, true } }

// WithProduction selects the production base URL.
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

// WithRetry sets the retry policy for replay-safe requests (GET reads only).
func WithRetry(p httpx.RetryPolicy) Option { return func(s *settings) { s.retry = &p } }

// Client is the Carrybee adapter, safe for concurrent use.
type Client struct {
	http    *httpx.Client
	creds   *auth.Resolver[Credentials]
	baseURL string
	log     *slog.Logger
	hookSec map[courier.AccountID]auth.Secret
}

var (
	_ courier.ShipmentCreator        = (*Client)(nil)
	_ courier.ShipmentTracker        = (*Client)(nil)
	_ courier.ShipmentCanceller      = (*Client)(nil)
	_ courier.WebhookHandlerProvider = (*Client)(nil)
	_ courier.WebhookDecoder         = (*Client)(nil)
)

func build(mode auth.Mode, opts []Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if !s.envSet {
		return nil, courier.Configuration(ProviderID, "choose the environment explicitly: WithSandbox, WithProduction or WithBaseURL")
	}
	var rp httpx.RetryPolicy
	if s.retry != nil {
		rp = *s.retry
	}
	hc, err := httpx.New(httpx.Config{Provider: ProviderID, HTTPClient: s.hc, UserAgent: s.userAgent, MaxResponseBytes: s.maxBody,
		MaxConcurrent: s.maxConc, Retry: rp, Logger: s.logger})
	if err != nil {
		return nil, err
	}
	c := &Client{http: hc, baseURL: strings.TrimRight(s.baseURL, "/"), log: s.logger, hookSec: s.hookSec}
	switch mode {
	case auth.ModeGlobal:
		var creds Credentials
		if s.creds != nil {
			creds = *s.creds
		} else {
			creds = Credentials{
				ClientID:      strings.TrimSpace(os.Getenv("CARRYBEE_CLIENT_ID")),
				ClientSecret:  auth.Secret(strings.TrimSpace(os.Getenv("CARRYBEE_CLIENT_SECRET"))),
				ClientContext: auth.Secret(strings.TrimSpace(os.Getenv("CARRYBEE_CLIENT_CONTEXT"))),
				AccountID:     courier.AccountID(os.Getenv("CARRYBEE_ACCOUNT_ID")),
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

// New creates a global-mode client (explicit credentials or the CARRYBEE_*
// environment variables; construction fails if missing; no network call).
func New(opts ...Option) (*Client, error) { return build(auth.ModeGlobal, opts) }

// NewTenant creates a tenant-mode client that holds no credentials.
func NewTenant(opts ...Option) (*Client, error) { return build(auth.ModeTenant, opts) }

// Mode reports the immutable credential mode.
func (c *Client) Mode() auth.Mode { return c.creds.Mode() }

type callSpec struct {
	op       string
	method   string
	path     string
	query    url.Values
	payload  any
	mutation bool
}

func (c *Client) call(ctx context.Context, s callSpec) (*httpx.Response, Credentials, error) {
	creds, err := c.creds.Resolve(ctx)
	if err != nil {
		return nil, creds, err
	}
	if err := creds.validate(); err != nil {
		return nil, creds, err
	}
	var body []byte
	if s.payload != nil {
		if body, err = json.Marshal(s.payload); err != nil {
			return nil, creds, courier.Validation("body", "cannot encode request")
		}
	}
	resp, err := c.http.Do(ctx, httpx.Request{Op: s.op, Method: s.method, URL: c.baseURL + apiPrefix + s.path, Query: s.query, Body: body,
		Mutation: s.mutation, ErrorMessage: errorText,
		Auth: http.Header{
			"Client-Id":      {strings.TrimSpace(creds.ClientID)},
			"Client-Secret":  {strings.TrimSpace(creds.ClientSecret.Reveal())},
			"Client-Context": {strings.TrimSpace(creds.ClientContext.Reveal())},
		}})
	if err != nil {
		return resp, creds, err
	}
	if e := embeddedError(s.op, resp.Body); e != nil {
		return resp, creds, e
	}
	return resp, creds, nil
}
