// Package omnibus deploys and wires the omnibus contracts: the operator's ledger and
// router, and for each member bank its holder registry and its ticker. Each
// party deploys and administers its own contracts with its own keys; the operator
// only admits the bank's ticker as a member.
package omnibus

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"omnibus-deposit-network/internal/chain"
)

// BankSpec describes a member bank.
type BankSpec struct {
	MemberID           string // BIC, used as the member id on the ledger
	Name               string
	Ticker             string
	ABA                string // routing number, used on Fedwire
	MasterAccount      string // its Fed master account
	RequiresAcceptance bool   // inbound cross-bank payments wait for its accept
}

// TCHKeys are the operator's separated operating keys.
type OperatorKeys struct {
	Admin      chain.Account // deploys; would sit behind a multisig and timelock
	Governor   chain.Account // admits and suspends members
	Funding    chain.Account // posts Fedwire credits and defund outcomes
	Reconciler chain.Account // attests the Fed statement; not the funding key
	Settlement chain.Account // submits verified netting cycles
	Pauser     chain.Account
}

// BankKeys are one bank's separated operating keys.
type BankKeys struct {
	Admin      chain.Account // administers its token and registry
	Operator   chain.Account // its gateway: defund requests, payment acceptance
	Approver   chain.Account // its treasury: approves each defund (maker-checker)
	Treasury   chain.Account // its settlement wallet, listed by the operator: holds other members' tokens
	Issuer     chain.Account // its mint/redeem service
	Compliance chain.Account
	Registrar  chain.Account // its onboarding service
}

// Bank is a deployed member.
type Bank struct {
	Spec     BankSpec
	Keys     BankKeys
	Token    *chain.Contract
	Registry *chain.Contract
}

// Network is the deployed omnibus.
type Network struct {
	C        *chain.Client
	Operator OperatorKeys
	Ledger   *chain.Contract
	Router   *chain.Contract
	Netting  *chain.Contract
	Banks    map[string]*Bank // by MemberID
}

// NewTCHKeys generates the operator's keys.
func NewOperatorKeys() (OperatorKeys, error) {
	var k OperatorKeys
	var err error
	for _, p := range []struct {
		dst  *chain.Account
		name string
	}{{&k.Admin, "operator-admin"}, {&k.Governor, "operator-governor"}, {&k.Funding, "operator-funding"}, {&k.Reconciler, "operator-reconciler"}, {&k.Settlement, "operator-settlement"}, {&k.Pauser, "operator-pauser"}} {
		if *p.dst, err = chain.NewAccount(p.name); err != nil {
			return k, err
		}
	}
	return k, nil
}

// NewBankKeys generates one bank's keys.
func NewBankKeys(prefix string) (BankKeys, error) {
	var k BankKeys
	var err error
	for _, p := range []struct {
		dst  *chain.Account
		name string
	}{{&k.Admin, "admin"}, {&k.Operator, "operator"}, {&k.Approver, "approver"}, {&k.Treasury, "treasury"}, {&k.Issuer, "issuer"}, {&k.Compliance, "compliance"}, {&k.Registrar, "registrar"}} {
		if *p.dst, err = chain.NewAccount(prefix + "-" + p.name); err != nil {
			return k, err
		}
	}
	return k, nil
}

func role(ctx context.Context, k *chain.Contract, name string) ([32]byte, error) {
	out, err := k.Call(ctx, name)
	if err != nil {
		return [32]byte{}, err
	}
	return out[0].([32]byte), nil
}

func grant(ctx context.Context, k *chain.Contract, admin chain.Account, roleName string, to common.Address) error {
	r, err := role(ctx, k, roleName)
	if err != nil {
		return err
	}
	_, err = k.Send(ctx, admin, "grantRole", r, to)
	return err
}

// demoAdminDelay is the admin-handover delay of the in-process demo network:
// zero, so a demo can be stood up in one go. A real deployment uses the
// Foundry scripts (contracts/script), which hand every admin to a timelock.
var demoAdminDelay = big.NewInt(0)

// Deploy stands the network up: the operator's contracts first, then each bank's
// registry and ticker under the bank's own admin key, then admission.
func Deploy(ctx context.Context, c *chain.Client, op OperatorKeys, specs []BankSpec, bankKeys map[string]BankKeys) (*Network, error) {
	for _, a := range []chain.Account{op.Admin, op.Governor, op.Funding, op.Reconciler, op.Settlement, op.Pauser} {
		if err := c.FundGas(ctx, a.Addr); err != nil {
			return nil, fmt.Errorf("fund %s: %w", a.Name, err)
		}
	}
	ledger, err := c.Deploy(ctx, op.Admin, "OmnibusLedger", op.Admin.Addr, demoAdminDelay)
	if err != nil {
		return nil, err
	}
	router, err := c.Deploy(ctx, op.Admin, "PaymentRouter", ledger.Addr, op.Admin.Addr, demoAdminDelay)
	if err != nil {
		return nil, err
	}
	netting, err := c.Deploy(ctx, op.Admin, "OmnibusNetting", ledger.Addr, op.Admin.Addr, demoAdminDelay)
	if err != nil {
		return nil, err
	}
	for _, g := range []struct {
		k    *chain.Contract
		role string
		to   common.Address
	}{
		{ledger, "GOVERNOR_ROLE", op.Governor.Addr},
		{ledger, "FUNDING_ROLE", op.Funding.Addr},
		{ledger, "RECONCILER_ROLE", op.Reconciler.Addr},
		{ledger, "ROUTER_ROLE", router.Addr},
		{ledger, "NETTING_ROLE", netting.Addr},
		{netting, "OPERATOR_ROLE", op.Settlement.Addr},
		{router, "PAUSER_ROLE", op.Pauser.Addr},
	} {
		if err := grant(ctx, g.k, op.Admin, g.role, g.to); err != nil {
			return nil, err
		}
	}

	n := &Network{C: c, Operator: op, Ledger: ledger, Router: router, Netting: netting, Banks: map[string]*Bank{}}
	for _, s := range specs {
		bk := bankKeys[s.MemberID]
		for _, a := range []chain.Account{bk.Admin, bk.Operator, bk.Approver, bk.Treasury, bk.Issuer, bk.Compliance, bk.Registrar} {
			if err := c.FundGas(ctx, a.Addr); err != nil {
				return nil, err
			}
		}
		reg, err := c.Deploy(ctx, bk.Admin, "HolderRegistry", bk.Admin.Addr, demoAdminDelay)
		if err != nil {
			return nil, err
		}
		tok, err := c.Deploy(ctx, bk.Admin, "BankToken", s.Name+" USD", s.Ticker, ledger.Addr, router.Addr, reg.Addr, bk.Admin.Addr, demoAdminDelay)
		if err != nil {
			return nil, err
		}
		for _, g := range []struct {
			k    *chain.Contract
			role string
			to   common.Address
		}{
			{reg, "REGISTRAR_ROLE", bk.Registrar.Addr},
			{tok, "ISSUER_ROLE", bk.Issuer.Addr},
			{tok, "COMPLIANCE_ROLE", bk.Compliance.Addr},
		} {
			if err := grant(ctx, g.k, bk.Admin, g.role, g.to); err != nil {
				return nil, err
			}
		}
		if _, err := ledger.Send(ctx, op.Governor, "admitMember", chain.Bytes32(s.MemberID), tok.Addr, bk.Operator.Addr, bk.Approver.Addr, s.RequiresAcceptance); err != nil {
			return nil, err
		}
		if _, err := ledger.Send(ctx, op.Governor, "registerWallet", chain.Bytes32(s.MemberID), bk.Treasury.Addr); err != nil {
			return nil, err
		}
		n.Banks[s.MemberID] = &Bank{Spec: s, Keys: bk, Token: tok, Registry: reg}
	}
	return n, nil
}

// BankByABA finds a member by routing number.
func (n *Network) BankByABA(aba string) (*Bank, bool) {
	for _, b := range n.Banks {
		if b.Spec.ABA == aba {
			return b, true
		}
	}
	return nil, false
}

// BankByToken finds a member by ticker address.
func (n *Network) BankByToken(addr common.Address) (*Bank, bool) {
	for _, b := range n.Banks {
		if b.Token.Addr == addr {
			return b, true
		}
	}
	return nil, false
}

// PaymentView mirrors PaymentRouter.Payment field for field (abi.ConvertType
// maps by position).
type PaymentView struct {
	Payer     common.Address
	Payee     common.Address
	FromToken common.Address
	ToToken   common.Address
	Amount    *big.Int
	Deadline  uint64
	Status    uint8 // 1 pending, 2 settled, 3 rejected, 4 expired
	ReturnOf  [32]byte
	Returned  *big.Int
}

// Payment reads a router payment.
func (n *Network) Payment(ctx context.Context, id [32]byte) (PaymentView, error) {
	out, err := n.Router.Call(ctx, "payment", id)
	if err != nil {
		return PaymentView{}, err
	}
	return *abi.ConvertType(out[0], new(PaymentView)).(*PaymentView), nil
}

// RejectReason returns the ISO reason code a receiving bank gave when it
// rejected a payment, or "" if it did not.
func (n *Network) RejectReason(ctx context.Context, id [32]byte) string {
	head, err := n.C.Head(ctx)
	if err != nil {
		return ""
	}
	logs, err := n.Router.EventsWithTopic(ctx, "PaymentRejected", id, 0, head)
	if err != nil || len(logs) == 0 {
		return ""
	}
	data := logs[len(logs)-1].Data
	if len(data) < 4 {
		return ""
	}
	return strings.TrimRight(string(data[:4]), "\x00")
}

// MintCapacity is how much more a member may issue now.
func (n *Network) MintCapacity(ctx context.Context, memberID string) (*big.Int, error) {
	return n.Ledger.BigInt(ctx, "mintCapacity", chain.Bytes32(memberID))
}
