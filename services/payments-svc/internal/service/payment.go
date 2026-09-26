package service

import (
	"context"

	pb "omnibus-deposit-network/services/payments-svc/api/payments/v1"
	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/iso"
)

// PaymentService implements api.payments.v1.PaymentService.
type PaymentService struct {
	pb.UnimplementedPaymentServiceServer
	uc *biz.PaymentUseCase
}

// NewPaymentService creates the service.
func NewPaymentService(uc *biz.PaymentUseCase) *PaymentService { return &PaymentService{uc: uc} }

func (s *PaymentService) CreatePayment(ctx context.Context, req *pb.CreatePaymentRequest) (*pb.CreatePaymentResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	o, dup, err := s.uc.Create(ctx, p, biz.PaymentRequest{
		DebtorAccount: req.DebtorAccount, Creditor: party(req.Creditor), Amount: amountOf(req.Amount),
		Priority: biz.Priority(req.Priority), Remittance: req.Remittance, EndToEndID: req.EndToEndId,
	}, header(ctx, "Idempotency-Key"))
	if err != nil {
		return nil, err
	}
	return &pb.CreatePaymentResponse{Payment: toPayment(o), DuplicateRequest: dup}, nil
}

func (s *PaymentService) GetPayment(ctx context.Context, req *pb.GetPaymentRequest) (*pb.Payment, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if p.Org == "" {
		return nil, biz.ErrForbidden("bank and operator staff use the operations API")
	}
	o, err := s.uc.Get(ctx, p, req.Id)
	if err != nil {
		return nil, err
	}
	return toPayment(o), nil
}

func (s *PaymentService) ListPayments(ctx context.Context, req *pb.ListPaymentsRequest) (*pb.ListPaymentsResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if p.Org == "" {
		return nil, biz.ErrForbidden("bank and operator staff use the operations API")
	}
	os, next, err := s.uc.List(ctx, p, req.Status, int(req.PageSize), req.PageToken)
	if err != nil {
		return nil, err
	}
	return &pb.ListPaymentsResponse{Payments: toPayments(os), NextPageToken: next}, nil
}

func (s *PaymentService) ApprovePayment(ctx context.Context, req *pb.ApprovePaymentRequest) (*pb.Payment, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.uc.Approve(ctx, p, req.Id)
	if err != nil {
		return nil, err
	}
	return toPayment(o), nil
}

func (s *PaymentService) DeclinePayment(ctx context.Context, req *pb.DeclinePaymentRequest) (*pb.Payment, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.uc.Decline(ctx, p, req.Id, req.Reason)
	if err != nil {
		return nil, err
	}
	return toPayment(o), nil
}

func (s *PaymentService) CancelPayment(ctx context.Context, req *pb.CancelPaymentRequest) (*pb.Payment, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.uc.Cancel(ctx, p, req.Id)
	if err != nil {
		return nil, err
	}
	return toPayment(o), nil
}

func (s *PaymentService) GetPaymentStatusReport(ctx context.Context, req *pb.GetPaymentRequest) (*pb.Document, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	b, err := s.uc.StatusReport(ctx, p, req.Id)
	if err != nil {
		return nil, err
	}
	return &pb.Document{MessageType: iso.Pain002Type, Xml: string(b)}, nil
}

func (s *PaymentService) SubmitPaymentFile(ctx context.Context, req *pb.SubmitPaymentFileRequest) (*pb.PaymentFile, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	res, dup, err := s.uc.SubmitFile(ctx, p, []byte(req.Xml), header(ctx, "Idempotency-Key"))
	if err != nil {
		return nil, err
	}
	out := &pb.PaymentFile{Id: res.File.ID, MessageId: res.File.MessageID, NumberOfTransactions: int32(res.File.Transactions),
		ControlSum: money(res.File.ControlSum), Payments: toPayments(res.Payments),
		StatusReport: &pb.Document{MessageType: iso.Pain002Type, Xml: string(res.Report)}, DuplicateRequest: dup}
	for _, r := range res.File.Rejections {
		out.Rejections = append(out.Rejections, &pb.FileRejection{EndToEndId: r.EndToEndID, ReasonCode: r.Reason, Message: r.Message})
	}
	return out, nil
}

func (s *PaymentService) GetPaymentFileStatusReport(ctx context.Context, req *pb.GetPaymentFileRequest) (*pb.Document, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	b, err := s.uc.FileStatusReport(ctx, p, req.Id)
	if err != nil {
		return nil, err
	}
	return &pb.Document{MessageType: iso.Pain002Type, Xml: string(b)}, nil
}

func (s *PaymentService) VerifyPayee(ctx context.Context, req *pb.Party) (*pb.PayeeCheck, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	c, err := s.uc.VerifyPayee(ctx, p, party(req))
	if err != nil {
		return nil, err
	}
	return &pb.PayeeCheck{AccountStatus: c.Account, NameMatch: c.Name, RegisteredName: c.Registered}, nil
}

func (s *PaymentService) ListParticipants(ctx context.Context, _ *pb.ListParticipantsRequest) (*pb.ListParticipantsResponse, error) {
	if _, err := principal(ctx); err != nil {
		return nil, err
	}
	ps, err := s.uc.Participants(ctx)
	if err != nil {
		return nil, err
	}
	out := &pb.ListParticipantsResponse{}
	for _, x := range ps {
		out.Participants = append(out.Participants, &pb.Participant{Name: x.Name, RoutingNumber: x.Routing, MemberId: x.MemberID,
			Status: x.Status, Instant_24X7: x.Status == "LIVE", ReceiverScreensInbound: x.RequiresAcceptance})
	}
	return out, nil
}
