// Command mocksite runs the localhost mock tiket.com server for end-to-end
// testing. Open /admin to flip events between sold-out and available.
//
// Usage:
//
//	go run ./cmd/mocksite                       # serves :8099, seeds "mock-bts-day1"
//	go run ./cmd/mocksite -addr :9000 evt-a evt-b
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/dimasfaid/tikom/internal/mocksite"
)

func main() {
	addr := flag.String("addr", ":8099", "listen address")
	flag.Parse()

	seeds := flag.Args()
	if len(seeds) == 0 {
		seeds = []string{"mock-bts-day1"}
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	srv := mocksite.New(seeds...)

	log.Info("mocksite listening", "addr", *addr, "events", seeds)
	log.Info("open the admin panel", "url", "http://localhost"+*addr+"/admin")
	for _, s := range seeds {
		log.Info("event page", "url", "http://localhost"+*addr+"/to-do/"+s)
	}

	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
