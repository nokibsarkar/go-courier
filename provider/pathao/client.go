package pathao

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
	"sync"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/httpx"
)

// ProviderID is the canonical provider identity.
const ProviderID courier.ProviderID = "pathao"

// Base URLs. The environment is always an explicit choice (never inferred).
const (
	// ProductionBaseURL is the default environment.
	ProductionBaseURL = "https://api-hermes.pathao.com"
	// SandboxBaseURL is selected by WithSandbox.
	SandboxBaseURL = "https://courier-api-sandbox.pathao.com"
)

const apiPrefix = "/aladdin/api/v1"

// Credentials are the typed Pathao credentials (OAuth password grant).
// AccountID is an optional application-chosen label used in ShipmentRef.Account
// and never sent to Pathao. DefaultStoreID is used when a request names no store.
type Credentials struct {
	ClientID       string
	ClientSecret   auth.Secret
	Username       string
	Password       auth.Secret
	AccountID      courier.AccountID
	DefaultStoreID courier.StoreID
}

// Clone implements auth.Cloner.
func (c Credentials) Clone() Credentials { return c }

func (c Credentials) validate() error {
	if strings.TrimSpace(c.ClientID) == "" || c.ClientSecret.IsZero() || strings.TrimSpace(c.Username) == "" || c.Password.IsZero() {
		return courier.Configuration(ProviderID, "client id, client secret, username and password are required")
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
	creds     *Credentials
	baseURL   string
	sandbox   bool
	hc        *http.Client
	logger    *slog.Logger
	userAgent string
	maxBody   int64
	maxConc   int
	retry     *httpx.RetryPolicy
}

// Option configures New / NewTenant.
type Option func(*settings)

// WithCredentials supplies explicit global credentials (global mode only).
func WithCredentials(c Credentials) Option { return func(s *settings) { s.creds = &c } }

// WithBaseURL overrides the API base URL (tests, proxies). Mutually exclusive
// with WithSandbox.
func WithBaseURL(u string) Option { return func(s *settings) { s.baseURL = u } }

// WithSandbox selects the Pathao sandbox environment explicitly.
func WithSandbox() Option { return func(s *settings) { s.sandbox = true } }

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

// WithRetry sets the retry policy for replay-safe reads. Mutations, price
// quotes and token requests are never retried automatically.
func WithRetry(p httpx.RetryPolicy) Option { return func(s *settings) { s.retry = &p } }

// Client is the Pathao adapter. It is safe for concurrent use.
type Client struct {
	http    *httpx.Client
	creds   *auth.Resolver[Credentials]
	baseURL string
	tokens  *auth.TokenCache
	refresh *refreshStore
}

var (
	_ courier.ShipmentCreator = (*Client)(nil)
	_ courier.ShipmentTracker = (*Client)(nil)
	_ courier.Quoter          = (*Client)(nil)
)

func build(mode auth.Mode, opts []Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	switch {
	case s.sandbox && strings.TrimSpace(s.baseURL) != "":
		return nil, courier.Configuration(ProviderID, "WithSandbox and WithBaseURL are mutually exclusive")
	case s.sandbox:
		s.baseURL = SandboxBaseURL
	case strings.TrimSpace(s.baseURL) == "":
		s.baseURL = ProductionBaseURL
	}
	s.baseURL = strings.TrimRight(strings.TrimSpace(s.baseURL), "/")
	if u, err := url.Parse(s.baseURL); err != nil || u.Host == "" {
		return nil, courier.Configuration(ProviderID, "invalid base URL")
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
	c := &Client{http: hc, baseURL: s.baseURL, tokens: auth.NewTokenCache(), refresh: newRefreshStore(256)}
	switch mode {
	case auth.ModeGlobal:
		var creds Credentials
		if s.creds != nil {
			creds = *s.creds
		} else {
			env := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
			creds = Credentials{
				ClientID:       env("PATHAO_CLIENT_ID"),
				ClientSecret:   auth.Secret(env("PATHAO_CLIENT_SECRET")),
				Username:       env("PATHAO_USERNAME"),
				Password:       auth.Secret(env("PATHAO_PASSWORD")),
				AccountID:      courier.AccountID(env("PATHAO_ACCOUNT_ID")),
				DefaultStoreID: courier.StoreID(env("PATHAO_STORE_ID")),
			}
		}
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
// PATHAO_CLIENT_ID / PATHAO_CLIENT_SECRET / PATHAO_USERNAME / PATHAO_PASSWORD
// (optional PATHAO_STORE_ID, PATHAO_ACCOUNT_ID); construction fails if a
// required one is missing. No network call is made.
func New(opts ...Option) (*Client, error) { return build(auth.ModeGlobal, opts) }

// NewTenant creates a tenant-mode client that never reads environment or
// global credentials; every call needs ContextWithCredentials.
func NewTenant(opts ...Option) (*Client, error) { return build(auth.ModeTenant, opts) }

// Mode reports the client's immutable credential mode.
func (c *Client) Mode() auth.Mode { return c.creds.Mode() }

// refreshStore keeps refresh tokens per credential identity (bounded).
type refreshStore struct {
	mu  sync.Mutex
	max int
	m   map[string]auth.Secret
}

func newRefreshStore(max int) *refreshStore {
	return &refreshStore{max: max, m: map[string]auth.Secret{}}
}

func (r *refreshStore) get(id string) auth.Secret {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

func (r *refreshStore) put(id string, v auth.Secret) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[id]; !ok && len(r.m) >= r.max {
		for k := range r.m {
			delete(r.m, k)
			break
		}
	}
	if v.IsZero() {
		delete(r.m, id)
		return
	}
	r.m[id] = v
}

// identity derives the token-cache key from every credential field and the
// base URL, so tokens can never cross tenants or environments.
func (c *Client) identity(cr Credentials) string {
	return auth.Identity("pathao", c.baseURL, cr.ClientID, cr.ClientSecret.Reveal(), cr.Username,
		cr.Password.Reveal(), string(cr.AccountID), string(cr.DefaultStoreID))
}

var numPattern = regexp.MustCompile(`[0-9]{7,}`)

// sanitizeMsg bounds provider text and masks long digit runs (phone-like).
func sanitizeMsg(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return numPattern.ReplaceAllString(s, "[redacted]")
}

func errMsg(_ int, body []byte) (string, string) {
	code, msg := httpx.JSONMessage(0, body)
	return code, sanitizeMsg(msg)
}

// requestToken performs one token-issue call. It is neither a mutation nor
// replay-safe (HTTP-03): it is never retried automatically.
func (c *Client) requestToken(ctx context.Context, body tokenRequest) (auth.Token, string, error) {
	const op = "auth.token"
	raw, err := json.Marshal(body)
	if err != nil {
		return auth.Token{}, "", courier.Validation("body", "cannot encode request")
	}
	resp, err := c.http.Do(ctx, httpx.Request{Op: op, Method: http.MethodPost, URL: c.baseURL + apiPrefix + "/issue-token",
		Body: raw, Mutation: false, ReplaySafe: false})
	if err != nil {
		var ce *courier.Error
		if errors.As(err, &ce) && ce.StatusCode >= 400 && ce.StatusCode < 500 && ce.StatusCode != 429 {
			return auth.Token{}, "", &courier.Error{Kind: courier.KindCredentials, Provider: ProviderID, Op: op,
				StatusCode: ce.StatusCode, Outcome: courier.OutcomeRejected, Message: "token request rejected"}
		}
		return auth.Token{}, "", err
	}
	var tr tokenResponse
	if err := httpx.DecodeJSON(ProviderID, op, resp.Body, &tr); err != nil {
		return auth.Token{}, "", err
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		return auth.Token{}, "", &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: op, Message: "response lacks an access token"}
	}
	tok := auth.Token{Value: auth.Secret(tr.AccessToken)}
	if secs, err := tr.ExpiresIn.Int64(); err == nil && secs > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(secs) * time.Second)
	}
	return tok, tr.RefreshToken, nil
}

// issue obtains a token: refresh grant when a refresh token is known, falling
// back to the password grant if Pathao rejects it.
func (c *Client) issue(ctx context.Context, cr Credentials, id string) (auth.Token, error) {
	if rt := c.refresh.get(id); !rt.IsZero() {
		tok, next, err := c.requestToken(ctx, tokenRequest{ClientID: cr.ClientID, ClientSecret: cr.ClientSecret.Reveal(),
			RefreshToken: rt.Reveal(), GrantType: "refresh_token"})
		if err == nil {
			c.refresh.put(id, auth.Secret(next))
			return tok, nil
		}
		if ctx.Err() != nil || !(courier.IsKind(err, courier.KindCredentials) || courier.IsKind(err, courier.KindProtocol)) {
			return auth.Token{}, err
		}
		c.refresh.put(id, "")
	}
	tok, next, err := c.requestToken(ctx, tokenRequest{ClientID: cr.ClientID, ClientSecret: cr.ClientSecret.Reveal(),
		Username: cr.Username, Password: cr.Password.Reveal(), GrantType: "password"})
	if err != nil {
		return auth.Token{}, err
	}
	c.refresh.put(id, auth.Secret(next))
	return tok, nil
}

type callSpec struct {
	op, method, path string
	query            url.Values
	payload          any
	mutation         bool
	replaySafe       bool
}

// call resolves credentials (before any dispatch), obtains a cached bearer
// token and performs the request. On HTTP 401 it invalidates exactly the
// rejected token, re-issues once and retries once: 401 means the request was
// not processed, so the retry is safe even for mutations.
func (c *Client) call(ctx context.Context, sp callSpec) (*httpx.Response, Credentials, error) {
	creds, err := c.creds.Resolve(ctx)
	if err != nil {
		return nil, creds, err
	}
	if err := creds.validate(); err != nil {
		return nil, creds, err
	}
	var body []byte
	if sp.payload != nil {
		if body, err = json.Marshal(sp.payload); err != nil {
			return nil, creds, courier.Validation("body", "cannot encode request")
		}
	}
	id := c.identity(creds)
	for attempt := 0; ; attempt++ {
		tok, err := c.tokens.Get(ctx, id, func(ctx context.Context) (auth.Token, error) { return c.issue(ctx, creds, id) })
		if err != nil {
			return nil, creds, err
		}
		resp, err := c.http.Do(ctx, httpx.Request{
			Op: sp.op, Method: sp.method, URL: c.baseURL + apiPrefix + sp.path, Query: sp.query, Body: body,
			Mutation: sp.mutation, ReplaySafe: sp.replaySafe, ErrorMessage: errMsg,
			Auth: http.Header{"Authorization": {"Bearer " + tok.Reveal()}},
		})
		if err != nil && resp != nil && resp.Status == http.StatusUnauthorized && attempt == 0 {
			c.tokens.Invalidate(id, tok)
			continue
		}
		return resp, creds, err
	}
}

// checkEnvelope converts an enveloped failure inside an HTTP 2xx into an
// error (ERR-02). For mutations a 5xx-class code is indeterminate.
func checkEnvelope(op string, env envelope, mutation bool) error {
	code := env.Code.String()
	typ := strings.ToLower(strings.TrimSpace(env.Type))
	if typ != "error" && (code == "" || code == "200" || code == "201" || code == "202") {
		return nil
	}
	e := &courier.Error{Provider: ProviderID, Op: op, Message: sanitizeMsg(env.Message), Outcome: courier.OutcomeRejected, ProviderCode: code}
	switch code {
	case "401":
		e.Kind = courier.KindCredentials
	case "403":
		e.Kind = courier.KindPermission
	case "404":
		e.Kind = courier.KindNotFound
	case "429":
		e.Kind, e.Retryable = courier.KindRateLimit, true
	case "400", "422":
		e.Kind = courier.KindValidation
	default:
		e.Kind = courier.KindRejected
		if n, err := env.Code.Int64(); err == nil && n >= 500 {
			if mutation {
				e.Kind, e.Outcome = courier.KindIndeterminate, courier.OutcomeIndeterminate
				e.Message = strings.TrimSpace(e.Message + "; the operation may have been applied")
			} else {
				e.Kind, e.Outcome, e.Retryable = courier.KindUnavailable, courier.OutcomeUnknown, true
			}
		}
	}
	return e
}

// asIndeterminate marks an unusable response to a mutation as indeterminate.
func asIndeterminate(err error) error {
	return &courier.Error{Kind: courier.KindIndeterminate, Provider: ProviderID, Outcome: courier.OutcomeIndeterminate,
		Message: "unusable response to a state-changing request; the operation may have been applied", Err: err}
}

func protocol(op, msg string) error {
	return &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: op, Message: msg}
}

// decode parses the envelope and its data into out. On a mutation any
// failure that is not a confirmed rejection is indeterminate.
func decode(op string, body []byte, mutation bool, out any) error {
	var env envelope
	if err := httpx.DecodeJSON(ProviderID, op, body, &env); err != nil {
		if mutation {
			return asIndeterminate(err)
		}
		return err
	}
	if err := checkEnvelope(op, env, mutation); err != nil {
		return err
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		err := protocol(op, "response lacks data")
		if mutation {
			return asIndeterminate(err)
		}
		return err
	}
	if err := httpx.DecodeJSON(ProviderID, op, env.Data, out); err != nil {
		if mutation {
			return asIndeterminate(err)
		}
		return err
	}
	return nil
}
