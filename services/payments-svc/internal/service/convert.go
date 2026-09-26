// Package service implements the generated gRPC and HTTP service interfaces:
// it reads the caller and headers from the transport, converts between proto
// messages and biz's domain, and leaves every decision to biz.
package service

import (
	"context"
	"time"

	"github.com/go-kratos/kratos/v2/transport"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "omnibus-deposit-network/services/payments-svc/api/payments/v1"
	"omnibus-deposit-network/services/payments-svc/internal/auth"
	"omnibus-deposit-network/services/payments-svc/internal/biz"
)

func principal(ctx context.Context) (biz.Principal, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return biz.Principal{}, biz.ErrUnauthenticated("not authenticated")
	}
	return p, nil
}

// header reads a request header (HTTP) or metadata key (gRPC).
func header(ctx context.Context, name string) string {
	if tr, ok := transport.FromServerContext(ctx); ok {
		return tr.RequestHeader().Get(name)
	}
	return ""
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func tsp(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func money(cents int64) *pb.Money { return &pb.Money{Amount: biz.FormatCents(cents), Currency: "USD"} }

func amountOf(m *pb.Money) string {
	if m == nil {
		return ""
	}
	if m.Currency != "" && m.Currency != "USD" {
		return "invalid-currency"
	}
	return m.Amount
}

func party(p *pb.Party) biz.Party {
	if p == nil {
		return biz.Party{}
	}
	return biz.Party{Name: p.Name, Routing: p.RoutingNumber, Account: p.Account}
}

func toPayment(o *biz.Order) *pb.Payment {
	out := &pb.Payment{
		Id: o.ID, EndToEndId: o.EndToEndID, Uetr: o.UETR, IdempotencyKey: o.IdempotencyKey, OrganizationId: o.Org,
		DebtorAccount: o.DebtorAccount,
		Creditor:      &pb.Party{Name: o.Creditor.Name, RoutingNumber: o.Creditor.Routing, Account: o.Creditor.Account},
		Amount:        money(o.Amount), Priority: string(o.Priority), Remittance: o.Remittance,
		Status: string(o.Status), IsoStatus: o.ISO, ReasonCode: o.Reason, Route: o.Route, RouteNote: o.RouteNote,
		PayeeCheck:        &pb.PayeeCheck{AccountStatus: o.PayeeCheck.Account, NameMatch: o.PayeeCheck.Name, RegisteredName: o.PayeeCheck.Registered},
		ApprovalsRequired: int32(o.ApprovalsRequired), CreatedBy: o.CreatedBy, CreatedAt: ts(o.CreatedAt),
		SettledAt: tsp(o.SettledAt), OfacReportDue: tsp(o.OFACReportDue), FileId: o.FileID, SanctionsMatch: o.SanctionsMatch,
		Approvals: []string{},
	}
	for _, a := range o.Approvals {
		out.Approvals = append(out.Approvals, a.ApproverID)
	}
	for _, e := range o.History {
		out.History = append(out.History, &pb.PaymentEvent{At: ts(e.At), Actor: e.Actor, Status: string(e.Status),
			IsoStatus: e.ISO, Note: e.Note, Detail: e.Detail})
	}
	return out
}

func toPayments(os []*biz.Order) []*pb.Payment {
	out := make([]*pb.Payment, 0, len(os))
	for _, o := range os {
		out = append(out, toPayment(o))
	}
	return out
}
