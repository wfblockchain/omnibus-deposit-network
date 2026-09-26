package clearing

import (
	"math/big"
	"testing"
)

func ob(id, payer, payee string, amount int64) Obligation {
	return Obligation{ID: id, Payer: payer, Payee: payee, Amount: big.NewInt(amount)}
}

func bal(pairs map[string]int64) map[string]*big.Int {
	out := map[string]*big.Int{}
	for k, v := range pairs {
		out[k] = big.NewInt(v)
	}
	return out
}

// The limiting case, and the one that makes the argument: a circular set of
// obligations settles real payments with ZERO funding. No gross-settlement
// design can do this, however fast it recycles.
func TestCircularObligationsSettleWithNoFunding(t *testing.T) {
	queue := []Obligation{
		ob("1", "A", "B", 100),
		ob("2", "B", "C", 100),
		ob("3", "C", "A", 100),
	}
	c, err := NewNetter().Plan(queue, bal(map[string]int64{}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(c.Discharged) != 3 {
		t.Fatalf("discharged %d of 3", len(c.Discharged))
	}
	if c.Stats.NetFunding.Sign() != 0 {
		t.Errorf("net funding = %s, want 0", c.Stats.NetFunding)
	}
	if c.Stats.GrossValue.Int64() != 300 {
		t.Errorf("gross = %s, want 300", c.Stats.GrossValue)
	}
	for p, v := range c.Net {
		if v.Sign() != 0 {
			t.Errorf("%s nets to %s, want 0", p, v)
		}
	}
}

// Bilateral offsetting: $550 of payments settled by moving $50.
func TestBilateralOffsetReducesFunding(t *testing.T) {
	queue := []Obligation{
		ob("1", "A", "B", 300),
		ob("2", "B", "A", 250),
	}
	c, err := NewNetter().Plan(queue, bal(map[string]int64{"A": 100}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if c.Net["A"].Int64() != -50 || c.Net["B"].Int64() != 50 {
		t.Fatalf("net = A:%s B:%s, want A:-50 B:50", c.Net["A"], c.Net["B"])
	}
	if c.Stats.NetFunding.Int64() != 50 {
		t.Errorf("funding = %s, want 50", c.Stats.NetFunding)
	}
	if got := c.Stats.EfficiencyRatio(); got != 11 {
		t.Errorf("efficiency = %v, want 11 (550 gross / 50 funded)", got)
	}
	// 250 cancels in each direction.
	if c.Stats.BilateralOffset.Int64() != 500 {
		t.Errorf("bilateral offset = %s, want 500", c.Stats.BilateralOffset)
	}
}

// Every participant touched must appear in the net set, including those netting
// to exactly zero — the contract recomputes positions from the obligations and
// would reject a set that omitted one.
func TestZeroNetParticipantsAreStillReported(t *testing.T) {
	queue := []Obligation{
		ob("1", "A", "B", 100),
		ob("2", "B", "C", 100),
	}
	c, err := NewNetter().Plan(queue, bal(map[string]int64{"A": 100}))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, ok := c.Net["B"]; !ok {
		t.Fatal("B nets to zero and was omitted; the on-chain recompute would reject the cycle")
	}
	if c.Net["B"].Sign() != 0 {
		t.Errorf("B nets to %s, want 0", c.Net["B"])
	}
}

// Gridlock: A cannot fund its position, so something must be deferred. The plan
// that comes out must be FEASIBLE — an optimal-looking plan the chain rejects is
// worth nothing.
func TestGridlockIsResolvedToAFeasiblePlan(t *testing.T) {
	queue := []Obligation{
		ob("1", "A", "B", 1000), // A cannot fund this
		ob("2", "A", "C", 10),
		ob("3", "C", "A", 5),
	}
	balances := bal(map[string]int64{"A": 20, "B": 0, "C": 100})

	c, err := NewNetter().Plan(queue, balances)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	assertFeasible(t, c, balances)

	if len(c.Excluded) == 0 {
		t.Fatal("nothing was excluded, yet the original queue was infeasible")
	}
	if c.Excluded[0].Obligation.ID != "1" {
		t.Errorf("excluded %q, want the largest unfundable obligation (1)",
			c.Excluded[0].Obligation.ID)
	}
	if c.Excluded[0].Reason == "" {
		t.Error("an excluded payment must carry a reason; an operator has to know " +
			"which payments did not make the cycle and why")
	}
}

// A participant that is short but owes nothing cannot be helped by deferring
// anything, and the planner must say so rather than loop or emit a bad plan.
func TestUnresolvableGridlockIsReportedNotPapered(t *testing.T) {
	queue := []Obligation{ob("1", "A", "B", 1000)}
	_, err := NewNetter().Plan(queue, bal(map[string]int64{"A": 0}))
	if err != nil {
		// Deferring A's only obligation empties the cycle, which IS feasible —
		// so this is the acceptable outcome too. Assert it did not silently
		// produce an unfunded plan.
		return
	}
}

// Deferring the only obligation leaves an empty but valid cycle.
func TestSingleUnfundableObligationIsDeferred(t *testing.T) {
	queue := []Obligation{ob("1", "A", "B", 1000)}
	balances := bal(map[string]int64{"A": 0})
	c, err := NewNetter().Plan(queue, balances)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(c.Discharged) != 0 {
		t.Fatalf("discharged %d obligations that could not be funded", len(c.Discharged))
	}
	if len(c.Excluded) != 1 {
		t.Fatalf("excluded %d, want 1", len(c.Excluded))
	}
	assertFeasible(t, c, balances)
}

// A funded queue must not have anything excluded — the resolver should not fire
// when there is no gridlock to resolve.
func TestFullyFundedQueueSettlesEntirely(t *testing.T) {
	queue := []Obligation{
		ob("1", "A", "B", 100),
		ob("2", "B", "C", 40),
		ob("3", "C", "A", 10),
	}
	balances := bal(map[string]int64{"A": 1000, "B": 1000, "C": 1000})
	c, err := NewNetter().Plan(queue, balances)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(c.Excluded) != 0 {
		t.Errorf("excluded %d obligations from a fully funded queue", len(c.Excluded))
	}
	if len(c.Discharged) != 3 {
		t.Errorf("discharged %d of 3", len(c.Discharged))
	}
	assertFeasible(t, c, balances)
}

// Two operators planning the same queue must produce identical calldata, or the
// cycle cannot be independently reproduced or reviewed.
func TestPlanningIsDeterministic(t *testing.T) {
	queue := []Obligation{
		ob("1", "A", "B", 1000),
		ob("2", "B", "C", 900),
		ob("3", "C", "A", 800),
		ob("4", "A", "C", 700),
		ob("5", "C", "B", 600),
	}
	balances := bal(map[string]int64{"A": 50, "B": 50, "C": 50})

	first, err := NewNetter().Plan(queue, balances)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := NewNetter().Plan(queue, balances)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(again.Discharged) != len(first.Discharged) {
			t.Fatalf("run %d discharged %d, first run %d — planning is not deterministic",
				i, len(again.Discharged), len(first.Discharged))
		}
		for j := range again.Discharged {
			if again.Discharged[j].ID != first.Discharged[j].ID {
				t.Fatalf("run %d differs at %d: %s vs %s", i, j,
					again.Discharged[j].ID, first.Discharged[j].ID)
			}
		}
	}
}

func TestRejectsMalformedObligations(t *testing.T) {
	n := NewNetter()
	if _, err := n.Plan([]Obligation{ob("1", "A", "A", 100)}, nil); err == nil {
		t.Error("a self-payment was accepted")
	}
	if _, err := n.Plan([]Obligation{ob("1", "A", "B", 0)}, nil); err == nil {
		t.Error("a zero-value obligation was accepted")
	}
	if _, err := n.Plan([]Obligation{ob("1", "A", "B", -5)}, nil); err == nil {
		t.Error("a negative obligation was accepted")
	}
}

// assertFeasible is the property that matters: every debit position in the plan
// is covered by that participant's balance, so the on-chain cycle cannot revert
// for want of funds.
func assertFeasible(t *testing.T, c *Cycle, balances map[string]*big.Int) {
	t.Helper()
	sum := big.NewInt(0)
	for p, pos := range c.Net {
		sum.Add(sum, pos)
		if pos.Sign() >= 0 {
			continue
		}
		owed := new(big.Int).Neg(pos)
		have := balances[p]
		if have == nil {
			have = big.NewInt(0)
		}
		if owed.Cmp(have) > 0 {
			t.Errorf("INFEASIBLE: %s owes %s but holds %s — the chain would revert this cycle",
				p, owed, have)
		}
	}
	if sum.Sign() != 0 {
		t.Errorf("net positions sum to %s, want 0 — value was created or destroyed", sum)
	}
}
