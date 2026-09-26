package demo

import (
	"bytes"
	"context"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"omnibus-deposit-network/internal/iso20022"
)

// TestWeekEndToEnd runs the scenario on a fresh anvil node. It needs anvil
// (ANVIL_BIN or PATH) and a Foundry build (contracts/out); without them it
// skips, so `go test ./...` stays green on a machine without Foundry.
func TestWeekEndToEnd(t *testing.T) {
	bin := os.Getenv("ANVIL_BIN")
	if bin == "" {
		if p, err := exec.LookPath("anvil"); err == nil {
			bin = p
		}
	}
	if bin == "" {
		t.Skip("anvil not found: set ANVIL_BIN or put Foundry on PATH")
	}
	artifacts := filepath.Join("..", "..", "contracts", "out")
	if _, err := os.Stat(filepath.Join(artifacts, "OmnibusLedger.sol", "OmnibusLedger.json")); err != nil {
		t.Skip("contracts not built: run `forge build` in contracts/")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	url, stop, err := StartAnvil(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	var out bytes.Buffer
	rep, err := Run(ctx, Config{RPC: url, Artifacts: artifacts, Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	t.Log("\n" + out.String())

	d := iso20022.Dollars
	eq := func(name string, got, want *big.Int) {
		t.Helper()
		if got == nil || got.Cmp(want) != 0 {
			t.Errorf("%s = %v, want %s", name, got, iso20022.FormatDollars(want))
		}
	}

	if len(rep.FundingErrors) != 0 {
		t.Fatalf("funding service errors: %v", rep.FundingErrors)
	}
	if !rep.Invariants {
		t.Fatal("ledger invariants do not hold")
	}
	// The replayed order found its payment; the exact totals below prove it
	// did not move money a second time.
	if !rep.ReplayDeduped {
		t.Fatal("a replayed order (same UETR) did not resolve to its original payment")
	}

	// The Fed and the operator agree to the cent: 50 + 40 + 30 (Bank C) - 10 - 5 + 5
	// (LMT, Saturday) + 20 million, plus $10,958.90 of interest.
	wantJoint := new(big.Int).Add(d(130_010_958), big.NewInt(900_000))
	eq("Fed joint account", rep.FedJoint, wantJoint)
	eq("ledger total", rep.LedgerTotal, wantJoint)

	// Backing equals supply: 2m + 1m minted, 250k paid to Bank B customers, 100k
	// paid to Bank B's treasury and settled, 50k back = 2.7m A-dT;
	// 250k in, 50k out, 100k redeemed = 100k B-dT.
	eq("A-dT supply", rep.Supply["A-dT"], d(2_700_000))
	eq("A-dT backing", rep.Backing["A-dT"], d(2_700_000))
	eq("held A-dT settled by Bank B", rep.HeldSettled, d(100_000))
	eq("B-dT supply", rep.Supply["B-dT"], d(100_000))
	eq("B-dT backing", rep.Backing["B-dT"], d(100_000))

	// Positions before interest:
	//   Bank A   50 - 0.25 - 0.1 (held, settled) - 2 (gross) - 8 (net) + 0.05 - 5 + 20 = 54.7m
	//   Bank B  40 + 0.25 + 0.1 + 5 (net) - 10 - 0.05 + 5 (LMT)                      = 40.3m
	//   Bank C 30 + 2 (gross) + 3 (net)                                              = 35.0m
	// Interest is split on top by time-weighted position.
	sumPos := new(big.Int).Add(rep.Positions["A-dT"], rep.Positions["B-dT"])
	sumPos.Add(sumPos, rep.Positions["C-dT"])
	sumPos.Add(sumPos, rep.Undistributed)
	eq("positions + undistributed", sumPos, wantJoint)
	for tk, floor := range map[string]int64{"A-dT": 54_700_000, "B-dT": 40_300_000, "C-dT": 35_000_000} {
		if rep.Positions[tk].Cmp(d(floor)) <= 0 || rep.Positions[tk].Cmp(d(floor+10_958)) > 0 {
			t.Errorf("%s position %s: want %d plus a share of interest", tk, iso20022.FormatDollars(rep.Positions[tk]), floor)
		}
	}

	// Thursday's cycle: 60 + 55 + 52 = 167m discharged by moving 8m. Friday's
	// planning run defers Bank C's unfundable 40m; it expires on Saturday and
	// Bank C re-credits Fabrikam (whose DDA is therefore unchanged by it).
	if len(rep.Cycles) != 2 {
		t.Fatalf("cycles: %d", len(rep.Cycles))
	}
	if rep.Cycles[1].Discharged != 0 || rep.Cycles[1].Deferred != 1 || rep.Expired != 1 {
		t.Errorf("deferral: discharged %d, deferred %d, expired %d", rep.Cycles[1].Discharged, rep.Cycles[1].Deferred, rep.Expired)
	}
	eq("cycle gross", rep.Cycles[0].Gross, d(167_000_000))
	eq("cycle net", rep.Cycles[0].Net, d(8_000_000))
	if rep.Cycles[0].Discharged != 3 || rep.Cycles[0].Deferred != 0 {
		t.Errorf("cycle discharged %d, deferred %d", rep.Cycles[0].Discharged, rep.Cycles[0].Deferred)
	}

	// Defunds reached the master accounts.
	eq("Bank A master", rep.FedMaster["Bank A"], d(5_000_000_000-50_000_000+5_000_000-20_000_000))
	eq("Bank B master", rep.FedMaster["Bank B"], d(5_000_000_000-40_000_000+10_000_000-5_000_000))
	eq("Bank C master", rep.FedMaster["Bank C"], d(5_000_000_000-30_000_000))

	// Customers.
	eq("Alice DDA", rep.DDA["Alice Corp"], d(2_000_000))
	eq("Bob DDA", rep.DDA["Bob Inc"], d(600_000))
	eq("Alice A-dT", rep.Tokens["Alice Corp A-dT"], d(2_600_000))
	eq("Northwind DDA", rep.DDA["Northwind Corp"], d(100_000_000-60_000_000-2_000_000+52_000_000))
	eq("Contoso DDA", rep.DDA["Contoso Ltd"], d(80_000_000+60_000_000-55_000_000))
	eq("Fabrikam DDA", rep.DDA["Fabrikam Inc"], d(70_000_000+55_000_000-52_000_000+2_000_000))
	eq("Carol A-dT", rep.Tokens["Carol LLC A-dT"], d(100_000))
	eq("Bob B-dT", rep.Tokens["Bob Inc B-dT"], d(100_000))

	want := map[string]string{
		"on-us Alice->Carol":                  "ACSC",
		"Alice->Bob@Bank B":                   "ACSC",
		"Alice->Dave (closed)":                "RJCT AC04",
		"Alice->Petrov (sanctioned)":          "RJCT RR04",
		"Sat Bob->Alice@Bank A":               "ACSC",
		"Northwind->Fabrikam (urgent, gross)": "ACSC",
	}
	for k, v := range want {
		if rep.Statuses[k] != v {
			t.Errorf("%s: status %q, want %q", k, rep.Statuses[k], v)
		}
	}

	// Reconciliation: Thursday clean, Monday break during the outage,
	// cleared after replay, clean at close.
	if len(rep.Recon) != 4 || rep.Recon[0].Break || !rep.Recon[1].Break || rep.Recon[2].Break || rep.Recon[3].Break {
		t.Errorf("reconciliation sequence wrong: %+v", rep.Recon)
	}
	if !rep.HaltRefused {
		t.Error("minting was not refused while the books disagreed")
	}
}
