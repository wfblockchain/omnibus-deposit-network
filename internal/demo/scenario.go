// Package demo runs the omnibus model end to end on a real EVM node: the
// contracts, the Fed simulator, the operator's back office and two bank gateways,
// through one business week that crosses a Fedwire close and a weekend.
package demo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"omnibus-deposit-network/internal/bank"
	"omnibus-deposit-network/internal/chain"
	"omnibus-deposit-network/internal/fedwire"
	"omnibus-deposit-network/internal/iso20022"
	"omnibus-deposit-network/internal/omnibus"
	"omnibus-deposit-network/internal/operator"
)

// Start is the scenario's first instant: Thursday 1 October 2026, 10:00 ET.
func Start() time.Time {
	loc, _ := time.LoadLocation("America/New_York")
	return time.Date(2026, 10, 1, 10, 0, 0, 0, loc)
}

// Config points the scenario at a node and at Foundry's artifacts.
type Config struct {
	RPC       string
	Artifacts string
	Out       io.Writer
}

// Report is the scenario's end state, for tests and for the printout.
type Report struct {
	Positions     map[string]*big.Int
	Backing       map[string]*big.Int
	Supply        map[string]*big.Int
	FedJoint      *big.Int
	FedMaster     map[string]*big.Int
	LedgerTotal   *big.Int
	Undistributed *big.Int
	Recon         []operator.ReconReport
	Invariants    bool
	Statuses      map[string]string // step -> pacs.002 status (+ reason)
	DDA           map[string]*big.Int
	Tokens        map[string]*big.Int
	HaltRefused   bool // minting was refused while the books disagreed
	Cycles        []operator.CycleReport
	HeldSettled   *big.Int // A-dT Bank B's treasury settled into its position
	ReplayDeduped bool     // resending an order with the same UETR found the payment instead of paying twice
	Expired       int      // obligations that expired unsettled
	FundingErrors []error
}

const (
	bankAID = "BNKAUS30"
	bankBID = "BNKBUS30"
	bankCID = "BNKCUS30"

	bankCABA = "012345672"
)

// Run executes the week and returns the end state.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	c, err := chain.Dial(ctx, cfg.RPC, cfg.Artifacts)
	if err != nil {
		return nil, err
	}
	cal, err := fedwire.NewCalendar()
	if err != nil {
		return nil, err
	}
	clock := fedwire.NewManualClock(Start())
	fed := fedwire.New(clock, cal)
	logf := func(format string, args ...any) {
		fmt.Fprintf(cfg.Out, "%s  %s\n", clock.Now().In(cal.Loc).Format("Mon 02 Jan 15:04 ET"), fmt.Sprintf(format, args...))
	}
	at := func(t time.Time) error {
		clock.Set(t)
		open := "closed"
		if cal.IsOpen(t) {
			open = "open"
		}
		fmt.Fprintf(cfg.Out, "\n-- %s (Fedwire %s)\n", t.In(cal.Loc).Format("Monday 2 January 15:04 MST"), open)
		return c.SetTime(ctx, t)
	}
	d := iso20022.Dollars
	rep := &Report{Statuses: map[string]string{}}

	// ── Day 1, Thursday: stand the network up ──────────────────────────────
	if err := at(Start()); err != nil {
		return nil, err
	}
	specs := []omnibus.BankSpec{
		{MemberID: bankAID, Name: "Bank A", Ticker: "A-dT", ABA: "123456780", MasterAccount: "FRB-MASTER-123456780", RequiresAcceptance: false},
		{MemberID: bankBID, Name: "Bank B", Ticker: "B-dT", ABA: "234567898", MasterAccount: "FRB-MASTER-234567898", RequiresAcceptance: true},
		{MemberID: bankCID, Name: "Bank C", Ticker: "C-dT", ABA: bankCABA, MasterAccount: "FRB-MASTER-012345672", RequiresAcceptance: false},
	}
	opKeys, err := omnibus.NewOperatorKeys()
	if err != nil {
		return nil, err
	}
	bankKeys := map[string]omnibus.BankKeys{}
	for _, s := range specs {
		if bankKeys[s.MemberID], err = omnibus.NewBankKeys(s.Ticker); err != nil {
			return nil, err
		}
	}
	net, err := omnibus.Deploy(ctx, c, opKeys, specs, bankKeys)
	if err != nil {
		return nil, fmt.Errorf("deploy: %w", err)
	}
	logf("the operator          deployed OmnibusLedger %s, PaymentRouter %s", short(net.Ledger.Addr.Hex()), short(net.Router.Addr.Hex()))
	for _, s := range specs {
		b := net.Banks[s.MemberID]
		logf("%-11s deployed %s %s and its holder registry; admitted by the operator (acceptance required: %v)", s.Name, s.Ticker, short(b.Token.Addr.Hex()), s.RequiresAcceptance)
	}

	fed.OpenAccount(operator.JointAccount, operator.OperatorAgent, big.NewInt(0))
	fed.EnableLMT(operator.JointAccount)
	for _, s := range specs {
		fed.OpenAccount(s.MasterAccount, iso20022.Agent{BICFI: s.MemberID, ABA: s.ABA}, d(5_000_000_000))
	}
	funding := operator.NewFundingService(net, fed, logf)
	dir := operator.NewDirectory()
	bankA := bank.NewGateway(net.Banks[bankAID], net, fed, dir, bank.ListScreener{Names: []string{"Petrov Trading"}}, logf)
	bankB := bank.NewGateway(net.Banks[bankBID], net, fed, dir, bank.ListScreener{}, logf)
	bankC := bank.NewGateway(net.Banks[bankCID], net, fed, dir, bank.ListScreener{}, logf)
	msgs := operator.NewMessages()
	netter := operator.NewNettingService(net, logf)
	ibBankA, ibBankB, ibBankC := bank.NewInterbank(bankA, msgs), bank.NewInterbank(bankB, msgs), bank.NewInterbank(bankC, msgs)

	if st := bankA.FundOmnibus(d(50_000_000)); st.TxSts != iso20022.StatusAccepted {
		return nil, fmt.Errorf("Bank A funding %s", st.Reason)
	}
	if st := bankB.FundOmnibus(d(40_000_000)); st.TxSts != iso20022.StatusAccepted {
		return nil, fmt.Errorf("Bank B funding %s", st.Reason)
	}
	if st := bankC.FundOmnibus(d(30_000_000)); st.TxSts != iso20022.StatusAccepted {
		return nil, fmt.Errorf("Bank C funding %s", st.Reason)
	}

	onboard := []struct {
		g    *bank.Gateway
		name string
		dda  string
		bal  int64
	}{
		{bankA, "Alice Corp", "A-1000001", 5_000_000},
		{bankA, "Carol LLC", "A-1000002", 1_000_000},
		{bankB, "Bob Inc", "B-2000001", 500_000},
		{bankB, "Dave Ltd", "B-2000002", 0},
		{bankB, "Petrov Trading LLC", "B-2000009", 0},
		{bankA, "Northwind Corp", "A-3000001", 100_000_000},
		{bankB, "Contoso Ltd", "B-4000001", 80_000_000},
		{bankC, "Fabrikam Inc", "C-5000001", 70_000_000},
	}
	for _, o := range onboard {
		if _, err := o.g.Onboard(ctx, o.name, o.dda, d(o.bal)); err != nil {
			return nil, err
		}
	}

	if err := bankA.Tokenize(ctx, "A-1000001", d(2_000_000)); err != nil {
		return nil, err
	}

	pay := func(step string, g *bank.Gateway, dda, name, aba, acct string, dollars int64) (bank.Result, error) {
		r, err := g.SendPayment(ctx, dda, name, aba, acct, d(dollars), step)
		if err != nil {
			return r, fmt.Errorf("%s: %w", step, err)
		}
		rep.Statuses[step] = strings.TrimSpace(r.Status.TxSts + " " + r.Status.Reason)
		return r, nil
	}
	if _, err := pay("on-us Alice->Carol", bankA, "A-1000001", "Carol LLC", "123456780", "A-1000002", 100_000); err != nil {
		return nil, err
	}
	rBob, err := pay("Alice->Bob@Bank B", bankA, "A-1000001", "Bob Inc", "234567898", "B-2000001", 250_000)
	if err != nil {
		return nil, err
	}
	if err := bankB.ProcessInbound(ctx); err != nil {
		return nil, err
	}
	if rep.Statuses["Alice->Bob@Bank B"], err = paymentStatus(ctx, net, rBob.PaymentID, bankB); err != nil {
		return nil, err
	}
	// A retry of the same order (same UETR), as after a crash between send and
	// record: the gateway finds the payment on-chain and does not send it again.
	rReplay, err := bankA.SendPaymentOpts(ctx, "A-1000001", "Bob Inc", "234567898", "B-2000001", d(250_000), "replay", true, rBob.Instruction.UETR)
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	rep.ReplayDeduped = rReplay.PaymentID == rBob.PaymentID

	bankB.Close("B-2000002")
	rDave, err := pay("Alice->Dave (closed)", bankA, "A-1000001", "Dave Ltd", "234567898", "B-2000002", 10_000)
	if err != nil {
		return nil, err
	}
	if err := bankB.ProcessInbound(ctx); err != nil {
		return nil, err
	}
	if rep.Statuses["Alice->Dave (closed)"], err = paymentStatus(ctx, net, rDave.PaymentID, bankB); err != nil {
		return nil, err
	}
	if _, err := pay("Alice->Petrov (sanctioned)", bankA, "A-1000001", "Petrov Trading LLC", "234567898", "B-2000009", 75_000); err != nil {
		return nil, err
	}

	// ── Thursday 14:00: banks holding each other's tokens; gross and netting ─
	if err := at(Start().Add(4 * time.Hour)); err != nil {
		return nil, err
	}
	if err := bankA.PayBankTreasury(ctx, "A-1000001", net.Banks[bankBID], d(100_000)); err != nil {
		return nil, err
	}
	if rep.HeldSettled, err = bankB.SettleHeld(ctx, net.Banks[bankAID]); err != nil {
		return nil, err
	}

	urgent, err := ibBankA.SendFromDeposit(ctx, "A-3000001", "Fabrikam Inc", bankCABA, "C-5000001", d(2_000_000), true)
	if err != nil {
		return nil, err
	}
	rep.Statuses["Northwind->Fabrikam (urgent, gross)"] = urgent.Status.TxSts
	for _, p := range []struct {
		ib        *bank.Interbank
		dda       string
		name, aba string
		acct      string
		dollars   int64
	}{
		{ibBankA, "A-3000001", "Contoso Ltd", "234567898", "B-4000001", 60_000_000},
		{ibBankB, "B-4000001", "Fabrikam Inc", bankCABA, "C-5000001", 55_000_000},
		{ibBankC, "C-5000001", "Northwind Corp", "123456780", "A-3000001", 52_000_000},
	} {
		if _, err := p.ib.SendFromDeposit(ctx, p.dda, p.name, p.aba, p.acct, d(p.dollars), false); err != nil {
			return nil, err
		}
	}
	cyc, err := netter.RunCycle(ctx)
	if err != nil {
		return nil, err
	}
	rep.Cycles = append(rep.Cycles, cyc)
	for _, ib := range []*bank.Interbank{ibBankA, ibBankB, ibBankC} {
		if err := ib.Sync(ctx); err != nil {
			return nil, err
		}
	}

	if err := at(Start().Add(8 * time.Hour)); err != nil { // 18:00 Thu
		return nil, err
	}
	if err := recon(ctx, net, fed, rep, logf); err != nil {
		return nil, err
	}

	// ── Friday 10:00: an obligation the payer cannot fund is deferred ──────
	if err := at(time.Date(2026, 10, 2, 10, 0, 0, 0, cal.Loc)); err != nil {
		return nil, err
	}
	if _, err := ibBankC.SendFromDeposit(ctx, "C-5000001", "Contoso Ltd", "234567898", "B-4000001", d(40_000_000), false); err != nil {
		return nil, err
	}
	cyc2, err := netter.RunCycle(ctx)
	if err != nil {
		return nil, err
	}
	rep.Cycles = append(rep.Cycles, cyc2)
	logf("operator netting  Bank C's $40,000,000.00 exceeds its free position and nothing offsets it: deferred, still queued (%d)", netter.Queued())

	// ── Day 2, Friday: defund before and after the Fedwire close ───────────
	if err := at(time.Date(2026, 10, 2, 18, 30, 0, 0, cal.Loc)); err != nil {
		return nil, err
	}
	if err := bankB.RequestDefund(ctx, d(10_000_000)); err != nil {
		return nil, err
	}
	if err := funding.Sync(ctx); err != nil {
		return nil, err
	}
	funding.Process(ctx)

	if err := at(time.Date(2026, 10, 2, 19, 30, 0, 0, cal.Loc)); err != nil {
		return nil, err
	}
	if err := bankA.RequestDefund(ctx, d(5_000_000)); err != nil {
		return nil, err
	}
	if err := funding.Sync(ctx); err != nil {
		return nil, err
	}
	funding.Process(ctx)

	// ── Saturday: the Fed is closed; tokens are not ────────────────────────
	if err := at(time.Date(2026, 10, 3, 11, 0, 0, 0, cal.Loc)); err != nil {
		return nil, err
	}
	if _, err := pay("Sat Bob->Alice@Bank A", bankB, "B-2000001", "Alice Corp", "123456780", "A-1000001", 50_000); err != nil {
		return nil, err
	}
	// Bank B tops up while Fedwire is closed, through FedNow LMT. Money can
	// come in on a Saturday; Bank A's defund still cannot go out.
	if st := bankB.FundOmnibus(d(5_000_000)); st.TxSts != iso20022.StatusAccepted {
		return nil, fmt.Errorf("Bank B LMT funding %s", st.Reason)
	}
	if err := bankB.Redeem(ctx, "B-2000001", d(100_000)); err != nil {
		return nil, err
	}
	if err := bankB.SyncRedemptions(ctx); err != nil {
		return nil, err
	}
	// The deferred obligation's day-long time-to-live ran out at 10:00.
	if rep.Expired, err = netter.ExpireStale(ctx, uint64(clock.Now().Unix())); err != nil {
		return nil, err
	}
	if err := ibBankC.Sync(ctx); err != nil {
		return nil, err
	}
	funding.Process(ctx)

	// ── Sunday 21:05: Fedwire opens for Monday's business day ──────────────
	if err := at(time.Date(2026, 10, 4, 21, 5, 0, 0, cal.Loc)); err != nil {
		return nil, err
	}
	funding.Process(ctx)

	// ── Monday: an outage the reconciliation catches ────────────────────────
	if err := at(time.Date(2026, 10, 5, 9, 0, 0, 0, cal.Loc)); err != nil {
		return nil, err
	}
	funding.SetDown(true)
	bankA.FundOmnibus(d(20_000_000))
	if err := recon(ctx, net, fed, rep, logf); err != nil {
		return nil, err
	}
	if err := bankA.Tokenize(ctx, "A-1000001", d(1_000_000)); err != nil {
		rep.HaltRefused = true
		logf("Bank A mint refused while the books disagree: %v", err)
	}
	funding.SetDown(false)
	funding.Replay(ctx)
	if err := recon(ctx, net, fed, rep, logf); err != nil {
		return nil, err
	}
	if err := bankA.Tokenize(ctx, "A-1000001", d(1_000_000)); err != nil {
		return nil, err
	}

	if err := at(time.Date(2026, 10, 5, 12, 0, 0, 0, cal.Loc)); err != nil {
		return nil, err
	}
	fed.PayInterest(operator.JointAccount, new(big.Int).Add(d(10_958), big.NewInt(900_000))) // $10,958.90

	if err := at(time.Date(2026, 10, 5, 18, 0, 0, 0, cal.Loc)); err != nil {
		return nil, err
	}
	if err := recon(ctx, net, fed, rep, logf); err != nil {
		return nil, err
	}

	// ── End state ──────────────────────────────────────────────────────────
	if err := snapshot(ctx, net, fed, rep, []*bank.Gateway{bankA, bankB, bankC}, specs); err != nil {
		return nil, err
	}
	rep.FundingErrors = funding.Errors
	printSummary(cfg.Out, rep, specs)
	return rep, nil
}

func recon(ctx context.Context, net *omnibus.Network, fed *fedwire.Service, rep *Report, logf operator.Logger) error {
	r, err := operator.Reconcile(ctx, net, fed)
	if err != nil {
		return err
	}
	rep.Recon = append(rep.Recon, r)
	if r.Break {
		diff := new(big.Int).Sub(r.FedBalance, r.LedgerTotal)
		logf("the operator recon    camt.052 $%s vs ledger $%s: BREAK %s; minting and defunding halted",
			iso20022.FormatDollars(r.FedBalance), iso20022.FormatDollars(r.LedgerTotal), iso20022.FormatDollars(diff))
	} else {
		logf("the operator recon    camt.052 $%s = ledger $%s; reconciled (invariants hold: %v)",
			iso20022.FormatDollars(r.FedBalance), iso20022.FormatDollars(r.LedgerTotal), r.Invariants)
	}
	return nil
}

func snapshot(ctx context.Context, net *omnibus.Network, fed *fedwire.Service, rep *Report, gws []*bank.Gateway, specs []omnibus.BankSpec) error {
	rep.Positions, rep.Backing, rep.Supply = map[string]*big.Int{}, map[string]*big.Int{}, map[string]*big.Int{}
	rep.FedMaster, rep.DDA, rep.Tokens = map[string]*big.Int{}, map[string]*big.Int{}, map[string]*big.Int{}
	for _, s := range specs {
		out, err := net.Ledger.Call(ctx, "member", chain.Bytes32(s.MemberID))
		if err != nil {
			return err
		}
		m := *abi.ConvertType(out[0], new(memberView)).(*memberView)
		rep.Positions[s.Ticker] = m.Position
		rep.Backing[s.Ticker] = m.Backing
		sup, err := net.Banks[s.MemberID].Token.BigInt(ctx, "totalSupply")
		if err != nil {
			return err
		}
		rep.Supply[s.Ticker] = sup
		rep.FedMaster[s.Name] = fed.Balance(s.MasterAccount)
	}
	var err error
	if rep.LedgerTotal, err = net.Ledger.BigInt(ctx, "omnibusTotal"); err != nil {
		return err
	}
	if rep.Undistributed, err = net.Ledger.BigInt(ctx, "undistributedInterest"); err != nil {
		return err
	}
	if rep.Invariants, err = net.Ledger.Bool(ctx, "invariantsHold"); err != nil {
		return err
	}
	rep.FedJoint = fed.Balance(operator.JointAccount)
	for _, g := range gws {
		for _, dda := range []string{"A-1000001", "A-1000002", "B-2000001", "A-3000001", "B-4000001", "C-5000001"} {
			if c := g.Customer(dda); c != nil {
				rep.DDA[c.Name] = g.Core.Balance(dda)
				for _, s := range specs {
					bal, err := g.TokenBalance(ctx, dda, net.Banks[s.MemberID])
					if err != nil {
						return err
					}
					if bal.Sign() > 0 {
						rep.Tokens[c.Name+" "+s.Ticker] = bal
					}
				}
			}
		}
	}
	return nil
}

func printSummary(w io.Writer, r *Report, specs []omnibus.BankSpec) {
	fmt.Fprintf(w, "\n== End of week\n")
	fmt.Fprintf(w, "%-8s %20s %20s %20s\n", "ticker", "omnibus position", "backing", "token supply")
	for _, s := range specs {
		fmt.Fprintf(w, "%-8s %20s %20s %20s\n", s.Ticker, iso20022.FormatDollars(r.Positions[s.Ticker]),
			iso20022.FormatDollars(r.Backing[s.Ticker]), iso20022.FormatDollars(r.Supply[s.Ticker]))
	}
	fmt.Fprintf(w, "Fed joint account %s | ledger total %s | undistributed interest %s | invariants hold: %v\n",
		iso20022.FormatDollars(r.FedJoint), iso20022.FormatDollars(r.LedgerTotal), iso20022.FormatDollars(r.Undistributed), r.Invariants)
	for _, c := range r.Cycles {
		if c.Discharged == 0 {
			fmt.Fprintf(w, "Netting planning run: nothing settleable, %d deferred\n", c.Deferred)
			continue
		}
		fmt.Fprintf(w, "Netting %s: %d obligations, gross %s, net moved %s (%.1f : 1)\n", c.CycleID, c.Discharged,
			iso20022.FormatDollars(c.Gross), iso20022.FormatDollars(c.Net), c.Efficiency())
	}
	fmt.Fprintf(w, "Payment outcomes:\n")
	for _, k := range []string{"on-us Alice->Carol", "Alice->Bob@Bank B", "Alice->Dave (closed)", "Alice->Petrov (sanctioned)", "Northwind->Fabrikam (urgent, gross)", "Sat Bob->Alice@Bank A"} {
		fmt.Fprintf(w, "  %-28s %s\n", k, r.Statuses[k])
	}
}

// memberView mirrors OmnibusLedger.Member field for field; abi.ConvertType
// maps by position, so the order must match the Solidity struct.
type memberView struct {
	Token              common.Address
	Operator           common.Address
	Approver           common.Address
	Admitted           bool
	Suspended          bool
	RequiresAcceptance bool
	Position           *big.Int
	Backing            *big.Int
	PendingDefund      *big.Int
	PositionSeconds    *big.Int
	LastTouch          uint64
	IssuanceCap        *big.Int
	PrefundRequirement *big.Int
	RemoteSupply       *big.Int
}

type paymentView struct {
	Payer     common.Address
	Payee     common.Address
	FromToken common.Address
	ToToken   common.Address
	Amount    *big.Int
	Deadline  uint64
	Status    uint8
	ReturnOf  [32]byte
	Returned  *big.Int
}

// paymentStatus reads a payment's status back from the router and renders
// it as a pacs.002 status, with the receiver's reason code on a reject.
func paymentStatus(ctx context.Context, n *omnibus.Network, id [32]byte, receiver *bank.Gateway) (string, error) {
	out, err := n.Router.Call(ctx, "payment", id)
	if err != nil {
		return "", err
	}
	p := *abi.ConvertType(out[0], new(paymentView)).(*paymentView)
	switch p.Status {
	case 1:
		return iso20022.StatusPending, nil
	case 2:
		return iso20022.StatusAccepted, nil
	case 3:
		reason := ""
		if k := len(receiver.Rejected); k > 0 {
			reason = " " + receiver.Rejected[k-1]
		}
		return iso20022.StatusRejected + reason, nil
	case 4:
		return "EXPIRED", nil
	}
	return "NONE", nil
}

func short(h string) string { return h[:6] + "…" + h[len(h)-4:] }

// StartAnvil launches a local node whose genesis sits a day before the
// scenario, so the scenario can move time forward from there.
func StartAnvil(ctx context.Context, bin string) (url string, stop func(), err error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	genesis := Start().Add(-24 * time.Hour).Unix()
	cmd := exec.CommandContext(ctx, bin, "--port", strconv.Itoa(port), "--timestamp", strconv.FormatInt(genesis, 10), "--silent")
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	stop = func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
	url = fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return url, stop, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	return "", nil, errors.New("anvil did not start")
}
