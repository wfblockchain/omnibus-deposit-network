package bank

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"

	"omnibus-deposit-network/internal/chain"
	"omnibus-deposit-network/internal/fedwire"
	"omnibus-deposit-network/internal/iso20022"
	"omnibus-deposit-network/internal/omnibus"
	"omnibus-deposit-network/internal/operator"
)

// Interbank is the bank's side of settlement between banks: deposit-funded
// payments that become obligations on operator netting contract, settled in
// a cycle or gross on request; and settlement of other banks' tokenized
// deposits its treasury holds.
type Interbank struct {
	G    *Gateway
	Msgs *operator.Messages

	mu       sync.Mutex
	outbound map[[32]byte]outboundPayment
	credited map[[32]byte]bool
	cursor   uint64
}

type outboundPayment struct {
	dda   string
	units *big.Int
	ref   string
}

func NewInterbank(g *Gateway, msgs *operator.Messages) *Interbank {
	return &Interbank{G: g, Msgs: msgs, outbound: map[[32]byte]outboundPayment{}, credited: map[[32]byte]bool{}}
}

// SendFromDeposit pays another member's customer out of a deposit account:
// the bank debits the account and owes the other bank. Urgent payments
// settle gross at once from the bank's free position; the rest wait for
// the next netting cycle.
func (ib *Interbank) SendFromDeposit(ctx context.Context, dda, cdtrName, cdtrABA, cdtrAcct string, units *big.Int, urgent bool) (Result, error) {
	return ib.SendFromDepositOpts(ctx, dda, cdtrName, cdtrABA, cdtrAcct, units, urgent, true)
}

// SendFromDepositOpts is SendFromDeposit with screening optional, for a
// payment compliance already reviewed and released.
func (ib *Interbank) SendFromDepositOpts(ctx context.Context, dda, cdtrName, cdtrABA, cdtrAcct string, units *big.Int, urgent, screen bool) (Result, error) {
	g := ib.G
	c := g.Customer(dda)
	if c == nil {
		return Result{}, fmt.Errorf("no customer %s", dda)
	}
	to, ok := g.Net.BankByABA(cdtrABA)
	if !ok || to.Spec.MemberID == g.Bank.Spec.MemberID {
		return Result{}, fmt.Errorf("no other member with ABA %s", cdtrABA)
	}
	amt, err := iso20022.USD(units)
	if err != nil {
		return Result{}, err
	}
	uetr := fedwire.NewUETR()
	instr := iso20022.Pacs008{
		GrpHdr: iso20022.GroupHeader{MsgID: g.ref("P8"), CreDtTm: g.Fed.Now()}, EndToEndID: g.ref("E2E"), UETR: uetr, Amount: amt,
		DebtorName: c.Name, DebtorAcct: dda, DebtorAgent: iso20022.Agent{BICFI: g.Bank.Spec.MemberID, ABA: g.Bank.Spec.ABA},
		CreditorName: cdtrName, CreditorAcct: cdtrAcct, CreditorAgt: iso20022.Agent{BICFI: to.Spec.MemberID, ABA: cdtrABA},
	}
	res := Result{Instruction: instr}
	status := func(sts, rsn string) iso20022.Pacs002 {
		return iso20022.Pacs002{GrpHdr: iso20022.GroupHeader{MsgID: g.ref("P2"), CreDtTm: g.Fed.Now()}, OrgnlUETR: uetr, TxSts: sts, Reason: rsn}
	}
	if hit, why := g.Screen.Screen(cdtrName, [20]byte{}); screen && hit {
		res.Status = status(iso20022.StatusRejected, iso20022.ReasonRegulatory)
		g.Log("%-11s pacs.008 %s -> %s $%s RJCT RR04 (outbound screening: %s)", g.name(), c.Name, cdtrName, amt.Value, why)
		return res, nil
	}

	ref := g.ref("OBL")
	if err := g.Core.Post(dda, new(big.Int).Neg(units), ref); err != nil {
		return res, err
	}
	rcpt, err := g.Net.Netting.Send(ctx, g.Bank.Keys.Operator, "submit",
		chain.Bytes32(g.Bank.Spec.MemberID), chain.Bytes32(to.Spec.MemberID), units, chain.RefOf(uetr))
	if err != nil {
		_ = g.Core.Post(dda, units, ref+"-REV")
		return res, err
	}
	var id [32]byte
	topic := g.Net.Netting.EventID("ObligationSubmitted")
	for _, l := range rcpt.Logs {
		if l.Address == g.Net.Netting.Addr && len(l.Topics) > 1 && l.Topics[0] == topic {
			id = l.Topics[1]
		}
	}
	res.PaymentID = id
	ib.Msgs.Put(id, instr)
	ib.mu.Lock()
	ib.outbound[id] = outboundPayment{dda: dda, units: units, ref: ref}
	ib.mu.Unlock()

	if urgent {
		if _, err := g.Net.Netting.Send(ctx, g.Bank.Keys.Operator, "settleGross", id); err != nil {
			return res, err
		}
		res.Status = status(iso20022.StatusAccepted, "")
		g.Log("%-11s pacs.008 %s -> %s@%s $%s: settled GROSS from %s's free position",
			g.name(), c.Name, cdtrName, to.Spec.Name, amt.Value, g.Bank.Spec.Name)
		return res, nil
	}
	res.Status = status(iso20022.StatusPending, "")
	g.Log("%-11s pacs.008 %s -> %s@%s $%s: DDA debited, obligation queued for netting",
		g.name(), c.Name, cdtrName, to.Spec.Name, amt.Value)
	return res, nil
}

type obligationView struct {
	Payer     [32]byte
	Payee     [32]byte
	Amount    *big.Int
	ExpiresAt uint64
	Status    uint8
}

// Sync credits payees for obligations owed to this bank that settled, and
// reverses the debit of its own customers' obligations that expired or were
// cancelled. Each is processed once.
func (ib *Interbank) Sync(ctx context.Context) error {
	g := ib.G
	head, err := g.Net.C.Head(ctx)
	if err != nil {
		return err
	}
	me := chain.Bytes32(g.Bank.Spec.MemberID)
	settled, err := g.Net.Netting.Events(ctx, "ObligationSettled", ib.cursor, head)
	if err != nil {
		return err
	}
	for _, l := range settled {
		id := l.Topics[1]
		out, err := g.Net.Netting.Call(ctx, "obligations", id)
		if err != nil {
			return err
		}
		o := obligationView{
			Payer: out[0].([32]byte), Payee: out[1].([32]byte),
			Amount: *abi.ConvertType(out[2], new(*big.Int)).(**big.Int), ExpiresAt: out[3].(uint64), Status: out[4].(uint8),
		}
		if o.Payee != me || ib.credited[id] {
			continue
		}
		msg, ok := ib.Msgs.Get(id)
		if !ok {
			return fmt.Errorf("obligation %x settled without a message", id)
		}
		if err := g.Core.Post(msg.CreditorAcct, o.Amount, fmt.Sprintf("OBL-IN-%x", id)); err != nil {
			return err
		}
		ib.credited[id] = true
		how := "in a netting cycle"
		if l.Topics[2] == ([32]byte{}) {
			how = "gross"
		}
		g.Log("%-11s credited %s (DDA %s) +$%s, settled %s", g.name(), msg.CreditorName, msg.CreditorAcct, iso20022.FormatDollars(o.Amount), how)
	}
	for _, name := range []string{"ObligationExpired", "ObligationCancelled"} {
		logs, err := g.Net.Netting.Events(ctx, name, ib.cursor, head)
		if err != nil {
			return err
		}
		for _, l := range logs {
			ib.mu.Lock()
			p, ok := ib.outbound[l.Topics[1]]
			delete(ib.outbound, l.Topics[1])
			ib.mu.Unlock()
			if ok {
				if err := g.Core.Post(p.dda, p.units, p.ref+"-REV"); err != nil {
					return err
				}
				g.Log("%-11s obligation %s: DDA %s re-credited $%s", g.name(), name[10:], p.dda, iso20022.FormatDollars(p.units))
			}
		}
	}
	ib.cursor = head + 1
	return nil
}

// Credited reports whether this bank has credited its customer for a
// settled obligation paid to it.
func (ib *Interbank) Credited(obligationID [32]byte) bool {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return ib.credited[obligationID]
}

// PayBankTreasury pays another member bank itself, in this bank's ticker:
// a fee, a margin call, an invoice. The other bank's treasury is a listed
// member wallet, so it may hold this ticker without being admitted here.
func (g *Gateway) PayBankTreasury(ctx context.Context, dda string, to *omnibus.Bank, units *big.Int) error {
	c := g.Customer(dda)
	if c == nil {
		return fmt.Errorf("no customer %s", dda)
	}
	if _, err := g.Bank.Token.Send(ctx, c.Wallet, "transfer", to.Keys.Treasury.Addr, units); err != nil {
		return err
	}
	g.Log("%-11s %s pays %s's treasury %s %s (a member wallet may hold any ticker)",
		g.name(), c.Name, to.Spec.Name, iso20022.FormatDollars(units), g.Bank.Spec.Ticker)
	return nil
}

// SettleHeld presents another member's tokens held in this bank's treasury:
// they burn, and the issuer's reserves become this bank's free position.
func (g *Gateway) SettleHeld(ctx context.Context, issuer *omnibus.Bank) (*big.Int, error) {
	bal, err := issuer.Token.BigInt(ctx, "balanceOf", g.Bank.Keys.Treasury.Addr)
	if err != nil || bal.Sign() == 0 {
		return bal, err
	}
	if _, err := g.Net.Router.Send(ctx, g.Bank.Keys.Treasury, "settleHeld", issuer.Token.Addr, bal); err != nil {
		return nil, err
	}
	g.Log("%-11s settles %s %s held in treasury: %s's reserves move to %s's free position",
		g.name(), iso20022.FormatDollars(bal), issuer.Spec.Ticker, issuer.Spec.Name, g.Bank.Spec.Name)
	return bal, nil
}
