package biz

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"omnibus-deposit-network/services/payments-svc/internal/iso"
)

// BlockedAccount is where a bank holds funds blocked under OFAC rules.
const BlockedAccount = "OFAC-BLOCKED"

// ─── Compliance ───

// ComplianceUseCase is a bank's sanctions review.
type ComplianceUseCase struct{ e *Engine }

// NewComplianceUseCase creates the use case.
func NewComplianceUseCase(e *Engine) *ComplianceUseCase { return &ComplianceUseCase{e: e} }

func compliance(p Principal) error {
	if p.Role != RoleCompliance || p.Org != "" {
		return ErrForbidden("sanctions review is for the bank's compliance staff")
	}
	return nil
}

// Holds lists the bank's held orders and its blocked or rejected cases.
func (uc *ComplianceUseCase) Holds(ctx context.Context, p Principal) (holds, cases []*Order, err error) {
	if err := compliance(p); err != nil {
		return nil, nil, err
	}
	if holds, err = uc.e.ordersIn(ctx, p.Bank, true, StatusOnHold); err != nil {
		return nil, nil, err
	}
	if cases, err = uc.e.r.Orders.ListCases(ctx, p.Bank); err != nil {
		return nil, nil, err
	}
	return viewsFor(p, holds), viewsFor(p, cases), nil
}

func (uc *ComplianceUseCase) held(ctx context.Context, p Principal, id string) (*Order, error) {
	if err := compliance(p); err != nil {
		return nil, err
	}
	o, err := uc.e.load(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if o.Status != StatusOnHold {
		return nil, ErrConflict("payment is %s", o.Status)
	}
	return o, nil
}

// Release clears a false positive; the order executes.
func (uc *ComplianceUseCase) Release(ctx context.Context, p Principal, id, note string) (*Order, error) {
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	o, err := uc.held(ctx, p, id)
	if err != nil {
		return nil, err
	}
	o.Released, o.Reason = true, ""
	e.event(o, p.Name, StatusInProcess, ISOPending, "released after compliance review", "false positive: "+note)
	e.execute(ctx, o)
	if err := e.save(ctx, o, false); err != nil {
		return nil, err
	}
	return ViewFor(p, o), nil
}

// Block handles a confirmed match in which a sanctioned party has an
// interest in the funds. OFAC requires the bank to block them, not return
// them: the amount leaves the client's account for a blocked account from
// which only OFAC-authorized debits are made, and the bank reports within
// 10 business days (31 CFR 501.603).
func (uc *ComplianceUseCase) Block(ctx context.Context, p Principal, id, note string) (*Order, error) {
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	o, err := uc.held(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if err := e.rails.Post(o.Bank, o.DebtorAccount, -o.Amount, o.ID+"-BLK"); err != nil {
		if stderrors.Is(err, ErrInsufficientFunds) {
			return nil, ErrConflict("the account cannot fund the payment, so there is nothing to block; reject it instead")
		}
		return nil, err
	}
	e.rails.EnsureAccount(o.Bank, BlockedAccount, "OFAC blocked property")
	_ = e.rails.Post(o.Bank, BlockedAccount, o.Amount, o.ID+"-BLK")
	due := e.cal.AddBusinessDays(e.now(), 10)
	o.OFACReportDue, o.Reason = &due, "RR04"
	from, _ := e.rails.Bank(o.Bank)
	e.event(o, p.Name, StatusBlocked, ISOBlocked, "funds blocked under US sanctions regulations",
		fmt.Sprintf("%s; $%s moved to %s's blocked account; report to OFAC due %s (31 CFR 501.603)",
			note, Readable(o.Amount), from.Name, due.Format("Mon 2 Jan 2006")))
	if err := e.save(ctx, o, false); err != nil {
		return nil, err
	}
	return ViewFor(p, o), nil
}

// Reject handles a prohibited payment in which no sanctioned party has an
// interest: nothing moves, and the rejection is reported within 10 business
// days (31 CFR 501.604).
func (uc *ComplianceUseCase) Reject(ctx context.Context, p Principal, id, note string) (*Order, error) {
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	o, err := uc.held(ctx, p, id)
	if err != nil {
		return nil, err
	}
	due := e.cal.AddBusinessDays(e.now(), 10)
	o.OFACReportDue, o.Reason = &due, "RR04"
	e.event(o, p.Name, StatusRejected, ISORejected, "rejected on regulatory grounds (RR04)",
		fmt.Sprintf("%s; report to OFAC due %s (31 CFR 501.604)", note, due.Format("Mon 2 Jan 2006")))
	if err := e.save(ctx, o, false); err != nil {
		return nil, err
	}
	return ViewFor(p, o), nil
}

// ─── Treasury ───

// TreasuryUseCase is a bank treasury's omnibus position.
type TreasuryUseCase struct{ e *Engine }

// NewTreasuryUseCase creates the use case.
func NewTreasuryUseCase(e *Engine) *TreasuryUseCase { return &TreasuryUseCase{e: e} }

// Liquidity returns the bank's omnibus position and what waits on it.
func (uc *TreasuryUseCase) Liquidity(ctx context.Context, p Principal) (*Liquidity, error) {
	if !p.IsBankStaff() {
		return nil, ErrForbidden("the omnibus position is for the bank's staff")
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	m, err := e.rails.Member(ctx, p.Bank)
	if err != nil {
		return nil, err
	}
	now := e.now()
	w := e.opts.Watermarks[p.Bank]
	v := &Liquidity{Member: m, LowWatermark: w.Low, NormalWatermark: w.Normal, FedwireOpen: e.rails.FedwireOpen(now),
		NextFedwireOpen: e.rails.NextFedwireOpen(now).In(e.et), Instrument: e.instrument()}
	waiting, err := e.ordersIn(ctx, p.Bank, true, StatusAwaitingLiquidity)
	if err != nil {
		return nil, err
	}
	v.AwaitingLiquidity = viewsFor(p, waiting)
	if v.Alerts, err = e.r.Alerts.ListByBank(ctx, p.Bank); err != nil {
		return nil, err
	}
	if v.Defunds, err = e.r.Defunds.ListByBank(ctx, p.Bank); err != nil {
		return nil, err
	}
	return v, nil
}

// Fund sends reserves into the omnibus: Fedwire while it is open, FedNow
// LMT while it is closed. Orders waiting for liquidity then proceed.
func (uc *TreasuryUseCase) Fund(ctx context.Context, p Principal, amount string) (FundResult, error) {
	if p.Role != RoleTreasury || p.Org != "" {
		return FundResult{}, ErrForbidden("only the bank's treasury funds the omnibus")
	}
	units, err := ParseAmount(amount)
	if err != nil {
		return FundResult{}, err
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	res := e.rails.Fund(ctx, p.Bank, units)
	if res.Status != "ACSC" {
		return res, nil
	}
	from, _ := e.rails.Bank(p.Bank)
	waiting, err := e.ordersIn(ctx, p.Bank, true, StatusAwaitingLiquidity)
	if err != nil {
		return res, err
	}
	for _, o := range waiting {
		e.event(o, from.Name, StatusAwaitingLiquidity, ISOPending, "liquidity available",
			fmt.Sprintf("%s funded the omnibus with $%s by %s", p.Name, Readable(units), res.Instrument))
		if err := e.save(ctx, o, false); err != nil {
			return res, err
		}
	}
	res.Released, err = e.retryLiquidity(ctx, p.Bank)
	return res, err
}

// RequestDefund is the maker half of taking free position back.
func (uc *TreasuryUseCase) RequestDefund(ctx context.Context, p Principal, amount string) (*Defund, error) {
	if p.Role != RoleTreasury || p.Org != "" {
		return nil, ErrForbidden("only the bank's treasury requests defunds")
	}
	units, err := ParseAmount(amount)
	if err != nil {
		return nil, err
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	ref, err := e.rails.RequestDefund(ctx, p.Bank, units)
	if err != nil {
		return nil, ErrLimit("defund refused by the ledger: %v", err)
	}
	now := e.now()
	d := &Defund{ID: newID(), Bank: p.Bank, Amount: units, RequestedByID: p.ID, RequestedBy: p.Name,
		Status: "AWAITING_APPROVAL", LedgerRef: ref, CreatedAt: now, UpdatedAt: now}
	return d, e.r.Defunds.Create(ctx, d)
}

// ApproveDefund is the checker half; the operator then sends it over Fedwire, now if
// Fedwire is open, at the next opening if not.
func (uc *TreasuryUseCase) ApproveDefund(ctx context.Context, p Principal, id string) (*Defund, error) {
	if p.Role != RoleTreasuryApprover || p.Org != "" {
		return nil, ErrForbidden("only a treasury approver approves defunds")
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.r.Defunds.Get(ctx, id)
	if stderrors.Is(err, ErrNoRows) || (err == nil && d.Bank != p.Bank) {
		return nil, ErrNotFound("no defund %s", id)
	}
	if err != nil {
		return nil, err
	}
	if d.Status != "AWAITING_APPROVAL" {
		return nil, ErrConflict("defund is %s", d.Status)
	}
	if d.RequestedByID == p.ID {
		return nil, ErrForbidden("the requester cannot approve")
	}
	if err := e.rails.ApproveDefund(ctx, p.Bank, d.LedgerRef); err != nil {
		return nil, err
	}
	d.ApprovedBy, d.Status, d.UpdatedAt = p.Name, "APPROVED", e.now()
	if err := e.r.Defunds.Update(ctx, d); err != nil {
		return nil, err
	}
	if err := e.updateDefunds(ctx); err != nil {
		return nil, err
	}
	return e.r.Defunds.Get(ctx, id)
}

// ─── operator operations ───

// NetworkUseCase is operator operations' console.
type NetworkUseCase struct{ e *Engine }

// NewNetworkUseCase creates the use case.
func NewNetworkUseCase(e *Engine) *NetworkUseCase { return &NetworkUseCase{e: e} }

func tchOps(p Principal) error {
	if p.Role != RoleOperatorOps {
		return ErrForbidden("operator operations only")
	}
	return nil
}

// View returns members, the Fed joint account against the ledger, cycles and
// reconciliations.
func (uc *NetworkUseCase) View(ctx context.Context, p Principal) (*NetworkView, error) {
	if err := tchOps(p); err != nil {
		return nil, err
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	total, err := e.rails.LedgerTotal(ctx)
	if err != nil {
		return nil, err
	}
	brk, inv := e.rails.Health(ctx)
	v := &NetworkView{Now: now, FedwireOpen: e.rails.FedwireOpen(now), NextFedwireOpen: e.rails.NextFedwireOpen(now).In(e.et),
		FedJoint: e.rails.FedJointBalance(), LedgerTotal: total, Break: brk, Invariants: inv,
		Queued: e.rails.QueuedObligations(ctx)}
	if !e.nextCycle.IsZero() {
		v.NextCycle = e.nextCycle.In(e.et)
	}
	for _, b := range e.rails.Banks() {
		m, err := e.rails.Member(ctx, b.MemberID)
		if err != nil {
			return nil, err
		}
		v.Members = append(v.Members, m)
	}
	if v.Cycles, err = e.r.Network.ListCycles(ctx, 50); err != nil {
		return nil, err
	}
	if v.Recons, err = e.r.Network.ListReconciliations(ctx, 50); err != nil {
		return nil, err
	}
	return v, nil
}

// RunCycle runs a netting cycle now.
func (uc *NetworkUseCase) RunCycle(ctx context.Context, p Principal) (*NettingCycle, error) {
	if err := tchOps(p); err != nil {
		return nil, err
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	c, err := e.runCycle(ctx, "run by "+p.Name)
	if err != nil {
		return nil, err
	}
	return c, e.tick(ctx)
}

// Reconcile attests the Fed's camt.052 balance for the joint account.
func (uc *NetworkUseCase) Reconcile(ctx context.Context, p Principal) (*Reconciliation, error) {
	if err := tchOps(p); err != nil {
		return nil, err
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.rails.Reconcile(ctx)
	if err != nil {
		return nil, err
	}
	r.ID = newID()
	return &r, e.r.Network.CreateReconciliation(ctx, &r)
}

// SetClock moves the scenario clock (and the chain's) forward and runs the
// background pass. The service layer only exposes it in demo builds.
func (uc *NetworkUseCase) SetClock(ctx context.Context, p Principal, t time.Time) (time.Time, error) {
	if err := tchOps(p); err != nil {
		return time.Time{}, err
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if !t.After(e.now()) {
		return time.Time{}, ErrInvalid("time only moves forward")
	}
	if err := e.rails.SetTime(ctx, t); err != nil {
		return time.Time{}, err
	}
	return e.now(), e.tick(ctx)
}

// ─── Account ───

// AccountUseCase is a client's deposit account.
type AccountUseCase struct{ e *Engine }

// NewAccountUseCase creates the use case.
func NewAccountUseCase(e *Engine) *AccountUseCase { return &AccountUseCase{e: e} }

func (uc *AccountUseCase) org(p Principal) (Org, error) {
	o, ok := uc.e.orgs[p.Org]
	if !ok {
		return Org{}, ErrForbidden("accounts are for client organizations")
	}
	return o, nil
}

// Get returns the organization's account.
func (uc *AccountUseCase) Get(ctx context.Context, p Principal) (*Account, error) {
	org, err := uc.org(p)
	if err != nil {
		return nil, err
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	b, _ := e.rails.Bank(org.Bank)
	return &Account{Org: org, BankName: b.Name, Routing: b.Routing, Balance: e.rails.Balance(org.Bank, org.Account),
		InFlight: e.rails.InFlight(ctx, org.Bank, org.Account), AsOf: e.now()}, nil
}

// Postings returns the account's entries.
func (uc *AccountUseCase) Postings(ctx context.Context, p Principal) ([]Posting, error) {
	org, err := uc.org(p)
	if err != nil {
		return nil, err
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rails.Postings(org.Bank, org.Account), nil
}

// Statement renders the account's postings as a camt.053.001.08.
func (uc *AccountUseCase) Statement(ctx context.Context, p Principal) ([]byte, error) {
	org, err := uc.org(p)
	if err != nil {
		return nil, err
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	b, _ := e.rails.Bank(org.Bank)
	ps := e.rails.Postings(org.Bank, org.Account)
	now := e.now()
	s := iso.Statement{MessageID: "STMT-" + newID()[:23], StatementID: fmt.Sprintf("%s-%s", org.Account, now.Format("20060102150405")),
		At: now, From: now, To: now, Account: org.Account, Owner: org.Name, Routing: b.Routing, Currency: "USD"}
	if len(ps) > 0 {
		s.From = ps[0].At
		s.Opening = ps[0].Balance - ps[0].Delta
		s.Closing = ps[len(ps)-1].Balance
	}
	for _, x := range ps {
		amt := x.Delta
		if amt < 0 {
			amt = -amt
		}
		s.Entries = append(s.Entries, iso.Entry{Ref: x.Ref, BookedAt: x.At, Amount: amt, Credit: x.Delta > 0})
	}
	return iso.Camt053(s)
}
