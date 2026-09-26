package clearing

import (
	"fmt"
	"math/big"
	"math/rand"
	"sort"
)

// Simulation runs a synthetic settlement day and measures what the design
// actually costs its participants.
//
// The point is to turn the economic claims into numbers that can be argued with.
// "Netting is more liquidity-efficient" is not a position anyone disputes; the
// question is by how much, at what queue depth, and what it costs a participant
// to fund the difference. Those are the outputs here.
type Simulation struct {
	Participants int
	// Payments generated across the day.
	Payments int
	// MeanPayment in token base units. Amounts are drawn around this.
	MeanPayment *big.Int
	// CycleSize is how many payments accumulate before a netting round runs.
	// This is the central operating dial: larger cycles find more offsets and
	// settle less often, so liquidity efficiency and settlement latency trade
	// directly against each other.
	CycleSize int
	// OpeningBalance each participant funds into the pool.
	OpeningBalance *big.Int
	// Seed makes a run reproducible.
	Seed int64
}

// Result is one simulated day.
type Result struct {
	GrossSettled *big.Int
	// NetFunded is the value that actually moved across all cycles.
	NetFunded *big.Int
	// PeakFunding is the largest drawdown any single participant took below its
	// opening balance. This — not the total — is what a treasurer provisions.
	PeakFunding *big.Int
	// GrossModePeakFunding is the same figure had every payment settled
	// individually. The ratio between the two is the liquidity saving.
	GrossModePeakFunding *big.Int
	Cycles               int
	Settled              int
	Deferred             int
}

// EfficiencyRatio is gross value settled per unit of value moved.
func (r Result) EfficiencyRatio() float64 {
	if r.NetFunded.Sign() == 0 {
		return maxRatio
	}
	g, _ := new(big.Float).SetInt(r.GrossSettled).Float64()
	n, _ := new(big.Float).SetInt(r.NetFunded).Float64()
	return g / n
}

// FundingSaving is how much less a participant must pre-fund under netting than
// under gross settlement, as a fraction. This is the number a bank treasurer
// cares about, because pre-funding is balance sheet that cannot be used for
// anything else.
func (r Result) FundingSaving() float64 {
	if r.GrossModePeakFunding == nil || r.GrossModePeakFunding.Sign() == 0 {
		return 0
	}
	net, _ := new(big.Float).SetInt(r.PeakFunding).Float64()
	gross, _ := new(big.Float).SetInt(r.GrossModePeakFunding).Float64()
	return 1 - net/gross
}

// Run executes the simulated day.
//
// Netted and gross modes are run over the SAME generated payment flow, so the
// comparison is like-for-like rather than two independent simulations that
// happened to differ.
//
// Funding requirement is measured as DRAWDOWN: how far below its opening
// balance a participant goes at its worst point. That is the quantity a
// treasurer actually has to provision, and measuring it the same way in both
// modes is what makes the saving meaningful. Opening balances are set
// deliberately generously so that nothing defers — this isolates the liquidity
// question from the gridlock question, which is measured separately by the
// netter's own tests.
func (s Simulation) Run() (*Result, error) {
	rng := rand.New(rand.NewSource(s.Seed))

	names := make([]string, s.Participants)
	for i := range names {
		names[i] = fmt.Sprintf("bank-%02d", i)
	}

	// Generate the day's flow once. Real payment values are heavily skewed, and
	// a uniform distribution would understate the funding peak that the tail
	// drives.
	payments := make([]Obligation, 0, s.Payments)
	for i := 0; i < s.Payments; i++ {
		payer := names[rng.Intn(len(names))]
		payee := names[rng.Intn(len(names))]
		for payee == payer {
			payee = names[rng.Intn(len(names))]
		}
		mult := rng.ExpFloat64()
		payments = append(payments, Obligation{
			ID:     fmt.Sprintf("p%06d", i),
			Payer:  payer,
			Payee:  payee,
			Amount: new(big.Int).Mul(s.MeanPayment, big.NewInt(int64(1+mult*3))),
		})
	}

	res := &Result{
		GrossSettled: big.NewInt(0),
		NetFunded:    big.NewInt(0),
	}

	// ── Netted mode ──────────────────────────────────────────────────────────
	balances := openingBalances(names, s.OpeningBalance)
	drawdown := zeroed(names)
	netter := NewNetter()

	for start := 0; start < len(payments); start += s.CycleSize {
		end := start + s.CycleSize
		if end > len(payments) {
			end = len(payments)
		}
		cycle, err := netter.Plan(payments[start:end], balances)
		if err != nil {
			return nil, fmt.Errorf("cycle %d: %w", res.Cycles, err)
		}
		for p, pos := range cycle.Net {
			balances[p].Add(balances[p], pos)
			if pos.Sign() < 0 {
				res.NetFunded.Add(res.NetFunded, new(big.Int).Neg(pos))
			}
			recordDrawdown(drawdown, p, balances[p], s.OpeningBalance)
		}
		for _, o := range cycle.Discharged {
			res.GrossSettled.Add(res.GrossSettled, o.Amount)
		}
		res.Settled += len(cycle.Discharged)
		res.Deferred += len(cycle.Excluded)
		res.Cycles++
	}

	// ── Gross mode, same flow ────────────────────────────────────────────────
	grossBalances := openingBalances(names, s.OpeningBalance)
	grossDrawdown := zeroed(names)
	for _, o := range payments {
		grossBalances[o.Payer].Sub(grossBalances[o.Payer], o.Amount)
		grossBalances[o.Payee].Add(grossBalances[o.Payee], o.Amount)
		recordDrawdown(grossDrawdown, o.Payer, grossBalances[o.Payer], s.OpeningBalance)
	}

	res.PeakFunding = maxOf(drawdown)
	res.GrossModePeakFunding = maxOf(grossDrawdown)
	return res, nil
}

func openingBalances(names []string, opening *big.Int) map[string]*big.Int {
	out := map[string]*big.Int{}
	for _, n := range names {
		out[n] = new(big.Int).Set(opening)
	}
	return out
}

func zeroed(names []string) map[string]*big.Int {
	out := map[string]*big.Int{}
	for _, n := range names {
		out[n] = big.NewInt(0)
	}
	return out
}

// recordDrawdown tracks how far below opening a participant has fallen.
func recordDrawdown(dd map[string]*big.Int, p string, balance, opening *big.Int) {
	used := new(big.Int).Sub(opening, balance)
	if used.Sign() > 0 && used.Cmp(dd[p]) > 0 {
		dd[p] = used
	}
}

func maxOf(m map[string]*big.Int) *big.Int {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := big.NewInt(0)
	for _, k := range keys {
		if m[k].Cmp(out) > 0 {
			out = new(big.Int).Set(m[k])
		}
	}
	return out
}

// ─── Accrual economics ───────────────────────────────────────────────────────

// AccrualModel converts a reserve pool and a rate into the per-participant
// entitlement the on-chain index distributes.
//
// It exists to make one point precisely: the size of this number is decided by
// where the reserve pool is held, not by the token design. The contract
// mechanism is identical whether the rate is 4% or 0% — and it is 0% if the pool
// sits in an account that pays no interest, which is the case for at least one
// of the account structures on the table.
type AccrualModel struct {
	// PoolBalance in token base units.
	PoolBalance *big.Int
	// AnnualRateBps on the pool, e.g. 400 for 4%.
	AnnualRateBps int64
	// Days accrued.
	Days int64
}

// PoolIncome is the amount credited to the accrual index over the period.
func (a AccrualModel) PoolIncome() *big.Int {
	if a.PoolBalance == nil || a.AnnualRateBps == 0 {
		return big.NewInt(0)
	}
	income := new(big.Int).Mul(a.PoolBalance, big.NewInt(a.AnnualRateBps))
	income.Mul(income, big.NewInt(a.Days))
	return income.Div(income, big.NewInt(10_000*365))
}

// SterilizationCost is what participants forgo if the pool earns nothing while
// the same reserves would otherwise have earned the rate.
//
// This is the "reserve sterilization" objection expressed as a number. If the
// pool earns the rate, the cost is zero and economics genuinely do travel with
// the token. If it earns nothing, this is the annual cost of the settlement
// asset to its participants, and no amount of contract design reduces it.
func (a AccrualModel) SterilizationCost(poolEarnsRate bool) *big.Int {
	if poolEarnsRate {
		return big.NewInt(0)
	}
	return a.PoolIncome()
}
