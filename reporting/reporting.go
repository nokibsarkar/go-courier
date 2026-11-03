// Package reporting fans an incident report out to multiple typed
// CustomerIncidentReporter consumers through an immutable dispatcher created
// at application startup (RPT-01..09). It is a typed dispatcher plus policy
// strategy, not a global hook system.
//
// Reporting is always an explicit, authorized action: the dispatcher requires
// an [Authorizer], runs a preflight over every selected destination before any
// submission, never retries automatically and never rolls back accepted
// reports.
package reporting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	courier "github.com/nokibsarkar/go-courier"
)

// ErrNoReportTargets means the selection was explicitly or effectively empty.
var ErrNoReportTargets = errors.New("reporting: no report targets selected")

// Consumer is a registered destination. Eligibility is optional and checks
// provider-specific eligibility (account, region) before submission.
type Consumer struct {
	ID          courier.ConsumerID
	Reporter    courier.CustomerIncidentReporter
	Eligibility func(context.Context, Request) error
}

// Request is the dispatch envelope, distinct from incident evidence (RPT-04).
type Request struct {
	Incident courier.CustomerIncidentReport
	// Targets: nil means unspecified; a non-nil empty slice means explicitly none.
	Targets []courier.ConsumerID
}

// Policy selects the destinations for one report.
type Policy interface {
	Select(context.Context, Request, []courier.ConsumerID) ([]courier.ConsumerID, error)
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(context.Context, Request, []courier.ConsumerID) ([]courier.ConsumerID, error)

// Select implements Policy.
func (f PolicyFunc) Select(ctx context.Context, r Request, all []courier.ConsumerID) ([]courier.ConsumerID, error) {
	return f(ctx, r, all)
}

// DefaultPolicy honours explicit Targets; otherwise selects all registered
// consumers. A non-nil empty Targets yields ErrNoReportTargets.
type DefaultPolicy struct{}

// Select implements Policy.
func (DefaultPolicy) Select(_ context.Context, r Request, all []courier.ConsumerID) ([]courier.ConsumerID, error) {
	if r.Targets == nil {
		return append([]courier.ConsumerID(nil), all...), nil
	}
	if len(r.Targets) == 0 {
		return nil, ErrNoReportTargets
	}
	return append([]courier.ConsumerID(nil), r.Targets...), nil
}

// Authorizer decides whether disclosure of this report to this destination is
// legally and contractually permitted. Registration alone never authorizes
// disclosure (RPT-05). The application owns this decision.
type Authorizer interface {
	AuthorizeReport(context.Context, courier.ConsumerID, Request) error
}

// AuthorizerFunc adapts a function to Authorizer.
type AuthorizerFunc func(context.Context, courier.ConsumerID, Request) error

// AuthorizeReport implements Authorizer.
func (f AuthorizerFunc) AuthorizeReport(ctx context.Context, id courier.ConsumerID, r Request) error {
	return f(ctx, id, r)
}

// Status is a per-consumer outcome.
type Status string

const (
	StatusAccepted      Status = "accepted"
	StatusFailed        Status = "failed"
	StatusIndeterminate Status = "indeterminate"
	StatusNotAttempted  Status = "not_attempted"
)

// ConsumerResult is one consumer's receipt or error.
type ConsumerResult struct {
	Consumer courier.ConsumerID
	Status   Status
	Receipt  *courier.IncidentReceipt
	Err      error
}

// Result retains a result per selected consumer in selection order (RPT-07).
type Result struct {
	Results []ConsumerResult
}

// Dispatcher is immutable after New and safe for concurrent use.
type Dispatcher struct {
	consumers   map[courier.ConsumerID]Consumer
	order       []courier.ConsumerID
	policy      Policy
	authorizer  Authorizer
	concurrency int
	log         *slog.Logger
}

// Option configures New.
type Option func(*Dispatcher) error

// WithConsumers registers consumers. nil reporters/typed-nil and duplicate or
// empty IDs are rejected.
func WithConsumers(cs ...Consumer) Option {
	return func(d *Dispatcher) error {
		for _, c := range cs {
			if c.ID == "" {
				return courier.Configuration("", "reporting consumer ID is required")
			}
			if isNil(c.Reporter) {
				return courier.Configuration("", fmt.Sprintf("reporting consumer %q has a nil reporter", c.ID))
			}
			if _, dup := d.consumers[c.ID]; dup {
				return courier.Configuration("", fmt.Sprintf("duplicate reporting consumer %q", c.ID))
			}
			d.consumers[c.ID] = c
			d.order = append(d.order, c.ID)
		}
		return nil
	}
}

// WithPolicy replaces the default policy entirely (RPT-02). A nil policy is an
// error, not a silent fallback.
func WithPolicy(p Policy) Option {
	return func(d *Dispatcher) error {
		if isNil(p) {
			return courier.Configuration("", "reporting policy must not be nil")
		}
		d.policy = p
		return nil
	}
}

// WithAuthorizer sets the mandatory disclosure authorizer.
func WithAuthorizer(a Authorizer) Option {
	return func(d *Dispatcher) error {
		if isNil(a) {
			return courier.Configuration("", "reporting authorizer must not be nil")
		}
		d.authorizer = a
		return nil
	}
}

// WithConcurrency bounds parallel submissions (default 4).
func WithConcurrency(n int) Option {
	return func(d *Dispatcher) error {
		if n < 1 {
			return courier.Configuration("", "concurrency must be at least 1")
		}
		d.concurrency = n
		return nil
	}
}

// WithLogger sets an optional logger; default is no-op. Incident contents are
// never logged (PRIV-02).
func WithLogger(l *slog.Logger) Option {
	return func(d *Dispatcher) error { d.log = l; return nil }
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

// New builds a Dispatcher. It requires at least one consumer and an Authorizer.
func New(opts ...Option) (*Dispatcher, error) {
	d := &Dispatcher{consumers: map[courier.ConsumerID]Consumer{}, policy: DefaultPolicy{}, concurrency: 4}
	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}
	if len(d.consumers) == 0 {
		return nil, courier.Configuration("", "at least one reporting consumer is required")
	}
	if d.authorizer == nil {
		return nil, courier.Configuration("", "an Authorizer is required: registration alone does not authorize disclosure")
	}
	d.log = courier.LoggerOrNop(d.log)
	return d, nil
}

// Report selects destinations, preflights ALL of them, then submits with
// bounded concurrency and best-effort failure isolation. It returns per-
// consumer results plus an errors.Join aggregate if any destination failed or
// was indeterminate. A preflight failure aborts before any submission and
// returns every preflight error with a nil Result.
func (d *Dispatcher) Report(ctx context.Context, req Request) (*Result, error) {
	ids, err := d.policy.Select(ctx, req, append([]courier.ConsumerID(nil), d.order...))
	if err != nil {
		return nil, fmt.Errorf("reporting: policy selection failed: %w", err)
	}
	if len(ids) == 0 {
		return nil, ErrNoReportTargets
	}

	// Preflight: registration, duplicates, eligibility, authorization.
	var pre []error
	seen := map[courier.ConsumerID]bool{}
	for _, id := range ids {
		c, ok := d.consumers[id]
		switch {
		case !ok:
			pre = append(pre, fmt.Errorf("reporting: consumer %q is not registered", id))
			continue
		case seen[id]:
			pre = append(pre, fmt.Errorf("reporting: consumer %q selected more than once", id))
			continue
		}
		seen[id] = true
		if err := d.authorizer.AuthorizeReport(ctx, id, req); err != nil {
			pre = append(pre, fmt.Errorf("reporting: consumer %q not authorized: %w", id, err))
			continue
		}
		if c.Eligibility != nil {
			if err := c.Eligibility(ctx, req); err != nil {
				pre = append(pre, fmt.Errorf("reporting: consumer %q not eligible: %w", id, err))
			}
		}
	}
	if len(pre) > 0 {
		return nil, errors.Join(pre...)
	}

	res := &Result{Results: make([]ConsumerResult, len(ids))}
	sem := make(chan struct{}, d.concurrency)
	var wg sync.WaitGroup
	for i, id := range ids {
		res.Results[i] = ConsumerResult{Consumer: id, Status: StatusNotAttempted}
		// Acquire in selection order so slots are granted deterministically
		// and cancellation stops all not-yet-started submissions.
		if err := ctx.Err(); err != nil {
			res.Results[i].Err = err
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			res.Results[i].Err = ctx.Err()
			continue
		}
		if err := ctx.Err(); err != nil {
			<-sem
			res.Results[i].Err = err
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rc, err := d.consumers[id].Reporter.ReportCustomerIncident(ctx, req.Incident)
			cr := &res.Results[i]
			switch {
			case err == nil && rc != nil:
				cr.Status, cr.Receipt = StatusAccepted, rc
			case err == nil:
				cr.Status = StatusFailed
				cr.Err = &courier.Error{Kind: courier.KindProtocol, Message: "consumer returned neither receipt nor error"}
			case courier.IsIndeterminate(err):
				cr.Status, cr.Err = StatusIndeterminate, err
			default:
				cr.Status, cr.Err = StatusFailed, err
			}
			d.log.LogAttrs(ctx, slog.LevelInfo, "incident dispatched", slog.String("consumer", string(id)), slog.String("status", string(cr.Status)))
		}()
	}
	wg.Wait()

	var errs []error
	for i := range res.Results {
		r := res.Results[i]
		if r.Status != StatusAccepted {
			if r.Err == nil {
				r.Err = errors.New("not attempted")
			}
			errs = append(errs, fmt.Errorf("reporting: consumer %q: %w", r.Consumer, r.Err))
		}
	}
	return res, errors.Join(errs...)
}
