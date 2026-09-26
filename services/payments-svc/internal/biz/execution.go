package biz

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
)

// RTPLimit is the RTP network's per-payment limit since 9 Feb 2025 (FedNow's
// since 12 Nov 2025).
var RTPLimit = Dollars(10_000_000)

// advance screens an approved order against the OFAC SDN list and executes
// it, or holds it for compliance.
func (e *Engine) advance(ctx context.Context, o *Order) {
	if !o.Released {
		if m, hit := e.screen.Screen(ctx, o.Creditor.Name); hit {
			o.Reason = "SANCTIONS"
			o.SanctionsMatch = describeMatch(m)
			e.event(o, "screening", StatusOnHold, ISOPending, "held for compliance review", "possible match: "+o.SanctionsMatch)
			return
		}
		e.event(o, "screening", o.Status, o.ISO, "sanctions screening clear", "")
	}
	e.execute(ctx, o)
}

func describeMatch(m SanctionsMatch) string {
	progs := ""
	if len(m.Programs) > 0 {
		progs = " (" + strings.Join(m.Programs, ", ") + ")"
	}
	return fmt.Sprintf("OFAC SDN #%s %s%s, score %.2f", m.SourceID, m.Name, progs, m.Score)
}

// execute routes the order: a payee at the same bank is a book transfer; a
// NORMAL order to another bank waits for a netting cycle; an URGENT one
// settles now as tokenized deposits.
func (e *Engine) execute(ctx context.Context, o *Order) {
	to, _ := e.rails.BankByRouting(o.Creditor.Routing)
	from, _ := e.rails.Bank(o.Bank)
	switch {
	case to.MemberID == o.Bank:
		o.Route = RouteBook
		rec, ok := e.rails.Account(o.Bank, o.Creditor.Account)
		if !ok || rec.Closed {
			e.reject(o, "AC04", "no such open account at "+from.Name)
			return
		}
		if err := e.rails.Post(o.Bank, o.DebtorAccount, -o.Amount, o.ID+"-DR"); err != nil {
			e.rejectOrFail(o, err)
			return
		}
		_ = e.rails.Post(o.Bank, o.Creditor.Account, o.Amount, o.ID+"-CR")
		e.settle(o, "booked between two accounts at "+from.Name, "")
		e.credited(o, from.Name, "")

	case o.Priority == Normal:
		o.Route = RouteNetting
		ref, err := e.rails.SubmitObligation(ctx, o.Bank, o.DebtorAccount, o.Creditor, o.Amount)
		if err != nil {
			e.rejectOrFail(o, err)
			return
		}
		o.ObligationRef = ref
		o.RouteNote = "funded from the deposit account; settles with every other obligation in the operator's next netting cycle, using only net liquidity"
		e.event(o, "system", StatusQueuedForNetting, ISOInProcess, "account debited; queued for the operator's next netting cycle",
			"obligation "+short(ref)+" in OmnibusNetting")

	default:
		o.Route = RouteInstant
		o.RouteNote = e.whyInstant(o)
		e.executeUrgent(ctx, o)
	}
}

// executeUrgent tokenizes the deposit against the bank's omnibus position
// and sends it across the network. Without enough mint capacity the order
// waits and the bank's treasury is alerted.
func (e *Engine) executeUrgent(ctx context.Context, o *Order) {
	from, _ := e.rails.Bank(o.Bank)
	if !o.Tokenized {
		m, err := e.rails.Member(ctx, o.Bank)
		if err != nil {
			e.failed(o, err)
			return
		}
		if m.MintCapacity < o.Amount {
			o.Reason = "LIQUIDITY"
			if o.Status != StatusAwaitingLiquidity {
				e.event(o, "system", StatusAwaitingLiquidity, ISOPending,
					"waiting for "+from.Name+" to make liquidity available; the payment goes as soon as it is",
					fmt.Sprintf("mint capacity $%s is short of $%s; alert raised to treasury", Readable(m.MintCapacity), Readable(o.Amount)))
				e.raiseLiquidityAlert(ctx, o, m.MintCapacity)
			}
			return
		}
		if err := e.rails.Tokenize(ctx, o.Bank, o.DebtorAccount, o.Amount); err != nil {
			e.rejectOrFail(o, err)
			return
		}
		o.Tokenized = true
		o.Reason = ""
		e.event(o, "system", StatusInProcess, ISOInProcess,
			fmt.Sprintf("account debited $%s; payment released to the network", Readable(o.Amount)),
			fmt.Sprintf("$%s tokenized as %s against %s's omnibus position", Readable(o.Amount), from.Ticker, from.Name))
	}
	res, err := e.rails.SendTokenPayment(ctx, o.Bank, o.DebtorAccount, o.Creditor, o.Amount, o.Remittance, o.UETR)
	if err != nil {
		e.refund(ctx, o)
		e.failed(o, err)
		return
	}
	o.PaymentRef = res.Ref
	switch res.State {
	case TokenRejected:
		e.refund(ctx, o)
		e.reject(o, res.Reason, "rejected before settlement")
	case TokenSettled:
		o.Tokenized = false
		e.settle(o, "settled on the network", e.ledgerDetail(o))
	default:
		to, _ := e.rails.BankByRouting(o.Creditor.Routing)
		e.event(o, "system", StatusPendingReceiver, ISOPending,
			"on the network, held against the payer; waiting for "+to.Name+" to screen and accept",
			"PaymentRouter payment "+short(res.Ref))
	}
}

// whyInstant says, in the client's words, why the order goes on-network.
func (e *Engine) whyInstant(o *Order) string {
	now := e.now()
	var why []string
	if !e.rails.FedwireOpen(now) {
		why = append(why, "Fedwire is closed until "+e.rails.NextFedwireOpen(now).In(e.et).Format("Mon 2 Jan 15:04 MST"))
	}
	if o.Amount > RTPLimit {
		why = append(why, "the amount is above RTP's $10,000,000.00 per-payment limit")
	}
	if len(why) == 0 {
		return "instant, final settlement on the network"
	}
	return strings.Join(why, " and ") + "; settled instantly on the network instead"
}

func (e *Engine) instrument() string {
	if e.rails.FedwireOpen(e.now()) {
		return "Fedwire BTRC is open"
	}
	return "Fedwire is closed; FedNow LMT can fund it now"
}

// refund redeems an order's tokens back into the payer's account.
func (e *Engine) refund(ctx context.Context, o *Order) {
	if !o.Tokenized {
		return
	}
	from, _ := e.rails.Bank(o.Bank)
	if err := e.rails.Redeem(ctx, o.Bank, o.DebtorAccount, o.Amount); err != nil {
		e.event(o, "system", o.Status, o.ISO, "refund pending; operations notified", "redeem failed: "+err.Error())
		return
	}
	o.Tokenized = false
	e.event(o, "system", o.Status, o.ISO, "account re-credited $"+Readable(o.Amount), from.Ticker+" redeemed back into the deposit")
}

func (e *Engine) settle(o *Order, note, detail string) {
	t := e.now()
	o.SettledAt = &t
	o.Reason = ""
	e.event(o, "system", StatusSettled, ISOSettled, note, detail)
}

// credited records the receiving bank's credit to the payee (ACCC).
func (e *Engine) credited(o *Order, bankName, detail string) {
	e.event(o, bankName, StatusSettled, ISOCredited, "credited to "+o.Creditor.Name+"'s account "+o.Creditor.Account, detail)
}

// ledgerDetail is what moved on the operator's ledger when a token payment settled.
func (e *Engine) ledgerDetail(o *Order) string {
	from, _ := e.rails.Bank(o.Bank)
	to, _ := e.rails.BankByRouting(o.Creditor.Routing)
	return fmt.Sprintf("%s burned; $%s of backing moved %s → %s on the operator's ledger; %s minted to %s",
		from.Ticker, Readable(o.Amount), from.Name, to.Name, to.Ticker, to.Name)
}

func (e *Engine) reject(o *Order, reason, note string) {
	o.Reason = reason
	e.event(o, "system", StatusRejected, ISORejected, fmt.Sprintf("%s (%s)", note, reason), "")
}

func (e *Engine) rejectOrFail(o *Order, err error) {
	if stderrors.Is(err, ErrInsufficientFunds) {
		e.reject(o, "AM04", "insufficient funds")
		return
	}
	e.failed(o, err)
}

func (e *Engine) failed(o *Order, err error) {
	e.log.Errorf("payment %s failed: %v", o.ID, err)
	o.Reason = "NARR"
	e.event(o, "system", StatusRejected, ISORejected, "processing error; operations notified (NARR)", err.Error())
}

// syncNettedOne applies an obligation's state on chain to its order.
func (e *Engine) syncNettedOne(ctx context.Context, o *Order, actor string) {
	st, err := e.rails.ObligationState(ctx, o.ObligationRef)
	if err != nil {
		return
	}
	from, _ := e.rails.Bank(o.Bank)
	to, _ := e.rails.BankByRouting(o.Creditor.Routing)
	switch st {
	case ObligationSettled:
		e.settle(o, "settled in an operator netting cycle",
			fmt.Sprintf("obligation discharged in a verified netting cycle; free position moved %s → %s net of the cycle", from.Name, to.Name))
	case ObligationCancelled:
		e.event(o, actor, StatusCancelled, ISOCancelled, "withdrawn from the netting queue; account re-credited", "")
	case ObligationExpired:
		o.Reason = "AB05"
		e.event(o, "system", StatusExpired, ISORejected, "not settled before the obligation expired; account re-credited (AB05)", "")
	}
}

func short(ref string) string {
	if len(ref) > 12 {
		return "0x" + ref[:10] + "…"
	}
	return ref
}
