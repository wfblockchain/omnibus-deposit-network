// Command omnibus-demo runs one business week of the omnibus model on a
// local EVM node: the operator's joint account at the Fed, two banks issuing their
// own tickers, and the back office on both sides.
//
//	cd contracts && forge build && cd ..
//	go run ./cmd/omnibus-demo                 # starts anvil from PATH
//	go run ./cmd/omnibus-demo -rpc http://...  # or use a running node
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"omnibus-deposit-network/internal/demo"
)

func main() {
	rpc := flag.String("rpc", "", "JSON-RPC URL of a running dev node; empty starts anvil")
	anvil := flag.String("anvil", "anvil", "anvil binary, used when -rpc is empty")
	artifacts := flag.String("artifacts", "contracts/out", "Foundry build output")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	url := *rpc
	if url == "" {
		bin, err := exec.LookPath(*anvil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "anvil not found; install Foundry or pass -rpc / -anvil")
			os.Exit(2)
		}
		u, stop, err := demo.StartAnvil(ctx, bin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer stop()
		url = u
	}

	rep, err := demo.Run(ctx, demo.Config{RPC: url, Artifacts: *artifacts, Out: os.Stdout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "scenario failed:", err)
		os.Exit(1)
	}
	if !rep.Invariants || len(rep.FundingErrors) > 0 {
		fmt.Fprintln(os.Stderr, "scenario finished with ledger problems:", rep.FundingErrors)
		os.Exit(1)
	}
}
