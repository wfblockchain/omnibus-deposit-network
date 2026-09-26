package service

import (
	"context"

	pb "omnibus-deposit-network/services/payments-svc/api/payments/v1"
	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/buildinfo"
	"omnibus-deposit-network/services/payments-svc/internal/iso"
)

// ─── AccountService ───

// AccountService implements api.payments.v1.AccountService.
type AccountService struct {
	pb.UnimplementedAccountServiceServer
	uc *biz.AccountUseCase
}

// NewAccountService creates the service.
func NewAccountService(uc *biz.AccountUseCase) *AccountService { return &AccountService{uc: uc} }

func (s *AccountService) WhoAmI(ctx context.Context, _ *pb.WhoAmIRequest) (*pb.Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.Principal{Id: p.ID, Name: p.Name, Role: string(p.Role), BankId: p.Bank, OrganizationId: p.Org}, nil
}

func (s *AccountService) GetAccount(ctx context.Context, _ *pb.GetAccountRequest) (*pb.Account, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.uc.Get(ctx, p)
	if err != nil {
		return nil, err
	}
	return &pb.Account{OrganizationId: a.Org.ID, OrganizationName: a.Org.Name, Account: a.Org.Account, BankId: a.Org.Bank,
		BankName: a.BankName, RoutingNumber: a.Routing, Balance: money(a.Balance), InFlight: money(a.InFlight), AsOf: ts(a.AsOf)}, nil
}

func (s *AccountService) ListStatementEntries(ctx context.Context, _ *pb.ListStatementEntriesRequest) (*pb.ListStatementEntriesResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := s.uc.Postings(ctx, p)
	if err != nil {
		return nil, err
	}
	out := &pb.ListStatementEntriesResponse{Entries: []*pb.StatementEntry{}}
	for _, x := range ps {
		amt := x.Delta
		if amt < 0 {
			amt = -amt
		}
		out.Entries = append(out.Entries, &pb.StatementEntry{BookingDate: ts(x.At), Reference: x.Ref, Credit: x.Delta > 0,
			Amount: money(amt), Balance: money(x.Balance)})
	}
	return out, nil
}

func (s *AccountService) GetStatement(ctx context.Context, _ *pb.GetAccountRequest) (*pb.Document, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	b, err := s.uc.Statement(ctx, p)
	if err != nil {
		return nil, err
	}
	return &pb.Document{MessageType: iso.Camt053Type, Xml: string(b)}, nil
}

// ─── WebhookService ───

// WebhookService implements api.payments.v1.WebhookService.
type WebhookService struct {
	pb.UnimplementedWebhookServiceServer
	uc *biz.WebhookUseCase
}

// NewWebhookService creates the service.
func NewWebhookService(uc *biz.WebhookUseCase) *WebhookService { return &WebhookService{uc: uc} }

func (s *WebhookService) CreateWebhook(ctx context.Context, req *pb.CreateWebhookRequest) (*pb.Webhook, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	sub, secret, err := s.uc.Create(ctx, p, req.Url)
	if err != nil {
		return nil, err
	}
	return &pb.Webhook{Id: sub.ID, Url: sub.URL, Secret: secret, CreatedAt: ts(sub.CreatedAt)}, nil
}

func (s *WebhookService) ListWebhooks(ctx context.Context, _ *pb.ListWebhooksRequest) (*pb.ListWebhooksResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	subs, err := s.uc.List(ctx, p)
	if err != nil {
		return nil, err
	}
	out := &pb.ListWebhooksResponse{Webhooks: []*pb.Webhook{}}
	for _, x := range subs {
		out.Webhooks = append(out.Webhooks, &pb.Webhook{Id: x.ID, Url: x.URL, CreatedAt: ts(x.CreatedAt)})
	}
	return out, nil
}

func (s *WebhookService) DeleteWebhook(ctx context.Context, req *pb.DeleteWebhookRequest) (*pb.DeleteWebhookResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.DeleteWebhookResponse{}, s.uc.Delete(ctx, p, req.Id)
}

func (s *WebhookService) ListWebhookDeliveries(ctx context.Context, req *pb.ListWebhookDeliveriesRequest) (*pb.ListWebhookDeliveriesResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ds, err := s.uc.Deliveries(ctx, p, req.Id)
	if err != nil {
		return nil, err
	}
	out := &pb.ListWebhookDeliveriesResponse{Deliveries: []*pb.WebhookDelivery{}}
	for _, d := range ds {
		w := &pb.WebhookDelivery{Id: d.ID, EventType: d.EventType, Status: d.Status, Attempts: int32(d.Attempts),
			LastResponseCode: int32(d.LastCode), LastError: d.LastError, CreatedAt: ts(d.CreatedAt)}
		if d.Status == "PENDING" {
			w.NextAttemptAt = ts(d.NextAttempt)
		}
		out.Deliveries = append(out.Deliveries, w)
	}
	return out, nil
}

// ─── BankOperationsService ───

// BankOperationsService implements api.payments.v1.BankOperationsService.
type BankOperationsService struct {
	pb.UnimplementedBankOperationsServiceServer
	payments   *biz.PaymentUseCase
	compliance *biz.ComplianceUseCase
	treasury   *biz.TreasuryUseCase
}

// NewBankOperationsService creates the service.
func NewBankOperationsService(p *biz.PaymentUseCase, c *biz.ComplianceUseCase, t *biz.TreasuryUseCase) *BankOperationsService {
	return &BankOperationsService{payments: p, compliance: c, treasury: t}
}

func staff(ctx context.Context) (biz.Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, err
	}
	if !p.IsBankStaff() {
		return p, biz.ErrForbidden("the operations API is for the bank's staff")
	}
	return p, nil
}

func (s *BankOperationsService) ListBankPayments(ctx context.Context, req *pb.ListBankPaymentsRequest) (*pb.ListBankPaymentsResponse, error) {
	p, err := staff(ctx)
	if err != nil {
		return nil, err
	}
	os, next, err := s.payments.List(ctx, p, req.Status, int(req.PageSize), req.PageToken)
	if err != nil {
		return nil, err
	}
	return &pb.ListBankPaymentsResponse{Payments: toPayments(os), NextPageToken: next}, nil
}

func (s *BankOperationsService) GetBankPayment(ctx context.Context, req *pb.GetBankPaymentRequest) (*pb.Payment, error) {
	p, err := staff(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.payments.Get(ctx, p, req.Id)
	if err != nil {
		return nil, err
	}
	return toPayment(o), nil
}

func (s *BankOperationsService) ListHolds(ctx context.Context, _ *pb.ListHoldsRequest) (*pb.ListHoldsResponse, error) {
	p, err := staff(ctx)
	if err != nil {
		return nil, err
	}
	holds, cases, err := s.compliance.Holds(ctx, p)
	if err != nil {
		return nil, err
	}
	return &pb.ListHoldsResponse{Holds: toPayments(holds), Cases: toPayments(cases)}, nil
}

func (s *BankOperationsService) decide(ctx context.Context, req *pb.HoldDecisionRequest,
	fn func(context.Context, biz.Principal, string, string) (*biz.Order, error)) (*pb.Payment, error) {
	p, err := staff(ctx)
	if err != nil {
		return nil, err
	}
	o, err := fn(ctx, p, req.Id, req.Note)
	if err != nil {
		return nil, err
	}
	return toPayment(o), nil
}

func (s *BankOperationsService) ReleaseHold(ctx context.Context, req *pb.HoldDecisionRequest) (*pb.Payment, error) {
	return s.decide(ctx, req, s.compliance.Release)
}

func (s *BankOperationsService) BlockHold(ctx context.Context, req *pb.HoldDecisionRequest) (*pb.Payment, error) {
	return s.decide(ctx, req, s.compliance.Block)
}

func (s *BankOperationsService) RejectHold(ctx context.Context, req *pb.HoldDecisionRequest) (*pb.Payment, error) {
	return s.decide(ctx, req, s.compliance.Reject)
}

func toDefund(d *biz.Defund) *pb.Defund {
	return &pb.Defund{Id: d.ID, Amount: money(d.Amount), RequestedBy: d.RequestedBy, ApprovedBy: d.ApprovedBy,
		Status: d.Status, CreatedAt: ts(d.CreatedAt)}
}

func (s *BankOperationsService) GetLiquidity(ctx context.Context, _ *pb.GetLiquidityRequest) (*pb.Liquidity, error) {
	p, err := staff(ctx)
	if err != nil {
		return nil, err
	}
	v, err := s.treasury.Liquidity(ctx, p)
	if err != nil {
		return nil, err
	}
	m := v.Member
	out := &pb.Liquidity{BankId: m.ID, BankName: m.Name, Ticker: m.Ticker, Position: money(m.Position),
		Backing: money(m.Backing), PendingDefund: money(m.PendingDefund), Free: money(m.Free), MintCapacity: money(m.MintCapacity),
		LowWatermark: money(v.LowWatermark), NormalWatermark: money(v.NormalWatermark), FedwireOpen: v.FedwireOpen,
		NextFedwireOpen: ts(v.NextFedwireOpen), FundingInstrument: v.Instrument,
		AwaitingLiquidity: toPayments(v.AwaitingLiquidity), Alerts: []*pb.Alert{}, Defunds: []*pb.Defund{}}
	for _, a := range v.Alerts {
		out.Alerts = append(out.Alerts, &pb.Alert{Id: a.ID, Kind: a.Kind, Message: a.Message, Open: a.Open, At: ts(a.At)})
	}
	for _, d := range v.Defunds {
		out.Defunds = append(out.Defunds, toDefund(d))
	}
	return out, nil
}

func (s *BankOperationsService) FundOmnibus(ctx context.Context, req *pb.FundOmnibusRequest) (*pb.FundOmnibusResponse, error) {
	p, err := staff(ctx)
	if err != nil {
		return nil, err
	}
	r, err := s.treasury.Fund(ctx, p, amountOf(req.Amount))
	if err != nil {
		return nil, err
	}
	return &pb.FundOmnibusResponse{Instrument: r.Instrument, Status: r.Status, Reason: r.Reason, PaymentsReleased: int32(r.Released)}, nil
}

func (s *BankOperationsService) RequestDefund(ctx context.Context, req *pb.RequestDefundRequest) (*pb.Defund, error) {
	p, err := staff(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.treasury.RequestDefund(ctx, p, amountOf(req.Amount))
	if err != nil {
		return nil, err
	}
	return toDefund(d), nil
}

func (s *BankOperationsService) ApproveDefund(ctx context.Context, req *pb.ApproveDefundRequest) (*pb.Defund, error) {
	p, err := staff(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.treasury.ApproveDefund(ctx, p, req.Id)
	if err != nil {
		return nil, err
	}
	return toDefund(d), nil
}

// ─── NetworkOperationsService ───

// NetworkOperationsService implements api.payments.v1.NetworkOperationsService.
type NetworkOperationsService struct {
	pb.UnimplementedNetworkOperationsServiceServer
	uc *biz.NetworkUseCase
}

// NewNetworkOperationsService creates the service.
func NewNetworkOperationsService(uc *biz.NetworkUseCase) *NetworkOperationsService {
	return &NetworkOperationsService{uc: uc}
}

func toCycle(c *biz.NettingCycle) *pb.NettingCycle {
	return &pb.NettingCycle{Id: c.ID, CycleRef: c.CycleRef, At: ts(c.At), Trigger: c.Trigger,
		Discharged: int32(c.Discharged), Deferred: int32(c.Deferred), Gross: money(c.Gross), Net: money(c.Net)}
}

func toRecon(r *biz.Reconciliation) *pb.Reconciliation {
	return &pb.Reconciliation{Id: r.ID, At: ts(r.At), FedBalance: money(r.FedBalance), LedgerTotal: money(r.LedgerTotal),
		ReconciliationBreak: r.Break, InvariantsHold: r.Invariants}
}

func (s *NetworkOperationsService) GetNetwork(ctx context.Context, _ *pb.GetNetworkRequest) (*pb.Network, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	v, err := s.uc.View(ctx, p)
	if err != nil {
		return nil, err
	}
	out := &pb.Network{Now: ts(v.Now), FedwireOpen: v.FedwireOpen, NextFedwireOpen: ts(v.NextFedwireOpen),
		FedJointAccount: money(v.FedJoint), LedgerTotal: money(v.LedgerTotal), ReconciliationBreak: v.Break,
		InvariantsHold: v.Invariants, ObligationsQueued: int32(v.Queued), NextScheduledCycle: ts(v.NextCycle),
		Members: []*pb.Member{}, Cycles: []*pb.NettingCycle{}, Reconciliations: []*pb.Reconciliation{}}
	for _, m := range v.Members {
		out.Members = append(out.Members, &pb.Member{Id: m.ID, Name: m.Name, Ticker: m.Ticker, Position: money(m.Position),
			Backing: money(m.Backing), Free: money(m.Free)})
	}
	for _, c := range v.Cycles {
		out.Cycles = append(out.Cycles, toCycle(c))
	}
	for _, r := range v.Recons {
		out.Reconciliations = append(out.Reconciliations, toRecon(r))
	}
	return out, nil
}

func (s *NetworkOperationsService) RunNettingCycle(ctx context.Context, _ *pb.RunNettingCycleRequest) (*pb.NettingCycle, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	c, err := s.uc.RunCycle(ctx, p)
	if err != nil {
		return nil, err
	}
	return toCycle(c), nil
}

func (s *NetworkOperationsService) Reconcile(ctx context.Context, _ *pb.ReconcileRequest) (*pb.Reconciliation, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	r, err := s.uc.Reconcile(ctx, p)
	if err != nil {
		return nil, err
	}
	return toRecon(r), nil
}

// SetClock exists in demo builds only; the scenario clock is a simulation
// control, not a production operation.
func (s *NetworkOperationsService) SetClock(ctx context.Context, req *pb.SetClockRequest) (*pb.SetClockResponse, error) {
	if !buildinfo.Demo {
		return nil, biz.ErrForbidden("the scenario clock exists in demo builds only")
	}
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if req.Time == nil {
		return nil, biz.ErrInvalid("time is required")
	}
	now, err := s.uc.SetClock(ctx, p, req.Time.AsTime())
	if err != nil {
		return nil, err
	}
	return &pb.SetClockResponse{Now: ts(now)}, nil
}
