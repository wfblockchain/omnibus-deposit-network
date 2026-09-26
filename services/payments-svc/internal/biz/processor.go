package biz

import (
	"context"
	"fmt"
	"time"
)

// Processor advances everything asynchronous: the rails' own processing,
// held payments the receiving bank answered, netting outcomes, scheduled
// cycles, expired obligations, payee credits, defunds, orders waiting for
// liquidity, and watermark alerts.
type Processor struct{ e *Engine }

// NewProcessor creates the processor.
func NewProcessor(e *Engine) *Processor { return &Processor{e: e} }

// Tick runs one pass.
func (pr *Processor) Tick(ctx context.Context) error {
	e := pr.e
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tick(ctx)
}

func (e *Engine) tick(ctx context.Context) error {
	if err := e.rails.Process(ctx); err != nil {
		e.log.Warnf("rails: %v", err)
	}
	if err := e.syncUrgent(ctx); err != nil {
		return err
	}
	if ran, err := e.scheduledNetting(ctx); err != nil {
		e.log.Errorf("scheduled netting: %v", err)
	} else if ran {
		_ = e.rails.Process(ctx)
	}
	if err := e.syncNetted(ctx); err != nil {
		return err
	}
	if err := e.rails.ExpireStaleObligations(ctx); err == nil {
		_ = e.rails.Process(ctx)
		if err := e.syncNetted(ctx); err != nil {
			return err
		}
	}
	if err := e.syncCredited(ctx); err != nil {
		return err
	}
	if err := e.updateDefunds(ctx); err != nil {
		return err
	}
	for _, b := range e.rails.Banks() {
		if _, err := e.retryLiquidity(ctx, b.MemberID); err != nil {
			return err
		}
		if err := e.watermarks(ctx, b.MemberID); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) ordersIn(ctx context.Context, bank string, oldestFirst bool, st ...Status) ([]*Order, error) {
	os, _, err := e.r.Orders.List(ctx, OrderFilter{Bank: bank, Statuses: st, PageSize: -1})
	if err != nil || !oldestFirst {
		return os, err
	}
	for i, j := 0, len(os)-1; i < j; i, j = i+1, j-1 {
		os[i], os[j] = os[j], os[i]
	}
	return os, nil
}

// syncUrgent applies the receiving bank's answer to held token payments.
func (e *Engine) syncUrgent(ctx context.Context) error {
	os, err := e.ordersIn(ctx, "", true, StatusPendingReceiver)
	if err != nil {
		return err
	}
	for _, o := range os {
		st, err := e.rails.TokenPaymentState(ctx, o.PaymentRef)
		if err != nil {
			continue
		}
		to, _ := e.rails.BankByRouting(o.Creditor.Routing)
		switch st.State {
		case TokenSettled:
			o.Tokenized = false
			e.settle(o, to.Name+" screened and accepted; settled on the network", e.ledgerDetail(o))
		case TokenRejected:
			e.refund(ctx, o)
			e.reject(o, st.Reason, "rejected by the receiving bank")
		case TokenExpired:
			e.refund(ctx, o)
			o.Reason = "AB05"
			e.event(o, "system", StatusExpired, ISORejected, "the receiving bank did not answer in time (AB05)", "")
		case TokenHeld:
			if e.now().After(st.Deadline) {
				if err := e.rails.ExpireTokenPayment(ctx, o.Bank, o.PaymentRef); err != nil {
					continue
				}
				e.refund(ctx, o)
				o.Reason = "AB05"
				e.event(o, "system", StatusExpired, ISORejected, "the receiving bank did not answer in time; hold released (AB05)", "")
			} else {
				continue
			}
		}
		if err := e.save(ctx, o, false); err != nil {
			return err
		}
	}
	return nil
}

// syncNetted applies netting outcomes to queued orders.
func (e *Engine) syncNetted(ctx context.Context) error {
	os, err := e.ordersIn(ctx, "", true, StatusQueuedForNetting)
	if err != nil {
		return err
	}
	for _, o := range os {
		before := len(o.History)
		e.syncNettedOne(ctx, o, "system")
		if len(o.History) == before {
			continue
		}
		if err := e.save(ctx, o, false); err != nil {
			return err
		}
	}
	return nil
}

// syncCredited confirms, for orders settled between the banks, that the
// receiving bank has credited the payee's deposit account (ACCC).
func (e *Engine) syncCredited(ctx context.Context) error {
	os, err := e.ordersIn(ctx, "", true, StatusSettled)
	if err != nil {
		return err
	}
	for _, o := range os {
		if o.ISO != ISOSettled {
			continue
		}
		to, ok := e.rails.BankByRouting(o.Creditor.Routing)
		if !ok {
			continue
		}
		switch {
		case o.PaymentRef != "" && e.rails.TokenPaymentCredited(to.MemberID, o.PaymentRef):
			e.credited(o, to.Name, to.Ticker+" redeemed into the payee's deposit account; the backing is "+to.Name+"'s free position again")
		case o.ObligationRef != "" && e.rails.ObligationCredited(to.MemberID, o.ObligationRef):
			e.credited(o, to.Name, "")
		default:
			continue
		}
		if err := e.save(ctx, o, false); err != nil {
			return err
		}
	}
	return nil
}

// scheduledNetting runs a cycle when the clock passes a boundary of the
// schedule and obligations are waiting.
func (e *Engine) scheduledNetting(ctx context.Context) (bool, error) {
	every := e.opts.NettingEvery
	if every <= 0 {
		return false, nil
	}
	now := e.now()
	due := !e.nextCycle.IsZero() && !now.Before(e.nextCycle)
	if e.nextCycle.IsZero() || due {
		e.nextCycle = now.Truncate(every).Add(every)
	}
	if !due || e.rails.QueuedObligations(ctx) == 0 {
		return false, nil
	}
	_, err := e.runCycle(ctx, "scheduled")
	return err == nil, err
}

func (e *Engine) runCycle(ctx context.Context, trigger string) (*NettingCycle, error) {
	r, err := e.rails.RunCycle(ctx)
	if err != nil {
		return nil, err
	}
	c := &NettingCycle{ID: newID(), CycleRef: r.CycleRef, At: e.now(), Trigger: trigger,
		Discharged: r.Discharged, Deferred: r.Deferred, Gross: r.Gross, Net: r.Net}
	return c, e.r.Network.CreateCycle(ctx, c)
}

// retryLiquidity executes a bank's orders waiting for liquidity, oldest
// first, and closes its liquidity alerts when none wait any more.
func (e *Engine) retryLiquidity(ctx context.Context, bank string) (int, error) {
	os, err := e.ordersIn(ctx, bank, true, StatusAwaitingLiquidity)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range os {
		e.executeUrgent(ctx, o)
		if o.Status != StatusAwaitingLiquidity {
			n++
			if err := e.save(ctx, o, false); err != nil {
				return n, err
			}
		}
	}
	if n == len(os) {
		open, err := e.r.Alerts.OpenOfKind(ctx, bank, "LIQUIDITY")
		if err != nil {
			return n, err
		}
		for _, a := range open {
			if err := e.r.Alerts.Close(ctx, a.ID, e.now()); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (e *Engine) raiseLiquidityAlert(ctx context.Context, o *Order, capacity int64) {
	from, _ := e.rails.Bank(o.Bank)
	a := &Alert{ID: newID(), Bank: o.Bank, Kind: "LIQUIDITY", Open: true, At: e.now(),
		Message: fmt.Sprintf("%s needs $%s of mint capacity for payment %s; it has $%s. Fund the omnibus (%s).",
			from.Name, Readable(o.Amount), o.ID, Readable(capacity), e.instrument())}
	if err := e.r.Alerts.Create(ctx, a); err != nil {
		e.log.Errorf("alert: %v", err)
	}
}

// watermarks opens a treasury alert when a bank's mint capacity falls below
// its low watermark and closes it once capacity is back at normal.
func (e *Engine) watermarks(ctx context.Context, bank string) error {
	w, ok := e.opts.Watermarks[bank]
	if !ok || w.Low <= 0 {
		return nil
	}
	m, err := e.rails.Member(ctx, bank)
	if err != nil {
		return nil
	}
	open, err := e.r.Alerts.OpenOfKind(ctx, bank, "LOW_WATERMARK")
	if err != nil {
		return err
	}
	switch {
	case len(open) == 0 && m.MintCapacity < w.Low:
		return e.r.Alerts.Create(ctx, &Alert{ID: newID(), Bank: bank, Kind: "LOW_WATERMARK", Open: true, At: e.now(),
			Message: fmt.Sprintf("%s can issue only $%s more, below its low watermark of $%s. Fund the omnibus (%s).",
				m.Name, Readable(m.MintCapacity), Readable(w.Low), e.instrument())})
	case len(open) > 0 && m.MintCapacity >= w.Normal:
		for _, a := range open {
			if err := e.r.Alerts.Close(ctx, a.ID, e.now()); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateDefunds records defunds the operator has completed or failed on Fedwire.
func (e *Engine) updateDefunds(ctx context.Context) error {
	ds, err := e.r.Defunds.ListByStatus(ctx, "APPROVED")
	if err != nil {
		return err
	}
	for _, d := range ds {
		st, err := e.rails.DefundState(ctx, d.LedgerRef)
		if err != nil || st == "APPROVED" {
			continue
		}
		d.Status, d.UpdatedAt = st, e.now()
		if err := e.r.Defunds.Update(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// NextCycle is when the next scheduled netting cycle runs, if scheduled.
func (e *Engine) NextCycle() time.Time { return e.nextCycle }
