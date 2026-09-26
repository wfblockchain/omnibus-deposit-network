// Package clearing is the off-chain half of the settlement model: the clearing
// house operator's services.
//
// The split between this package and the contracts is deliberate and is the
// central architectural claim of the model. Choosing WHICH obligations to settle
// in a cycle is an optimisation problem — with funding constraints it is a
// knapsack-shaped search, and at realistic volumes it is not something to run in
// a block. Verifying a proposed answer, by contrast, is a linear scan.
//
// So the optimiser runs here and the chain checks its work. NettingEngine
// recomputes net positions from the discharged obligations and rejects anything
// that does not follow from them, which means a compromised or simply buggy
// optimiser cannot move value the obligations do not imply. That is the property
// that makes it safe to run this off-chain at all.
package clearing

import (
	"fmt"
	"math/big"
	"sort"
)

// Obligation is a queued payment that has not settled. It moves no value until a
// cycle discharges it.
type Obligation struct {
	ID     string
	Payer  string
	Payee  string
	Amount *big.Int
}

// Cycle is a proposed settlement round: the obligations to discharge and the net
// position each participant ends up with.
type Cycle struct {
	Discharged []Obligation
	// Net position per participant. Negative pays, positive receives.
	Net map[string]*big.Int
	// Excluded obligations, with the reason. Never silently dropped: an operator
	// needs to know which payments did not make the cycle and why.
	Excluded []Exclusion
	Stats    Stats
}

// Exclusion records an obligation the optimiser could not include.
type Exclusion struct {
	Obligation Obligation
	Reason     string
}

// Stats are the numbers the economic argument rests on. Reported per cycle so
// the claim is measured rather than asserted.
type Stats struct {
	// GrossValue is the total face value of the discharged obligations.
	GrossValue *big.Int
	// NetFunding is what actually has to move: the sum of the debit positions.
	NetFunding *big.Int
	// BilateralOffset is the value cancelled by simple pairwise matching, i.e.
	// how much of the saving needs no multilateral cleverness at all.
	BilateralOffset *big.Int
	// Participants in the cycle.
	Participants int
	// Obligations discharged.
	Obligations int
}

// EfficiencyRatio is gross value settled per unit of funding moved. This is the
// figure quoted as "26:1" or "29:1" for established netting systems.
//
// Returns +Inf when a cycle nets to nothing — a perfectly circular set of
// obligations settles real payments with zero funding, which is the limiting
// case of the whole argument and not an error.
func (s Stats) EfficiencyRatio() float64 {
	if s.NetFunding == nil || s.NetFunding.Sign() == 0 {
		if s.GrossValue != nil && s.GrossValue.Sign() > 0 {
			return maxRatio
		}
		return 0
	}
	g, _ := new(big.Float).SetInt(s.GrossValue).Float64()
	n, _ := new(big.Float).SetInt(s.NetFunding).Float64()
	return g / n
}

const maxRatio = 1e9

// Netter computes settleable cycles from a queue of obligations.
type Netter struct {
	// MaxIterations bounds gridlock resolution. Each iteration removes at least
	// one obligation, so this only ever fires on a pathological queue.
	MaxIterations int
}

func NewNetter() *Netter { return &Netter{MaxIterations: 10_000} }

// Plan produces a settleable cycle from the queue, given each participant's
// available balance.
//
// Two phases, because they fail differently:
//
//  1. Net every obligation multilaterally. This is the cheap, always-correct
//     part: a participant's position is simply what it receives minus what it
//     owes, and the positions necessarily sum to zero.
//
//  2. Resolve gridlock. A participant whose debit position exceeds its balance
//     makes the whole cycle unsettleable, so obligations must be excluded until
//     every debit is funded. This is where the heuristics live, and where an
//     operator's policy choices show up.
//
// The result is always feasible: applying it on-chain cannot revert for want of
// funds. An infeasible plan that merely looks optimal is worthless, because the
// contract will reject it.
func (n *Netter) Plan(queue []Obligation, balances map[string]*big.Int) (*Cycle, error) {
	for _, o := range queue {
		if o.Amount == nil || o.Amount.Sign() <= 0 {
			return nil, fmt.Errorf("obligation %s has a non-positive amount", o.ID)
		}
		if o.Payer == o.Payee {
			return nil, fmt.Errorf("obligation %s pays its own payer", o.ID)
		}
	}

	included := make([]Obligation, len(queue))
	copy(included, queue)
	var excluded []Exclusion

	for i := 0; ; i++ {
		if i > n.MaxIterations {
			return nil, fmt.Errorf("gridlock resolution did not converge in %d iterations", n.MaxIterations)
		}

		net := netPositions(included)
		short, shortfall := mostShort(net, balances)
		if short == "" {
			// Every debit position is funded.
			return n.assemble(included, net, excluded), nil
		}

		// Drop the largest obligation the short participant owes. Largest first
		// because it relieves the most shortfall per payment removed, which keeps
		// the discharged count high — the operator's objective is settling as
		// many payments as possible, not as much value as possible.
		//
		// It is a heuristic, not an optimum. The optimum is a knapsack problem,
		// and the gap matters far less than the guarantee that what comes out is
		// feasible and auditable.
		victim, idx := largestOutgoing(included, short)
		if idx < 0 {
			// The participant is short but owes nothing we can remove — it is
			// short because of an obligation someone else cannot fund either.
			// Removing any incoming payment cannot help it, so the queue as
			// given has no settleable subset containing these participants.
			return nil, fmt.Errorf(
				"participant %s is short by %s with no outgoing obligation to defer; "+
					"the queue has no feasible cycle", short, shortfall)
		}

		included = append(included[:idx], included[idx+1:]...)
		excluded = append(excluded, Exclusion{
			Obligation: victim,
			Reason: fmt.Sprintf("deferred: %s was short %s at cycle time",
				short, shortfall),
		})
	}
}

func (n *Netter) assemble(included []Obligation, net map[string]*big.Int, excluded []Exclusion) *Cycle {
	stats := Stats{
		GrossValue:      big.NewInt(0),
		NetFunding:      big.NewInt(0),
		BilateralOffset: bilateralOffset(included),
		Obligations:     len(included),
		Participants:    len(net),
	}
	for _, o := range included {
		stats.GrossValue.Add(stats.GrossValue, o.Amount)
	}
	for _, v := range net {
		if v.Sign() < 0 {
			stats.NetFunding.Add(stats.NetFunding, new(big.Int).Neg(v))
		}
	}
	// Deterministic order, so a cycle is reproducible and two operators
	// independently planning the same queue produce byte-identical calldata.
	sort.Slice(included, func(i, j int) bool { return included[i].ID < included[j].ID })
	sort.Slice(excluded, func(i, j int) bool { return excluded[i].Obligation.ID < excluded[j].Obligation.ID })

	return &Cycle{Discharged: included, Net: net, Excluded: excluded, Stats: stats}
}

// netPositions computes each participant's multilateral net.
//
// Every participant touched by an obligation appears, including those netting to
// exactly zero — they still have to be in the on-chain net set, because the
// contract recomputes positions from the obligations and would otherwise find a
// participant it was not given.
func netPositions(obs []Obligation) map[string]*big.Int {
	net := map[string]*big.Int{}
	touch := func(p string) *big.Int {
		if _, ok := net[p]; !ok {
			net[p] = big.NewInt(0)
		}
		return net[p]
	}
	for _, o := range obs {
		touch(o.Payer).Sub(net[o.Payer], o.Amount)
		touch(o.Payee).Add(net[o.Payee], o.Amount)
	}
	return net
}

// mostShort returns the participant with the largest unfunded debit position.
func mostShort(net map[string]*big.Int, balances map[string]*big.Int) (string, *big.Int) {
	var worst string
	worstBy := big.NewInt(0)

	// Sorted iteration: map order is random in Go, and a planner that produces
	// different cycles for the same input is not one an operator can reason about.
	names := make([]string, 0, len(net))
	for p := range net {
		names = append(names, p)
	}
	sort.Strings(names)

	for _, p := range names {
		pos := net[p]
		if pos.Sign() >= 0 {
			continue
		}
		owed := new(big.Int).Neg(pos)
		bal := balances[p]
		if bal == nil {
			bal = big.NewInt(0)
		}
		if owed.Cmp(bal) <= 0 {
			continue
		}
		by := new(big.Int).Sub(owed, bal)
		if by.Cmp(worstBy) > 0 {
			worst, worstBy = p, by
		}
	}
	return worst, worstBy
}

// largestOutgoing finds the biggest obligation a participant owes.
func largestOutgoing(obs []Obligation, payer string) (Obligation, int) {
	best := -1
	for i, o := range obs {
		if o.Payer != payer {
			continue
		}
		if best < 0 || o.Amount.Cmp(obs[best].Amount) > 0 ||
			(o.Amount.Cmp(obs[best].Amount) == 0 && o.ID > obs[best].ID) {
			best = i
		}
	}
	if best < 0 {
		return Obligation{}, -1
	}
	return obs[best], best
}

// bilateralOffset is the value cancelled by pairwise matching alone.
//
// Reported separately because it answers a question an operator will ask: how
// much of the liquidity saving comes from simple two-party offsetting, and how
// much needs the multilateral round. If the two numbers are close, the
// multilateral machinery is not earning its complexity.
func bilateralOffset(obs []Obligation) *big.Int {
	pair := map[[2]string]*big.Int{}
	for _, o := range obs {
		k := [2]string{o.Payer, o.Payee}
		if pair[k] == nil {
			pair[k] = big.NewInt(0)
		}
		pair[k].Add(pair[k], o.Amount)
	}
	total := big.NewInt(0)
	seen := map[[2]string]bool{}
	for k, v := range pair {
		rev := [2]string{k[1], k[0]}
		if seen[k] || seen[rev] {
			continue
		}
		seen[k] = true
		if r, ok := pair[rev]; ok {
			seen[rev] = true
			// Each direction cancels the smaller of the two, on both sides.
			m := v
			if r.Cmp(m) < 0 {
				m = r
			}
			total.Add(total, new(big.Int).Mul(m, big.NewInt(2)))
		}
	}
	return total
}
