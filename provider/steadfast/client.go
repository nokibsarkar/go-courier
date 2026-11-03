package steadfast

import (
	"context"
	"encoding/json"
	"fmt"
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
const ProviderID courier.ProviderID = "steadfast"

// DefaultBaseURL is the production merchant API.
const DefaultBaseURL = "https://portal.packzy.com/api/v1"

// MaxCOD is the largest accepted cod_amount, in BDT.
const MaxCOD = 1_000_000

// MaxBulk is the documented per-request bulk limit.
const MaxBulk = 500

// Documented field limits. Steadfast silently truncates longer values; the SDK
// rejects them instead so the stored value is never different from the input.
const (
	MaxInvoiceLen = 100
	MaxAddressLen = 490
	MaxNoteLen    = 480
)

// Credentials are the typed Steadfast credentials: two authentication headers
// (Api-Key, Secret-Key). AccountID is an optional application-chosen opaque
// label used in ShipmentRef.Account; it is never sent to Steadfast.
type Credentials struct {
	APIKey    auth.Secret
	SecretKey auth.Secret
	AccountID courier.AccountID
}

// Clone implements auth.Cloner.
func (c Credentials) Clone() Credentials { return c }

func (c Credentials) validate() error {
	if c.APIKey.IsZero() || c.SecretKey.IsZero() {
		return courier.Configuration(ProviderID, "api key and secret key are required")
	}
	return nil
}

var credKey = auth.NewKey[Credentials]()

// ContextWithCredentials attaches tenant credentials to ctx for a
// tenant-mode client. A global-mode client rejects them (AUTH-02).
func ContextWithCredentials(ctx context.Context, c Credentials) context.Context {
	return credKey.With(ctx, c)
}

type settings struct {
	creds     *Credentials
	baseURL   string
	hc        *http.Client
	logger    *slog.Logger
	userAgent string
	maxBody   int64
	maxConc   int
	retry     *httpx.RetryPolicy

	webhookTokens map[courier.AccountID]auth.Secret
}

// Option configures New / NewTenant.
type Option func(*settings)

// WithCredentials supplies explicit global credentials (global mode only).
func WithCredentials(c Credentials) Option { return func(s *settings) { s.creds = &c } }

// WithBaseURL overrides the API base URL (sandbox, tests).
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

// WithRetry sets the retry policy for replay-safe reads. Mutations are never
// retried automatically.
func WithRetry(p httpx.RetryPolicy) Option { return func(s *settings) { s.retry = &p } }

// Client is the Steadfast adapter. It is safe for concurrent use.
type Client struct {
	http          *httpx.Client
	creds         *auth.Resolver[Credentials]
	baseURL       string
	log           *slog.Logger
	webhookTokens map[courier.AccountID]auth.Secret
}

var (
	_ courier.ShipmentCreator      = (*Client)(nil)
	_ courier.BatchShipmentCreator = (*Client)(nil)
	_ courier.ShipmentTracker      = (*Client)(nil)
	_ courier.BalanceReader        = (*Client)(nil)
)

func build(mode auth.Mode, opts []Option) (*Client, error) {
	var s settings
	for _, o := range opts {
		o(&s)
	}
	if s.baseURL == "" {
		s.baseURL = DefaultBaseURL
	}
	s.baseURL = strings.TrimRight(strings.TrimSpace(s.baseURL), "/")
	retry := httpx.RetryPolicy{MaxAttempts: 3}
	if s.retry != nil {
		retry = *s.retry
	}
	hc, err := httpx.New(httpx.Config{Provider: ProviderID, HTTPClient: s.hc, UserAgent: s.userAgent,
		MaxResponseBytes: s.maxBody, MaxConcurrent: s.maxConc, Retry: retry, Logger: s.logger})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for acct, tok := range s.webhookTokens {
		if tok.IsZero() || seen[tok.Reveal()] {
			return nil, courier.Configuration(ProviderID, "webhook tokens must be non-empty and unique per account ("+string(acct)+")")
		}
		seen[tok.Reveal()] = true
	}
	c := &Client{http: hc, baseURL: s.baseURL, log: courier.LoggerOrNop(s.logger), webhookTokens: s.webhookTokens}
	switch mode {
	case auth.ModeGlobal:
		var creds Credentials
		if s.creds != nil {
			creds = *s.creds
		} else {
			creds = Credentials{
				APIKey:    auth.Secret(strings.TrimSpace(os.Getenv("STEADFAST_API_KEY"))),
				SecretKey: auth.Secret(strings.TrimSpace(os.Getenv("STEADFAST_SECRET_KEY"))),
				AccountID: courier.AccountID(os.Getenv("STEADFAST_ACCOUNT_ID")),
			}
			if s.baseURL == DefaultBaseURL {
				if u := strings.TrimSpace(os.Getenv("STEADFAST_BASE_URL")); u != "" {
					c.baseURL = strings.TrimRight(u, "/")
				}
			}
		}
		if err := creds.validate(); err != nil { // AUTH-07: fail on missing local credentials
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
// the STEADFAST_API_KEY / STEADFAST_SECRET_KEY environment variables, and
// construction fails if either is missing. No network call is made.
func New(opts ...Option) (*Client, error) { return build(auth.ModeGlobal, opts) }

// NewTenant creates a tenant-mode client that never reads environment or
// global credentials; every call needs ContextWithCredentials.
func NewTenant(opts ...Option) (*Client, error) { return build(auth.ModeTenant, opts) }

// Mode reports the client's immutable credential mode.
func (c *Client) Mode() auth.Mode { return c.creds.Mode() }

func errMsg(_ int, body []byte) (string, string) { return httpx.JSONMessage(0, body) }

// call resolves credentials (before any dispatch) and performs the request.
func (c *Client) call(ctx context.Context, op, method, path string, payload any, mutation bool) (*httpx.Response, Credentials, error) {
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
	}
	resp, err := c.http.Do(ctx, httpx.Request{
		Op: op, Method: method, URL: c.baseURL + path, Body: body, Mutation: mutation, ErrorMessage: errMsg,
		Auth: http.Header{"Api-Key": {creds.APIKey.Reveal()}, "Secret-Key": {creds.SecretKey.Reveal()}},
	})
	return resp, creds, err
}

// checkEmbedded turns an HTTP 200 body that embeds a failure into an error.
func checkEmbedded(op string, env envelope) error {
	s := env.Status.String()
	if s == "" || s == "200" || s == "201" || s == "1" {
		return nil
	}
	e := &courier.Error{Provider: ProviderID, Op: op, Message: truncateMsg(env.Message), Outcome: courier.OutcomeRejected}
	switch s {
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
	}
	return e
}

func truncateMsg(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// CreateShipment creates one consignment. Money must be BDT. A timeout or 5xx
// is returned as an indeterminate error; reconcile by merchant order ID.
func (c *Client) CreateShipment(ctx context.Context, req courier.CreateShipmentRequest) (*courier.Shipment, error) {
	order, err := buildOrder(req)
	if err != nil {
		return nil, err
	}
	resp, creds, err := c.call(ctx, shipmentCreateOp, http.MethodPost, "/create_order", order, true)
	if err != nil {
		return nil, err
	}
	var out createResponse
	if err := httpx.DecodeJSON(ProviderID, shipmentCreateOp, resp.Body, &out); err != nil {
		return nil, asIndeterminate(err)
	}
	if err := checkEmbedded(shipmentCreateOp, out.envelope); err != nil {
		return nil, err
	}
	if out.Consignment == nil || out.Consignment.ConsignmentID.String() == "" {
		return nil, indeterminateProtocol(shipmentCreateOp, "response lacks a consignment")
	}
	sh, err := mapConsignment(creds.AccountID, out.Consignment, resp.Body)
	if err != nil {
		return nil, asIndeterminate(err)
	}
	return sh, nil
}

// asIndeterminate marks a malformed response to a mutation as indeterminate:
// the request was sent, so the shipment may exist.
func asIndeterminate(err error) error {
	return &courier.Error{Kind: courier.KindIndeterminate, Provider: ProviderID, Outcome: courier.OutcomeIndeterminate,
		Message: "unusable response to a state-changing request; the operation may have been applied", Err: err}
}

func indeterminateProtocol(op, msg string) error {
	return asIndeterminate(&courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: op, Message: msg})
}

// CreateShipments creates up to 500 consignments in one request and retains a
// per-item receipt aligned to the request slice (ERR-04). Items failing local
// validation are BatchFailed and are not sent. A non-nil result may accompany
// a non-nil error.
func (c *Client) CreateShipments(ctx context.Context, reqs []courier.CreateShipmentRequest) (*courier.BatchResult, error) {
	if len(reqs) == 0 {
		return nil, courier.Validation("shipments", "at least one shipment is required")
	}
	if len(reqs) > MaxBulk {
		return nil, courier.Validation("shipments", fmt.Sprintf("at most %d shipments per bulk request", MaxBulk))
	}
	res := &courier.BatchResult{Items: make([]courier.BatchItem, len(reqs))}
	var orders []orderRequest
	var sentIdx []int         // request index of each sent order, in order
	index := map[string]int{} // invoice -> request index
	var errs []error
	for i, r := range reqs {
		res.Items[i] = courier.BatchItem{Index: i, Status: courier.BatchNotAttempted}
		o, err := buildOrder(r)
		if err == nil {
			if _, dup := index[o.Invoice]; dup {
				err = courier.Validation("merchant_order_id", "duplicate merchant order ID within the batch")
			}
		}
		if err != nil {
			res.Items[i].Status, res.Items[i].Err = courier.BatchFailed, err
			errs = append(errs, fmt.Errorf("item %d: %w", i, err))
			continue
		}
		index[o.Invoice] = i
		sentIdx = append(sentIdx, i)
		orders = append(orders, o)
	}
	if len(orders) == 0 {
		return res, joinErr(errs)
	}
	resp, creds, err := c.call(ctx, shipmentBatchOp, http.MethodPost, "/create_order/bulk-order", bulkRequest{Data: orders}, true)
	if err != nil {
		// Whole-request failure: sent items are unresolved, not failed.
		st := courier.BatchFailed
		if courier.IsIndeterminate(err) {
			st = courier.BatchIndeterminate
		}
		for _, i := range index {
			if res.Items[i].Status == courier.BatchNotAttempted {
				res.Items[i].Status, res.Items[i].Err = st, err
			}
		}
		return res, joinErr(append(errs, err))
	}
	res.Raw = resp.Body
	items, err := decodeBulkItems(resp.Body)
	if err != nil {
		// A 200 that is not a list may be an embedded failure envelope.
		var env envelope
		if httpx.DecodeJSON(ProviderID, shipmentBatchOp, resp.Body, &env) == nil {
			if e := checkEmbedded(shipmentBatchOp, env); e != nil {
				for _, i := range index {
					res.Items[i].Status, res.Items[i].Err = courier.BatchFailed, e
				}
				return res, joinErr(append(errs, e))
			}
		}
		for _, i := range index {
			res.Items[i].Status, res.Items[i].Err = courier.BatchIndeterminate, asIndeterminate(err)
		}
		return res, joinErr(append(errs, asIndeterminate(err)))
	}
	// The provider answers in request order, one entry per order: match by
	// position when the counts agree (cross-checking the invoice), otherwise by
	// invoice.
	byPosition := len(items) == len(orders)
	for k, it := range items {
		it.normalise()
		var i int
		var ok bool
		if byPosition {
			i, ok = sentIdx[k], it.Invoice == "" || it.Invoice == orders[k].Invoice
		} else {
			i, ok = index[it.Invoice]
		}
		if !ok || res.Items[i].Status != courier.BatchNotAttempted {
			errs = append(errs, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: shipmentBatchOp,
				Message: "response item does not match a submitted order"})
			continue
		}
		raw, _ := json.Marshal(it)
		if it.errText() != "" || strings.EqualFold(it.Status, "error") || it.ConsignmentID.String() == "" || it.ConsignmentID.String() == "null" {
			msg := it.errText()
			if msg == "" {
				msg = "provider rejected the item"
			}
			e := &courier.Error{Kind: courier.KindRejected, Provider: ProviderID, Op: shipmentBatchOp, Message: truncateMsg(msg), Outcome: courier.OutcomeRejected}
			res.Items[i].Status, res.Items[i].Err = courier.BatchFailed, e
			errs = append(errs, fmt.Errorf("item %d: %w", i, e))
			continue
		}
		sh, err := mapConsignment(creds.AccountID, &consignment{ConsignmentID: it.ConsignmentID, Invoice: it.Invoice,
			TrackingCode: it.TrackingCode, Status: it.Status, CODAmount: it.CODAmount}, raw)
		if err != nil {
			res.Items[i].Status, res.Items[i].Err = courier.BatchIndeterminate, asIndeterminate(err)
			errs = append(errs, fmt.Errorf("item %d: %w", i, res.Items[i].Err))
			continue
		}
		if strings.EqualFold(it.Status, "success") {
			// Bulk items report the creation outcome, not a delivery status; the
			// raw "success" stays in ProviderStatus.
			sh.Status = courier.StatusCreated
		}
		res.Items[i].Status, res.Items[i].Shipment = courier.BatchSucceeded, sh
	}
	for _, i := range index { // sent but unanswered
		if res.Items[i].Status == courier.BatchNotAttempted {
			e := indeterminateProtocol(shipmentBatchOp, "item missing from response")
			res.Items[i].Status, res.Items[i].Err = courier.BatchIndeterminate, e
			errs = append(errs, fmt.Errorf("item %d: %w", i, e))
		}
	}
	return res, joinErr(errs)
}

func joinErr(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errorsJoin(errs)
}

// TrackShipment looks up the current status. Steadfast exposes no history, so
// the result declares CoverageCurrentOnly and has no events (TRK-06/07).
func (c *Client) TrackShipment(ctx context.Context, req courier.TrackingRequest) (*courier.TrackingResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	path, err := trackingPath(req)
	if err != nil {
		return nil, err
	}
	resp, creds, err := c.call(ctx, trackingGetOp, http.MethodGet, path, nil, false)
	if err != nil {
		return nil, err
	}
	var out statusResponse
	if err := httpx.DecodeJSON(ProviderID, trackingGetOp, resp.Body, &out); err != nil {
		return nil, err
	}
	if err := checkEmbedded(trackingGetOp, out.envelope); err != nil {
		return nil, err
	}
	if out.DeliveryStatus == nil || strings.TrimSpace(*out.DeliveryStatus) == "" {
		return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: trackingGetOp, Message: "response lacks delivery_status"}
	}
	return mapStatus(creds.AccountID, req, *out.DeliveryStatus, resp.Body, time.Now().UTC()), nil
}

// GetBalance returns the merchant's current balance in BDT.
func (c *Client) GetBalance(ctx context.Context) (*courier.Balance, error) {
	resp, creds, err := c.call(ctx, balanceGetOp, http.MethodGet, "/get_balance", nil, false)
	if err != nil {
		return nil, err
	}
	var out balanceResponse
	if err := httpx.DecodeJSON(ProviderID, balanceGetOp, resp.Body, &out); err != nil {
		return nil, err
	}
	if err := checkEmbedded(balanceGetOp, out.envelope); err != nil {
		return nil, err
	}
	d, ok, err := courier.ParseAmount(toAmountInput(out.CurrentBalance))
	if err != nil || !ok {
		return nil, &courier.Error{Kind: courier.KindProtocol, Provider: ProviderID, Op: balanceGetOp, Message: "missing or invalid current_balance", Err: err}
	}
	return &courier.Balance{Provider: ProviderID, Account: creds.AccountID, Amount: courier.NewBDT(d), Raw: resp.Body}, nil
}

// Ping calls the unauthenticated /ping endpoint: if it succeeds, the base URL
// is right and the network can reach Steadfast, so later failures are about
// credentials or the request. It sends no credentials and works in both modes.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.http.Do(ctx, httpx.Request{Op: "ping", Method: http.MethodGet, URL: c.baseURL + "/ping", ErrorMessage: errMsg})
	return err
}
