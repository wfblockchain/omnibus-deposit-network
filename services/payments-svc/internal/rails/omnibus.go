// Package rails is the payment hub's view of everything behind it: the operator
// omnibus network on chain (ledger, router, netting), the Fed simulator with
// the joint account, the operator's funding and netting services, and each member
// bank's core deposit ledger and token gateway. It adapts the model's back
// office (omnibus-deposit-network/internal/...) to biz.Rails, in cents.
//
// The chain keys and the simulators are shared state; the biz engine
// serializes every call.
package rails

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/go-kratos/kratos/v2/log"

	"omnibus-deposit-network/internal/bank"
	"omnibus-deposit-network/internal/chain"
	"omnibus-deposit-network/internal/demo"
	"omnibus-deposit-network/internal/fedwire"
	"omnibus-deposit-network/internal/iso20022"
	"omnibus-deposit-network/internal/omnibus"
	"omnibus-deposit-network/internal/operator"

	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
)

// unitsPerCent is the ledger scale: 6 decimals per dollar.
var unitsPerCent = big.NewInt(10_000)

func units(cents int64) *big.Int { return new(big.Int).Mul(big.NewInt(cents), unitsPerCent) }

func centsOf(u *big.Int) int64 {
	if u == nil {
		return 0
	}
	return new(big.Int).Quo(u, unitsPerCent).Int64()
}

type node struct {
	g             *bank.Gateway
	ib            *bank.Interbank
	inboundCursor uint64
	redeemed      map[[32]byte]bool
}

// Omnibus implements biz.Rails over the model's back office.
type Omnibus struct {
	net     *omnibus.Network
	fed     *fedwire.Service
	clock   *fedwire.ManualClock
	funding *operator.FundingService
	netter  *operator.NettingService
	nodes   map[string]*node
	ids     []string
	log     *log.Helper
}

var _ biz.Rails = (*Omnibus)(nil)

// New connects to the chain (starting anvil when no RPC URL is given),
// deploys the network, opens the Fed accounts, funds the members' positions
// and opens the configured deposit accounts.
func New(ctx context.Context, c *conf.Network, screener bank.Screener, logger log.Logger) (*Omnibus, func(), error) {
	h := log.NewHelper(logger)
	cleanup := func() {}
	rpc := c.RPCURL
	if rpc == "" {
		bin := c.AnvilBin
		if bin == "" {
			bin = "anvil"
		}
		path, err := exec.LookPath(bin)
		if err != nil {
			return nil, nil, fmt.Errorf("network.rpc_url is empty and anvil %q is not on PATH", bin)
		}
		url, stop, err := demo.StartAnvil(context.Background(), path)
		if err != nil {
			return nil, nil, err
		}
		rpc, cleanup = url, stop
		h.Infof("started anvil at %s", url)
	}
	o, err := build(ctx, c, rpc, screener, logger)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return o, cleanup, nil
}

func build(ctx context.Context, c *conf.Network, rpc string, screener bank.Screener, logger log.Logger) (*Omnibus, error) {
	h := log.NewHelper(logger)
	tlog := func(format string, args ...any) { h.Debugf(format, args...) }
	cl, err := chain.Dial(ctx, rpc, c.Artifacts)
	if err != nil {
		return nil, fmt.Errorf("chain: %w", err)
	}
	cal, err := fedwire.NewCalendar()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	if c.StartTime != "" {
		if start, err = time.Parse(time.RFC3339, c.StartTime); err != nil {
			return nil, fmt.Errorf("network.start_time: %w", err)
		}
	}
	clock := fedwire.NewManualClock(start)
	if err := cl.SetTime(ctx, start); err != nil {
		return nil, err
	}
	fed := fedwire.New(clock, cal)

	var specs []omnibus.BankSpec
	for _, b := range c.Banks {
		specs = append(specs, omnibus.BankSpec{MemberID: b.MemberID, Name: b.Name, Ticker: b.Ticker, ABA: b.RoutingNumber,
			MasterAccount: "FRB-MASTER-" + b.RoutingNumber, RequiresAcceptance: b.RequiresAcceptance})
	}
	opKeys, err := omnibus.NewOperatorKeys()
	if err != nil {
		return nil, err
	}
	keys := map[string]omnibus.BankKeys{}
	for _, s := range specs {
		if keys[s.MemberID], err = omnibus.NewBankKeys(s.Ticker); err != nil {
			return nil, err
		}
	}
	net, err := omnibus.Deploy(ctx, cl, opKeys, specs, keys)
	if err != nil {
		return nil, fmt.Errorf("deploy: %w", err)
	}
	fed.OpenAccount(operator.JointAccount, operator.OperatorAgent, big.NewInt(0))
	fed.EnableLMT(operator.JointAccount)
	for _, s := range specs {
		fed.OpenAccount(s.MasterAccount, iso20022.Agent{BICFI: s.MemberID, ABA: s.ABA}, iso20022.Dollars(5_000_000_000))
	}
	o := &Omnibus{
		net: net, fed: fed, clock: clock,
		funding: operator.NewFundingService(net, fed, tlog),
		netter:  operator.NewNettingService(net, tlog),
		nodes:   map[string]*node{},
		log:     h,
	}
	dir, msgs := operator.NewDirectory(), operator.NewMessages()
	for _, s := range specs {
		g := bank.NewGateway(net.Banks[s.MemberID], net, fed, dir, screener, tlog)
		o.nodes[s.MemberID] = &node{g: g, ib: bank.NewInterbank(g, msgs), redeemed: map[[32]byte]bool{}}
		o.ids = append(o.ids, s.MemberID)
	}
	sort.Strings(o.ids)
	for _, b := range c.Banks {
		if b.InitialFunding == "" {
			continue
		}
		amt, err := biz.ParseAmount(b.InitialFunding)
		if err != nil {
			return nil, fmt.Errorf("bank %s initial_funding: %w", b.MemberID, err)
		}
		if st := o.nodes[b.MemberID].g.FundOmnibus(units(amt)); st.TxSts != iso20022.StatusAccepted {
			return nil, fmt.Errorf("fund %s: %s", b.MemberID, st.Reason)
		}
	}
	for _, a := range c.Accounts {
		n, ok := o.nodes[a.Bank]
		if !ok {
			return nil, fmt.Errorf("account %s: unknown bank %s", a.Account, a.Bank)
		}
		var opening int64
		if a.Opening != "" {
			if opening, err = biz.ParseAmount(a.Opening); err != nil {
				return nil, fmt.Errorf("account %s opening: %w", a.Account, err)
			}
		}
		if _, err := n.g.Onboard(ctx, a.Name, a.Account, units(opening)); err != nil {
			return nil, fmt.Errorf("onboard %s: %w", a.Account, err)
		}
		if a.Closed {
			n.g.Close(a.Account)
		}
	}
	h.Infof("network deployed: %d banks, %d accounts, clock %s", len(specs), len(c.Accounts), start.Format(time.RFC3339))
	return o, nil
}

func refOf(id [32]byte) string { return hex.EncodeToString(id[:]) }

func idOf(ref string) ([32]byte, error) {
	var id [32]byte
	b, err := hex.DecodeString(ref)
	if err != nil || len(b) != 32 {
		return id, fmt.Errorf("bad reference %q", ref)
	}
	copy(id[:], b)
	return id, nil
}

func (o *Omnibus) node(bank string) (*node, error) {
	n, ok := o.nodes[bank]
	if !ok {
		return nil, fmt.Errorf("unknown bank %s", bank)
	}
	return n, nil
}

func insufficient(err error) error {
	if err != nil && strings.Contains(err.Error(), "insufficient funds") {
		return biz.ErrInsufficientFunds
	}
	return err
}

// ─── Time ───

func (o *Omnibus) Now() time.Time                        { return o.clock.Now() }
func (o *Omnibus) FedwireOpen(t time.Time) bool          { return o.fed.Calendar().IsOpen(t) }
func (o *Omnibus) NextFedwireOpen(t time.Time) time.Time { return o.fed.Calendar().NextOpen(t) }

func (o *Omnibus) SetTime(ctx context.Context, t time.Time) error {
	o.clock.Set(t)
	return o.net.C.SetTime(ctx, t)
}

// ─── Directory ───

func info(s omnibus.BankSpec) biz.BankInfo {
	return biz.BankInfo{MemberID: s.MemberID, Name: s.Name, Ticker: s.Ticker, Routing: s.ABA, RequiresAcceptance: s.RequiresAcceptance}
}

func (o *Omnibus) Banks() []biz.BankInfo {
	var out []biz.BankInfo
	for _, id := range o.ids {
		out = append(out, info(o.nodes[id].g.Bank.Spec))
	}
	return out
}

func (o *Omnibus) BankByRouting(routing string) (biz.BankInfo, bool) {
	b, ok := o.net.BankByABA(routing)
	if !ok {
		return biz.BankInfo{}, false
	}
	return info(b.Spec), true
}

func (o *Omnibus) Bank(memberID string) (biz.BankInfo, bool) {
	n, ok := o.nodes[memberID]
	if !ok {
		return biz.BankInfo{}, false
	}
	return info(n.g.Bank.Spec), true
}

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

func (o *Omnibus) Member(ctx context.Context, id string) (biz.Member, error) {
	n, err := o.node(id)
	if err != nil {
		return biz.Member{}, err
	}
	out, err := o.net.Ledger.Call(ctx, "member", chain.Bytes32(id))
	if err != nil {
		return biz.Member{}, err
	}
	m := *abi.ConvertType(out[0], new(memberView)).(*memberView)
	free, err := o.net.Ledger.BigInt(ctx, "freePosition", chain.Bytes32(id))
	if err != nil {
		return biz.Member{}, err
	}
	capacity, err := o.net.MintCapacity(ctx, id)
	if err != nil {
		return biz.Member{}, err
	}
	s := n.g.Bank.Spec
	return biz.Member{ID: id, Name: s.Name, Ticker: s.Ticker, Routing: s.ABA,
		Position: centsOf(m.Position), Backing: centsOf(m.Backing), PendingDefund: centsOf(m.PendingDefund),
		Free: centsOf(free), MintCapacity: centsOf(capacity), Admitted: m.Admitted, Suspended: m.Suspended}, nil
}

// ─── Core banking ───

func (o *Omnibus) Account(bankID, account string) (biz.AccountRecord, bool) {
	n, ok := o.nodes[bankID]
	if !ok {
		return biz.AccountRecord{}, false
	}
	if c := n.g.Customer(account); c != nil {
		return biz.AccountRecord{Name: c.Name, Closed: c.Closed}, true
	}
	if n.g.Core.Has(account) {
		return biz.AccountRecord{Name: account}, true
	}
	return biz.AccountRecord{}, false
}

func (o *Omnibus) Balance(bankID, account string) int64 {
	n, ok := o.nodes[bankID]
	if !ok || !n.g.Core.Has(account) {
		return 0
	}
	return centsOf(n.g.Core.Balance(account))
}

func (o *Omnibus) Postings(bankID, account string) []biz.Posting {
	n, ok := o.nodes[bankID]
	if !ok {
		return nil
	}
	var out []biz.Posting
	for _, e := range n.g.Core.Entries(account) {
		out = append(out, biz.Posting{At: e.At, Ref: e.Ref, Delta: centsOf(e.Delta), Balance: centsOf(e.Balance)})
	}
	return out
}

func (o *Omnibus) Post(bankID, account string, delta int64, ref string) error {
	n, err := o.node(bankID)
	if err != nil {
		return err
	}
	return insufficient(n.g.Core.Post(account, units(delta), ref))
}

func (o *Omnibus) EnsureAccount(bankID, account, _ string) {
	if n, ok := o.nodes[bankID]; ok && !n.g.Core.Has(account) {
		n.g.Core.Open(account, big.NewInt(0))
	}
}

func (o *Omnibus) InFlight(ctx context.Context, bankID, account string) int64 {
	n, ok := o.nodes[bankID]
	if !ok {
		return 0
	}
	c := n.g.Customer(account)
	if c == nil {
		return 0
	}
	b, err := n.g.Bank.Token.BigInt(ctx, "balanceOf", c.Wallet.Addr)
	if err != nil {
		return 0
	}
	return centsOf(b)
}

// ─── Gross, 24x7 ───

func (o *Omnibus) Tokenize(ctx context.Context, bankID, account string, amount int64) error {
	n, err := o.node(bankID)
	if err != nil {
		return err
	}
	return insufficient(n.g.Tokenize(ctx, account, units(amount)))
}

func (o *Omnibus) SendTokenPayment(ctx context.Context, bankID, account string, to biz.Party, amount int64, remittance, uetr string) (biz.TokenPayment, error) {
	n, err := o.node(bankID)
	if err != nil {
		return biz.TokenPayment{}, err
	}
	// Screening happened in the hub before execution; the receiving bank
	// screens again on arrival.
	res, err := n.g.SendPaymentOpts(ctx, account, to.Name, to.Routing, to.Account, units(amount), remittance, false, uetr)
	if err != nil {
		return biz.TokenPayment{}, err
	}
	p := biz.TokenPayment{Ref: refOf(res.PaymentID), State: biz.TokenHeld}
	switch res.Status.TxSts {
	case iso20022.StatusRejected:
		p.State, p.Reason = biz.TokenRejected, res.Status.Reason
	case iso20022.StatusAccepted:
		p.State = biz.TokenSettled
	}
	return p, nil
}

func (o *Omnibus) TokenPaymentState(ctx context.Context, ref string) (biz.TokenState, error) {
	id, err := idOf(ref)
	if err != nil {
		return biz.TokenState{}, err
	}
	pv, err := o.net.Payment(ctx, id)
	if err != nil {
		return biz.TokenState{}, err
	}
	st := biz.TokenState{Deadline: time.Unix(int64(pv.Deadline), 0)}
	switch pv.Status {
	case 1:
		st.State = biz.TokenHeld
	case 2:
		st.State = biz.TokenSettled
	case 3:
		st.State, st.Reason = biz.TokenRejected, o.net.RejectReason(ctx, id)
	case 4:
		st.State = biz.TokenExpired
	default:
		return st, fmt.Errorf("payment %s has unknown status %d", ref, pv.Status)
	}
	return st, nil
}

func (o *Omnibus) ExpireTokenPayment(ctx context.Context, bankID, ref string) error {
	n, err := o.node(bankID)
	if err != nil {
		return err
	}
	id, err := idOf(ref)
	if err != nil {
		return err
	}
	_, err = o.net.Router.Send(ctx, n.g.Bank.Keys.Operator, "expire", id)
	return err
}

func (o *Omnibus) Redeem(ctx context.Context, bankID, account string, amount int64) error {
	n, err := o.node(bankID)
	if err != nil {
		return err
	}
	if err := n.g.Redeem(ctx, account, units(amount)); err != nil {
		return err
	}
	return n.g.SyncRedemptions(ctx)
}

func (o *Omnibus) TokenPaymentCredited(receivingBank, ref string) bool {
	n, ok := o.nodes[receivingBank]
	id, err := idOf(ref)
	return ok && err == nil && n.redeemed[id]
}

// ─── Netted ───

func (o *Omnibus) SubmitObligation(ctx context.Context, bankID, account string, to biz.Party, amount int64) (string, error) {
	n, err := o.node(bankID)
	if err != nil {
		return "", err
	}
	res, err := n.ib.SendFromDepositOpts(ctx, account, to.Name, to.Routing, to.Account, units(amount), false, false)
	if err != nil {
		return "", insufficient(err)
	}
	return refOf(res.PaymentID), nil
}

func (o *Omnibus) ObligationState(ctx context.Context, ref string) (string, error) {
	id, err := idOf(ref)
	if err != nil {
		return "", err
	}
	out, err := o.net.Netting.Call(ctx, "obligations", id)
	if err != nil {
		return "", err
	}
	switch out[4].(uint8) {
	case 1:
		return biz.ObligationQueued, nil
	case 2:
		return biz.ObligationSettled, nil
	case 3:
		return biz.ObligationCancelled, nil
	case 4:
		return biz.ObligationExpired, nil
	}
	return "", fmt.Errorf("obligation %s has unknown status %d", ref, out[4].(uint8))
}

func (o *Omnibus) CancelObligation(ctx context.Context, bankID, ref string) error {
	n, err := o.node(bankID)
	if err != nil {
		return err
	}
	id, err := idOf(ref)
	if err != nil {
		return err
	}
	if _, err := o.net.Netting.Send(ctx, n.g.Bank.Keys.Operator, "cancel", id); err != nil {
		return err
	}
	return n.ib.Sync(ctx)
}

func (o *Omnibus) ObligationCredited(receivingBank, ref string) bool {
	n, ok := o.nodes[receivingBank]
	id, err := idOf(ref)
	return ok && err == nil && n.ib.Credited(id)
}

func (o *Omnibus) QueuedObligations(ctx context.Context) int {
	_ = o.netter.Sync(ctx)
	return o.netter.Queued()
}

func (o *Omnibus) RunCycle(ctx context.Context) (biz.CycleReport, error) {
	r, err := o.netter.RunCycle(ctx)
	if err != nil {
		return biz.CycleReport{}, err
	}
	return biz.CycleReport{CycleRef: r.CycleID, Discharged: r.Discharged, Deferred: r.Deferred,
		Gross: centsOf(r.Gross), Net: centsOf(r.Net)}, nil
}

func (o *Omnibus) ExpireStaleObligations(ctx context.Context) error {
	_, err := o.netter.ExpireStale(ctx, uint64(o.Now().Unix()))
	return err
}

// ─── Treasury and the Fed ───

func (o *Omnibus) Fund(ctx context.Context, bankID string, amount int64) biz.FundResult {
	n, err := o.node(bankID)
	if err != nil {
		return biz.FundResult{Status: iso20022.StatusRejected, Reason: err.Error()}
	}
	instrument := "BTRC"
	if !o.FedwireOpen(o.Now()) {
		instrument = "LMT1"
	}
	st := n.g.FundOmnibus(units(amount))
	if err := o.funding.Sync(ctx); err == nil {
		o.funding.Process(ctx)
	}
	return biz.FundResult{Instrument: instrument, Status: st.TxSts, Reason: st.Reason}
}

func (o *Omnibus) RequestDefund(ctx context.Context, bankID string, amount int64) (string, error) {
	n, err := o.node(bankID)
	if err != nil {
		return "", err
	}
	id, err := n.g.RequestDefundOnly(ctx, units(amount))
	if err != nil {
		return "", err
	}
	return refOf(id), nil
}

func (o *Omnibus) ApproveDefund(ctx context.Context, bankID, ref string) error {
	n, err := o.node(bankID)
	if err != nil {
		return err
	}
	id, err := idOf(ref)
	if err != nil {
		return err
	}
	if err := n.g.ApproveDefund(ctx, id); err != nil {
		return err
	}
	if err := o.funding.Sync(ctx); err == nil {
		o.funding.Process(ctx)
	}
	return nil
}

func (o *Omnibus) DefundState(ctx context.Context, ref string) (string, error) {
	id, err := idOf(ref)
	if err != nil {
		return "", err
	}
	out, err := o.net.Ledger.Call(ctx, "defunds", id)
	if err != nil {
		return "", err
	}
	switch out[2].(uint8) {
	case 3:
		return "COMPLETED", nil
	case 4:
		return "FAILED", nil
	}
	return "APPROVED", nil
}

// ─── the operator ───

func (o *Omnibus) FedJointBalance() int64 { return centsOf(o.fed.Balance(operator.JointAccount)) }

func (o *Omnibus) LedgerTotal(ctx context.Context) (int64, error) {
	t, err := o.net.Ledger.BigInt(ctx, "omnibusTotal")
	return centsOf(t), err
}

func (o *Omnibus) Health(ctx context.Context) (bool, bool) {
	brk, _ := o.net.Ledger.Bool(ctx, "reconciliationBreak")
	inv, _ := o.net.Ledger.Bool(ctx, "invariantsHold")
	return brk, inv
}

func (o *Omnibus) Reconcile(ctx context.Context) (biz.Reconciliation, error) {
	r, err := operator.Reconcile(ctx, o.net, o.fed)
	if err != nil {
		return biz.Reconciliation{}, err
	}
	return biz.Reconciliation{At: o.Now(), FedBalance: centsOf(r.FedBalance), LedgerTotal: centsOf(r.LedgerTotal),
		Break: r.Break, Invariants: r.Invariants}, nil
}

// ─── Background ───

// Process advances the asynchronous side of the rails: Fed advices and
// defunds, receiving banks answering held payments, payees credited, and
// each bank's view of settled, cancelled and expired obligations.
func (o *Omnibus) Process(ctx context.Context) error {
	if err := o.funding.Sync(ctx); err == nil {
		o.funding.Process(ctx)
	}
	var errs []string
	for _, id := range o.ids {
		if err := o.nodes[id].g.ProcessInbound(ctx); err != nil {
			errs = append(errs, id+": inbound: "+err.Error())
		}
	}
	for _, id := range o.ids {
		if err := o.creditInbound(ctx, o.nodes[id]); err != nil {
			errs = append(errs, id+": credit: "+err.Error())
		}
	}
	for _, id := range o.ids {
		if err := o.nodes[id].ib.Sync(ctx); err != nil {
			errs = append(errs, id+": interbank: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("rails: %s", strings.Join(errs, "; "))
	}
	return nil
}

// creditInbound redeems tokens that arrived for a bank's customers straight
// into their deposit accounts, so payees never hold tokens.
func (o *Omnibus) creditInbound(ctx context.Context, n *node) error {
	head, err := o.net.C.Head(ctx)
	if err != nil {
		return err
	}
	logs, err := o.net.Router.Events(ctx, "PaymentSettled", n.inboundCursor, head)
	if err != nil {
		return err
	}
	for _, l := range logs {
		var ev struct {
			PaymentId [32]byte
			Payer     common.Address
			Payee     common.Address
			FromToken common.Address
			ToToken   common.Address
			Amount    *big.Int
		}
		if err := o.net.Router.Unpack(&ev, "PaymentSettled", l); err != nil {
			continue
		}
		id := [32]byte(l.Topics[1])
		payee := common.BytesToAddress(l.Topics[3].Bytes())
		if ev.ToToken != n.g.Bank.Token.Addr || n.redeemed[id] {
			continue
		}
		c := n.g.CustomerByWallet(payee)
		if c == nil {
			continue
		}
		if err := n.g.Redeem(ctx, c.DDA, ev.Amount); err != nil {
			return err
		}
		n.redeemed[id] = true
	}
	if err := n.g.SyncRedemptions(ctx); err != nil {
		return err
	}
	n.inboundCursor = head + 1
	return nil
}
