// Package intelligence aggregates independent CustomerRiskReader sources. It
// preserves every source's raw score/metrics and provenance, never merges
// ratings (RISK-04), never invents exact combined totals when overlap is
// unknown (RISK-03) and never turns missing counts into zero (RISK-02).
package intelligence

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"

	courier "github.com/nokibsarkar/go-courier"
)

// Source is a named risk reader.
type Source struct {
	ID     courier.ProviderID
	Reader courier.CustomerRiskReader
}

// SourceResult is one source's outcome, in registration order.
type SourceResult struct {
	Source courier.ProviderID
	Risk   *courier.CustomerRisk
	Err    error
}

// Bounds is an inclusive range for a combined count. Max is nil when it
// cannot be bounded because a source did not report the count.
type Bounds struct {
	Min int
	Max *int
}

// CombinedBounds are valid bounds on the true combined count under unknown
// overlap: Min is the largest single-source value; Max is the sum. They are
// not an exact total.
type CombinedBounds struct {
	Total, Delivered, Cancelled, Returned *Bounds
}

// Aggregate holds per-source results and, when at least one source answered,
// overlap-safe bounds. ExactTotalsAvailable is always false: overlap between
// independent sources cannot be established by this package.
type Aggregate struct {
	Results              []SourceResult
	Bounds               *CombinedBounds
	ExactTotalsAvailable bool
}

// Err joins per-source failures (nil when all sources succeeded).
func (a *Aggregate) Err() error {
	var errs []error
	for _, r := range a.Results {
		if r.Err != nil {
			errs = append(errs, r.Err)
		}
	}
	return errors.Join(errs...)
}

// Aggregator fans a request out to sources with bounded concurrency.
type Aggregator struct {
	sources     []Source
	concurrency int
	log         *slog.Logger
}

// Option configures New.
type Option func(*Aggregator)

// WithSources registers sources; IDs must be unique and readers non-nil.
func WithSources(s ...Source) Option {
	return func(a *Aggregator) { a.sources = append(a.sources, s...) }
}

// WithConcurrency bounds parallel source calls (default 4).
func WithConcurrency(n int) Option { return func(a *Aggregator) { a.concurrency = n } }

// WithLogger sets an optional logger; default is no-op.
func WithLogger(l *slog.Logger) Option { return func(a *Aggregator) { a.log = l } }

// New builds an Aggregator depending only on CustomerRiskReader.
func New(opts ...Option) (*Aggregator, error) {
	a := &Aggregator{concurrency: 4}
	for _, o := range opts {
		o(a)
	}
	if a.concurrency < 1 {
		return nil, courier.Configuration("", "concurrency must be at least 1")
	}
	if len(a.sources) == 0 {
		return nil, courier.Configuration("", "at least one risk source is required")
	}
	seen := map[courier.ProviderID]bool{}
	for _, s := range a.sources {
		if s.ID == "" || isNil(s.Reader) {
			return nil, courier.Configuration("", "risk source requires an ID and a non-nil reader")
		}
		if seen[s.ID] {
			return nil, courier.Configuration("", "duplicate risk source ID "+string(s.ID))
		}
		seen[s.ID] = true
	}
	a.log = courier.LoggerOrNop(a.log)
	return a, nil
}

func isNil(r courier.CustomerRiskReader) bool {
	if r == nil {
		return true
	}
	v := reflect.ValueOf(r)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// GetCustomerRisk queries all sources. One source failing never cancels the
// others; caller cancellation does.
func (a *Aggregator) GetCustomerRisk(ctx context.Context, req courier.CustomerRiskRequest) (*Aggregate, error) {
	res := make([]SourceResult, len(a.sources))
	sem := make(chan struct{}, a.concurrency)
	var wg sync.WaitGroup
	for i, s := range a.sources {
		res[i].Source = s.ID
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				res[i].Err = ctx.Err()
				return
			}
			if err := ctx.Err(); err != nil {
				res[i].Err = err
				return
			}
			risk, err := s.Reader.GetCustomerRisk(ctx, req)
			if err == nil && risk == nil {
				err = &courier.Error{Kind: courier.KindProtocol, Provider: s.ID, Message: "source returned no result"}
			}
			res[i].Risk, res[i].Err = risk, err
			a.log.LogAttrs(ctx, slog.LevelDebug, "risk source done", slog.String("source", string(s.ID)), slog.Bool("ok", err == nil))
		}()
	}
	wg.Wait()
	agg := &Aggregate{Results: res, Bounds: bounds(res)}
	return agg, agg.Err()
}

func bounds(res []SourceResult) *CombinedBounds {
	var ok []*courier.CustomerRisk
	for _, r := range res {
		if r.Err == nil && r.Risk != nil {
			ok = append(ok, r.Risk)
		}
	}
	if len(ok) == 0 {
		return nil
	}
	pick := func(f func(*courier.DeliveryCounts) *int) *Bounds {
		minV, sum, seen, all := 0, 0, false, true
		for _, r := range ok {
			var p *int
			if r.Counts != nil {
				p = f(r.Counts)
			}
			if p == nil {
				all = false
				continue
			}
			seen = true
			sum += *p
			minV = max(minV, *p)
		}
		if !seen {
			return nil
		}
		b := &Bounds{Min: minV}
		if all {
			b.Max = &sum
		}
		return b
	}
	return &CombinedBounds{
		Total:     pick(func(c *courier.DeliveryCounts) *int { return c.Total }),
		Delivered: pick(func(c *courier.DeliveryCounts) *int { return c.Delivered }),
		Cancelled: pick(func(c *courier.DeliveryCounts) *int { return c.Cancelled }),
		Returned:  pick(func(c *courier.DeliveryCounts) *int { return c.Returned }),
	}
}
