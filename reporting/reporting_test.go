package reporting

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	courier "github.com/nokibsarkar/go-courier"
)

type rep struct {
	calls atomic.Int32
	fn    func(ctx context.Context) (*courier.IncidentReceipt, error)
}

func (r *rep) ReportCustomerIncident(ctx context.Context, _ courier.CustomerIncidentReport) (*courier.IncidentReceipt, error) {
	r.calls.Add(1)
	if r.fn != nil {
		return r.fn(ctx)
	}
	return &courier.IncidentReceipt{ID: "ok"}, nil
}

func allow(context.Context, courier.ConsumerID, Request) error { return nil }

func build(t *testing.T, opts ...Option) *Dispatcher {
	t.Helper()
	d, err := New(append([]Option{WithAuthorizer(AuthorizerFunc(allow))}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestConstructionRules(t *testing.T) {
	r := &rep{}
	var nilRep *rep
	for name, opts := range map[string][]Option{
		"no consumers":  {WithAuthorizer(AuthorizerFunc(allow))},
		"no authorizer": {WithConsumers(Consumer{ID: "a", Reporter: r})},
		"nil reporter":  {WithAuthorizer(AuthorizerFunc(allow)), WithConsumers(Consumer{ID: "a"})},
		"typed nil":     {WithAuthorizer(AuthorizerFunc(allow)), WithConsumers(Consumer{ID: "a", Reporter: nilRep})},
		"dup":           {WithAuthorizer(AuthorizerFunc(allow)), WithConsumers(Consumer{ID: "a", Reporter: r}, Consumer{ID: "a", Reporter: r})},
		"empty id":      {WithAuthorizer(AuthorizerFunc(allow)), WithConsumers(Consumer{Reporter: r})},
		"nil policy":    {WithAuthorizer(AuthorizerFunc(allow)), WithConsumers(Consumer{ID: "a", Reporter: r}), WithPolicy(nil)},
	} {
		if _, err := New(opts...); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestDefaultPolicyNilVersusEmptyTargets(t *testing.T) {
	a, b := &rep{}, &rep{}
	d := build(t, WithConsumers(Consumer{ID: "a", Reporter: a}, Consumer{ID: "b", Reporter: b}))
	res, err := d.Report(context.Background(), Request{})
	if err != nil || len(res.Results) != 2 || res.Results[0].Consumer != "a" || res.Results[1].Consumer != "b" {
		t.Fatalf("nil targets -> all in order: %+v %v", res, err)
	}
	if _, err := d.Report(context.Background(), Request{Targets: []courier.ConsumerID{}}); !errors.Is(err, ErrNoReportTargets) {
		t.Fatalf("empty targets: %v", err)
	}
	res, err = d.Report(context.Background(), Request{Targets: []courier.ConsumerID{"b"}})
	if err != nil || len(res.Results) != 1 || res.Results[0].Consumer != "b" {
		t.Fatal(res, err)
	}
}

func TestCustomPolicyReplacesDefaultAndErrorsDoNotFallBack(t *testing.T) {
	a, b := &rep{}, &rep{}
	pol := PolicyFunc(func(_ context.Context, _ Request, all []courier.ConsumerID) ([]courier.ConsumerID, error) {
		return all[1:], nil
	})
	d := build(t, WithConsumers(Consumer{ID: "a", Reporter: a}, Consumer{ID: "b", Reporter: b}), WithPolicy(pol))
	if _, err := d.Report(context.Background(), Request{}); err != nil || a.calls.Load() != 0 || b.calls.Load() != 1 {
		t.Fatalf("custom policy ignored: a=%d b=%d %v", a.calls.Load(), b.calls.Load(), err)
	}
	bad := PolicyFunc(func(context.Context, Request, []courier.ConsumerID) ([]courier.ConsumerID, error) {
		return nil, errors.New("policy boom")
	})
	d = build(t, WithConsumers(Consumer{ID: "a", Reporter: a}), WithPolicy(bad))
	before := a.calls.Load()
	if _, err := d.Report(context.Background(), Request{}); err == nil || a.calls.Load() != before {
		t.Fatal("policy error must not fall back to default")
	}
	empty := PolicyFunc(func(context.Context, Request, []courier.ConsumerID) ([]courier.ConsumerID, error) { return nil, nil })
	d = build(t, WithConsumers(Consumer{ID: "a", Reporter: a}), WithPolicy(empty))
	if _, err := d.Report(context.Background(), Request{}); !errors.Is(err, ErrNoReportTargets) {
		t.Fatal(err)
	}
}

func TestPreflightAbortsBeforeAnySubmission(t *testing.T) {
	a, b := &rep{}, &rep{}
	deny := AuthorizerFunc(func(_ context.Context, id courier.ConsumerID, _ Request) error {
		if id == "b" {
			return errors.New("no data-sharing agreement")
		}
		return nil
	})
	d, _ := New(WithAuthorizer(deny), WithConsumers(
		Consumer{ID: "a", Reporter: a},
		Consumer{ID: "b", Reporter: b},
		Consumer{ID: "c", Reporter: &rep{}, Eligibility: func(context.Context, Request) error { return errors.New("region") }},
	))
	res, err := d.Report(context.Background(), Request{})
	if err == nil || res != nil || a.calls.Load() != 0 || b.calls.Load() != 0 {
		t.Fatalf("preflight must abort everything: %v a=%d", err, a.calls.Load())
	}
	// Both failures reported.
	if joined, ok := err.(interface{ Unwrap() []error }); !ok || len(joined.Unwrap()) != 2 {
		t.Fatalf("want 2 preflight errors, got %v", err)
	}
	if _, err := d.Report(context.Background(), Request{Targets: []courier.ConsumerID{"zzz"}}); err == nil {
		t.Fatal("unregistered target")
	}
	if _, err := d.Report(context.Background(), Request{Targets: []courier.ConsumerID{"a", "a"}}); err == nil {
		t.Fatal("duplicate target")
	}
}

func TestFailureIsolationOrderAndJoin(t *testing.T) {
	ok := &rep{}
	failing := &rep{fn: func(context.Context) (*courier.IncidentReceipt, error) { return nil, errors.New("down") }}
	indet := &rep{fn: func(context.Context) (*courier.IncidentReceipt, error) {
		return nil, &courier.Error{Kind: courier.KindIndeterminate, Outcome: courier.OutcomeIndeterminate}
	}}
	d := build(t, WithConsumers(Consumer{ID: "a", Reporter: failing}, Consumer{ID: "b", Reporter: ok}, Consumer{ID: "c", Reporter: indet}))
	res, err := d.Report(context.Background(), Request{})
	if err == nil {
		t.Fatal("aggregate error expected")
	}
	if ok.calls.Load() != 1 {
		t.Fatal("one failure cancelled another submission")
	}
	want := []Status{StatusFailed, StatusAccepted, StatusIndeterminate}
	for i, w := range want {
		if res.Results[i].Status != w {
			t.Errorf("result %d = %s want %s", i, res.Results[i].Status, w)
		}
	}
	if res.Results[1].Receipt == nil {
		t.Fatal("success receipt retained")
	}
}

func TestCallerCancellationStopsPendingNotIndependentCalls(t *testing.T) {
	gate := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)
	slow := &rep{fn: func(ctx context.Context) (*courier.IncidentReceipt, error) {
		started.Done()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-gate:
			return &courier.IncidentReceipt{}, nil
		}
	}}
	waiting := &rep{}
	d := build(t, WithConcurrency(1), WithConsumers(Consumer{ID: "a", Reporter: slow}, Consumer{ID: "b", Reporter: waiting}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := d.Report(ctx, Request{}); done <- err }()
	started.Wait()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation not honored")
	}
	if waiting.calls.Load() != 0 {
		t.Fatal("not-attempted consumer was called after cancellation")
	}
}
