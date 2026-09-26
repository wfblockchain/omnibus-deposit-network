package workflow

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"omnibus-deposit-network/internal/bank"
	"omnibus-deposit-network/internal/chain"
	"omnibus-deposit-network/internal/fedwire"
	"omnibus-deposit-network/internal/iso20022"
	"omnibus-deposit-network/internal/omnibus"
	"omnibus-deposit-network/internal/operator"
)

// RTPLimit is the RTP network's per-payment limit since 9 Feb 2025.
var RTPLimit = iso20022.Dollars(10_000_000)

// BankNode is one member bank's systems as the workflow uses them.
type BankNode struct {
	G  *bank.Gateway
	IB *bank.Interbank

	inboundCursor uint64
	redeemed      map[[32]byte]bool

	// Watermarks on mint capacity, as RTP participants set them on their
	// prefunded position: below Low an alert opens; at Normal it closes.
	Low, Normal *big.Int
	lowAlert    *Alert
}

func NewBankNode(g *bank.Gateway, ib *bank.Interbank) *BankNode {
	return &BankNode{G: g, IB: ib, redeemed: map[[32]byte]bool{}}
}

// Service is the workflow engine. Every mutation happens under one lock,
// because each one may send chain transactions from shared keys.
type Service struct {
	mu      sync.Mutex
	Net     *omnibus.Network
	Fed     *fedwire.Service
	Clock   *fedwire.ManualClock
	Funding *operator.FundingService
	Netter  *operator.NettingService
	Banks   map[string]*BankNode
	log     operator.Logger

	orgs      map[string]*Org
	users     map[string]Principal
	byID      map[string]Principal
	orders    map[string]*Order
	orderIDs  []string
	byKey     map[string]string
	byE2E     map[string]string
	alerts    []*Alert
	defunds   map[string]*DefundRequest
	defundIDs []string
	cycles    []operator.CycleReport
	recons    []operator.ReconReport
	seq       int
	defundSeq int

	// NettingEvery schedules the operator's netting cycles on the scenario clock (on
	// the hour by default); zero leaves them to operator operations.
	NettingEvery time.Duration
	nextCycle    time.Time

	// OnChange, when set, hears every change of an order's status or ISO
	// status (for client notifications). It runs under the service lock
	// and must not call back into the service.
	OnChange func(Order, Event)
}

func New(net *omnibus.Network, fed *fedwire.Service, clock *fedwire.ManualClock, funding *operator.FundingService,
	netter *operator.NettingService, banks map[string]*BankNode, log operator.Logger) *Service {
	return &Service{
		Net: net, Fed: fed, Clock: clock, Funding: funding, Netter: netter, Banks: banks, log: log,
		orgs: map[string]*Org{}, users: map[string]Principal{}, byID: map[string]Principal{},
		orders: map[string]*Order{}, byKey: map[string]string{}, byE2E: map[string]string{}, defunds: map[string]*DefundRequest{},
	}
}

// AddOrg registers a corporate client.
func (s *Service) AddOrg(o Org) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := o
	s.orgs[o.ID] = &cp
}

// AddUser registers a person with a bearer token.
func (s *Service) AddUser(p Principal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[p.Token] = p
	s.byID[p.ID] = p
}

// Authenticate resolves a bearer token.
func (s *Service) Authenticate(token string) (Principal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.users[token]
	return p, ok
}

// Users lists everyone, for the demo sign-in page.
func (s *Service) Users() []Principal {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Principal, 0, len(s.users))
	for _, p := range s.users {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bank != out[j].Bank {
			return out[i].Bank < out[j].Bank
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// OrgOf returns a principal's organization.
func (s *Service) OrgOf(p Principal) (Org, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orgs[p.Org]
	if !ok {
		return Org{}, false
	}
	return *o, true
}

// Now is the scenario clock.
func (s *Service) Now() time.Time { return s.Clock.Now() }

func (s *Service) et(t time.Time) time.Time { return t.In(s.Fed.Calendar().Loc) }

/*─────────────────────────────── corporate ───────────────────────────────*/

// PaymentRequest is what a corporate submits (the fields of a pain.001).
type PaymentRequest struct {
	DebtorAccount string   `json:"debtorAccount"`
	Creditor      Party    `json:"creditor"`
	Amount        string   `json:"amount"` // dollars, e.g. "25000000.00"
	Priority      Priority `json:"priority"`
	Remittance    string   `json:"remittance"`
	EndToEndID    string   `json:"endToEndId"` // optional; defaults to the order id
}

// ParseUSD reads a dollar amount with at most two decimals.
func ParseUSD(v string) (*big.Int, error) {
	v = strings.ReplaceAll(strings.TrimSpace(v), ",", "")
	if v == "" {
		return nil, fmt.Errorf("%w: amount is required", ErrInvalid)
	}
	whole, frac, _ := strings.Cut(v, ".")
	if len(frac) > 2 {
		return nil, fmt.Errorf("%w: amount has more than two decimals", ErrInvalid)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	cents, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok || cents.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount must be a positive number of dollars", ErrInvalid)
	}
	return cents.Mul(cents, big.NewInt(10_000)), nil
}

// Submit records a new payment order from a maker. With an idempotency key
// already used by the same organization, it returns the earlier order if
// the request is the same and a conflict if it is not.
func (s *Service) Submit(ctx context.Context, p Principal, req PaymentRequest, key string) (Order, bool, error) {
	o, created, err := s.submit(p, req, key)
	return o, created, err
}

func (s *Service) submit(p Principal, req PaymentRequest, key string) (Order, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role != RoleMaker {
		return Order{}, false, fmt.Errorf("%w: only a maker creates payment orders", ErrForbidden)
	}
	org, ok := s.orgs[p.Org]
	if !ok {
		return Order{}, false, fmt.Errorf("%w: no organization", ErrForbidden)
	}
	if req.DebtorAccount == "" {
		req.DebtorAccount = org.DDA
	}
	if req.DebtorAccount != org.DDA {
		return Order{}, false, fmt.Errorf("%w: account %s does not belong to %s", ErrForbidden, req.DebtorAccount, org.Name)
	}
	amount, err := ParseUSD(req.Amount)
	if err != nil {
		return Order{}, false, err
	}
	if req.Priority == "" {
		req.Priority = Normal
	}
	if req.Priority != Urgent && req.Priority != Normal {
		return Order{}, false, fmt.Errorf("%w: priority must be URGENT or NORMAL", ErrInvalid)
	}
	if req.Creditor.Name == "" || req.Creditor.ABA == "" || req.Creditor.Account == "" {
		return Order{}, false, fmt.Errorf("%w: creditor name, routing number and account are required", ErrInvalid)
	}
	if _, member := s.Net.BankByABA(req.Creditor.ABA); !member {
		return Order{}, false, fmt.Errorf("%w: routing number %s is not a network member", ErrInvalid, req.Creditor.ABA)
	}
	if key != "" {
		if id, seen := s.byKey[org.ID+"/"+key]; seen {
			o := s.orders[id]
			if o.Amount.Cmp(amount) == 0 && o.Creditor == req.Creditor && o.Priority == req.Priority &&
				(req.EndToEndID == "" || req.EndToEndID == o.EndToEndID) {
				return s.viewFor(p, o), false, nil
			}
			return Order{}, false, fmt.Errorf("%w: idempotency key %q was used for a different payment", ErrInvalid, key)
		}
	}
	if len(req.EndToEndID) > 35 {
		return Order{}, false, fmt.Errorf("%w: endToEndId is at most 35 characters", ErrInvalid)
	}
	if req.EndToEndID != "" {
		if id, seen := s.byE2E[org.ID+"/"+req.EndToEndID]; seen {
			return Order{}, false, fmt.Errorf("%w: DU04 duplicate: endToEndId %q is already used by %s", ErrConflict, req.EndToEndID, id)
		}
	}
	if org.PerTxLimit != nil && amount.Cmp(org.PerTxLimit) > 0 {
		return Order{}, false, fmt.Errorf("%w: $%s exceeds the per-payment limit of $%s", ErrInvalid,
			iso20022.Readable(amount), iso20022.Readable(org.PerTxLimit))
	}
	if org.DailyLimit != nil {
		used := s.usedToday(org.ID)
		if new(big.Int).Add(used, amount).Cmp(org.DailyLimit) > 0 {
			return Order{}, false, fmt.Errorf("%w: $%s would exceed today's limit of $%s ($%s already used)", ErrInvalid,
				iso20022.Readable(amount), iso20022.Readable(org.DailyLimit), iso20022.Readable(used))
		}
	}

	s.seq++
	now := s.Now()
	o := &Order{
		ID: fmt.Sprintf("PO-%06d", s.seq), IdempotencyKey: key, EndToEndID: req.EndToEndID, UETR: uuid4(),
		Org: org.ID, Bank: org.Bank, DebtorAccount: req.DebtorAccount, Creditor: req.Creditor, Amount: amount,
		Priority: req.Priority, Remittance: req.Remittance, CreatedBy: p.ID, CreatedAt: now, Approvals: []string{},
	}
	if o.EndToEndID == "" {
		o.EndToEndID = o.ID
	}
	o.PayeeCheck = s.verifyPayee(req.Creditor)
	s.event(o, p.Name, StatusAwaitingApproval, ISOReceived, "received from "+p.Name)
	need := s.needed(o, org)
	check := fmt.Sprintf("payee check: account %s, name %s", o.PayeeCheck.Account, o.PayeeCheck.Name)
	if o.PayeeCheck.Registered != "" {
		check += " (registered as " + o.PayeeCheck.Registered + ")"
	}
	if !o.PayeeCheck.Passed() {
		check += "; one more approval is needed to override"
	}
	s.event(o, "system", StatusAwaitingApproval, ISOAcceptedTechnical,
		fmt.Sprintf("validated against limits; %s; needs %d approval%s from someone other than the maker", check, need, plural(need)))
	s.orders[o.ID] = o
	s.orderIDs = append(s.orderIDs, o.ID)
	if key != "" {
		s.byKey[org.ID+"/"+key] = o.ID
	}
	s.byE2E[org.ID+"/"+o.EndToEndID] = o.ID
	return s.viewFor(p, o), true, nil
}

func (s *Service) usedToday(org string) *big.Int {
	day := s.et(s.Now()).Format("2006-01-02")
	sum := big.NewInt(0)
	for _, id := range s.orderIDs {
		o := s.orders[id]
		if o.Org == org && s.et(o.CreatedAt).Format("2006-01-02") == day && o.Status != StatusRejected && o.Status != StatusCancelled && o.Status != StatusExpired {
			sum.Add(sum, o.Amount)
		}
	}
	return sum
}

func (s *Service) needed(o *Order, org *Org) int {
	n := 1
	if org.SecondApprovalAbove != nil && o.Amount.Cmp(org.SecondApprovalAbove) > 0 {
		n = 2
	}
	if !o.PayeeCheck.Passed() {
		n++ // someone else must override a failed payee check
	}
	return n
}

// VerifyPayee asks the receiving bank, through the network, whether an
// account is open and whether the name matches its records.
func (s *Service) VerifyPayee(p Principal, payee Party) (PayeeCheck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Org == "" {
		return PayeeCheck{}, ErrForbidden
	}
	if _, member := s.Net.BankByABA(payee.ABA); !member {
		return PayeeCheck{}, fmt.Errorf("%w: routing number %s is not a network member", ErrInvalid, payee.ABA)
	}
	return s.verifyPayee(payee), nil
}

func (s *Service) verifyPayee(payee Party) PayeeCheck {
	to, ok := s.Net.BankByABA(payee.ABA)
	if !ok {
		return PayeeCheck{Account: "NOT_FOUND", Name: "NO_MATCH"}
	}
	c := s.Banks[to.Spec.MemberID].G.Customer(payee.Account)
	if c == nil {
		return PayeeCheck{Account: "NOT_FOUND", Name: "NO_MATCH"}
	}
	out := PayeeCheck{Account: "OPEN", Name: NameMatch(payee.Name, c.Name)}
	if c.Closed {
		out.Account = "CLOSED"
	}
	if out.Name == "CLOSE_MATCH" {
		out.Registered = c.Name
	}
	return out
}

// NameMatch compares a typed payee name with the registered one, ignoring
// case, punctuation and legal-form words: MATCH, CLOSE_MATCH (one or two
// typing slips, or most words shared) or NO_MATCH.
func NameMatch(typed, registered string) string {
	a, b := normName(typed), normName(registered)
	switch {
	case a == "" || b == "":
		return "NO_MATCH"
	case a == b:
		return "MATCH"
	case editDistance(a, b) <= 2:
		return "CLOSE_MATCH"
	}
	aw, bw := strings.Fields(a), strings.Fields(b)
	shared := 0
	for _, x := range aw {
		for _, y := range bw {
			if x == y {
				shared++
				break
			}
		}
	}
	if 2*shared >= max(len(aw), len(bw)) && shared > 0 {
		return "CLOSE_MATCH"
	}
	return "NO_MATCH"
}

var legalForms = map[string]bool{"ltd": true, "limited": true, "inc": true, "incorporated": true, "llc": true,
	"corp": true, "corporation": true, "co": true, "company": true, "plc": true, "lp": true, "llp": true, "the": true}

func normName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	var out []string
	for _, w := range strings.Fields(b.String()) {
		if !legalForms[w] {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// Approve records an approver's sign-off. The maker can never approve its
// own order, and an approver counts once. With enough approvals the order
// moves to screening and execution.
func (s *Service) Approve(ctx context.Context, p Principal, id string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.orgOrder(p, id)
	if err != nil {
		return Order{}, err
	}
	if p.Role != RoleApprover {
		return Order{}, fmt.Errorf("%w: only an approver approves", ErrForbidden)
	}
	if o.Status != StatusAwaitingApproval {
		return Order{}, fmt.Errorf("%w: order is %s", ErrConflict, o.Status)
	}
	if p.ID == o.CreatedBy {
		return Order{}, fmt.Errorf("%w: the maker of an order cannot approve it", ErrForbidden)
	}
	for _, a := range o.Approvals {
		if a == p.ID {
			return Order{}, fmt.Errorf("%w: %s already approved this order", ErrConflict, p.Name)
		}
	}
	o.Approvals = append(o.Approvals, p.ID)
	need := s.needed(o, s.orgs[o.Org])
	if len(o.Approvals) < need {
		s.event(o, p.Name, StatusAwaitingApproval, ISOPartlyApproved,
			fmt.Sprintf("approval %d of %d by %s", len(o.Approvals), need, p.Name))
		return s.viewFor(p, o), nil
	}
	s.event(o, p.Name, StatusInProcess, ISOAcceptedCustomer, fmt.Sprintf("approved by %s", p.Name))
	s.advance(ctx, o)
	return s.viewFor(p, o), nil
}

// Decline is an approver refusing an order.
func (s *Service) Decline(ctx context.Context, p Principal, id, reason string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.orgOrder(p, id)
	if err != nil {
		return Order{}, err
	}
	if p.Role != RoleApprover {
		return Order{}, fmt.Errorf("%w: only an approver declines", ErrForbidden)
	}
	if o.Status != StatusAwaitingApproval {
		return Order{}, fmt.Errorf("%w: order is %s", ErrConflict, o.Status)
	}
	s.event(o, p.Name, StatusCancelled, ISOCancelled, "declined by "+p.Name+": "+reason)
	return s.viewFor(p, o), nil
}

// Cancel is the maker withdrawing an order not yet approved.
func (s *Service) Cancel(ctx context.Context, p Principal, id string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.orgOrder(p, id)
	if err != nil {
		return Order{}, err
	}
	switch {
	case o.Status == StatusAwaitingApproval && p.ID == o.CreatedBy:
		s.event(o, p.Name, StatusCancelled, ISOCancelled, "cancelled by the maker")
	case o.Status == StatusAwaitingApproval:
		return Order{}, fmt.Errorf("%w: before approval only the maker cancels; an approver declines", ErrForbidden)
	case o.Status == StatusQueuedForNetting && (p.Role == RoleMaker || p.Role == RoleApprover):
		// Until the operator's cycle takes it, a netted payment can still be pulled;
		// the bank cancels the obligation and re-credits the account.
		node := s.Banks[o.Bank]
		if _, err := s.Net.Netting.Send(ctx, node.G.Bank.Keys.Operator, "cancel", o.obligationID); err != nil {
			return Order{}, fmt.Errorf("%w: the netting cycle has already taken it", ErrConflict)
		}
		_ = node.IB.Sync(ctx)
		s.syncNetted(ctx)
		if o.Status == StatusCancelled {
			o.History[len(o.History)-1].Actor = p.Name
		}
	default:
		return Order{}, fmt.Errorf("%w: order is %s and can no longer be cancelled", ErrConflict, o.Status)
	}
	return s.viewFor(p, o), nil
}

// Orders lists what a principal may see: its organization's orders for a
// corporate user, its bank's clients' orders for bank staff, all for the operator.
func (s *Service) Orders(p Principal) []Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Order{}
	for i := len(s.orderIDs) - 1; i >= 0; i-- {
		o := s.orders[s.orderIDs[i]]
		if s.visible(p, o) {
			out = append(out, s.viewFor(p, o))
		}
	}
	return out
}

// Order returns one order a principal may see.
func (s *Service) Order(p Principal, id string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok || !s.visible(p, o) {
		return Order{}, ErrNotFound
	}
	return s.viewFor(p, o), nil
}

func (s *Service) visible(p Principal, o *Order) bool {
	switch {
	case p.Org != "":
		return o.Org == p.Org
	case p.Role == RoleOperatorOps:
		return true
	default:
		return o.Bank == p.Bank
	}
}

func (s *Service) orgOrder(p Principal, id string) (*Order, error) {
	o, ok := s.orders[id]
	if !ok || p.Org == "" || o.Org != p.Org {
		return nil, ErrNotFound
	}
	return o, nil
}

// AccountView is what a corporate sees: its deposit account.
type AccountView struct {
	Org      string    `json:"org"`
	Account  string    `json:"account"`
	Bank     string    `json:"bank"`
	BankName string    `json:"bankName"`
	Routing  string    `json:"routingNumber"`
	Balance  string    `json:"balance"`
	Currency string    `json:"currency"`
	InFlight string    `json:"inFlight"` // debited for payments not yet settled
	AsOf     time.Time `json:"asOf"`
}

// Account returns a corporate's deposit account.
func (s *Service) Account(ctx context.Context, p Principal) (AccountView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	org, ok := s.orgs[p.Org]
	if !ok {
		return AccountView{}, ErrForbidden
	}
	node := s.Banks[org.Bank]
	inflight := big.NewInt(0)
	if c := node.G.Customer(org.DDA); c != nil {
		if b, err := node.G.Bank.Token.BigInt(ctx, "balanceOf", c.Wallet.Addr); err == nil {
			inflight = b
		}
	}
	return AccountView{
		Org: org.Name, Account: org.DDA, Bank: org.Bank, BankName: node.G.Bank.Spec.Name, Routing: node.G.Bank.Spec.ABA,
		Balance: iso20022.FormatDollars(node.G.Core.Balance(org.DDA)), Currency: "USD",
		InFlight: iso20022.FormatDollars(inflight), AsOf: s.Now(),
	}, nil
}

// StatementLine is one posting (camt.053 entry).
type StatementLine struct {
	At      time.Time `json:"bookingDate"`
	Ref     string    `json:"reference"`
	Credit  bool      `json:"credit"`
	Amount  string    `json:"amount"`
	Balance string    `json:"balance"`
}

// Statement returns the postings on a corporate's account.
func (s *Service) Statement(p Principal) ([]StatementLine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	org, ok := s.orgs[p.Org]
	if !ok {
		return nil, ErrForbidden
	}
	var out []StatementLine
	for _, e := range s.Banks[org.Bank].G.Core.Entries(org.DDA) {
		d := new(big.Int).Abs(e.Delta)
		out = append(out, StatementLine{At: e.At, Ref: e.Ref, Credit: e.Delta.Sign() > 0,
			Amount: iso20022.FormatDollars(d), Balance: iso20022.FormatDollars(e.Balance)})
	}
	return out, nil
}

/*──────────────────────────── screening & routing ────────────────────────*/

// advance takes an approved order through screening and into execution.
func (s *Service) advance(ctx context.Context, o *Order) {
	node := s.Banks[o.Bank]
	if !o.released {
		payee, _ := node.G.Dir.Resolve(o.Creditor.ABA, o.Creditor.Account)
		if hit, why := node.G.Screen.Screen(o.Creditor.Name, payee); hit {
			o.Reason = "SANCTIONS"
			s.event(o, "screening", StatusOnHold, ISOPending, "held for compliance review: "+why)
			return
		}
		s.event(o, "screening", o.Status, o.ISO, "sanctions screening clear")
	}
	s.execute(ctx, o)
}

func (s *Service) execute(ctx context.Context, o *Order) {
	node := s.Banks[o.Bank]
	g := node.G
	switch {
	case o.Creditor.ABA == g.Bank.Spec.ABA:
		o.Route = "book transfer, same bank"
		if !g.Core.Has(o.Creditor.Account) {
			s.reject(o, "AC04", "no such account at "+g.Bank.Spec.Name)
			return
		}
		if err := g.Core.Post(o.DebtorAccount, new(big.Int).Neg(o.Amount), o.ID+"-DR"); err != nil {
			s.reject(o, "AM04", "insufficient funds")
			return
		}
		_ = g.Core.Post(o.Creditor.Account, o.Amount, o.ID+"-CR")
		s.settle(o, "booked between two accounts at "+g.Bank.Spec.Name)
		s.credited(o, g.Bank.Spec.Name)

	case o.Priority == Normal:
		o.Route = "network netting cycle"
		res, err := node.IB.SendFromDepositOpts(ctx, o.DebtorAccount, o.Creditor.Name, o.Creditor.ABA, o.Creditor.Account, o.Amount, false, false)
		if err != nil {
			s.failed(o, err)
			return
		}
		o.obligationID = res.PaymentID
		o.RouteNote = "funded from the deposit account; settles with every other obligation in the operator's next netting cycle, using only net liquidity"
		s.event(o, "system", StatusQueuedForNetting, ISOInProcess, "account debited; obligation queued for the operator's next netting cycle")

	default:
		o.Route = "on-network instant, 24x7"
		o.RouteNote = s.whyTokens(o)
		s.executeUrgent(ctx, o)
	}
}

func (s *Service) executeUrgent(ctx context.Context, o *Order) {
	node := s.Banks[o.Bank]
	g := node.G
	if !o.tokenized {
		capacity, err := s.Net.MintCapacity(ctx, o.Bank)
		if err != nil {
			s.failed(o, err)
			return
		}
		if capacity.Cmp(o.Amount) < 0 {
			o.Reason = "LIQUIDITY"
			msg := fmt.Sprintf("%s needs $%s of free omnibus position for %s; it has $%s. Fund the omnibus (%s).",
				g.Bank.Spec.Name, iso20022.Readable(o.Amount), o.ID, iso20022.Readable(capacity), s.instrument())
			if o.Status != StatusAwaitingLiquidity {
				s.eventDetail(o, "system", StatusAwaitingLiquidity, ISOPending, "waiting for "+g.Bank.Spec.Name+" to make liquidity available; the payment goes as soon as it is",
					fmt.Sprintf("mint capacity $%s is short of $%s; alert raised to treasury", iso20022.Readable(capacity), iso20022.Readable(o.Amount)))
				s.alerts = append(s.alerts, &Alert{At: s.Now(), Bank: o.Bank, Kind: "LIQUIDITY", Message: msg, Open: true})
			}
			return
		}
		if err := g.Tokenize(ctx, o.DebtorAccount, o.Amount); err != nil {
			if strings.Contains(err.Error(), "insufficient funds") {
				s.reject(o, "AM04", "insufficient funds")
				return
			}
			s.failed(o, err)
			return
		}
		o.tokenized = true
		o.Reason = ""
		s.eventDetail(o, "system", StatusInProcess, ISOInProcess, fmt.Sprintf("account debited $%s; payment released to the network", iso20022.Readable(o.Amount)),
			fmt.Sprintf("$%s tokenized as %s against %s's omnibus position", iso20022.Readable(o.Amount), g.Bank.Spec.Ticker, g.Bank.Spec.Name))
	}
	res, err := g.SendPaymentOpts(ctx, o.DebtorAccount, o.Creditor.Name, o.Creditor.ABA, o.Creditor.Account, o.Amount, o.Remittance, false, o.UETR)
	if err != nil {
		s.refundTokens(ctx, o)
		s.failed(o, err)
		return
	}
	o.paymentID = res.PaymentID
	switch res.Status.TxSts {
	case iso20022.StatusRejected:
		s.refundTokens(ctx, o)
		s.reject(o, res.Status.Reason, "rejected before settlement")
	case iso20022.StatusAccepted:
		o.tokenized = false
		s.settle(o, "settled on the network")
	default:
		to, _ := s.Net.BankByABA(o.Creditor.ABA)
		s.event(o, "system", StatusPendingReceiver, ISOPending, "on the network, held against the payer; waiting for "+to.Spec.Name+" to screen and accept")
	}
}

// whyTokens explains, in the payer's terms, what today's rails could not do.
func (s *Service) whyTokens(o *Order) string {
	now := s.Now()
	cal := s.Fed.Calendar()
	var why []string
	if !cal.IsOpen(now) {
		why = append(why, "Fedwire is closed until "+s.et(cal.NextOpen(now)).Format("Mon 2 Jan 15:04 MST"))
	}
	if o.Amount.Cmp(RTPLimit) > 0 {
		why = append(why, "the amount is above RTP's $10,000,000.00 per-payment limit")
	}
	if len(why) == 0 {
		return "instant, final settlement on the network"
	}
	return strings.Join(why, " and ") + "; settled instantly on the network instead"
}

func (s *Service) instrument() string {
	if s.Fed.Calendar().IsOpen(s.Now()) {
		return "Fedwire BTRC is open"
	}
	return "Fedwire is closed; FedNow LMT can fund it now"
}

func (s *Service) refundTokens(ctx context.Context, o *Order) {
	if !o.tokenized {
		return
	}
	g := s.Banks[o.Bank].G
	if err := g.Redeem(ctx, o.DebtorAccount, o.Amount); err != nil {
		s.event(o, "system", o.Status, o.ISO, "refund failed, operations must repair: "+err.Error())
		return
	}
	if err := g.SyncRedemptions(ctx); err != nil {
		s.event(o, "system", o.Status, o.ISO, "refund posting failed: "+err.Error())
		return
	}
	o.tokenized = false
	s.eventDetail(o, "system", o.Status, o.ISO, "account re-credited $"+iso20022.Readable(o.Amount), g.Bank.Spec.Ticker+" redeemed back into the deposit")
}

func (s *Service) settle(o *Order, note string) {
	o.SettledAt = s.Now()
	o.Reason = ""
	s.eventDetail(o, "system", StatusSettled, ISOSettled, note, s.settleDetail(o))
}

// settleDetail is what moved on the ledger, for staff.
func (s *Service) settleDetail(o *Order) string {
	from, to := s.Banks[o.Bank].G.Bank, (*omnibus.Bank)(nil)
	if b, ok := s.Net.BankByABA(o.Creditor.ABA); ok {
		to = b
	}
	switch {
	case to == nil || to.Spec.MemberID == from.Spec.MemberID:
		return ""
	case o.paymentID != [32]byte{}:
		return fmt.Sprintf("%s burned; $%s of backing moved %s → %s on the operator's ledger; %s minted to %s",
			from.Spec.Ticker, iso20022.Readable(o.Amount), from.Spec.Name, to.Spec.Name, to.Spec.Ticker, to.Spec.Name)
	default:
		return fmt.Sprintf("obligation discharged in a verified netting cycle; free position moved %s → %s net of the cycle", from.Spec.Name, to.Spec.Name)
	}
}

// credited records the receiving bank's credit to the payee (ACCC).
func (s *Service) credited(o *Order, bankName string) {
	detail := ""
	if b, ok := s.Net.BankByABA(o.Creditor.ABA); ok && o.paymentID != [32]byte{} {
		detail = b.Spec.Ticker + " redeemed into the payee's deposit account; the backing is " + b.Spec.Name + "'s free position again"
	}
	s.eventDetail(o, bankName, StatusSettled, ISOCredited, "credited to "+o.Creditor.Name+"'s account "+o.Creditor.Account, detail)
}

// syncCredited confirms, for orders settled between the banks, that the
// receiving bank has posted the credit to the payee's deposit account.
func (s *Service) syncCredited() {
	for _, id := range s.orderIDs {
		o := s.orders[id]
		if o.Status != StatusSettled || o.ISO != ISOSettled {
			continue
		}
		to, ok := s.Net.BankByABA(o.Creditor.ABA)
		if !ok {
			continue
		}
		node := s.Banks[to.Spec.MemberID]
		switch {
		case o.paymentID != [32]byte{} && node.redeemed[o.paymentID]:
			s.credited(o, to.Spec.Name)
		case o.obligationID != [32]byte{} && node.IB.Credited(o.obligationID):
			s.credited(o, to.Spec.Name)
		}
	}
}

func (s *Service) reject(o *Order, reason, note string) {
	o.Reason = reason
	s.event(o, "system", StatusRejected, ISORejected, fmt.Sprintf("%s (%s)", note, reason))
}

func (s *Service) failed(o *Order, err error) {
	s.reject(o, "NARR", "processing error, operations notified: "+err.Error())
}

func (s *Service) event(o *Order, actor string, st Status, iso, note string) {
	s.eventDetail(o, actor, st, iso, note, "")
}

// eventDetail records an event whose detail (tickers, positions, liquidity
// operations) only bank and operator staff see.
func (s *Service) eventDetail(o *Order, actor string, st Status, iso, note, detail string) {
	changed := st != o.Status || iso != o.ISO
	o.Status, o.ISO = st, iso
	ev := Event{At: s.Now(), Actor: actor, Status: st, ISO: iso, Note: note, Detail: detail}
	o.History = append(o.History, ev)
	if changed && s.OnChange != nil {
		cp := clientView(snapshot(o))
		ev.Detail = ""
		s.OnChange(cp, ev)
	}
}

/*─────────────────────────────── compliance ─────────────────────────────*/

// Holds lists the orders held for a bank's compliance review.
func (s *Service) Holds(p Principal) ([]Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role != RoleCompliance {
		return nil, ErrForbidden
	}
	out := []Order{}
	for _, id := range s.orderIDs {
		o := s.orders[id]
		if o.Bank == p.Bank && o.Status == StatusOnHold {
			out = append(out, snapshot(o))
		}
	}
	return out, nil
}

// Release clears a screening hold after review; the order executes.
func (s *Service) Release(ctx context.Context, p Principal, id, note string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.heldOrder(p, id)
	if err != nil {
		return Order{}, err
	}
	o.released = true
	o.Reason = ""
	s.event(o, p.Name, StatusInProcess, ISOPending, "released after review: "+note)
	s.execute(ctx, o)
	return snapshot(o), nil
}

// BlockedAccount is where a bank holds funds blocked under OFAC rules.
const BlockedAccount = "OFAC-BLOCKED"

// Block handles a confirmed match in which a sanctioned party has an
// interest in the funds. OFAC requires the bank to block such funds, not
// return them: the amount leaves the client's account for a blocked,
// interest-bearing account from which only OFAC-authorized debits are made,
// and the bank reports it within 10 business days (31 CFR 501.603).
func (s *Service) Block(ctx context.Context, p Principal, id, note string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.heldOrder(p, id)
	if err != nil {
		return Order{}, err
	}
	g := s.Banks[o.Bank].G
	if err := g.Core.Post(o.DebtorAccount, new(big.Int).Neg(o.Amount), o.ID+"-BLK"); err != nil {
		return Order{}, fmt.Errorf("%w: the account cannot fund the payment, so there is nothing to block; reject it instead", ErrConflict)
	}
	if !g.Core.Has(BlockedAccount) {
		g.Core.Open(BlockedAccount, big.NewInt(0))
	}
	_ = g.Core.Post(BlockedAccount, o.Amount, o.ID+"-BLK")
	o.Reason = "RR04"
	o.OFACReportDue = s.businessDaysAfter(s.Now(), 10)
	s.event(o, p.Name, StatusBlocked, ISOBlocked, fmt.Sprintf("blocked after sanctions review (%s): $%s moved to %s's blocked account; report to OFAC due %s",
		note, iso20022.Readable(o.Amount), g.Bank.Spec.Name, o.OFACReportDue.Format("Mon 2 Jan 2006")))
	return snapshot(o), nil
}

// RejectHold handles a prohibited payment in which no sanctioned party has
// an interest: nothing moves, and the rejected transaction is reported
// within 10 business days (31 CFR 501.604).
func (s *Service) RejectHold(ctx context.Context, p Principal, id, note string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.heldOrder(p, id)
	if err != nil {
		return Order{}, err
	}
	o.OFACReportDue = s.businessDaysAfter(s.Now(), 10)
	s.reject(o, "RR04", fmt.Sprintf("rejected by %s after sanctions review (%s); report to OFAC due %s",
		p.Name, note, o.OFACReportDue.Format("Mon 2 Jan 2006")))
	return snapshot(o), nil
}

// SanctionsCases lists a bank's blocked and rejected sanctions cases with
// their OFAC reporting deadlines.
func (s *Service) SanctionsCases(p Principal) ([]Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role != RoleCompliance {
		return nil, ErrForbidden
	}
	out := []Order{}
	for _, id := range s.orderIDs {
		if o := s.orders[id]; o.Bank == p.Bank && !o.OFACReportDue.IsZero() {
			out = append(out, snapshot(o))
		}
	}
	return out, nil
}

// businessDaysAfter counts n business days after t, skipping weekends and
// Federal Reserve holidays, and returns that date in ET.
func (s *Service) businessDaysAfter(t time.Time, n int) time.Time {
	cal := s.Fed.Calendar()
	d := s.et(t)
	d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, cal.Loc)
	for n > 0 {
		d = d.AddDate(0, 0, 1)
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday || cal.Holidays[d.Format("2006-01-02")] {
			continue
		}
		n--
	}
	return d
}

func (s *Service) heldOrder(p Principal, id string) (*Order, error) {
	if p.Role != RoleCompliance {
		return nil, ErrForbidden
	}
	o, ok := s.orders[id]
	if !ok || o.Bank != p.Bank {
		return nil, ErrNotFound
	}
	if o.Status != StatusOnHold {
		return nil, fmt.Errorf("%w: order is %s", ErrConflict, o.Status)
	}
	return o, nil
}

/*──────────────────────────────── treasury ──────────────────────────────*/

// LiquidityView is a bank treasury's view of its omnibus position.
type LiquidityView struct {
	Bank         string          `json:"bank"`
	BankName     string          `json:"bankName"`
	Ticker       string          `json:"ticker"`
	Position     string          `json:"position"`
	Backing      string          `json:"backing"`
	Pending      string          `json:"pendingDefund"`
	Free         string          `json:"free"`
	MintCapacity string          `json:"mintCapacity"`
	FedwireOpen  bool            `json:"fedwireOpen"`
	NextOpen     time.Time       `json:"nextFedwireOpen"`
	Instrument   string          `json:"fundingInstrument"`
	Waiting      []Order         `json:"awaitingLiquidity"`
	Alerts       []Alert         `json:"alerts"`
	Defunds      []DefundRequest `json:"defunds"`
}

type memberView struct {
	Token              common.Address
	Operator           common.Address
	Approver           common.Address
	Admitted           bool
	Suspended          bool
	RequiresAcceptance bool
	Position           *big.Int
	Backing            *big.Int
	PendingDefund      *big.Int
	PositionSeconds    *big.Int
	LastTouch          uint64
	IssuanceCap        *big.Int
	PrefundRequirement *big.Int
	RemoteSupply       *big.Int
}

func (s *Service) member(ctx context.Context, id string) (memberView, error) {
	out, err := s.Net.Ledger.Call(ctx, "member", chain.Bytes32(id))
	if err != nil {
		return memberView{}, err
	}
	return *abi.ConvertType(out[0], new(memberView)).(*memberView), nil
}

func isBankStaff(p Principal) bool {
	return p.Org == "" && (p.Role == RoleTreasury || p.Role == RoleTreasuryApprover || p.Role == RoleCompliance)
}

// Liquidity returns the bank's omnibus position and what waits on it.
func (s *Service) Liquidity(ctx context.Context, p Principal) (LiquidityView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !isBankStaff(p) {
		return LiquidityView{}, ErrForbidden
	}
	node := s.Banks[p.Bank]
	m, err := s.member(ctx, p.Bank)
	if err != nil {
		return LiquidityView{}, err
	}
	capacity, err := s.Net.MintCapacity(ctx, p.Bank)
	if err != nil {
		return LiquidityView{}, err
	}
	free, err := s.Net.Ledger.BigInt(ctx, "freePosition", chain.Bytes32(p.Bank))
	if err != nil {
		return LiquidityView{}, err
	}
	v := LiquidityView{
		Bank: p.Bank, BankName: node.G.Bank.Spec.Name, Ticker: node.G.Bank.Spec.Ticker,
		Position: iso20022.FormatDollars(m.Position), Backing: iso20022.FormatDollars(m.Backing),
		Pending: iso20022.FormatDollars(m.PendingDefund), Free: iso20022.FormatDollars(free),
		MintCapacity: iso20022.FormatDollars(capacity), FedwireOpen: s.Fed.Calendar().IsOpen(s.Now()),
		NextOpen: s.et(s.Fed.Calendar().NextOpen(s.Now())), Instrument: s.instrument(),
		Waiting: []Order{}, Alerts: []Alert{}, Defunds: []DefundRequest{},
	}
	for _, id := range s.orderIDs {
		if o := s.orders[id]; o.Bank == p.Bank && o.Status == StatusAwaitingLiquidity {
			v.Waiting = append(v.Waiting, snapshot(o))
		}
	}
	for i := len(s.alerts) - 1; i >= 0; i-- {
		if s.alerts[i].Bank == p.Bank {
			v.Alerts = append(v.Alerts, *s.alerts[i])
		}
	}
	for i := len(s.defundIDs) - 1; i >= 0; i-- {
		if d := s.defunds[s.defundIDs[i]]; d.Bank == p.Bank {
			v.Defunds = append(v.Defunds, *d)
		}
	}
	return v, nil
}

// FundResult is the Fed's answer to a funding transfer.
type FundResult struct {
	Instrument string `json:"instrument"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Released   int    `json:"ordersReleased"`
}

// Fund sends reserves into the omnibus: Fedwire while it is open, FedNow
// LMT while it is closed. Orders waiting for liquidity then proceed.
func (s *Service) Fund(ctx context.Context, p Principal, amount string) (FundResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role != RoleTreasury {
		return FundResult{}, fmt.Errorf("%w: only bank treasury funds the omnibus", ErrForbidden)
	}
	units, err := ParseUSD(amount)
	if err != nil {
		return FundResult{}, err
	}
	node := s.Banks[p.Bank]
	instrument := "BTRC"
	if !s.Fed.Calendar().IsOpen(s.Now()) {
		instrument = "LMT1"
	}
	st := node.G.FundOmnibus(units)
	res := FundResult{Instrument: instrument, Status: st.TxSts, Reason: st.Reason}
	if st.TxSts != iso20022.StatusAccepted {
		return res, nil
	}
	for _, id := range s.orderIDs {
		if o := s.orders[id]; o.Bank == p.Bank && o.Status == StatusAwaitingLiquidity {
			s.eventDetail(o, s.Banks[p.Bank].G.Bank.Spec.Name, StatusAwaitingLiquidity, ISOPending, "liquidity available",
				fmt.Sprintf("%s funded the omnibus with $%s by %s", p.Name, iso20022.Readable(units), instrument))
		}
	}
	res.Released = s.retryLiquidity(ctx, p.Bank)
	return res, nil
}

func (s *Service) retryLiquidity(ctx context.Context, bankID string) int {
	n := 0
	for _, id := range s.orderIDs {
		o := s.orders[id]
		if o.Bank != bankID || o.Status != StatusAwaitingLiquidity {
			continue
		}
		s.executeUrgent(ctx, o)
		if o.Status != StatusAwaitingLiquidity {
			n++
		}
	}
	waiting := false
	for _, id := range s.orderIDs {
		if o := s.orders[id]; o.Bank == bankID && o.Status == StatusAwaitingLiquidity {
			waiting = true
		}
	}
	if !waiting {
		for _, a := range s.alerts {
			if a.Bank == bankID && a.Kind == "LIQUIDITY" {
				a.Open = false
			}
		}
	}
	return n
}

// RequestDefund is the maker half of taking free position back.
func (s *Service) RequestDefund(ctx context.Context, p Principal, amount string) (DefundRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role != RoleTreasury {
		return DefundRequest{}, ErrForbidden
	}
	units, err := ParseUSD(amount)
	if err != nil {
		return DefundRequest{}, err
	}
	lid, err := s.Banks[p.Bank].G.RequestDefundOnly(ctx, units)
	if err != nil {
		return DefundRequest{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	s.defundSeq++
	d := &DefundRequest{ID: fmt.Sprintf("DF-%04d", s.defundSeq), Bank: p.Bank, AmountUSD: iso20022.FormatDollars(units),
		RequestedBy: p.Name, Status: "AWAITING_APPROVAL", At: s.Now(), ledgerID: lid}
	s.defunds[d.ID] = d
	s.defundIDs = append(s.defundIDs, d.ID)
	return *d, nil
}

// ApproveDefund is the checker half; the operator then sends it over Fedwire, now
// if Fedwire is open, at the next opening if not.
func (s *Service) ApproveDefund(ctx context.Context, p Principal, id string) (DefundRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role != RoleTreasuryApprover {
		return DefundRequest{}, ErrForbidden
	}
	d, ok := s.defunds[id]
	if !ok || d.Bank != p.Bank {
		return DefundRequest{}, ErrNotFound
	}
	if d.Status != "AWAITING_APPROVAL" {
		return DefundRequest{}, fmt.Errorf("%w: defund is %s", ErrConflict, d.Status)
	}
	if d.RequestedBy == p.Name {
		return DefundRequest{}, fmt.Errorf("%w: the requester cannot approve", ErrForbidden)
	}
	if err := s.Banks[p.Bank].G.ApproveDefund(ctx, d.ledgerID); err != nil {
		return DefundRequest{}, err
	}
	d.ApprovedBy = p.Name
	d.Status = "APPROVED"
	if err := s.Funding.Sync(ctx); err == nil {
		s.Funding.Process(ctx)
	}
	s.updateDefunds(ctx)
	return *d, nil
}

func (s *Service) updateDefunds(ctx context.Context) {
	for _, id := range s.defundIDs {
		d := s.defunds[id]
		if d.Status != "APPROVED" {
			continue
		}
		out, err := s.Net.Ledger.Call(ctx, "defunds", d.ledgerID)
		if err != nil {
			continue
		}
		switch out[2].(uint8) {
		case 3:
			d.Status = "COMPLETED"
		case 4:
			d.Status = "FAILED"
		}
	}
}

/*─────────────────────────────── operator ops ────────────────────────────────*/

// MemberLine is one bank in the operator's network view.
type MemberLine struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Ticker   string `json:"ticker"`
	Position string `json:"position"`
	Backing  string `json:"backing"`
	Free     string `json:"free"`
}

// NetworkView is operator operations' console.
type NetworkView struct {
	Now         time.Time              `json:"now"`
	FedwireOpen bool                   `json:"fedwireOpen"`
	NextOpen    time.Time              `json:"nextFedwireOpen"`
	FedJoint    string                 `json:"fedJointAccount"`
	LedgerTotal string                 `json:"ledgerTotal"`
	Break       bool                   `json:"reconciliationBreak"`
	Invariants  bool                   `json:"invariantsHold"`
	Members     []MemberLine           `json:"members"`
	Queued      int                    `json:"obligationsQueued"`
	NextCycle   time.Time              `json:"nextScheduledCycle,omitzero"`
	Cycles      []operator.CycleReport `json:"cycles"`
	Recons      []operator.ReconReport `json:"reconciliations"`
}

// Network returns operator operations view.
func (s *Service) Network(ctx context.Context, p Principal) (NetworkView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role != RoleOperatorOps {
		return NetworkView{}, ErrForbidden
	}
	total, err := s.Net.Ledger.BigInt(ctx, "omnibusTotal")
	if err != nil {
		return NetworkView{}, err
	}
	brk, _ := s.Net.Ledger.Bool(ctx, "reconciliationBreak")
	inv, _ := s.Net.Ledger.Bool(ctx, "invariantsHold")
	_ = s.Netter.Sync(ctx)
	v := NetworkView{
		Now: s.et(s.Now()), FedwireOpen: s.Fed.Calendar().IsOpen(s.Now()), NextOpen: s.et(s.Fed.Calendar().NextOpen(s.Now())),
		FedJoint: iso20022.FormatDollars(s.Fed.Balance(operator.JointAccount)), LedgerTotal: iso20022.FormatDollars(total),
		Break: brk, Invariants: inv, Queued: s.Netter.Queued(), Cycles: s.cycles, Recons: s.recons,
	}
	if !s.nextCycle.IsZero() {
		v.NextCycle = s.et(s.nextCycle)
	}
	ids := make([]string, 0, len(s.Banks))
	for id := range s.Banks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m, err := s.member(ctx, id)
		if err != nil {
			return NetworkView{}, err
		}
		free, _ := s.Net.Ledger.BigInt(ctx, "freePosition", chain.Bytes32(id))
		spec := s.Banks[id].G.Bank.Spec
		v.Members = append(v.Members, MemberLine{ID: id, Name: spec.Name, Ticker: spec.Ticker,
			Position: iso20022.FormatDollars(m.Position), Backing: iso20022.FormatDollars(m.Backing), Free: iso20022.FormatDollars(free)})
	}
	return v, nil
}

// RunNetting runs a netting cycle now and settles what it can.
func (s *Service) RunNetting(ctx context.Context, p Principal) (operator.CycleReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role != RoleOperatorOps {
		return operator.CycleReport{}, ErrForbidden
	}
	rep, err := s.runCycle(ctx, "run by "+p.Name)
	if err != nil {
		return operator.CycleReport{}, err
	}
	s.tick(ctx)
	return rep, nil
}

func (s *Service) runCycle(ctx context.Context, trigger string) (operator.CycleReport, error) {
	rep, err := s.Netter.RunCycle(ctx)
	if err != nil {
		return operator.CycleReport{}, err
	}
	if rep.Gross == nil {
		rep.Gross, rep.Net = big.NewInt(0), big.NewInt(0)
	}
	rep.At, rep.Trigger = s.Now(), trigger
	s.cycles = append(s.cycles, rep)
	return rep, nil
}

// scheduledNetting runs a cycle when the clock passes a boundary of the
// schedule and obligations are waiting.
func (s *Service) scheduledNetting(ctx context.Context) {
	if s.NettingEvery <= 0 {
		return
	}
	now := s.Now()
	due := !s.nextCycle.IsZero() && !now.Before(s.nextCycle)
	if s.nextCycle.IsZero() || due {
		s.nextCycle = now.Truncate(s.NettingEvery).Add(s.NettingEvery)
	}
	if !due {
		return
	}
	if err := s.Netter.Sync(ctx); err != nil || s.Netter.Queued() == 0 {
		return
	}
	if _, err := s.runCycle(ctx, "scheduled"); err != nil {
		s.log("operator netting  scheduled cycle failed: %v", err)
	}
}

// Participant is one bank a client can pay on the network.
type Participant struct {
	Name          string `json:"name"`
	RoutingNumber string `json:"routingNumber"`
	MemberID      string `json:"memberId"`
	Status        string `json:"status"` // LIVE or SUSPENDED
	Instant       bool   `json:"instant24x7"`
	Screening     bool   `json:"receiverScreensInbound"`
}

// Participants lists the network's banks, so a payer can see whether a
// payee's bank is reachable before submitting.
func (s *Service) Participants(ctx context.Context) ([]Participant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.Net.Banks))
	for id := range s.Net.Banks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := []Participant{}
	for _, id := range ids {
		b := s.Net.Banks[id]
		m, err := s.member(ctx, id)
		if err != nil {
			return nil, err
		}
		st := "LIVE"
		if m.Suspended || !m.Admitted {
			st = "SUSPENDED"
		}
		out = append(out, Participant{Name: b.Spec.Name, RoutingNumber: b.Spec.ABA, MemberID: id, Status: st,
			Instant: st == "LIVE", Screening: b.Spec.RequiresAcceptance})
	}
	return out, nil
}

// NextCycle is when the next scheduled netting cycle runs, if any.
func (s *Service) NextCycle() time.Time { return s.nextCycle }

// Reconcile attests the Fed's camt.052 balance for the joint account.
func (s *Service) Reconcile(ctx context.Context, p Principal) (operator.ReconReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role != RoleOperatorOps {
		return operator.ReconReport{}, ErrForbidden
	}
	r, err := operator.Reconcile(ctx, s.Net, s.Fed)
	if err != nil {
		return operator.ReconReport{}, err
	}
	s.recons = append(s.recons, r)
	return r, nil
}

// SetClock moves the demo clock (and the chain's) forward.
func (s *Service) SetClock(ctx context.Context, p Principal, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Role != RoleOperatorOps {
		return ErrForbidden
	}
	if !t.After(s.Now()) {
		return fmt.Errorf("%w: time only moves forward", ErrInvalid)
	}
	s.Clock.Set(t)
	if err := s.Net.C.SetTime(ctx, t); err != nil {
		return err
	}
	s.tick(ctx)
	return nil
}

/*────────────────────────────── background ──────────────────────────────*/

// Tick advances everything asynchronous: Fedwire defunds, receiving banks'
// screening and acceptance, crediting payees, settled and expired
// payments, netting outcomes, and orders waiting for liquidity.
func (s *Service) Tick(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick(ctx)
	return nil
}

func (s *Service) tick(ctx context.Context) {
	if err := s.Funding.Sync(ctx); err == nil {
		s.Funding.Process(ctx)
	}
	ids := make([]string, 0, len(s.Banks))
	for id := range s.Banks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		_ = s.Banks[id].G.ProcessInbound(ctx)
	}
	for _, id := range ids {
		s.creditInbound(ctx, s.Banks[id])
	}
	s.syncUrgent(ctx)
	s.scheduledNetting(ctx)
	for _, id := range ids {
		_ = s.Banks[id].IB.Sync(ctx)
	}
	s.syncNetted(ctx)
	if _, err := s.Netter.ExpireStale(ctx, uint64(s.Now().Unix())); err == nil {
		for _, id := range ids {
			_ = s.Banks[id].IB.Sync(ctx)
		}
		s.syncNetted(ctx)
	}
	s.syncCredited()
	s.updateDefunds(ctx)
	for _, id := range ids {
		s.retryLiquidity(ctx, id)
		s.watermarks(ctx, id)
	}
}

// watermarks opens a treasury alert when a bank's mint capacity falls
// below its low watermark and closes it once capacity is back at normal.
func (s *Service) watermarks(ctx context.Context, bankID string) {
	node := s.Banks[bankID]
	if node.Low == nil {
		return
	}
	capacity, err := s.Net.MintCapacity(ctx, bankID)
	if err != nil {
		return
	}
	switch {
	case node.lowAlert == nil && capacity.Cmp(node.Low) < 0:
		node.lowAlert = &Alert{At: s.Now(), Bank: bankID, Kind: "LOW_WATERMARK", Open: true,
			Message: fmt.Sprintf("%s can issue only $%s more, below its low watermark of $%s. Fund the omnibus (%s).",
				node.G.Bank.Spec.Name, iso20022.Readable(capacity), iso20022.Readable(node.Low), s.instrument())}
		s.alerts = append(s.alerts, node.lowAlert)
	case node.lowAlert != nil && node.Normal != nil && capacity.Cmp(node.Normal) >= 0:
		node.lowAlert.Open = false
		node.lowAlert = nil
	}
}

// creditInbound redeems tokens that arrived for a bank's customers straight
// into their deposit accounts: payees never hold tokens.
func (s *Service) creditInbound(ctx context.Context, node *BankNode) {
	head, err := s.Net.C.Head(ctx)
	if err != nil {
		return
	}
	logs, err := s.Net.Router.Events(ctx, "PaymentSettled", node.inboundCursor, head)
	if err != nil {
		return
	}
	for _, l := range logs {
		var ev struct {
			PaymentId [32]byte
			Payer     common.Address
			Payee     common.Address
			FromToken common.Address
			ToToken   common.Address
			Amount    *big.Int
		}
		if err := s.Net.Router.Unpack(&ev, "PaymentSettled", l); err != nil {
			continue
		}
		id := l.Topics[1]
		payee := common.BytesToAddress(l.Topics[3].Bytes())
		if ev.ToToken != node.G.Bank.Token.Addr || node.redeemed[id] {
			continue
		}
		c := node.G.CustomerByWallet(payee)
		if c == nil {
			continue
		}
		if err := node.G.Redeem(ctx, c.DDA, ev.Amount); err != nil {
			continue
		}
		node.redeemed[id] = true
	}
	_ = node.G.SyncRedemptions(ctx)
	node.inboundCursor = head + 1
}

func (s *Service) syncUrgent(ctx context.Context) {
	for _, id := range s.orderIDs {
		o := s.orders[id]
		if o.Status != StatusPendingReceiver {
			continue
		}
		pv, err := s.Net.Payment(ctx, o.paymentID)
		if err != nil {
			continue
		}
		switch pv.Status {
		case 2:
			o.tokenized = false
			to, _ := s.Net.BankByABA(o.Creditor.ABA)
			s.settle(o, to.Spec.Name+" screened and accepted; settled on the network")
		case 3:
			reason := s.Net.RejectReason(ctx, o.paymentID)
			s.refundTokens(ctx, o)
			s.reject(o, reason, "rejected by the receiving bank")
		case 4:
			s.refundTokens(ctx, o)
			s.event(o, "system", StatusExpired, ISORejected, "the receiving bank did not answer in time")
		case 1:
			if uint64(s.Now().Unix()) > pv.Deadline {
				g := s.Banks[o.Bank].G
				if _, err := s.Net.Router.Send(ctx, g.Bank.Keys.Operator, "expire", o.paymentID); err == nil {
					s.refundTokens(ctx, o)
					s.event(o, "system", StatusExpired, ISORejected, "the receiving bank did not answer in time; hold released")
				}
			}
		}
	}
}

func (s *Service) syncNetted(ctx context.Context) {
	for _, id := range s.orderIDs {
		o := s.orders[id]
		if o.Status != StatusQueuedForNetting {
			continue
		}
		out, err := s.Net.Netting.Call(ctx, "obligations", o.obligationID)
		if err != nil {
			continue
		}
		switch out[4].(uint8) {
		case 2:
			s.settle(o, "settled in an operator netting cycle")
		case 3:
			s.event(o, "system", StatusCancelled, ISOCancelled, "withdrawn from the netting queue; account re-credited")
		case 4:
			s.event(o, "system", StatusExpired, ISORejected, "not settled before the obligation expired; account re-credited")
		}
	}
}

/*──────────────────────────────── helpers ───────────────────────────────*/

func snapshot(o *Order) Order {
	cp := *o
	cp.AmountUSD = iso20022.FormatDollars(o.Amount)
	cp.Approvals = append([]string{}, o.Approvals...)
	cp.History = append([]Event(nil), o.History...)
	return cp
}

// viewFor is an order as the principal may see it: clients get their
// bank's account-level story, staff the token mechanics too.
func (s *Service) viewFor(p Principal, o *Order) Order {
	if p.Org != "" {
		return clientView(snapshot(o))
	}
	return snapshot(o)
}

func clientView(o Order) Order {
	for i := range o.History {
		o.History[i].Detail = ""
	}
	return o
}

// uuid4 is a random RFC 9562 version 4 UUID, used as the UETR.
func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
