package operator

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"omnibus-deposit-network/internal/chain"
	"omnibus-deposit-network/internal/clearing"
	"omnibus-deposit-network/internal/iso20022"
	"omnibus-deposit-network/internal/omnibus"
)

// Messages carries the customer detail behind each interbank obligation
// (its pacs.008) from the sending bank to the receiving bank, off-chain, so
// the receiver knows whose account to credit when the obligation settles.
// The chain carries only bank, bank and amount.
type Messages struct {
	mu sync.Mutex
	m  map[[32]byte]iso20022.Pacs008
}

func NewMessages() *Messages { return &Messages{m: map[[32]byte]iso20022.Pacs008{}} }

func (b *Messages) Put(obligationID [32]byte, m iso20022.Pacs008) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[obligationID] = m
}

func (b *Messages) Get(obligationID [32]byte) (iso20022.Pacs008, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.m[obligationID]
	return m, ok
}

type obligation struct {
	id      [32]byte
	payer   string
	payee   string
	amount  *big.Int
	expires uint64
}

// CycleReport summarises one netting cycle.
type CycleReport struct {
	CycleID    string
	Discharged int
	Deferred   int
	Gross      *big.Int
	Net        *big.Int
	At         time.Time // set by the caller: when, and who or what ran it
	Trigger    string
}

// Efficiency is value discharged per unit of free position moved.
func (r CycleReport) Efficiency() float64 {
	if r.Net.Sign() == 0 {
		return 0
	}
	g, _ := new(big.Float).SetInt(r.Gross).Float64()
	n, _ := new(big.Float).SetInt(r.Net).Float64()
	return g / n
}

// NettingService plans and submits multilateral netting cycles. The plan
// comes from the Go netting planner, fed with each member's free position
// read from the ledger; the contract verifies every net before the ledger
// moves anything. Obligations the planner defers stay queued.
type NettingService struct {
	net *omnibus.Network
	log Logger

	mu      sync.Mutex
	cursor  uint64
	queue   map[[32]byte]obligation
	settled uint64
	cycles  int
}

func NewNettingService(net *omnibus.Network, log Logger) *NettingService {
	return &NettingService{net: net, log: log, queue: map[[32]byte]obligation{}}
}

// Sync reads new obligations and drops those settled, cancelled or expired.
func (s *NettingService) Sync(ctx context.Context) error {
	head, err := s.net.C.Head(ctx)
	if err != nil {
		return err
	}
	subs, err := s.net.Netting.Events(ctx, "ObligationSubmitted", s.cursor, head)
	if err != nil {
		return err
	}
	for _, l := range subs {
		var ev struct {
			ObligationId [32]byte
			Payer        [32]byte
			Payee        [32]byte
			Amount       *big.Int
			ExpiresAt    uint64
		}
		if err := s.net.Netting.Unpack(&ev, "ObligationSubmitted", l); err != nil {
			return err
		}
		id, payer, payee := l.Topics[1], l.Topics[2], l.Topics[3]
		s.mu.Lock()
		s.queue[id] = obligation{id: id, payer: memberName(payer), payee: memberName(payee), amount: ev.Amount, expires: ev.ExpiresAt}
		s.mu.Unlock()
	}
	for _, name := range []string{"ObligationSettled", "ObligationCancelled", "ObligationExpired"} {
		logs, err := s.net.Netting.Events(ctx, name, s.cursor, head)
		if err != nil {
			return err
		}
		s.mu.Lock()
		for _, l := range logs {
			delete(s.queue, l.Topics[1])
		}
		s.mu.Unlock()
	}
	s.cursor = head + 1
	return nil
}

// Queued is how many obligations wait for a cycle.
func (s *NettingService) Queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// RunCycle plans a cycle over the queue and submits it. With nothing
// settleable it does nothing.
func (s *NettingService) RunCycle(ctx context.Context) (CycleReport, error) {
	if err := s.Sync(ctx); err != nil {
		return CycleReport{}, err
	}
	s.mu.Lock()
	queue := make([]clearing.Obligation, 0, len(s.queue))
	byHex := map[string][32]byte{}
	for id, o := range s.queue {
		h := hex.EncodeToString(id[:])
		byHex[h] = id
		queue = append(queue, clearing.Obligation{ID: h, Payer: o.payer, Payee: o.payee, Amount: o.amount})
	}
	s.mu.Unlock()
	if len(queue) == 0 {
		return CycleReport{}, nil
	}

	balances := map[string]*big.Int{}
	for id := range s.net.Banks {
		free, err := s.net.Ledger.BigInt(ctx, "freePosition", chain.Bytes32(id))
		if err != nil {
			return CycleReport{}, err
		}
		balances[id] = free
	}
	plan, err := clearing.NewNetter().Plan(queue, balances)
	if err != nil {
		return CycleReport{}, err
	}
	if len(plan.Discharged) == 0 {
		return CycleReport{Deferred: len(plan.Excluded), Gross: big.NewInt(0), Net: big.NewInt(0)}, nil
	}

	// Calldata: members and obligation ids strictly increasing as bytes32.
	members := make([][32]byte, 0, len(plan.Net))
	for m := range plan.Net {
		members = append(members, chain.Bytes32(m))
	}
	sort.Slice(members, func(i, j int) bool { return bytes.Compare(members[i][:], members[j][:]) < 0 })
	nets := make([]*big.Int, len(members))
	for i, m := range members {
		nets[i] = plan.Net[memberName(m)]
	}
	discharged := make([][32]byte, len(plan.Discharged))
	for i, o := range plan.Discharged {
		discharged[i] = byHex[o.ID]
	}
	sort.Slice(discharged, func(i, j int) bool { return bytes.Compare(discharged[i][:], discharged[j][:]) < 0 })

	s.cycles++
	cycleID := fmt.Sprintf("CYCLE-%d", s.cycles)
	if _, err := s.net.Netting.Send(ctx, s.net.Operator.Settlement, "settleCycle", chain.Bytes32(cycleID), members, nets, discharged); err != nil {
		return CycleReport{}, err
	}
	rep := CycleReport{
		CycleID: cycleID, Discharged: len(plan.Discharged), Deferred: len(plan.Excluded),
		Gross: plan.Stats.GrossValue, Net: plan.Stats.NetFunding,
	}
	s.log("operator netting  %s: %d obligations, gross $%s settled by moving $%s net (%.1f : 1); %d deferred",
		cycleID, rep.Discharged, iso20022.FormatDollars(rep.Gross), iso20022.FormatDollars(rep.Net), rep.Efficiency(), rep.Deferred)
	return rep, s.Sync(ctx)
}

// ExpireStale expires every queued obligation whose time-to-live has run
// out as of `now` (anyone may; the operator does it as housekeeping). The sending
// bank then re-credits its customer.
func (s *NettingService) ExpireStale(ctx context.Context, now uint64) (int, error) {
	s.mu.Lock()
	var stale [][32]byte
	for id, o := range s.queue {
		if now > o.expires {
			stale = append(stale, id)
		}
	}
	s.mu.Unlock()
	for _, id := range stale {
		if _, err := s.net.Netting.Send(ctx, s.net.Operator.Settlement, "expire", id); err != nil {
			return 0, err
		}
		s.log("operator netting  obligation %x… expired unsettled", id[:4])
	}
	return len(stale), s.Sync(ctx)
}

func memberName(b [32]byte) string { return string(trimZero(b[:])) }
