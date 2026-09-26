// Package biz is the payment hub's business layer: the payment-order
// lifecycle a corporate treasury and a bank's operations staff work
// through, the controls banks apply, and the treasury and operator operations
// behind it.
//
// Corporate clients see deposit accounts and payment orders only. Tokenized
// deposits are the settlement instrument underneath: the paying bank
// tokenizes a payment just in time, the network settles it, and the
// receiving bank redeems it straight into the payee's account. Repository
// interfaces are implemented in data/, the settlement rails in rails/, OFAC
// screening in sanctions/, and ISO 20022 documents in iso/.
package biz

import (
	"time"
)

// Role is what a person may do.
type Role string

const (
	RoleMaker            Role = "maker"             // corporate: creates payment orders
	RoleApprover         Role = "approver"          // corporate: approves orders; manages webhooks
	RoleViewer           Role = "viewer"            // corporate: read only
	RoleCompliance       Role = "compliance"        // bank: sanctions review
	RoleTreasury         Role = "treasury"          // bank: funds the omnibus, requests defunds
	RoleTreasuryApprover Role = "treasury-approver" // bank: approves defunds
	RoleOperatorOps      Role = "operator-ops"      // the operator: netting, reconciliation, network view
)

// Principal is an authenticated person.
type Principal struct {
	ID   string
	Name string
	Role Role
	Bank string // member id of the bank they work at, or bank the organization banks with
	Org  string // corporate organization id; empty for bank and operator staff
}

// IsBankStaff reports whether p works at a member bank.
func (p Principal) IsBankStaff() bool {
	return p.Org == "" && (p.Role == RoleCompliance || p.Role == RoleTreasury || p.Role == RoleTreasuryApprover)
}

// Org is a corporate client of a member bank, with its entitlements.
type Org struct {
	ID                  string
	Name                string
	Bank                string
	Account             string
	PerPaymentLimit     int64 // cents; 0 = none
	DailyLimit          int64 // cents; 0 = none
	SecondApprovalAbove int64 // cents; orders above this need two approvers
}

// Party is a payee.
type Party struct {
	Name    string
	Routing string
	Account string
}

// Priority chooses the settlement route. It maps to the ISO 20022 service
// levels URGP and NURG.
type Priority string

const (
	Urgent Priority = "URGENT" // settles now, 24x7, as tokenized deposits
	Normal Priority = "NORMAL" // funded from the deposit, settled in the next netting cycle
)

// Status is where an order is in its lifecycle.
type Status string

const (
	StatusAwaitingApproval  Status = "AWAITING_APPROVAL"
	StatusInProcess         Status = "IN_PROCESS"
	StatusOnHold            Status = "ON_HOLD"
	StatusAwaitingLiquidity Status = "AWAITING_LIQUIDITY"
	StatusPendingReceiver   Status = "PENDING_RECEIVER"
	StatusQueuedForNetting  Status = "QUEUED_FOR_NETTING"
	StatusSettled           Status = "SETTLED"
	StatusRejected          Status = "REJECTED"
	StatusCancelled         Status = "CANCELLED"
	StatusExpired           Status = "EXPIRED"
	StatusBlocked           Status = "BLOCKED"
)

// Terminal reports whether nothing more will happen to an order's status.
func (s Status) Terminal() bool {
	switch s {
	case StatusSettled, StatusRejected, StatusCancelled, StatusExpired, StatusBlocked:
		return true
	}
	return false
}

// ISO 20022 pain.002 transaction statuses.
const (
	ISOReceived          = "RCVD"
	ISOAcceptedTechnical = "ACTC" // validated, awaiting approval
	ISOPartlyApproved    = "PATC" // some but not all of the approvals
	ISOAcceptedCustomer  = "ACCP" // approved
	ISOPending           = "PDNG"
	ISOInProcess         = "ACSP" // accepted for execution
	ISOSettled           = "ACSC" // settled between the banks
	ISOCredited          = "ACCC" // credited to the payee's account
	ISORejected          = "RJCT"
	ISOCancelled         = "CANC"
	ISOBlocked           = "BLCK"
)

// Routes, as the client reads them.
const (
	RouteInstant = "on-network instant, 24x7"
	RouteNetting = "network netting cycle"
	RouteBook    = "book transfer, same bank"
)

// Event is one line of an order's audit trail. Detail is for bank and the operator
// staff; clients never see token mechanics.
type Event struct {
	Seq    int
	At     time.Time
	Actor  string
	Status Status
	ISO    string
	Note   string
	Detail string
}

// Approval is one approver's sign-off.
type Approval struct {
	ApproverID   string
	ApproverName string
	At           time.Time
}

// PayeeCheck is the receiving bank's answer about a payee.
type PayeeCheck struct {
	Account    string // OPEN | CLOSED | NOT_FOUND
	Name       string // MATCH | CLOSE_MATCH | NO_MATCH
	Registered string // on a close match only
}

// Passed reports whether the payer can rely on the details as typed.
func (c PayeeCheck) Passed() bool { return c.Account == "OPEN" && c.Name == "MATCH" }

// Order is a payment order.
type Order struct {
	ID                string
	Org               string
	Bank              string
	FileID            string
	IdempotencyKey    string
	RequestHash       string
	EndToEndID        string
	UETR              string
	DebtorAccount     string
	Creditor          Party
	Amount            int64 // cents
	Priority          Priority
	Remittance        string
	Status            Status
	ISO               string
	Reason            string
	Route             string
	RouteNote         string
	PayeeCheck        PayeeCheck
	ApprovalsRequired int
	Approvals         []Approval
	CreatedBy         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SettledAt         *time.Time
	OFACReportDue     *time.Time
	SanctionsMatch    string
	PaymentRef        string // router payment id (URGENT, other bank)
	ObligationRef     string // netting obligation id (NORMAL, other bank)
	Tokenized         bool   // the order's deposit is tokenized and not yet settled or refunded
	Released          bool   // compliance released a screening hold
	Version           int
	History           []Event

	// SavedEvents and SavedApprovals are how many of History and Approvals
	// are already stored; the repository appends the rest.
	SavedEvents    int
	SavedApprovals int
}

// Alert is something a bank's treasury must act on.
type Alert struct {
	ID       string
	Bank     string
	Kind     string // LIQUIDITY | LOW_WATERMARK
	Message  string
	Open     bool
	At       time.Time
	ClosedAt *time.Time
}

// Defund is treasury's maker-checker request to take free position back.
type Defund struct {
	ID            string
	Bank          string
	Amount        int64
	RequestedByID string
	RequestedBy   string
	ApprovedBy    string
	Status        string // AWAITING_APPROVAL | APPROVED | COMPLETED | FAILED
	LedgerRef     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NettingCycle is one cycle the operator ran.
type NettingCycle struct {
	ID         string
	CycleRef   string
	At         time.Time
	Trigger    string
	Discharged int
	Deferred   int
	Gross      int64
	Net        int64
}

// Reconciliation is one attestation of the Fed's balance against the ledger.
type Reconciliation struct {
	ID          string
	At          time.Time
	FedBalance  int64
	LedgerTotal int64
	Break       bool
	Invariants  bool
}

// PaymentFile is a pain.001 the client sent.
type PaymentFile struct {
	ID             string
	Org            string
	MessageID      string
	RequestHash    string
	IdempotencyKey string
	Transactions   int
	ControlSum     int64
	CreatedBy      string
	CreatedAt      time.Time
	Rejections     []FileRejection
}

// FileRejection is a transaction of a file refused at validation.
type FileRejection struct {
	EndToEndID string
	Reason     string
	Message    string
}

// Subscription is a webhook endpoint of an organization.
type Subscription struct {
	ID        string
	Org       string
	URL       string
	Secret    []byte // plaintext in memory only; encrypted at rest
	CreatedBy string
	CreatedAt time.Time
}

// Delivery is one event on its way to one endpoint.
type Delivery struct {
	ID           string // webhook-id; stable across retries
	Subscription string
	Org          string
	EventType    string
	Payload      []byte
	Status       string // PENDING | DELIVERED | FAILED
	Attempts     int
	LastCode     int
	LastError    string
	NextAttempt  time.Time
	CreatedAt    time.Time
}

// Participant is a bank a client can pay.
type Participant struct {
	Name               string
	Routing            string
	MemberID           string
	Status             string // LIVE | SUSPENDED
	RequiresAcceptance bool
}

// Posting is one entry on a deposit account.
type Posting struct {
	At      time.Time
	Ref     string
	Delta   int64
	Balance int64
}

// Member is a bank's standing on the operator's ledger.
type Member struct {
	ID            string
	Name          string
	Ticker        string
	Routing       string
	Position      int64
	Backing       int64
	PendingDefund int64
	Free          int64
	MintCapacity  int64
	Admitted      bool
	Suspended     bool
}

// Liquidity is a bank treasury's view.
type Liquidity struct {
	Member            Member
	LowWatermark      int64
	NormalWatermark   int64
	FedwireOpen       bool
	NextFedwireOpen   time.Time
	Instrument        string
	AwaitingLiquidity []*Order
	Alerts            []*Alert
	Defunds           []*Defund
}

// NetworkView is operator operations' console.
type NetworkView struct {
	Now             time.Time
	FedwireOpen     bool
	NextFedwireOpen time.Time
	FedJoint        int64
	LedgerTotal     int64
	Break           bool
	Invariants      bool
	Members         []Member
	Queued          int
	NextCycle       time.Time
	Cycles          []*NettingCycle
	Recons          []*Reconciliation
}

// Account is a client's deposit account.
type Account struct {
	Org      Org
	BankName string
	Routing  string
	Balance  int64
	InFlight int64
	AsOf     time.Time
}

// FundResult is the Fed's answer to a funding transfer.
type FundResult struct {
	Instrument string
	Status     string
	Reason     string
	Released   int
}
