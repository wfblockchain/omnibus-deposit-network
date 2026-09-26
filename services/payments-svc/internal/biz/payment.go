package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moov-io/ach"
)

// PaymentRequest is a payment order as a maker submits it.
type PaymentRequest struct {
	DebtorAccount string
	Creditor      Party
	Amount        string
	Priority      Priority
	Remittance    string
	EndToEndID    string
}

func (r PaymentRequest) hash() string {
	h := sha256.Sum256([]byte(strings.Join([]string{r.DebtorAccount, r.Creditor.Name, r.Creditor.Routing, r.Creditor.Account,
		r.Amount, string(r.Priority), r.Remittance, r.EndToEndID}, "\x1f")))
	return hex.EncodeToString(h[:])
}

// PaymentUseCase is the corporate client's payment channel.
type PaymentUseCase struct{ e *Engine }

// NewPaymentUseCase creates the use case.
func NewPaymentUseCase(e *Engine) *PaymentUseCase { return &PaymentUseCase{e: e} }

// CheckRouting validates an ABA routing number's check digit with moov-io/ach.
func CheckRouting(r string) error {
	if err := ach.CheckRoutingNumber(r); err != nil {
		return ErrInvalid("routing number %q: %v", r, err)
	}
	return nil
}

// Create records a payment order from a maker. With an idempotency key the
// organization used before, the same request returns the earlier order
// (duplicate = true) and a different one is refused.
func (uc *PaymentUseCase) Create(ctx context.Context, p Principal, req PaymentRequest, key string) (*Order, bool, error) {
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if p.Role != RoleMaker {
		return nil, false, ErrForbidden("only a maker creates payment orders")
	}
	if key == "" || len(key) > 64 {
		return nil, false, ErrIdempotencyMissing()
	}
	org, ok := e.orgs[p.Org]
	if !ok {
		return nil, false, ErrForbidden("no organization")
	}
	if req.DebtorAccount == "" {
		req.DebtorAccount = org.Account
	}
	if req.Priority == "" {
		req.Priority = Normal
	}
	if prev, err := e.r.Orders.GetByIdempotencyKey(ctx, org.ID, key); err == nil {
		if prev.RequestHash != req.hash() {
			return nil, false, ErrIdempotencyReused(key)
		}
		return ViewFor(p, prev), true, nil
	} else if !stderrors.Is(err, ErrNoRows) {
		return nil, false, err
	}
	o, err := e.newOrder(ctx, p, org, req)
	if err != nil {
		return nil, false, err
	}
	o.IdempotencyKey = key
	if err := e.save(ctx, o, true); err != nil {
		return nil, false, err
	}
	return ViewFor(p, o), false, nil
}

// newOrder validates a request against the organization's entitlements and
// builds the order with its first audit events. It does not store it.
func (e *Engine) newOrder(ctx context.Context, p Principal, org Org, req PaymentRequest) (*Order, error) {
	if req.DebtorAccount != org.Account {
		return nil, ErrForbidden("account %s does not belong to %s", req.DebtorAccount, org.Name)
	}
	amount, err := ParseAmount(req.Amount)
	if err != nil {
		return nil, err
	}
	if req.Priority != Urgent && req.Priority != Normal {
		return nil, ErrInvalid("priority must be URGENT or NORMAL")
	}
	c := req.Creditor
	if strings.TrimSpace(c.Name) == "" || c.Routing == "" || strings.TrimSpace(c.Account) == "" {
		return nil, ErrInvalid("creditor name, routing number and account are required")
	}
	if err := CheckRouting(c.Routing); err != nil {
		return nil, err
	}
	if _, member := e.rails.BankByRouting(c.Routing); !member {
		return nil, ErrInvalid("routing number %s is not a network member", c.Routing)
	}
	if len(req.EndToEndID) > 35 {
		return nil, ErrInvalid("endToEndId is at most 35 characters")
	}
	if req.EndToEndID != "" {
		if prev, err := e.r.Orders.GetByEndToEndID(ctx, org.ID, req.EndToEndID); err == nil {
			return nil, ErrDuplicate(req.EndToEndID, prev.ID)
		} else if !stderrors.Is(err, ErrNoRows) {
			return nil, err
		}
	}
	if org.PerPaymentLimit > 0 && amount > org.PerPaymentLimit {
		return nil, ErrLimit("$%s exceeds the per-payment limit of $%s", Readable(amount), Readable(org.PerPaymentLimit))
	}
	if org.DailyLimit > 0 {
		now := e.now()
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		used, err := e.r.Orders.SumSince(ctx, org.ID, day)
		if err != nil {
			return nil, err
		}
		if used+amount > org.DailyLimit {
			return nil, ErrLimit("$%s would exceed today's limit of $%s ($%s already used)", Readable(amount), Readable(org.DailyLimit), Readable(used))
		}
	}
	now := e.now()
	o := &Order{
		ID: newID(), Org: org.ID, Bank: org.Bank, RequestHash: req.hash(), EndToEndID: req.EndToEndID,
		UETR: uuid.NewString(), DebtorAccount: req.DebtorAccount, Creditor: c, Amount: amount,
		Priority: req.Priority, Remittance: req.Remittance, CreatedBy: p.ID, CreatedAt: now, UpdatedAt: now,
	}
	if o.EndToEndID == "" {
		o.EndToEndID = strings.ReplaceAll(o.ID, "-", "")
	}
	o.PayeeCheck = e.verifyPayee(c)
	o.ApprovalsRequired = e.needed(o, org)
	e.event(o, p.Name, StatusAwaitingApproval, ISOReceived, "received from "+p.Name, "")
	check := fmt.Sprintf("payee check: account %s, name %s", o.PayeeCheck.Account, o.PayeeCheck.Name)
	if o.PayeeCheck.Registered != "" {
		check += " (registered as " + o.PayeeCheck.Registered + ")"
	}
	if !o.PayeeCheck.Passed() {
		check += "; one more approval is needed to override"
	}
	e.event(o, "system", StatusAwaitingApproval, ISOAcceptedTechnical,
		fmt.Sprintf("validated against limits; %s; needs %d approval%s from someone other than the maker", check, o.ApprovalsRequired, plural(o.ApprovalsRequired)), "")
	return o, nil
}

// needed is how many approvals an order takes: one, two above the
// organization's threshold, and one more to override a failed payee check.
func (e *Engine) needed(o *Order, org Org) int {
	n := 1
	if org.SecondApprovalAbove > 0 && o.Amount > org.SecondApprovalAbove {
		n = 2
	}
	if !o.PayeeCheck.Passed() {
		n++
	}
	return n
}

// Approve records an approver's sign-off. The maker never approves its own
// order and an approver counts once; with enough approvals the order goes to
// screening and execution.
func (uc *PaymentUseCase) Approve(ctx context.Context, p Principal, id string) (*Order, error) {
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	o, err := e.clientOrder(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if p.Role != RoleApprover {
		return nil, ErrForbidden("only an approver approves")
	}
	if o.Status != StatusAwaitingApproval {
		return nil, ErrConflict("payment is %s", o.Status)
	}
	if p.ID == o.CreatedBy {
		return nil, ErrForbidden("the maker of a payment cannot approve it")
	}
	for _, a := range o.Approvals {
		if a.ApproverID == p.ID {
			return nil, ErrConflict("%s already approved this payment", p.Name)
		}
	}
	o.Approvals = append(o.Approvals, Approval{ApproverID: p.ID, ApproverName: p.Name, At: e.now()})
	if len(o.Approvals) < o.ApprovalsRequired {
		e.event(o, p.Name, StatusAwaitingApproval, ISOPartlyApproved,
			fmt.Sprintf("approval %d of %d by %s", len(o.Approvals), o.ApprovalsRequired, p.Name), "")
	} else {
		e.event(o, p.Name, StatusInProcess, ISOAcceptedCustomer, "approved by "+p.Name, "")
		e.advance(ctx, o)
	}
	if err := e.save(ctx, o, false); err != nil {
		return nil, err
	}
	return ViewFor(p, o), nil
}

// Decline is an approver refusing an order.
func (uc *PaymentUseCase) Decline(ctx context.Context, p Principal, id, reason string) (*Order, error) {
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	o, err := e.clientOrder(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if p.Role != RoleApprover {
		return nil, ErrForbidden("only an approver declines")
	}
	if o.Status != StatusAwaitingApproval {
		return nil, ErrConflict("payment is %s", o.Status)
	}
	e.event(o, p.Name, StatusCancelled, ISOCancelled, "declined by "+p.Name+": "+reason, "")
	if err := e.save(ctx, o, false); err != nil {
		return nil, err
	}
	return ViewFor(p, o), nil
}

// Cancel withdraws an order not yet approved (its maker), or pulls a NORMAL
// order from the netting queue before its cycle takes it (maker or
// approver): the bank cancels the obligation and re-credits the account.
func (uc *PaymentUseCase) Cancel(ctx context.Context, p Principal, id string) (*Order, error) {
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	o, err := e.clientOrder(ctx, p, id)
	if err != nil {
		return nil, err
	}
	switch {
	case o.Status == StatusAwaitingApproval && p.ID == o.CreatedBy:
		e.event(o, p.Name, StatusCancelled, ISOCancelled, "cancelled by the maker", "")
	case o.Status == StatusAwaitingApproval:
		return nil, ErrForbidden("before approval only the maker cancels; an approver declines")
	case o.Status == StatusQueuedForNetting && (p.Role == RoleMaker || p.Role == RoleApprover):
		if err := e.rails.CancelObligation(ctx, o.Bank, o.ObligationRef); err != nil {
			return nil, ErrConflict("the netting cycle has already taken it")
		}
		e.syncNettedOne(ctx, o, p.Name)
	default:
		return nil, ErrConflict("payment is %s and can no longer be cancelled", o.Status)
	}
	if err := e.save(ctx, o, false); err != nil {
		return nil, err
	}
	return ViewFor(p, o), nil
}

// Get returns one order the principal may see.
func (uc *PaymentUseCase) Get(ctx context.Context, p Principal, id string) (*Order, error) {
	o, err := uc.e.load(ctx, p, id)
	if err != nil {
		return nil, err
	}
	return ViewFor(p, o), nil
}

// List lists what the principal may see, newest first.
func (uc *PaymentUseCase) List(ctx context.Context, p Principal, status string, pageSize int, pageToken string) ([]*Order, string, error) {
	f := OrderFilter{PageSize: pageSize, PageToken: pageToken}
	switch {
	case p.Org != "":
		f.Org = p.Org
	case p.IsBankStaff():
		f.Bank = p.Bank
	case p.Role == RoleOperatorOps:
	default:
		return nil, "", ErrForbidden("no access to payments")
	}
	if status != "" {
		f.Statuses = []Status{Status(status)}
	}
	os, next, err := uc.e.r.Orders.List(ctx, f)
	if err != nil {
		return nil, "", err
	}
	return viewsFor(p, os), next, nil
}

// clientOrder is an order of the principal's organization.
func (e *Engine) clientOrder(ctx context.Context, p Principal, id string) (*Order, error) {
	if p.Org == "" {
		return nil, ErrForbidden("only a client organization acts on its payments")
	}
	return e.load(ctx, p, id)
}

// Participants lists the network's banks.
func (uc *PaymentUseCase) Participants(ctx context.Context) ([]Participant, error) {
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Participant
	for _, b := range e.rails.Banks() {
		m, err := e.rails.Member(ctx, b.MemberID)
		if err != nil {
			return nil, err
		}
		st := "LIVE"
		if m.Suspended || !m.Admitted {
			st = "SUSPENDED"
		}
		out = append(out, Participant{Name: b.Name, Routing: b.Routing, MemberID: b.MemberID, Status: st, RequiresAcceptance: b.RequiresAcceptance})
	}
	return out, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
