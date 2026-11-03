// Command steadfast fetches Steadfast police stations and return requests,
// then dumps the decoded responses as JSON.
//
//	export STEADFAST_API_KEY=...
//	export STEADFAST_SECRET_KEY=...
//	go run ./examples/steadfast
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/nokibsarkar/go-courier/provider/steadfast"
)

func main() {
	client, err := steadfast.New()
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stations, err := client.ListPoliceStations(ctx)
	if err != nil {
		log.Fatalf("fetch police stations: %v", err)
	}
	dump("police stations", stations)

	returns, err := client.ListReturnRequests(ctx, 0)
	if err != nil {
		log.Fatalf("fetch return requests: %v", err)
	}
	dump("return requests", returns)
}

func dump(label string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		log.Fatalf("encode %s: %v", label, err)
	}
	os.Stdout.WriteString(label + ":\n")
	os.Stdout.Write(data)
	os.Stdout.WriteString("\n")
}
