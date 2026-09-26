// Package fedwire simulates the Federal Reserve side of the omnibus model:
// master accounts, the operator's joint account, the Fedwire Funds operating day,
// FedNow liquidity management transfers, and the advices and camt.052
// account reports account holders receive.
//
// It exists so the operator and bank services can be exercised against the rules
// that actually constrain them: reserves move only between Fed accounts,
// Fedwire moves them only in its operating day, the FedNow LMT service can
// fund a joint account while Fedwire is closed but never defund it, and the
// Reserve Bank gives a joint account no intraday or overnight credit.
package fedwire

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"omnibus-deposit-network/internal/iso20022"
)

// Clock lets a scenario move time across weekends and holidays.
type Clock interface{ Now() time.Time }

// ManualClock is a settable clock.
type ManualClock struct {
	mu sync.Mutex
	t  time.Time
}

func NewManualClock(t time.Time) *ManualClock { return &ManualClock{t: t} }

func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// Calendar is the Fedwire Funds operating calendar. Business day D runs from
// 9:00 p.m. ET on the preceding calendar day to 7:00 p.m. ET on D (the
// pacs.009 cutoff; customer pacs.008 cuts off at 6:45 p.m.). Today weekends
// and Federal Reserve holidays are not business days. The Board's announced
// 22x6 schedule (expected 2028 or 2029) keeps the daily hours but operates
// Sunday through Friday, including weekday holidays: set SixDay.
type Calendar struct {
	Loc      *time.Location
	Holidays map[string]bool // "2006-01-02"
	SixDay   bool
}

// NewCalendar returns the Eastern-time calendar with the 2026 Federal
// Reserve holidays. Independence Day 2026 falls on a Saturday; the Fed is
// open on the preceding Friday, so it does not appear here.
func NewCalendar() (Calendar, error) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return Calendar{}, err
	}
	h := map[string]bool{}
	for _, d := range []string{
		"2026-01-01", "2026-01-19", "2026-02-16", "2026-05-25", "2026-06-19",
		"2026-09-07", "2026-10-12", "2026-11-11", "2026-11-26", "2026-12-25",
	} {
		h[d] = true
	}
	return Calendar{Loc: loc, Holidays: h}, nil
}

// NewCalendar22x6 returns the announced 22x6 calendar: business days Sunday
// through Friday, holidays included.
func NewCalendar22x6() (Calendar, error) {
	c, err := NewCalendar()
	c.SixDay = true
	return c, err
}

// BusinessDay reports whether the calendar date of t (in ET) is a Fedwire
// business day.
func (c Calendar) BusinessDay(t time.Time) bool {
	et := t.In(c.Loc)
	if c.SixDay {
		return et.Weekday() != time.Saturday
	}
	if et.Weekday() == time.Saturday || et.Weekday() == time.Sunday {
		return false
	}
	return !c.Holidays[et.Format("2006-01-02")]
}

// IsOpen reports whether Fedwire Funds accepts transfers at t.
func (c Calendar) IsOpen(t time.Time) bool {
	et := t.In(c.Loc)
	switch {
	case et.Hour() >= 21:
		return c.BusinessDay(et.AddDate(0, 0, 1))
	case et.Hour() < 19:
		return c.BusinessDay(et)
	default:
		return false
	}
}

// NextOpen returns the first instant at or after t when Fedwire is open.
func (c Calendar) NextOpen(t time.Time) time.Time {
	if c.IsOpen(t) {
		return t
	}
	et := t.In(c.Loc)
	for i := 0; i < 24*14; i++ {
		cand := time.Date(et.Year(), et.Month(), et.Day(), et.Hour(), 0, 0, 0, c.Loc).Add(time.Duration(i+1) * time.Hour)
		if c.IsOpen(cand) {
			return cand
		}
	}
	return time.Time{}
}

// Service is the simulated Fed: accounts, Fedwire transfers, notifications.
type Service struct {
	mu       sync.Mutex
	clock    Clock
	cal      Calendar
	balances map[string]*big.Int
	agents   map[string]iso20022.Agent
	entries  map[string]int
	subs     map[string][]func(iso20022.Advice)
	lmt      map[string]bool // joint accounts the FedNow LMT service may fund
	seq      atomic.Int64
}

func New(clock Clock, cal Calendar) *Service {
	return &Service{
		clock:    clock,
		cal:      cal,
		balances: map[string]*big.Int{},
		agents:   map[string]iso20022.Agent{},
		entries:  map[string]int{},
		subs:     map[string][]func(iso20022.Advice){},
		lmt:      map[string]bool{},
	}
}

// EnableLMT lets account holders fund a joint account through the FedNow
// liquidity management transfer service while Fedwire is closed. RTP's rules
// allow it for the RTP joint account from 1 June 2026; outflows still need
// Fedwire.
func (s *Service) EnableLMT(jointAccount string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lmt[jointAccount] = true
}

func (s *Service) Calendar() Calendar { return s.cal }

// Now is the Fed's current time.
func (s *Service) Now() time.Time { return s.clock.Now() }

// OpenAccount opens a master account (or the joint account) at the Fed.
func (s *Service) OpenAccount(id string, owner iso20022.Agent, opening *big.Int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balances[id] = new(big.Int).Set(opening)
	s.agents[id] = owner
}

// Subscribe registers a receiver for advices on an account.
func (s *Service) Subscribe(account string, fn func(iso20022.Advice)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs[account] = append(s.subs[account], fn)
}

// Balance returns an account's current balance in ledger units.
func (s *Service) Balance(account string) *big.Int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return new(big.Int).Set(s.balances[account])
}

// NewUETR returns a random RFC 4122 v4 UUID, the unique end-to-end
// transaction reference every ISO 20022 payment carries.
func NewUETR() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Send executes a pacs.009 between two Fed accounts. A Fedwire transfer
// (BTRC) is refused outside the operating day; a FedNow LMT (LMT1) only
// runs while Fedwire is closed and only into an LMT-enabled joint account.
// Either is refused when the debit account cannot cover it: joint accounts
// get no daylight credit, so the model gives none to anyone.
func (s *Service) Send(m iso20022.Pacs009, units *big.Int) iso20022.Pacs002 {
	now := s.clock.Now()
	status := iso20022.Pacs002{GrpHdr: iso20022.GroupHeader{MsgID: s.nextID("STS"), CreDtTm: now}, OrgnlUETR: m.UETR}
	if m.LclInstrm == "LMT1" {
		s.mu.Lock()
		ok := s.lmt[m.CreditorAcct]
		s.mu.Unlock()
		if !ok || s.cal.IsOpen(now) {
			status.TxSts, status.Reason = iso20022.StatusRejected, iso20022.ReasonNotOpen
			return status
		}
	} else if !s.cal.IsOpen(now) {
		status.TxSts, status.Reason = iso20022.StatusRejected, iso20022.ReasonNotOpen
		return status
	}

	s.mu.Lock()
	from, ok1 := s.balances[m.DebtorAcct]
	to, ok2 := s.balances[m.CreditorAcct]
	if !ok1 || !ok2 {
		s.mu.Unlock()
		status.TxSts, status.Reason = iso20022.StatusRejected, iso20022.ReasonClosedAccount
		return status
	}
	if from.Cmp(units) < 0 {
		s.mu.Unlock()
		status.TxSts, status.Reason = iso20022.StatusRejected, iso20022.ReasonInsufficient
		return status
	}
	from.Sub(from, units)
	to.Add(to, units)
	s.entries[m.DebtorAcct]++
	s.entries[m.CreditorAcct]++
	date := s.sttlmDate(now)
	debit := s.notification(m.DebtorAcct, "DBIT", m, units, m.Creditor, date)
	credit := s.notification(m.CreditorAcct, "CRDT", m, units, m.Debtor, date)
	dsubs := append([]func(iso20022.Advice){}, s.subs[m.DebtorAcct]...)
	csubs := append([]func(iso20022.Advice){}, s.subs[m.CreditorAcct]...)
	s.mu.Unlock()

	for _, fn := range dsubs {
		fn(debit)
	}
	for _, fn := range csubs {
		fn(credit)
	}
	status.TxSts = iso20022.StatusAccepted
	return status
}

// PayInterest credits interest on reserve balances to an account. The Fed
// books it as an accounting entry, not a Fedwire transfer, so it arrives
// whether or not Fedwire is open, flagged with purpose INTR.
func (s *Service) PayInterest(account string, units *big.Int) iso20022.Advice {
	now := s.clock.Now()
	s.mu.Lock()
	s.balances[account].Add(s.balances[account], units)
	s.entries[account]++
	amt, _ := iso20022.USD(units)
	n := iso20022.Advice{
		GrpHdr: iso20022.GroupHeader{MsgID: s.nextID("NTF"), CreDtTm: now}, Account: account,
		CdtDbtInd: "CRDT", Amount: amt, BookingDate: now.In(s.cal.Loc).Format("2006-01-02"),
		UETR: NewUETR(), Purpose: "INTR", Units: new(big.Int).Set(units),
	}
	subs := append([]func(iso20022.Advice){}, s.subs[account]...)
	s.mu.Unlock()
	for _, fn := range subs {
		fn(n)
	}
	return n
}

// Statement returns the camt.052 account report for an account as of now.
func (s *Service) Statement(account string) iso20022.Camt052 {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	bal := new(big.Int).Set(s.balances[account])
	amt, _ := iso20022.USD(bal)
	id := s.nextID("STM")
	return iso20022.Camt052{
		GrpHdr:     iso20022.GroupHeader{MsgID: id, CreDtTm: now},
		StmtID:     fmt.Sprintf("%s-%s-%s", account, now.In(s.cal.Loc).Format("20060102"), id),
		Account:    account,
		ClosingBal: amt,
		Entries:    s.entries[account],
		Units:      bal,
	}
}

func (s *Service) notification(acct, ind string, m iso20022.Pacs009, units *big.Int, cp iso20022.Agent, date string) iso20022.Advice {
	return iso20022.Advice{
		GrpHdr:      iso20022.GroupHeader{MsgID: s.nextID("NTF"), CreDtTm: s.clock.Now()},
		Account:     acct,
		CdtDbtInd:   ind,
		Amount:      m.Amount,
		BookingDate: date,
		UETR:        m.UETR,
		EndToEndID:  m.EndToEndID,
		LclInstrm:   m.LclInstrm,
		Counterpart: cp,
		Purpose:     m.Purpose,
		Units:       new(big.Int).Set(units),
	}
}

// sttlmDate is the business day a transfer settles on: after 9 p.m. ET it
// belongs to the next calendar day.
func (s *Service) sttlmDate(t time.Time) string {
	et := t.In(s.cal.Loc)
	if et.Hour() >= 21 {
		et = et.AddDate(0, 0, 1)
	}
	return et.Format("2006-01-02")
}

func (s *Service) nextID(prefix string) string {
	return fmt.Sprintf("%s%08d", prefix, s.seq.Add(1))
}
