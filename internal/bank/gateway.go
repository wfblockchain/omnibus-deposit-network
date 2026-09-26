// Package bank is one member bank's side of the omnibus: its core deposit
// ledger, its customers' custodial wallets, sanctions screening, and the
// gateway that turns deposits into its ticker, customer instructions into
// token payments, and inbound payments into accept/reject decisions.
package bank

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"omnibus-deposit-network/internal/chain"
	"omnibus-deposit-network/internal/fedwire"
	"omnibus-deposit-network/internal/iso20022"
	"omnibus-deposit-network/internal/omnibus"
	"omnibus-deposit-network/internal/operator"
)

// Screener is the bank's sanctions screening.
type Screener interface {
	Screen(name string, wallet common.Address) (hit bool, detail string)
}

// ListScreener matches names (case-insensitive substring) and wallets.
type ListScreener struct {
	Names   []string
	Wallets map[common.Address]string
}

func (l ListScreener) Screen(name string, wallet common.Address) (bool, string) {
	for _, n := range l.Names {
		if name != "" && strings.Contains(strings.ToLower(name), strings.ToLower(n)) {
			return true, "name match: " + n
		}
	}
	if d, ok := l.Wallets[wallet]; ok {
		return true, "wallet match: " + d
	}
	return false, ""
}

// Core is the bank's core deposit ledger (demand deposit accounts).
type Core struct {
	mu       sync.Mutex
	balances map[string]*big.Int
	posted   map[string]bool
	entries  map[string][]Entry
	clock    func() time.Time
}

// Entry is one posting on a deposit account, for statements (camt.053).
type Entry struct {
	At      time.Time
	Ref     string
	Delta   *big.Int
	Balance *big.Int
}

func NewCore() *Core {
	return &Core{balances: map[string]*big.Int{}, posted: map[string]bool{}, entries: map[string][]Entry{}, clock: time.Now}
}

// SetClock makes postings carry the scenario's time.
func (c *Core) SetClock(f func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock = f
}

// Entries returns an account's postings, oldest first.
func (c *Core) Entries(acct string) []Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Entry(nil), c.entries[acct]...)
}

// Has reports whether an account exists.
func (c *Core) Has(acct string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.balances[acct]
	return ok
}

func (c *Core) Open(acct string, opening *big.Int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.balances[acct] = new(big.Int).Set(opening)
}

// Post applies a signed amount once per reference.
func (c *Core) Post(acct string, delta *big.Int, ref string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.posted[ref] {
		return nil
	}
	b, ok := c.balances[acct]
	if !ok {
		return fmt.Errorf("no account %s", acct)
	}
	nb := new(big.Int).Add(b, delta)
	if nb.Sign() < 0 {
		return fmt.Errorf("account %s: insufficient funds", acct)
	}
	c.balances[acct] = nb
	c.posted[ref] = true
	c.entries[acct] = append(c.entries[acct], Entry{At: c.clock(), Ref: ref, Delta: new(big.Int).Set(delta), Balance: new(big.Int).Set(nb)})
	return nil
}

func (c *Core) Balance(acct string) *big.Int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(c.balances[acct])
}

// Customer is a depositor with a custodial wallet.
type Customer struct {
	Name   string
	DDA    string
	Wallet chain.Account
	Closed bool
}

// Gateway is the bank's integration with the operator, the Fed and the chain.
type Gateway struct {
	Bank   *omnibus.Bank
	Net    *omnibus.Network
	Fed    *fedwire.Service
	Dir    *operator.Directory
	Core   *Core
	Screen Screener
	Log    operator.Logger

	mu            sync.Mutex
	customers     map[string]*Customer // by DDA
	byWallet      map[common.Address]*Customer
	inboundCursor uint64
	redeemCursor  uint64
	answered      map[[32]byte]bool
	seq           int
	Rejected      []string
}

func NewGateway(b *omnibus.Bank, net *omnibus.Network, fed *fedwire.Service, dir *operator.Directory, scr Screener, log operator.Logger) *Gateway {
	g := &Gateway{
		Bank: b, Net: net, Fed: fed, Dir: dir, Core: NewCore(), Screen: scr, Log: log,
		customers: map[string]*Customer{}, byWallet: map[common.Address]*Customer{}, answered: map[[32]byte]bool{},
	}
	g.Core.SetClock(fed.Now)
	return g
}

func (g *Gateway) ref(kind string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq++
	return fmt.Sprintf("%s-%s-%06d", g.Bank.Spec.MemberID, kind, g.seq)
}

func (g *Gateway) name() string { return g.Bank.Spec.Name }

// Onboard opens a deposit account, creates a custodial wallet, admits it to
// the bank's ticker and publishes it in the operator's directory.
func (g *Gateway) Onboard(ctx context.Context, name, dda string, opening *big.Int) (*Customer, error) {
	w, err := chain.NewAccount(name)
	if err != nil {
		return nil, err
	}
	if err := g.Net.C.FundGas(ctx, w.Addr); err != nil {
		return nil, err
	}
	if _, err := g.Bank.Registry.Send(ctx, g.Bank.Keys.Registrar, "authorize", w.Addr, chain.RefOf("KYC-"+dda)); err != nil {
		return nil, err
	}
	g.Core.Open(dda, opening)
	c := &Customer{Name: name, DDA: dda, Wallet: w}
	g.mu.Lock()
	g.customers[dda] = c
	g.byWallet[w.Addr] = c
	g.mu.Unlock()
	g.Dir.Register(g.Bank.Spec.ABA, dda, w.Addr)
	g.Log("%-11s onboarded %s, DDA %s, deposit $%s", g.name(), name, dda, iso20022.FormatDollars(opening))
	return c, nil
}

// Close marks a customer's account closed; inbound payments to it are
// rejected with AC04.
func (g *Gateway) Close(dda string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.customers[dda]; ok {
		c.Closed = true
	}
}

// Customer looks up a customer by DDA.
func (g *Gateway) Customer(dda string) *Customer {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.customers[dda]
}

// FundOmnibus sends reserves from the bank's master account to the operator's joint
// account: a Fedwire pacs.009 BTRC carrying the member id in EndToEndId.
// the operator posts the position when the Fed's advice arrives.
func (g *Gateway) FundOmnibus(units *big.Int) iso20022.Pacs002 {
	amt, err := iso20022.USD(units)
	if err != nil {
		return iso20022.Pacs002{TxSts: iso20022.StatusRejected, Reason: iso20022.ReasonInsufficient}
	}
	uetr := fedwire.NewUETR()
	m := iso20022.Pacs009{
		GrpHdr: iso20022.GroupHeader{MsgID: g.ref("FW")}, UETR: uetr, Amount: amt,
		Debtor: iso20022.Agent{BICFI: g.Bank.Spec.MemberID, ABA: g.Bank.Spec.ABA}, Creditor: operator.OperatorAgent,
		DebtorAcct: g.Bank.Spec.MasterAccount, CreditorAcct: operator.JointAccount, Purpose: "FUND",
		EndToEndID: g.Bank.Spec.MemberID, LclInstrm: g.fundingInstrument(), CtgyPurp: operator.FundingPurpose,
	}
	g.Log("Fed          pacs.009 %s %s master -> the joint $%s", m.LclInstrm, g.name(), amt.Value)
	st := g.Fed.Send(m, units)
	if st.TxSts != iso20022.StatusAccepted {
		g.Log("Fedwire      pacs.002 %s %s", st.TxSts, st.Reason)
	}
	return st
}

// CustomerByWallet finds the customer a custodial wallet belongs to.
func (g *Gateway) CustomerByWallet(addr common.Address) *Customer {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.byWallet[addr]
}

// RequestDefundOnly is the maker half of a defund: the operator key
// requests it and the id comes back for a separate approver.
func (g *Gateway) RequestDefundOnly(ctx context.Context, units *big.Int) ([32]byte, error) {
	rcpt, err := g.Net.Ledger.Send(ctx, g.Bank.Keys.Operator, "requestDefund", chain.Bytes32(g.Bank.Spec.MemberID), units, chain.RefOf(g.ref("DEF")))
	if err != nil {
		return [32]byte{}, err
	}
	topic := g.Net.Ledger.EventID("DefundRequested")
	for _, l := range rcpt.Logs {
		if l.Address == g.Net.Ledger.Addr && len(l.Topics) > 1 && l.Topics[0] == topic {
			return l.Topics[1], nil
		}
	}
	return [32]byte{}, fmt.Errorf("no DefundRequested event")
}

// ApproveDefund is the checker half, signed by the treasury approver key.
func (g *Gateway) ApproveDefund(ctx context.Context, id [32]byte) error {
	_, err := g.Net.Ledger.Send(ctx, g.Bank.Keys.Approver, "approveDefund", id)
	return err
}

// RequestDefund asks the operator to return free position to the master account.
// The operator key requests and a separate treasury key approves, the two
// portal users an RTP disbursement needs.
func (g *Gateway) RequestDefund(ctx context.Context, units *big.Int) error {
	rcpt, err := g.Net.Ledger.Send(ctx, g.Bank.Keys.Operator, "requestDefund", chain.Bytes32(g.Bank.Spec.MemberID), units, chain.RefOf(g.ref("DEF")))
	if err != nil {
		return err
	}
	var id [32]byte
	topic := g.Net.Ledger.EventID("DefundRequested")
	for _, l := range rcpt.Logs {
		if l.Address == g.Net.Ledger.Addr && len(l.Topics) > 1 && l.Topics[0] == topic {
			id = l.Topics[1]
		}
	}
	if _, err := g.Net.Ledger.Send(ctx, g.Bank.Keys.Approver, "approveDefund", id); err != nil {
		return err
	}
	g.Log("%-11s defund $%s requested by operator, approved by treasury", g.name(), iso20022.FormatDollars(units))
	return nil
}

// fundingInstrument is Fedwire (BTRC) during its operating day and a FedNow
// liquidity management transfer (LMT1) outside it.
func (g *Gateway) fundingInstrument() string {
	if g.Fed.Calendar().IsOpen(g.Fed.Now()) {
		return "BTRC"
	}
	return "LMT1"
}

// Tokenize debits a customer's deposit and mints the same amount of the
// bank's ticker to the customer's wallet. If the mint fails, the debit is
// reversed.
func (g *Gateway) Tokenize(ctx context.Context, dda string, units *big.Int) error {
	c := g.Customer(dda)
	if c == nil {
		return fmt.Errorf("no customer %s", dda)
	}
	ref := g.ref("MINT")
	if err := g.Core.Post(dda, new(big.Int).Neg(units), ref); err != nil {
		return err
	}
	if _, err := g.Bank.Token.Send(ctx, g.Bank.Keys.Issuer, "mint", c.Wallet.Addr, units, chain.RefOf(ref)); err != nil {
		_ = g.Core.Post(dda, units, ref+"-REV")
		return err
	}
	g.Log("%-11s %s: DDA -$%s, mint %s %s", g.name(), c.Name, iso20022.FormatDollars(units), iso20022.FormatDollars(units), g.Bank.Spec.Ticker)
	return nil
}

// Redeem burns a customer's tokens; SyncRedemptions credits the deposit.
func (g *Gateway) Redeem(ctx context.Context, dda string, units *big.Int) error {
	c := g.Customer(dda)
	if c == nil {
		return fmt.Errorf("no customer %s", dda)
	}
	_, err := g.Bank.Token.Send(ctx, c.Wallet, "redeem", units, chain.RefOf(g.ref("RED")))
	if err == nil {
		g.Log("%-11s %s redeems %s %s", g.name(), c.Name, iso20022.FormatDollars(units), g.Bank.Spec.Ticker)
	}
	return err
}

// SyncRedemptions credits deposits for Redeemed events, once each.
func (g *Gateway) SyncRedemptions(ctx context.Context) error {
	head, err := g.Net.C.Head(ctx)
	if err != nil {
		return err
	}
	logs, err := g.Bank.Token.Events(ctx, "Redeemed", g.redeemCursor, head)
	if err != nil {
		return err
	}
	for _, l := range logs {
		var ev struct {
			Holder common.Address
			Amount *big.Int
			Ref    [32]byte
		}
		if err := g.Bank.Token.Unpack(&ev, "Redeemed", l); err != nil {
			return err
		}
		holder := common.BytesToAddress(l.Topics[1].Bytes())
		g.mu.Lock()
		c := g.byWallet[holder]
		g.mu.Unlock()
		if c == nil {
			continue
		}
		if err := g.Core.Post(c.DDA, ev.Amount, fmt.Sprintf("RED-%x", l.Topics[2])); err != nil {
			return err
		}
		g.Log("%-11s DDA %s credited +$%s (redemption)", g.name(), c.DDA, iso20022.FormatDollars(ev.Amount))
	}
	g.redeemCursor = head + 1
	return nil
}

// Result of a customer payment instruction.
type Result struct {
	Instruction iso20022.Pacs008
	Status      iso20022.Pacs002
	PaymentID   [32]byte
}

// SendPayment executes a customer credit transfer. Same bank: a token
// transfer. Another member: screened, resolved through the operator's directory, and
// sent through the router, which settles now or waits for the receiver.
func (g *Gateway) SendPayment(ctx context.Context, dda, cdtrName, cdtrABA, cdtrAcct string, units *big.Int, remit string) (Result, error) {
	return g.SendPaymentOpts(ctx, dda, cdtrName, cdtrABA, cdtrAcct, units, remit, true, "")
}

// SendPaymentOpts is SendPayment with screening optional: a payment that
// compliance already reviewed and released is not screened again.
//
// uetr is the client order's UETR, the payment's end-to-end identity ("" mints
// a fresh one). It becomes the router's clientRef, so the on-chain payment id
// derives from it: the chain record traces back to the order, and a retried
// order finds its payment instead of paying twice.
func (g *Gateway) SendPaymentOpts(ctx context.Context, dda, cdtrName, cdtrABA, cdtrAcct string, units *big.Int, remit string, screen bool, uetr string) (Result, error) {
	c := g.Customer(dda)
	if c == nil {
		return Result{}, fmt.Errorf("no customer %s", dda)
	}
	amt, err := iso20022.USD(units)
	if err != nil {
		return Result{}, err
	}
	if uetr == "" {
		uetr = fedwire.NewUETR()
	}
	instr := iso20022.Pacs008{
		GrpHdr: iso20022.GroupHeader{MsgID: g.ref("P8"), CreDtTm: g.Fed.Now()}, EndToEndID: g.ref("E2E"), UETR: uetr, Amount: amt,
		DebtorName: c.Name, DebtorAcct: dda, DebtorAgent: iso20022.Agent{BICFI: g.Bank.Spec.MemberID, ABA: g.Bank.Spec.ABA},
		CreditorName: cdtrName, CreditorAcct: cdtrAcct, CreditorAgt: iso20022.Agent{ABA: cdtrABA}, RemitInfo: remit,
	}
	res := Result{Instruction: instr}
	status := func(sts, rsn string) iso20022.Pacs002 {
		return iso20022.Pacs002{GrpHdr: iso20022.GroupHeader{MsgID: g.ref("P2"), CreDtTm: g.Fed.Now()}, OrgnlUETR: uetr, TxSts: sts, Reason: rsn}
	}

	payee, ok := g.Dir.Resolve(cdtrABA, cdtrAcct)
	if !ok {
		res.Status = status(iso20022.StatusRejected, iso20022.ReasonClosedAccount)
		g.Log("%-11s pacs.008 %s -> %s $%s RJCT AC04 (not in the operator directory)", g.name(), c.Name, cdtrName, amt.Value)
		return res, nil
	}
	if hit, why := g.Screen.Screen(cdtrName, payee); screen && hit {
		res.Status = status(iso20022.StatusRejected, iso20022.ReasonRegulatory)
		g.Log("%-11s pacs.008 %s -> %s $%s RJCT RR04 (outbound screening: %s)", g.name(), c.Name, cdtrName, amt.Value, why)
		return res, nil
	}

	if cdtrABA == g.Bank.Spec.ABA {
		if _, err := g.Bank.Token.Send(ctx, c.Wallet, "transfer", payee, units); err != nil {
			return res, err
		}
		res.Status = status(iso20022.StatusAccepted, "")
		g.Log("%-11s pacs.008 %s -> %s $%s on-us %s transfer ACSC (omnibus unchanged)", g.name(), c.Name, cdtrName, amt.Value, g.Bank.Spec.Ticker)
		return res, nil
	}

	to, ok := g.Net.BankByABA(cdtrABA)
	if !ok {
		res.Status = status(iso20022.StatusRejected, iso20022.ReasonClosedAccount)
		return res, nil
	}
	// Exactly once per UETR: the router's payment id is a function of payer and
	// clientRef, so an order already on-chain (a retry after a crash, say) is
	// found and reported, not sent again.
	id := g.paymentID(c.Wallet.Addr, chain.RefOf(uetr))
	var settled bool
	if pv, perr := g.Net.Payment(ctx, id); perr != nil {
		return res, perr
	} else if pv.Status != 0 {
		settled = pv.Status == 2
		g.Log("%-11s pacs.008 %s UETR %s already on-chain (status %d): not sent again", g.name(), c.Name, uetr, pv.Status)
	} else {
		rcpt, err := g.Net.Router.Send(ctx, c.Wallet, "pay", g.Bank.Token.Addr, to.Token.Addr, payee, units, chain.RefOf(uetr))
		if err != nil {
			return res, err
		}
		if id, settled, err = g.paymentOutcome(rcpt); err != nil {
			return res, err
		}
	}
	res.PaymentID = id
	if settled {
		res.Status = status(iso20022.StatusAccepted, "")
		g.Log("%-11s pacs.008 %s -> %s@%s $%s: %s burned, reserves moved in omnibus, %s minted ACSC",
			g.name(), c.Name, cdtrName, to.Spec.Name, amt.Value, g.Bank.Spec.Ticker, to.Spec.Ticker)
	} else {
		res.Status = status(iso20022.StatusPending, "")
		g.Log("%-11s pacs.008 %s -> %s@%s $%s PDNG (held; awaiting %s)", g.name(), c.Name, cdtrName, to.Spec.Name, amt.Value, to.Spec.Name)
	}
	return res, nil
}

// paymentID is PaymentRouter's id for a payer's clientRef:
// keccak256(abi.encode(chainid, router, payer, clientRef)).
func (g *Gateway) paymentID(payer common.Address, clientRef [32]byte) [32]byte {
	enc := make([]byte, 0, 128)
	enc = append(enc, common.LeftPadBytes(g.Net.C.ChainID.Bytes(), 32)...)
	enc = append(enc, common.LeftPadBytes(g.Net.Router.Addr.Bytes(), 32)...)
	enc = append(enc, common.LeftPadBytes(payer.Bytes(), 32)...)
	enc = append(enc, clientRef[:]...)
	return crypto.Keccak256Hash(enc)
}

func (g *Gateway) paymentOutcome(rcpt *types.Receipt) ([32]byte, bool, error) {
	settled := g.Net.Router.EventID("PaymentSettled")
	pending := g.Net.Router.EventID("PaymentPending")
	for _, l := range rcpt.Logs {
		if l.Address != g.Net.Router.Addr || len(l.Topics) < 2 {
			continue
		}
		switch l.Topics[0] {
		case settled:
			return l.Topics[1], true, nil
		case pending:
			return l.Topics[1], false, nil
		}
	}
	return [32]byte{}, false, errors.New("router emitted no payment event")
}

// ProcessInbound answers every pending payment addressed to this bank:
// payee must be an open account here, payer and payee must clear
// screening. Accept is pacs.002 ACSC; reject is RJCT with a reason code.
func (g *Gateway) ProcessInbound(ctx context.Context) error {
	head, err := g.Net.C.Head(ctx)
	if err != nil {
		return err
	}
	logs, err := g.Net.Router.Events(ctx, "PaymentPending", g.inboundCursor, head)
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
			Deadline  uint64
		}
		if err := g.Net.Router.Unpack(&ev, "PaymentPending", l); err != nil {
			return err
		}
		ev.PaymentId = l.Topics[1]
		ev.Payer = common.BytesToAddress(l.Topics[2].Bytes())
		ev.Payee = common.BytesToAddress(l.Topics[3].Bytes())
		if ev.ToToken != g.Bank.Token.Addr || g.answered[ev.PaymentId] {
			continue
		}
		g.answered[ev.PaymentId] = true
		from, _ := g.Net.BankByToken(ev.FromToken)
		g.mu.Lock()
		c := g.byWallet[ev.Payee]
		g.mu.Unlock()

		reason := ""
		switch {
		case c == nil || c.Closed:
			reason = iso20022.ReasonClosedAccount
		default:
			if hit, _ := g.Screen.Screen("", ev.Payer); hit {
				reason = iso20022.ReasonRegulatory
			}
		}
		amt := iso20022.FormatDollars(ev.Amount)
		if reason != "" {
			if _, err := g.Net.Router.Send(ctx, g.Bank.Keys.Operator, "reject", ev.PaymentId, chain.Bytes4(reason)); err != nil {
				return err
			}
			g.Rejected = append(g.Rejected, reason)
			g.Log("%-11s pacs.002 RJCT %s: inbound $%s from %s (hold released)", g.name(), reason, amt, from.Spec.Name)
			continue
		}
		if _, err := g.Net.Router.Send(ctx, g.Bank.Keys.Operator, "accept", ev.PaymentId); err != nil {
			return err
		}
		g.Log("%-11s pacs.002 ACSC: inbound $%s from %s to %s; %s burned, reserves moved, %s minted",
			g.name(), amt, from.Spec.Name, c.Name, from.Spec.Ticker, g.Bank.Spec.Ticker)
	}
	g.inboundCursor = head + 1
	return nil
}

// TokenBalance reads a customer's balance of any member's ticker.
func (g *Gateway) TokenBalance(ctx context.Context, dda string, of *omnibus.Bank) (*big.Int, error) {
	c := g.Customer(dda)
	if c == nil {
		return nil, fmt.Errorf("no customer %s", dda)
	}
	return of.Token.BigInt(ctx, "balanceOf", c.Wallet.Addr)
}
