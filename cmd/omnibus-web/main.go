// Command omnibus-web runs the web2 layer over the omnibus network: a
// REST/JSON API and server-rendered portals for corporate treasuries, member
// banks' operations staff and operator operations, on a local EVM node.
//
//	(cd contracts && forge soldeer install && forge build)
//	go run ./cmd/omnibus-web            # starts anvil from PATH, serves :8080
//	open http://127.0.0.1:8080
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"omnibus-deposit-network/internal/api"
	"omnibus-deposit-network/internal/demo"
	"omnibus-deposit-network/internal/stack"
	"omnibus-deposit-network/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	rpc := flag.String("rpc", "", "JSON-RPC URL of a running dev node; empty starts anvil")
	anvil := flag.String("anvil", "anvil", "anvil binary, used when -rpc is empty")
	artifacts := flag.String("artifacts", "contracts/out", "Foundry build output")
	tick := flag.Duration("tick", 2*time.Second, "how often background processing runs")
	netting := flag.Duration("netting-every", time.Hour, "netting cycle schedule on the scenario clock; 0 leaves cycles to operator operations")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	url := *rpc
	if url == "" {
		bin, err := exec.LookPath(*anvil)
		if err != nil {
			log.Fatal("anvil not found; install Foundry or pass -rpc / -anvil")
		}
		u, stopAnvil, err := demo.StartAnvil(ctx, bin)
		if err != nil {
			log.Fatal(err)
		}
		defer stopAnvil()
		url = u
	}

	logger := func(format string, args ...any) { log.Printf(format, args...) }
	log.Printf("deploying the network and seeding banks, clients and staff...")
	st, err := stack.Build(ctx, url, *artifacts, logger)
	if err != nil {
		log.Fatal(err)
	}
	st.Service.NettingEvery = *netting

	mux := http.NewServeMux()
	mux.Handle("/api/", api.Handler(st.Service, st.Hooks))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("/", web.Handler(st.Service))
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go st.Hooks.Run(ctx, time.Second)
	go func() {
		t := time.NewTicker(*tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = st.Service.Tick(ctx)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	fmt.Printf(`
the network web2 layer on http://%[1]s
  portals  http://%[1]s/login        (pick a person: treasurer, bank ops, operator ops)
  API      http://%[1]s/api/v1/openapi.yaml
  example  curl -s -H 'Authorization: Bearer tok-nw-maker' http://%[1]s/api/v1/account
  clock    %[2]s (Fedwire open); operator ops can move it to Saturday to see 24x7 settlement
`, *addr, stack.Start().Format("Mon 2 Jan 15:04 MST"))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
