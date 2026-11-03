// Command tracking shows tracking-only usage: one capability, no registry.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/provider/steadfast"
)

func main() {
	// Global mode: credentials from STEADFAST_API_KEY / STEADFAST_SECRET_KEY.
	sf, err := steadfast.New()
	if err != nil {
		log.Fatal(err)
	}

	// Callers depend on the narrow capability, not on the provider package.
	var tracker courier.ShipmentTracker = sf

	// Several trackers may be held in a plain map; nothing probes or fans out.
	trackers := map[string]courier.ShipmentTracker{"steadfast": tracker}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := trackers["steadfast"].TrackShipment(ctx, courier.TrackingRequest{Code: "15BAEB8A"})
	switch {
	case errors.Is(err, courier.ErrNotFound):
		fmt.Println("no such shipment")
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Printf("provider=%q canonical=%s coverage=%s events=%d\n", res.ProviderStatus.Code, res.Status, res.Coverage, len(res.Events))
	}
}
