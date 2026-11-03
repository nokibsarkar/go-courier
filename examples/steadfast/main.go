// Command steadfast is a first live check for a Steadfast account: it pings the
// API, reads the balance and, only when asked, books one real test order and
// looks it up again.
//
//	export STEADFAST_API_KEY=... STEADFAST_SECRET_KEY=...
//	export STEADFAST_WEBHOOK_TOKEN=...   # only for -serve
//	go run ./examples/steadfast                       # ping + balance only
//	go run ./examples/steadfast -book -phone 01712345678
//	LOG_LEVEL=debug go run ./examples/steadfast       # logs bodies: includes personal data
//	go run ./examples/steadfast -serve :8080          # receive webhooks at /steadfast/webhook
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	courier "github.com/nokibsarkar/go-courier"
	"github.com/nokibsarkar/go-courier/auth"
	"github.com/nokibsarkar/go-courier/provider/steadfast"
)

func main() {
	book := flag.Bool("book", false, "book ONE real order (creates a parcel at Steadfast)")
	phone := flag.String("phone", "", "recipient phone for -book (Bangladeshi mobile)")
	invoice := flag.String("invoice", fmt.Sprintf("TEST-%d", time.Now().Unix()), "merchant order ID (unique per merchant)")
	cod := flag.String("cod", "0", "cash to collect in BDT")
	serve := flag.String("serve", "", "listen address for webhooks, e.g. :8080")
	flag.Parse()

	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug // request and response bodies are logged; credentials stay masked
	}
	lg := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Global mode: STEADFAST_API_KEY / STEADFAST_SECRET_KEY. Webhooks also need
	// the token you registered in the Steadfast dashboard (STEADFAST_WEBHOOK_TOKEN);
	// without it webhooks are refused (fail closed).
	opts := []steadfast.Option{steadfast.WithLogger(lg)}
	if tok := os.Getenv("STEADFAST_WEBHOOK_TOKEN"); tok != "" {
		opts = append(opts, steadfast.WithWebhookTokens(map[courier.AccountID]auth.Secret{
			courier.AccountID(os.Getenv("STEADFAST_ACCOUNT_ID")): auth.Secret(tok),
		}))
	}
	sf, err := steadfast.New(opts...)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sf.Ping(ctx); err != nil {
		log.Fatalf("ping: %v", err) // a credential failure is never retried: fix the keys first
	}
	fmt.Println("ping: ok")

	bal, err := sf.GetBalance(ctx)
	if err != nil {
		log.Fatalf("balance: %v", err)
	}
	fmt.Printf("balance: %s %s\n", bal.Amount.Amount, bal.Amount.Currency)

	if *book {
		if *phone == "" {
			log.Fatal("-book needs -phone")
		}
		amount, err := courier.ParseMoney(*cod, courier.BDT)
		if err != nil {
			log.Fatal(err)
		}
		sh, err := sf.CreateShipment(ctx, courier.CreateShipmentRequest{
			MerchantOrderID: courier.MerchantOrderID(*invoice),
			Recipient: courier.Party{
				Contact: courier.Contact{Name: "Test Recipient", Phone: *phone},
				Address: courier.Address{Text: "House 1, Road 1, Dhanmondi, Dhaka"},
			},
			COD:  &amount,
			Note: "SDK test order",
		})
		switch {
		case courier.IsIndeterminate(err):
			// The request may have been applied. NEVER create again blindly:
			// look the invoice up. Found = it worked; not found = it did not.
			fmt.Println("create outcome unknown; checking by invoice")
			if err := lookup(ctx, sf, *invoice); err != nil {
				log.Fatal(err)
			}
		case errors.Is(err, courier.ErrConflict):
			fmt.Println("invoice already exists at Steadfast; looking it up")
			if err := lookup(ctx, sf, *invoice); err != nil {
				log.Fatal(err)
			}
		case err != nil:
			log.Fatalf("create: %v", err)
		default:
			fmt.Printf("created: id=%s tracking=%s status=%s\n", sh.Ref.ID, sh.TrackingCode, sh.Status)
			if err := lookup(ctx, sf, *invoice); err != nil {
				log.Fatal(err)
			}
		}
	}

	if *serve != "" {
		h, err := sf.WebhookHandler(courier.EventHandlerFunc(func(_ context.Context, ev courier.Event) error {
			// Events can be redelivered: dedupe on ev.ID (the Idempotency-Key)
			// in your own store before acting. Returning an error makes
			// Steadfast retry.
			lg.Info("steadfast event", "id", ev.ID, "type", ev.Type)
			if ev.Tracking != nil {
				// ProviderStatus keeps the raw code; approval-pending and
				// partial_delivered stay StatusUnknown: do not settle money on them.
				lg.Info("tracking", "raw", ev.Tracking.ProviderStatus.Code, "status", ev.Tracking.Status)
			}
			return nil
		}))
		if err != nil {
			log.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.Handle("/steadfast/webhook", h)
		srv := &http.Server{Addr: *serve, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		fmt.Println("webhooks on", *serve, "/steadfast/webhook")
		log.Fatal(srv.ListenAndServe())
	}
}

func lookup(ctx context.Context, sf *steadfast.Client, invoice string) error {
	res, err := sf.TrackShipment(ctx, courier.TrackingRequest{
		Lookups: []courier.Lookup{{Type: courier.LookupMerchantOrderID, Value: invoice}},
	})
	if errors.Is(err, courier.ErrNotFound) {
		fmt.Println("lookup: no such invoice (the create did not happen)")
		return nil
	}
	if err != nil {
		return fmt.Errorf("lookup: %w", err)
	}
	fmt.Printf("lookup: raw=%q status=%s (cached upstream for 60s)\n", res.ProviderStatus.Code, res.Status)
	return nil
}
