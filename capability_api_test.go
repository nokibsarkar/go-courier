package courier_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	courier "github.com/nokibsarkar/go-courier"
)

// QA-15 / ARC-03: published capability interfaces must not gain, lose or
// change methods within a major version. This golden table is the freeze; a
// failing test means an accidental breaking change (add a NEW interface
// instead, or bump the major version and update the table deliberately).
func TestCapabilityInterfacesAreFrozen(t *testing.T) {
	golden := map[string][]string{
		"ShipmentCreator":          {"CreateShipment(context.Context, courier.CreateShipmentRequest) (*courier.Shipment, error)"},
		"BatchShipmentCreator":     {"CreateShipments(context.Context, []courier.CreateShipmentRequest) (*courier.BatchResult, error)"},
		"ShipmentReader":           {"GetShipment(context.Context, courier.ShipmentRef) (*courier.Shipment, error)"},
		"ShipmentCanceller":        {"CancelShipment(context.Context, courier.ShipmentRef) error"},
		"ShipmentTracker":          {"TrackShipment(context.Context, courier.TrackingRequest) (*courier.TrackingResult, error)"},
		"BulkShipmentTracker":      {"TrackShipments(context.Context, []courier.TrackingRequest) ([]courier.TrackingOutcome, error)"},
		"ProofOfDeliveryReader":    {"GetProofOfDelivery(context.Context, courier.ShipmentRef) (*courier.Document, error)"},
		"ShipmentLabelReader":      {"GetShipmentLabel(context.Context, courier.ShipmentRef) (*courier.Document, error)"},
		"Quoter":                   {"Quote(context.Context, courier.QuoteRequest) (*courier.Quote, error)"},
		"QuoteReader":              {"GetQuote(context.Context, courier.QuoteID) (*courier.Quote, error)"},
		"ServiceabilityChecker":    {"CheckServiceability(context.Context, courier.ServiceabilityRequest) (*courier.Serviceability, error)"},
		"PickupRequester":          {"RequestPickup(context.Context, courier.PickupRequest) (*courier.Pickup, error)"},
		"ReturnRequester":          {"RequestReturn(context.Context, courier.ReturnRequest) (*courier.ReturnReceipt, error)"},
		"BalanceReader":            {"GetBalance(context.Context) (*courier.Balance, error)"},
		"CustomerRiskReader":       {"GetCustomerRisk(context.Context, courier.CustomerRiskRequest) (*courier.CustomerRisk, error)"},
		"CustomerIncidentReporter": {"ReportCustomerIncident(context.Context, courier.CustomerIncidentReport) (*courier.IncidentReceipt, error)"},
		"WebhookHandlerProvider":   {"WebhookHandler(courier.EventHandler) (http.Handler, error)"},
		"WebhookDecoder":           {"DecodeWebhook(context.Context, http.Header, []byte) ([]courier.Event, error)"},
		"EventHandler":             {"HandleEvent(context.Context, courier.Event) error"},
	}
	ifaces := map[string]reflect.Type{
		"ShipmentCreator": reflect.TypeOf((*courier.ShipmentCreator)(nil)).Elem(), "BatchShipmentCreator": reflect.TypeOf((*courier.BatchShipmentCreator)(nil)).Elem(),
		"ShipmentReader": reflect.TypeOf((*courier.ShipmentReader)(nil)).Elem(), "ShipmentCanceller": reflect.TypeOf((*courier.ShipmentCanceller)(nil)).Elem(),
		"ShipmentTracker": reflect.TypeOf((*courier.ShipmentTracker)(nil)).Elem(), "BulkShipmentTracker": reflect.TypeOf((*courier.BulkShipmentTracker)(nil)).Elem(),
		"ProofOfDeliveryReader": reflect.TypeOf((*courier.ProofOfDeliveryReader)(nil)).Elem(), "ShipmentLabelReader": reflect.TypeOf((*courier.ShipmentLabelReader)(nil)).Elem(),
		"Quoter": reflect.TypeOf((*courier.Quoter)(nil)).Elem(), "QuoteReader": reflect.TypeOf((*courier.QuoteReader)(nil)).Elem(),
		"ServiceabilityChecker": reflect.TypeOf((*courier.ServiceabilityChecker)(nil)).Elem(), "PickupRequester": reflect.TypeOf((*courier.PickupRequester)(nil)).Elem(),
		"ReturnRequester": reflect.TypeOf((*courier.ReturnRequester)(nil)).Elem(), "BalanceReader": reflect.TypeOf((*courier.BalanceReader)(nil)).Elem(),
		"CustomerRiskReader": reflect.TypeOf((*courier.CustomerRiskReader)(nil)).Elem(), "CustomerIncidentReporter": reflect.TypeOf((*courier.CustomerIncidentReporter)(nil)).Elem(),
		"WebhookHandlerProvider": reflect.TypeOf((*courier.WebhookHandlerProvider)(nil)).Elem(), "WebhookDecoder": reflect.TypeOf((*courier.WebhookDecoder)(nil)).Elem(),
		"EventHandler": reflect.TypeOf((*courier.EventHandler)(nil)).Elem(),
	}
	if len(ifaces) != len(golden) {
		t.Fatalf("golden/iface table mismatch")
	}
	for name, typ := range ifaces {
		var got []string
		for i := 0; i < typ.NumMethod(); i++ {
			m := typ.Method(i)
			sig := strings.TrimPrefix(m.Type.String(), "func")
			got = append(got, m.Name+sig)
		}
		sort.Strings(got)
		want := golden[name]
		if len(got) != len(want) {
			t.Errorf("%s: method set changed: %v", name, got)
			continue
		}
		for i := range want {
			if norm(got[i]) != norm(want[i]) {
				t.Errorf("%s: signature changed:\n got  %s\n want %s", name, got[i], want[i])
			}
		}
	}
}

func norm(s string) string { return strings.NewReplacer(" ", "", "[]uint8", "[]byte").Replace(s) }
