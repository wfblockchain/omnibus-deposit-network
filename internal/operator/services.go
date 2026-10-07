// Package operator holds the network operator's back office for the omnibus: the
// funding service that turns Fed notifications into ledger postings and
// executes defunds over Fedwire, the reconciler that attests the Fed
// statement, and the directory that maps bank accounts to wallets.
package operator

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"

	"omnibus-deposit-network/internal/chain"
	"omnibus-deposit-network/internal/fedwire"
	"omnibus-deposit-network/internal/iso20022"
	"omnibus-deposit-network/internal/omnibus"
)

// Logger receives one line per business event.
type Logger func(format string, args ...any)

// JointAccount is the Fed account id of the joint account backing the
// tickers: a dedicated account for the token network, with the operator as
// agent for the member banks that jointly own it. The Fed's joint-account
// guidelines tie balances to settling the associated system, so an existing
// payment system's joint account would not serve.
const JointAccount = "FED-TOKEN-JOINT"

// FundingPurpose is the category purpose members put on funding transfers.
const FundingPurpose = "TOKEN FUNDING"

// OperatorAgent identifies the operator on Fedwire.
var OperatorAgent = iso20022.Agent{BICFI: "OPERUS30", ABA: "098765438"}

type defundReq struct {
	ID     [32]byte
	Member string
	Units  *big.Int
}

// FundingService posts every movement on the joint account to the ledger
// and executes defund requests over Fedwire when it is open.
type FundingService struct {
	net *omnibus.Network
	fed *fedwire.Service
	log Logger

	mu       sync.Mutex
	queue    []defundReq
	inflight map[string]defundReq // by UETR
	cursor   uint64
	down     bool
	backlog  []iso20022.Advice
	Errors   []error
}

func NewFundingService(net *omnibus.Network, fed *fedwire.Service, log Logger) *FundingService {
	s := &FundingService{net: net, fed: fed, log: log, inflight: map[string]defundReq{}}
	fed.Subscribe(JointAccount, s.onNotification)
	return s
}

// SetDown simulates an outage: notifications pile up unposted.
func (s *FundingService) SetDown(down bool) {
	s.mu.Lock()
	s.down = down
	s.mu.Unlock()
}

// Replay posts notifications missed during an outage. Posting is
// idempotent on the Fedwire reference, so replaying twice is harmless.
func (s *FundingService) Replay(ctx context.Context) {
	s.mu.Lock()
	b := s.backlog
	s.backlog = nil
	s.mu.Unlock()
	for _, n := range b {
		s.post(ctx, n)
	}
}

func (s *FundingService) onNotification(n iso20022.Advice) {
	s.mu.Lock()
	if s.down {
		s.backlog = append(s.backlog, n)
		s.mu.Unlock()
		s.log("the operator funding  Fed advice %s $%s missed (service down)", n.CdtDbtInd, n.Amount.Value)
		return
	}
	s.mu.Unlock()
	s.post(context.Background(), n)
}

func (s *FundingService) post(ctx context.Context, n iso20022.Advice) {
	ref := chain.RefOf(n.UETR)
	var err error
	switch {
	case n.CdtDbtInd == "CRDT" && n.Purpose == "INTR":
		_, err = s.net.Ledger.Send(ctx, s.net.Operator.Funding, "distributeInterest", n.Units, ref)
		if err == nil {
			s.log("the operator funding  Fed advice CRDT INTR $%s -> distributeInterest (time-weighted)", n.Amount.Value)
		}
	case n.CdtDbtInd == "CRDT":
		// The member id travels in EndToEndId, as the RTP participant id does
		// on RTP funding; the sender's routing number is the fallback.
		b, ok := s.net.Banks[n.EndToEndID]
		if !ok {
			b, ok = s.net.BankByABA(n.Counterpart.ABA)
		}
		if !ok {
			err = fmt.Errorf("credit %s from unknown member %q / ABA %s", n.UETR, n.EndToEndID, n.Counterpart.ABA)
			break
		}
		_, err = s.net.Ledger.Send(ctx, s.net.Operator.Funding, "creditFunding", chain.Bytes32(b.Spec.MemberID), n.Units, ref)
		if err == nil {
			s.log("the operator funding  Fed advice CRDT $%s (%s) from %s -> creditFunding(%s)", n.Amount.Value, n.LclInstrm, b.Spec.Name, b.Spec.MemberID)
		}
	case n.CdtDbtInd == "DBIT":
		s.mu.Lock()
		req, ok := s.inflight[n.UETR]
		delete(s.inflight, n.UETR)
		s.mu.Unlock()
		if !ok {
			err = fmt.Errorf("debit %s matches no defund", n.UETR)
			break
		}
		_, err = s.net.Ledger.Send(ctx, s.net.Operator.Funding, "confirmDefund", req.ID, ref)
		if err == nil {
			s.log("the operator funding  Fed advice DBIT $%s -> confirmDefund(%s)", n.Amount.Value, req.Member)
		}
	}
	if err != nil {
		s.mu.Lock()
		s.Errors = append(s.Errors, err)
		s.mu.Unlock()
		s.log("the operator funding  ERROR %v", err)
	}
}

// Sync reads newly approved defunds into the queue. A request the member's
// approver has not signed is not the operator's to act on.
func (s *FundingService) Sync(ctx context.Context) error {
	head, err := s.net.C.Head(ctx)
	if err != nil {
		return err
	}
	if head < s.cursor {
		return nil
	}
	logs, err := s.net.Ledger.Events(ctx, "DefundApproved", s.cursor, head)
	if err != nil {
		return err
	}
	for _, l := range logs {
		var ev struct {
			DefundId [32]byte
			MemberId [32]byte
			Amount   *big.Int
		}
		if err := s.net.Ledger.Unpack(&ev, "DefundApproved", l); err != nil {
			return err
		}
		ev.DefundId, ev.MemberId = l.Topics[1], l.Topics[2]
		member := string(trimZero(ev.MemberId[:]))
		s.mu.Lock()
		s.queue = append(s.queue, defundReq{ID: ev.DefundId, Member: member, Units: ev.Amount})
		s.mu.Unlock()
		s.log("the operator funding  DefundApproved %s $%s queued for Fedwire", member, iso20022.FormatDollars(ev.Amount))
	}
	s.cursor = head + 1
	return nil
}

// Process sends queued defunds over Fedwire if it is open. It returns how
// many remain queued.
func (s *FundingService) Process(ctx context.Context) int {
	if !s.fed.Calendar().IsOpen(s.fed.Now()) {
		s.mu.Lock()
		n := len(s.queue)
		s.mu.Unlock()
		if n > 0 {
			s.log("the operator funding  Fedwire closed: %d defund(s) wait for the next operating day", n)
		}
		return n
	}
	s.mu.Lock()
	q := s.queue
	s.queue = nil
	s.mu.Unlock()
	for _, r := range q {
		b := s.net.Banks[r.Member]
		amt, err := iso20022.USD(r.Units)
		if err != nil {
			s.fail(ctx, r, "SUBCENT")
			continue
		}
		uetr := fedwire.NewUETR()
		m := iso20022.Pacs009{
			GrpHdr: iso20022.GroupHeader{MsgID: "operator-" + uetr[:8]}, UETR: uetr, Amount: amt,
			Debtor: OperatorAgent, Creditor: iso20022.Agent{BICFI: b.Spec.MemberID, ABA: b.Spec.ABA},
			DebtorAcct: JointAccount, CreditorAcct: b.Spec.MasterAccount, Purpose: "DEFUND",
			EndToEndID: b.Spec.MemberID, LclInstrm: "BTRC",
		}
		s.mu.Lock()
		s.inflight[uetr] = r
		s.mu.Unlock()
		s.log("Fedwire      pacs.009 the joint -> %s master $%s", b.Spec.Name, amt.Value)
		st := s.fed.Send(m, r.Units)
		if st.TxSts != iso20022.StatusAccepted {
			s.log("Fedwire      pacs.002 %s %s", st.TxSts, st.Reason)
		}
		if st.TxSts != iso20022.StatusAccepted {
			s.mu.Lock()
			delete(s.inflight, uetr)
			s.mu.Unlock()
			s.fail(ctx, r, st.Reason)
		}
	}
	return 0
}

func (s *FundingService) fail(ctx context.Context, r defundReq, reason string) {
	if _, err := s.net.Ledger.Send(ctx, s.net.Operator.Funding, "failDefund", r.ID, chain.Bytes32(reason)); err != nil {
		s.mu.Lock()
		s.Errors = append(s.Errors, err)
		s.mu.Unlock()
	}
	s.log("the operator funding  failDefund(%s) %s", r.Member, reason)
}

// ReconReport is one reconciliation of the ledger to the Fed statement.
type ReconReport struct {
	StatementID string
	FedBalance  *big.Int
	LedgerTotal *big.Int
	Break       bool
	Invariants  bool
}

// Reconcile attests the joint account's balance from the Fed's camt.052
// account report with the reconciler
// key, then reads back whether the ledger halted.
func Reconcile(ctx context.Context, net *omnibus.Network, fed *fedwire.Service) (ReconReport, error) {
	st := fed.Statement(JointAccount)
	if _, err := net.Ledger.Send(ctx, net.Operator.Reconciler, "attestFedBalance", st.Units, chain.RefOf(st.StmtID)); err != nil {
		return ReconReport{}, err
	}
	total, err := net.Ledger.BigInt(ctx, "omnibusTotal")
	if err != nil {
		return ReconReport{}, err
	}
	brk, err := net.Ledger.Bool(ctx, "reconciliationBreak")
	if err != nil {
		return ReconReport{}, err
	}
	inv, err := net.Ledger.Bool(ctx, "invariantsHold")
	if err != nil {
		return ReconReport{}, err
	}
	return ReconReport{StatementID: st.StmtID, FedBalance: st.Units, LedgerTotal: total, Break: brk, Invariants: inv}, nil
}

// Directory maps a bank account (routing number + account number) to the
// wallet that receives tokens for it, the role RTP's routing and account
// numbers play today.
type Directory struct {
	mu sync.Mutex
	m  map[string]common.Address
}

func NewDirectory() *Directory { return &Directory{m: map[string]common.Address{}} }

func (d *Directory) Register(aba, account string, wallet common.Address) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m[aba+"/"+account] = wallet
}

func (d *Directory) Resolve(aba, account string) (common.Address, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w, ok := d.m[aba+"/"+account]
	return w, ok
}

func trimZero(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}
