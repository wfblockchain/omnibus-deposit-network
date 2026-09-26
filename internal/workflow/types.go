// Package workflow is the business layer between people and the settlement
// network: the payment-order lifecycle a corporate treasury and a bank's
// operations staff actually work through, with the controls banks apply
// today, and the bank treasury and operator operations workflows behind it.
//
// Corporate clients see deposit accounts and payment orders only. Tokenized
// deposits are the settlement instrument underneath: the sending bank
// tokenizes a payment just in time, the network settles it, and the
// receiving bank redeems it straight into the payee's account.
package workflow

import (
	"errors"
	"math/big"
	"time"
)

// Role is what a person may do.
type Role string

const (
	RoleMaker            Role = "maker"             // corporate: creates payment orders
	RoleApprover         Role = "approver"          // corporate: approves orders (checker)
	RoleViewer           Role = "viewer"            // corporate: read only
	RoleCompliance       Role = "compliance"        // bank: releases or rejects screening holds
	RoleTreasury         Role = "treasury"          // bank: funds the omnibus, requests defunds
	RoleTreasuryApprover Role = "treasury-approver" // bank: approves defunds
	RoleOperatorOps      Role = "operator-ops"      // the operator: netting, reconciliation, network view
)

// Principal is an authenticated person.
type Principal struct {
	Token string `json:"-"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Role  Role   `json:"role"`
	Bank  string `json:"bank,omitempty"` // member id of the bank they belong to (or bank for the org)
	Org   string `json:"org,omitempty"`  // corporate org id; empty for bank and operator staff
}

// Org is a corporate client of a member bank, with its entitlements.
type Org struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Bank                string   `json:"bank"`
	DDA                 string   `json:"account"`
	PerTxLimit          *big.Int `json:"-"`
	DailyLimit          *big.Int `json:"-"`
	SecondApprovalAbove *big.Int `json:"-"` // orders above this need two approvers
}

// Party is the payee.
type Party struct {
	Name    string `json:"name"`
	ABA     string `json:"routingNumber"`
	Account string `json:"account"`
}

// Priority chooses the settlement route.
type Priority string

const (
	// Urgent settles now, 24x7 and at any size within limits, as tokenized
	// deposits moved across the network.
	Urgent Priority = "URGENT"
	// Normal is funded from the deposit and settled in the next netting
	// cycle, which uses far less bank liquidity.
	Normal Priority = "NORMAL"
)

// Status is where an order is in its lifecycle.
type Status string

const (
	StatusAwaitingApproval  Status = "AWAITING_APPROVAL"
	StatusInProcess         Status = "IN_PROCESS"         // approved; screening and execution under way
	StatusOnHold            Status = "ON_HOLD"            // sanctions review
	StatusAwaitingLiquidity Status = "AWAITING_LIQUIDITY" // bank must fund the omnibus
	StatusPendingReceiver   Status = "PENDING_RECEIVER"   // receiving bank screening
	StatusQueuedForNetting  Status = "QUEUED_FOR_NETTING"
	StatusSettled           Status = "SETTLED"
	StatusRejected          Status = "REJECTED"
	StatusCancelled         Status = "CANCELLED"
	StatusExpired           Status = "EXPIRED"
	StatusBlocked           Status = "BLOCKED" // OFAC: funds held in a blocked account
)

// Terminal reports whether nothing more will happen to an order.
func (s Status) Terminal() bool {
	switch s {
	case StatusSettled, StatusRejected, StatusCancelled, StatusExpired, StatusBlocked:
		return true
	}
	return false
}

// ISO 20022 pain.002 status codes shown to the corporate.
const (
	ISOReceived          = "RCVD"
	ISOAcceptedTechnical = "ACTC" // validated, awaiting approval
	ISOPartlyApproved    = "PATC" // some but not all of the approvals needed
	ISOAcceptedCustomer  = "ACCP" // approved
	ISOPending           = "PDNG"
	ISOInProcess         = "ACSP" // accepted for execution
	ISOSettled           = "ACSC" // settled between the banks
	ISOCredited          = "ACCC" // credited to the payee's account
	ISORejected          = "RJCT"
	ISOCancelled         = "CANC"
	ISOBlocked           = "BLCK" // neither paid nor returned
)

// Event is one line of an order's audit trail.
type Event struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Status Status    `json:"status"`
	ISO    string    `json:"iso"`
	Note   string    `json:"note"`
	Detail string    `json:"detail,omitempty"` // for bank and operator staff; clients never see token mechanics
}

// Order is a payment order.
type Order struct {
	ID             string     `json:"id"`
	IdempotencyKey string     `json:"idempotencyKey"`
	EndToEndID     string     `json:"endToEndId"` // the client's reference, unique per organization
	UETR           string     `json:"uetr"`       // unique end-to-end transaction reference (UUID)
	Org            string     `json:"org"`
	Bank           string     `json:"bank"`
	DebtorAccount  string     `json:"debtorAccount"`
	Creditor       Party      `json:"creditor"`
	Amount         *big.Int   `json:"-"`
	AmountUSD      string     `json:"amount"`
	Priority       Priority   `json:"priority"`
	Remittance     string     `json:"remittance,omitempty"`
	Status         Status     `json:"status"`
	ISO            string     `json:"isoStatus"`
	Reason         string     `json:"reasonCode,omitempty"`
	Route          string     `json:"route,omitempty"`
	RouteNote      string     `json:"routeNote,omitempty"`
	CreatedBy      string     `json:"createdBy"`
	Approvals      []string   `json:"approvals"`
	CreatedAt      time.Time  `json:"createdAt"`
	SettledAt      time.Time  `json:"settledAt,omitzero"`
	OFACReportDue  time.Time  `json:"ofacReportDue,omitzero"` // blocked or rejected on sanctions grounds
	PayeeCheck     PayeeCheck `json:"payeeCheck"`
	History        []Event    `json:"history"`

	paymentID    [32]byte // router payment (urgent, cross-bank)
	obligationID [32]byte // netting obligation (normal, cross-bank)
	released     bool     // compliance released a screening hold
	tokenized    bool     // the order's deposit was tokenized
}

// PayeeCheck is the network's answer about a payee before any money moves,
// as the Fed's Payee Name Verification and banks' account-validation
// services give it: whether the account is open, and whether the name the
// payer typed matches the one the receiving bank holds.
type PayeeCheck struct {
	Account    string `json:"accountStatus"`            // OPEN, CLOSED, NOT_FOUND
	Name       string `json:"nameMatch"`                // MATCH, CLOSE_MATCH, NO_MATCH
	Registered string `json:"registeredName,omitempty"` // given back on a close match only
}

// Passed reports whether the payer can rely on the details as typed.
func (c PayeeCheck) Passed() bool { return c.Account == "OPEN" && c.Name == "MATCH" }

// Alert is something bank treasury must act on.
type Alert struct {
	At      time.Time `json:"at"`
	Bank    string    `json:"bank"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
	Open    bool      `json:"open"`
}

// DefundRequest is bank treasury's maker-checker request to take free
// position back to the master account.
type DefundRequest struct {
	ID          string    `json:"id"`
	Bank        string    `json:"bank"`
	AmountUSD   string    `json:"amount"`
	RequestedBy string    `json:"requestedBy"`
	ApprovedBy  string    `json:"approvedBy,omitempty"`
	Status      string    `json:"status"` // AWAITING_APPROVAL, APPROVED, COMPLETED, FAILED
	At          time.Time `json:"at"`

	ledgerID [32]byte
}

// Errors the API maps to HTTP status codes.
var (
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("conflict")
	ErrInvalid   = errors.New("invalid request")
)
