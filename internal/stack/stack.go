// Package stack wires the whole system for the web server and its tests:
// the contracts on a node, the Fed simulator, the operator's back office, three
// member banks with their corporate clients and staff, and the workflow
// service the API and portals sit on.
package stack

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"omnibus-deposit-network/internal/bank"
	"omnibus-deposit-network/internal/chain"
	"omnibus-deposit-network/internal/fedwire"
	"omnibus-deposit-network/internal/iso20022"
	"omnibus-deposit-network/internal/omnibus"
	"omnibus-deposit-network/internal/operator"
	"omnibus-deposit-network/internal/webhook"
	"omnibus-deposit-network/internal/workflow"
)

const (
	BankA = "BNKAUS30"
	BankB = "BNKBUS30"
	BankC = "BNKCUS30"

	BankAABA = "123456780"
	BankBABA = "234567898"
	BankCABA = "012345672"
)

// Start is when the demo clock begins: Friday 2 October 2026, 16:00 ET,
// three hours before Fedwire closes for the weekend.
func Start() time.Time {
	loc, _ := time.LoadLocation("America/New_York")
	return time.Date(2026, 10, 2, 16, 0, 0, 0, loc)
}

// Stack is the running system.
type Stack struct {
	Service *workflow.Service
	Hooks   *webhook.Dispatcher
	Net     *omnibus.Network
	Fed     *fedwire.Service
}

// Build deploys and seeds everything against a node at rpc.
func Build(ctx context.Context, rpc, artifacts string, log operator.Logger) (*Stack, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	c, err := chain.Dial(ctx, rpc, artifacts)
	if err != nil {
		return nil, err
	}
	cal, err := fedwire.NewCalendar()
	if err != nil {
		return nil, err
	}
	clock := fedwire.NewManualClock(Start())
	if err := c.SetTime(ctx, Start()); err != nil {
		return nil, err
	}
	fed := fedwire.New(clock, cal)

	specs := []omnibus.BankSpec{
		{MemberID: BankA, Name: "Bank A", Ticker: "A-dT", ABA: BankAABA, MasterAccount: "FRB-MASTER-" + BankAABA},
		{MemberID: BankB, Name: "Bank B", Ticker: "B-dT", ABA: BankBABA, MasterAccount: "FRB-MASTER-" + BankBABA, RequiresAcceptance: true},
		{MemberID: BankC, Name: "Bank C", Ticker: "C-dT", ABA: BankCABA, MasterAccount: "FRB-MASTER-" + BankCABA},
	}
	tchKeys, err := omnibus.NewOperatorKeys()
	if err != nil {
		return nil, err
	}
	keys := map[string]omnibus.BankKeys{}
	for _, s := range specs {
		if keys[s.MemberID], err = omnibus.NewBankKeys(s.Ticker); err != nil {
			return nil, err
		}
	}
	net, err := omnibus.Deploy(ctx, c, tchKeys, specs, keys)
	if err != nil {
		return nil, fmt.Errorf("deploy: %w", err)
	}

	fed.OpenAccount(operator.JointAccount, operator.OperatorAgent, big.NewInt(0))
	fed.EnableLMT(operator.JointAccount)
	for _, s := range specs {
		fed.OpenAccount(s.MasterAccount, iso20022.Agent{BICFI: s.MemberID, ABA: s.ABA}, iso20022.Dollars(5_000_000_000))
	}
	funding := operator.NewFundingService(net, fed, log)
	netter := operator.NewNettingService(net, log)
	dir := operator.NewDirectory()
	msgs := operator.NewMessages()
	sanctions := bank.ListScreener{Names: []string{"Petrov Trading"}}

	nodes := map[string]*workflow.BankNode{}
	for _, s := range specs {
		g := bank.NewGateway(net.Banks[s.MemberID], net, fed, dir, sanctions, log)
		nodes[s.MemberID] = workflow.NewBankNode(g, bank.NewInterbank(g, msgs))
	}
	d := iso20022.Dollars
	for id, amt := range map[string]int64{BankA: 20_000_000, BankB: 40_000_000, BankC: 30_000_000} {
		if st := nodes[id].G.FundOmnibus(d(amt)); st.TxSts != iso20022.StatusAccepted {
			return nil, fmt.Errorf("fund %s: %s", id, st.Reason)
		}
	}

	svc := workflow.New(net, fed, clock, funding, netter, nodes, log)
	svc.NettingEvery = time.Hour
	hooks := webhook.New()
	hooks.AllowLoopbackHTTP = true // local demo endpoints; production endpoints are https
	svc.OnChange = func(o workflow.Order, ev workflow.Event) {
		typ := "payment." + strings.ToLower(string(ev.Status))
		if ev.ISO == workflow.ISOCredited {
			typ = "payment.credited"
		}
		data := map[string]any{
			"paymentId": o.ID, "endToEndId": o.EndToEndID, "uetr": o.UETR,
			"status": o.Status, "isoStatus": o.ISO,
			"amount": o.AmountUSD, "currency": "USD", "statusUpdatedAt": ev.At, "note": ev.Note,
		}
		if o.Reason != "" {
			data["reasonCode"] = o.Reason
		}
		hooks.Publish(o.Org, typ, "/api/v1/payments/"+o.ID, ev.At, data)
	}

	type client struct {
		org     workflow.Org
		opening int64
	}
	clients := []client{
		{workflow.Org{ID: "northwind", Name: "Northwind Corp", Bank: BankA, DDA: "A-3000001",
			PerTxLimit: d(50_000_000), DailyLimit: d(100_000_000), SecondApprovalAbove: d(10_000_000)}, 150_000_000},
		{workflow.Org{ID: "contoso", Name: "Contoso Ltd", Bank: BankB, DDA: "B-4000001",
			PerTxLimit: d(50_000_000), DailyLimit: d(100_000_000), SecondApprovalAbove: d(25_000_000)}, 80_000_000},
		{workflow.Org{ID: "fabrikam", Name: "Fabrikam Inc", Bank: BankC, DDA: "C-5000001",
			PerTxLimit: d(50_000_000), DailyLimit: d(100_000_000), SecondApprovalAbove: d(25_000_000)}, 70_000_000},
	}
	for _, cl := range clients {
		if _, err := nodes[cl.org.Bank].G.Onboard(ctx, cl.org.Name, cl.org.DDA, d(cl.opening)); err != nil {
			return nil, err
		}
		svc.AddOrg(cl.org)
	}
	// Two more Bank B accounts: one closed, one whose name matches the
	// sanctions list. Neither has users.
	for _, o := range []struct{ name, dda string }{{"Dave Ltd", "B-2000002"}, {"Petrov Trading LLC", "B-2000009"}} {
		if _, err := nodes[BankB].G.Onboard(ctx, o.name, o.dda, big.NewInt(0)); err != nil {
			return nil, err
		}
	}
	nodes[BankB].G.Close("B-2000002")
	for _, n := range nodes {
		n.Low, n.Normal = d(5_000_000), d(10_000_000)
	}

	for _, u := range Users() {
		svc.AddUser(u)
	}
	return &Stack{Service: svc, Hooks: hooks, Net: net, Fed: fed}, nil
}

// Users are the demo people. Tokens stand in for OAuth2 access tokens.
func Users() []workflow.Principal {
	u := func(token, id, name string, role workflow.Role, bank, org string) workflow.Principal {
		return workflow.Principal{Token: token, ID: id, Name: name, Role: role, Bank: bank, Org: org}
	}
	return []workflow.Principal{
		u("tok-nw-maker", "nw-maker", "Priya Shah, AP specialist", workflow.RoleMaker, BankA, "northwind"),
		u("tok-nw-treasurer", "nw-treasurer", "Tom Reyes, Treasurer", workflow.RoleApprover, BankA, "northwind"),
		u("tok-nw-cfo", "nw-cfo", "Lena Ortiz, CFO", workflow.RoleApprover, BankA, "northwind"),
		u("tok-nw-viewer", "nw-viewer", "Sam Lee, Auditor", workflow.RoleViewer, BankA, "northwind"),
		u("tok-co-maker", "co-maker", "Ari Cohen, AP specialist", workflow.RoleMaker, BankB, "contoso"),
		u("tok-co-approver", "co-approver", "Mei Tan, Treasurer", workflow.RoleApprover, BankB, "contoso"),
		u("tok-fa-maker", "fa-maker", "Luis Gomez, AP specialist", workflow.RoleMaker, BankC, "fabrikam"),
		u("tok-fa-approver", "fa-approver", "Nora Kim, Treasurer", workflow.RoleApprover, BankC, "fabrikam"),
		u("tok-bank-a-compliance", "bank-a-compliance", "Bank A sanctions review", workflow.RoleCompliance, BankA, ""),
		u("tok-bank-a-treasury", "bank-a-treasury", "Bank A liquidity desk", workflow.RoleTreasury, BankA, ""),
		u("tok-bank-a-treasury-approver", "bank-a-treasury-approver", "Bank A treasury approver", workflow.RoleTreasuryApprover, BankA, ""),
		u("tok-bank-b-compliance", "bank-b-compliance", "Bank B sanctions review", workflow.RoleCompliance, BankB, ""),
		u("tok-bank-b-treasury", "bank-b-treasury", "Bank B liquidity desk", workflow.RoleTreasury, BankB, ""),
		u("tok-bank-b-treasury-approver", "bank-b-treasury-approver", "Bank B treasury approver", workflow.RoleTreasuryApprover, BankB, ""),
		u("tok-bank-c-treasury", "bank-c-treasury", "Bank C liquidity desk", workflow.RoleTreasury, BankC, ""),
		u("tok-operator-ops", "operator-ops", "the network operations", workflow.RoleOperatorOps, "", ""),
	}
}
