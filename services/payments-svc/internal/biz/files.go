package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"strings"

	kerrors "github.com/go-kratos/kratos/v2/errors"

	"omnibus-deposit-network/services/payments-svc/internal/iso"
)

// FileResult is what a submitted pain.001 produced.
type FileResult struct {
	File     *PaymentFile
	Payments []*Order
	Report   []byte // pain.002.001.11
}

// SubmitFile accepts a pain.001.001.10 from a maker. Each transaction is
// validated like an API payment and becomes an order awaiting approval;
// transactions that fail are reported in the pain.002 with a reason code.
// A message id the organization sent before is refused (DU01); an
// idempotency key replays the earlier answer.
func (uc *PaymentUseCase) SubmitFile(ctx context.Context, p Principal, xml []byte, key string) (*FileResult, bool, error) {
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if p.Role != RoleMaker {
		return nil, false, ErrForbidden("only a maker submits payment files")
	}
	if key == "" || len(key) > 64 {
		return nil, false, ErrIdempotencyMissing()
	}
	org, ok := e.orgs[p.Org]
	if !ok {
		return nil, false, ErrForbidden("no organization")
	}
	sum := sha256.Sum256(xml)
	hash := hex.EncodeToString(sum[:])
	if prev, err := e.r.Files.GetByIdempotencyKey(ctx, org.ID, key); err == nil {
		if prev.RequestHash != hash {
			return nil, false, ErrIdempotencyReused(key)
		}
		res, err := e.fileResult(ctx, p, prev)
		return res, true, err
	} else if !stderrors.Is(err, ErrNoRows) {
		return nil, false, err
	}
	doc, err := iso.ParsePain001(xml)
	if err != nil {
		return nil, false, ErrInvalid("pain.001: %v", err)
	}
	if prev, err := e.r.Files.GetByMessageID(ctx, org.ID, doc.MessageID); err == nil {
		return nil, false, kerrors.Conflict("DU01", fmt.Sprintf("DU01 duplicate message: MsgId %q was file %s", doc.MessageID, prev.ID))
	} else if !stderrors.Is(err, ErrNoRows) {
		return nil, false, err
	}
	if doc.Transactions != doc.Count() {
		return nil, false, ErrInvalid("pain.001: NbOfTxs says %d, the file carries %d", doc.Transactions, doc.Count())
	}
	if doc.ControlSum != 0 && doc.ControlSum != doc.Sum() {
		return nil, false, ErrInvalid("pain.001: CtrlSum says %s, the transactions add up to %s", FormatCents(doc.ControlSum), FormatCents(doc.Sum()))
	}
	f := &PaymentFile{ID: newID(), Org: org.ID, MessageID: doc.MessageID, RequestHash: hash, IdempotencyKey: key,
		Transactions: doc.Count(), ControlSum: doc.Sum(), CreatedBy: p.ID, CreatedAt: e.now()}
	var orders []*Order
	for _, ins := range doc.Instructions {
		for _, tx := range ins.Transactions {
			req := PaymentRequest{DebtorAccount: ins.DebtorAccount, Amount: FormatCents(tx.Amount), Remittance: tx.Remittance,
				EndToEndID: tx.EndToEndID, Priority: Normal,
				Creditor: Party{Name: tx.CreditorName, Routing: tx.CreditorRouting, Account: tx.CreditorAccount}}
			if tx.ServiceLevel == "URGP" {
				req.Priority = Urgent
			}
			if tx.Currency != "USD" {
				f.Rejections = append(f.Rejections, FileRejection{EndToEndID: tx.EndToEndID, Reason: "AM03", Message: "currency " + tx.Currency + " is not supported"})
				continue
			}
			o, err := e.newOrder(ctx, p, org, req)
			if err != nil {
				f.Rejections = append(f.Rejections, FileRejection{EndToEndID: tx.EndToEndID, Reason: reasonCode(err), Message: kerrors.FromError(err).Message})
				continue
			}
			o.FileID = f.ID
			orders = append(orders, o)
		}
	}
	if err := e.r.Files.Create(ctx, f); err != nil {
		return nil, false, err
	}
	for _, o := range orders {
		if err := e.save(ctx, o, true); err != nil {
			return nil, false, err
		}
	}
	res, err := e.fileResult(ctx, p, f)
	return res, false, err
}

// reasonCode maps a validation error to an ISO external status reason code.
func reasonCode(err error) string {
	ke := kerrors.FromError(err)
	msg := ke.Message
	switch {
	case ke.Reason == ReasonDuplicate:
		return "DU05"
	case ke.Reason == ReasonLimit:
		return "AM02"
	case strings.Contains(msg, "routing number"):
		return "RC01"
	case strings.Contains(msg, "does not belong"):
		return "AC01"
	case strings.Contains(msg, "amount"):
		return "AM12"
	}
	return "NARR"
}

func (e *Engine) fileResult(ctx context.Context, p Principal, f *PaymentFile) (*FileResult, error) {
	orders, err := e.r.Orders.ListByFile(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	report, err := e.fileReport(f, orders)
	if err != nil {
		return nil, err
	}
	return &FileResult{File: f, Payments: viewsFor(p, orders), Report: report}, nil
}

// fileReport renders a file's current statuses as a pain.002.
func (e *Engine) fileReport(f *PaymentFile, orders []*Order) ([]byte, error) {
	r := iso.StatusReport{MessageID: "STS-" + newID()[:23], At: e.now(), OrigMessageID: f.MessageID, OrigMessageType: iso.Pain001Type,
		OrigCount: f.Transactions, OrigSum: f.ControlSum, PaymentInfoID: f.MessageID}
	for _, o := range orders {
		r.Transactions = append(r.Transactions, txStatus(o))
	}
	for _, rj := range f.Rejections {
		r.Transactions = append(r.Transactions, iso.TxStatus{EndToEndID: rj.EndToEndID, Status: ISORejected, Reason: rj.Reason, Info: rj.Message})
	}
	switch {
	case len(orders) == 0:
		r.GroupStatus = ISORejected
	case len(f.Rejections) > 0:
		r.GroupStatus = "PART"
	default:
		r.GroupStatus = ISOAcceptedTechnical
	}
	return iso.Pain002(r)
}

func txStatus(o *Order) iso.TxStatus {
	t := iso.TxStatus{EndToEndID: o.EndToEndID, UETR: o.UETR, Status: o.ISO}
	if o.Status == StatusRejected || o.Status == StatusExpired || o.Status == StatusBlocked {
		t.Reason = o.Reason
	}
	return t
}

// FileStatusReport renders a file's current pain.002.
func (uc *PaymentUseCase) FileStatusReport(ctx context.Context, p Principal, id string) ([]byte, error) {
	e := uc.e
	f, err := e.r.Files.Get(ctx, id)
	if stderrors.Is(err, ErrNoRows) || (err == nil && f.Org != p.Org) {
		return nil, ErrNotFound("no payment file %s", id)
	}
	if err != nil {
		return nil, err
	}
	orders, err := e.r.Orders.ListByFile(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	return e.fileReport(f, orders)
}

// StatusReport renders one payment's status as a pain.002. A payment that
// came in a file is reported against that file's message; one created
// through the API against its idempotency key, with message name "API".
func (uc *PaymentUseCase) StatusReport(ctx context.Context, p Principal, id string) ([]byte, error) {
	e := uc.e
	o, err := e.load(ctx, p, id)
	if err != nil {
		return nil, err
	}
	r := iso.StatusReport{MessageID: "STS-" + newID()[:23], At: e.now(), OrigMessageID: o.IdempotencyKey, OrigMessageType: "API",
		OrigCount: 1, PaymentInfoID: o.ID[:35], Transactions: []iso.TxStatus{txStatus(o)}}
	if o.FileID != "" {
		if f, err := e.r.Files.Get(ctx, o.FileID); err == nil {
			r.OrigMessageID, r.OrigMessageType, r.PaymentInfoID = f.MessageID, iso.Pain001Type, f.MessageID
		}
	}
	if r.OrigMessageID == "" {
		r.OrigMessageID = o.EndToEndID
	}
	return iso.Pain002(r)
}
