// Command reporting shows the reporting dispatcher wiring with an explicit
// authorizer. The consumers here are stubs: the SDK ships no incident-report
// provider because no documented submission API has been verified (PROV-03).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/reporting"
)

type stub struct{ id string }

func (s stub) ReportCustomerIncident(context.Context, courier.CustomerIncidentReport) (*courier.IncidentReceipt, error) {
	return &courier.IncidentReceipt{ID: courier.IncidentID("receipt-" + s.id), Status: "accepted"}, nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	d, err := reporting.New(
		reporting.WithConsumers(
			reporting.Consumer{ID: "bureau-a", Reporter: stub{"a"}},
			reporting.Consumer{ID: "bureau-b", Reporter: stub{"b"}},
		),
		// Registration alone never authorizes disclosure: the application decides.
		reporting.WithAuthorizer(reporting.AuthorizerFunc(func(_ context.Context, id courier.ConsumerID, _ reporting.Request) error {
			if id == "bureau-b" {
				return errors.New("no data-sharing agreement")
			}
			return nil
		})),
		reporting.WithLogger(logger),
	)
	if err != nil {
		log.Fatal(err)
	}
	// nil Targets => all registered consumers; preflight fails for bureau-b, so nothing is sent.
	_, err = d.Report(context.Background(), reporting.Request{Incident: courier.CustomerIncidentReport{
		Phone: "+8801711111111", Summary: "Recipient refused a confirmed COD parcel at the door.", Source: "merchant"}})
	fmt.Println("error:", err)

	res, err := d.Report(context.Background(), reporting.Request{
		Incident: courier.CustomerIncidentReport{Phone: "+8801711111111", Summary: "factual summary", Source: "merchant"},
		Targets:  []courier.ConsumerID{"bureau-a"},
	})
	fmt.Println(res.Results[0].Consumer, res.Results[0].Status, err)
}
