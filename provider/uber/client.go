package uber

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/httpx"
)

// ProviderID is the canonical provider identity.
const ProviderID courier.ProviderID = "uber"

const (
	// DefaultBaseURL is the production API root (customer path is appended).
	DefaultBaseURL = "https://api.uber.com/v1"
	// DefaultAuthURL is the production OAuth2 token endpoint.
	DefaultAuthURL = "https://auth.uber.com/oauth/v2/token"
	tokenScope     = "eats.deliveries"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Credentials are the typed Uber Direct credentials. ClientID/ClientSecret
// authenticate the OAuth2 client; CustomerID is a separate concept (one
// client may span several customers) and is part of every API path. AccountID
// is an optional application-chosen label used in ShipmentRef.Account; when
// empty the CustomerID is used, since delivery IDs are scoped by customer.
type Credentials struct {
	ClientID     auth.Secret
	ClientSecret auth.Secret
	CustomerID   string
	AccountID    courier.AccountID
}

// Clone implements auth.Cloner.
func (c Credentials) Clone() Credentials { return c }

func (c Credentials) normalized() Credentials {
	c.ClientID = auth.Secret(strings.TrimSpace(c.ClientID.Reveal()))
	c.ClientSecret = auth.Secret(strings.TrimSpace(c.ClientSecret.Reveal()))
	c.CustomerID = strings.TrimSpace(c.CustomerID)
	return c
}

func (c Credentials) validate() error {
	if c.ClientID.IsZero() || c.ClientSecret.IsZero() {
		return courier.Configuration(ProviderID, "client ID and client secret are required")
	}
	if c.CustomerID == "" || len(c.CustomerID) > 128 {
		return courier.Configuration(ProviderID, "customer ID is required (at most 128 characters)")
	}
	for _, r := range c.CustomerID {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return courier.Configuration(ProviderID, "customer ID contains invalid characters")
		}
	}
	return nil
}

func (c Credentials) account() courier.AccountID {
	if c.AccountID != "" {
		return c.AccountID
	}
	return courier.AccountID(c.CustomerID)
}

var credKey = auth.NewKey[Credentials]()

// ContextWithCredentials attaches tenant credentials to ctx for a tenant-mode
// client. A global-mode client rejects them (AUTH-02).
func ContextWithCredentials(ctx context.Context, c Credentials) context.Context {
	return credKey.With(ctx, c)
}

type settings struct {
	creds     *Credentials
	baseURL   string
	authURL   string
	hc        *http.Client
	logger    *slog.Logger
	userAgent string
	maxBody   int64
	maxConc   int
	retry     *httpx.RetryPolicy
	hookKeys  map[courier.AccountID]auth.Secret
}

// Option configures New / NewTenant.
type Option func(*settings)

// WithCredentials supplies explicit global credentials (global mode only).
func WithCredentials(c Credentials) Option { return func(s *settings) { c = c.Clone(); s.creds = &c } }

// WithBaseURL overrides the API root, e.g. https://api.uber.com/v1.
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

// WithAuthURL overrides the full OAuth2 token endpoint URL.
func WithAuthURL(u string) Option { return func(s *settings) { s.authURL = u } }

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

// WithRetry sets the retry policy for replay-safe reads. Mutations are never
// retried automatically.
func WithRetry(p httpx.RetryPolicy) Option { return func(s *settings) { s.retry = &p } }

// WithWebhookSigningKeys configures the per-account webhook signing keys. A
// webhook is attributed to an account only when exactly one key verifies it.
func WithWebhookSigningKeys(keys map[courier.AccountID]auth.Secret) Option {
	return func(s *settings) {
		s.hookKeys = make(map[courier.AccountID]auth.Secret, len(keys))
		for k, v := range keys {
			s.hookKeys[k] = v
		}
	}
}

// Client is the Uber Direct adapter. It is safe for concurrent use.
type Client struct {
	http     *httpx.Client
	creds    *auth.Resolver[Credentials]
	tokens   *auth.TokenCache
	baseURL  string
	authURL  string
	hookKeys map[courier.AccountID]auth.Secret
	log      *slog.Logger
}

var (
	_ courier.Quoter                 = (*Client)(nil)
	_ courier.ShipmentCreator        = (*Client)(nil)
	_ courier.ShipmentReader         = (*Client)(nil)
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
	if s.baseURL == "" {
		s.baseURL = DefaultBaseURL
	}
	if s.authURL == "" {
		s.authURL = DefaultAuthURL
	}
	for k, v := range s.hookKeys {
		if k == "" || v.IsZero() {
			return nil, courier.Configuration(ProviderID, "webhook signing keys need a non-empty account and key")
		}
	}
	retry := httpx.RetryPolicy{MaxAttempts: 3}
	if s.retry != nil {
		retry = *s.retry
	}
	hc, err := httpx.New(httpx.Config{Provider: ProviderID, HTTPClient: s.hc, UserAgent: s.userAgent,
		MaxResponseBytes: s.maxBody, MaxConcurrent: s.maxConc, Retry: retry, Logger: s.logger})
	if err != nil {
		return nil, err
	}
	c := &Client{http: hc, tokens: auth.NewTokenCache(), hookKeys: s.hookKeys, log: courier.LoggerOrNop(s.logger),
		baseURL: strings.TrimRight(strings.TrimSpace(s.baseURL), "/"), authURL: strings.TrimSpace(s.authURL)}
	switch mode {
	case auth.ModeGlobal:
		var creds Credentials
		if s.creds != nil {
			creds = *s.creds
		} else {
			creds = Credentials{
				ClientID:     auth.Secret(os.Getenv("UBER_DIRECT_CLIENT_ID")),
				ClientSecret: auth.Secret(os.Getenv("UBER_DIRECT_CLIENT_SECRET")),
				CustomerID:   os.Getenv("UBER_DIRECT_CUSTOMER_ID"),
			}
		}
		creds = creds.normalized()
		if err := creds.validate(); err != nil { // AUTH-07
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

// New creates a global-mode client. Credentials come from WithCredentials or
// UBER_DIRECT_CLIENT_ID / UBER_DIRECT_CLIENT_SECRET / UBER_DIRECT_CUSTOMER_ID;
// construction fails if any is missing. No network call is made.
func New(opts ...Option) (*Client, error) { return build(auth.ModeGlobal, opts) }

// NewTenant creates a tenant-mode client that never reads environment or
// global credentials; every API call needs ContextWithCredentials. Webhook
// verification needs no outbound credentials.
func NewTenant(opts ...Option) (*Client, error) { return build(auth.ModeTenant, opts) }

// Mode reports the client's immutable credential mode.
func (c *Client) Mode() auth.Mode { return c.creds.Mode() }

// errCode keeps only the provider error code: provider messages may echo
// addresses or phone numbers (LOG-02, PRIV-01).
func errCode(_ int, body []byte) (string, string) {
	code, _ := httpx.JSONMessage(0, body)
	return code, ""
}

func (c *Client) token(ctx context.Context, creds Credentials) (auth.Secret, string, error) {
	id := auth.Identity(creds.ClientID.Reveal(), creds.ClientSecret.Reveal(), tokenScope)
	tok, err := c.tokens.Get(ctx, id, func(ctx context.Context) (auth.Token, error) {
		return c.fetchToken(ctx, creds)
	})
	return tok, id, err
}

// fetchToken issues a new token. The request is a POST but not a business
// mutation, and it is never replayed (100/hour limit).
func (c *Client) fetchToken(ctx context.Context, creds Credentials) (auth.Token, error) {
	form := url.Values{
		"client_id":     {creds.ClientID.Reveal()},
		"client_secret": {creds.ClientSecret.Reveal()},
		"grant_type":    {"client_credentials"},
		"scope":         {tokenScope},
	}
	resp, err := c.http.Do(ctx, httpx.Request{Op: "auth.token", Method: http.MethodPost, URL: c.authURL,
		Body: []byte(form.Encode()), ContentType: "application/x-www-form-urlencoded",
		Mutation: false, ReplaySafe: false, ErrorMessage: errCode})
	if err != nil {
		var ce *courier.Error
		if resp != nil && errors.As(err, &ce) && resp.Status >= 400 && resp.Status < 500 && resp.Status != 429 {
			return auth.Token{}, &courier.Error{Kind: courier.KindCredentials, Provider: ProviderID, Op: "auth.token",
				StatusCode: resp.Status, ProviderCode: ce.ProviderCode, Outcome: courier.OutcomeRejected,
				Message: "token request rejected"}
		}
		return auth.Token{}, err
	}
	var tr tokenResponse
	if err := httpx.DecodeJSON(ProviderID, "auth.token", resp.Body, &tr); err != nil {
		return auth.Token{}, err
	}
	if tr.Error != "" {
		return auth.Token{}, &courier.Error{Kind: courier.KindCredentials, Provider: ProviderID, Op: "auth.token",
			ProviderCode: truncate(tr.Error, 64), Outcome: courier.OutcomeRejected, Message: "token request rejected"}
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		return auth.Token{}, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "auth.token", Message: "response lacks access_token"}
	}
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "bearer") {
		return auth.Token{}, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "auth.token", Message: "unexpected token type"}
	}
	tok := auth.Token{Value: auth.Secret(tr.AccessToken)}
	if tr.ExpiresIn != "" {
		n, err := tr.ExpiresIn.Int64()
		if err != nil || n <= 0 {
			return auth.Token{}, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: "auth.token", Message: "invalid expires_in"}
		}
		tok.ExpiresAt = time.Now().Add(time.Duration(n) * time.Second)
	}
	return tok, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// call resolves credentials (before any dispatch), checks the expected
// account scope, obtains a cached token and performs the request. On an API
// 401 it invalidates exactly the rejected token, re-issues once and retries
// once; 401 means the request was not processed, so this is safe for
// mutations too.
func (c *Client) call(ctx context.Context, op, method, path string, payload any, mutation, replaySafe bool, wantAccount courier.AccountID) (*httpx.Response, Credentials, error) {
	creds, err := c.creds.Resolve(ctx)
	if err != nil {
		return nil, creds, err
	}
	creds = creds.normalized()
	if err := creds.validate(); err != nil {
		return nil, creds, err
	}
	if wantAccount != "" && wantAccount != creds.account() {
		return nil, creds, courier.Validation("account", "reference belongs to a different account than the active credentials")
	}
	var body []byte
	if payload != nil {
		if body, err = json.Marshal(payload); err != nil {
			return nil, creds, courier.Validation("body", "cannot encode request")
		}
	}
	target := c.baseURL + "/customers/" + url.PathEscape(creds.CustomerID) + "/" + path
	for attempt := 0; ; attempt++ {
		tok, id, err := c.token(ctx, creds)
		if err != nil {
			return nil, creds, err
		}
		resp, err := c.http.Do(ctx, httpx.Request{Op: op, Method: method, URL: target, Body: body,
			Mutation: mutation, ReplaySafe: replaySafe, ErrorMessage: errCode,
			Auth: http.Header{"Authorization": {"Bearer " + tok.Reveal()}}})
		if err != nil && attempt == 0 && resp != nil && resp.Status == http.StatusUnauthorized {
			c.tokens.Invalidate(id, tok)
			continue
		}
		return resp, creds, err
	}
}
