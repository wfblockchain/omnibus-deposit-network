// Package test drives payments-svc end to end as a black box: it builds the
// demo binary, starts it against anvil, the Foundry-built contracts, the
// real OFAC SDN list and a database, and runs the business week through the
// generated Kratos HTTP clients (and one call over gRPC).
package test

import (
	"context"
	"encoding/json"
	nethttp "net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	kerrors "github.com/go-kratos/kratos/v2/errors"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/moov-io/iso20022/pkg/camt_v08"
	"github.com/moov-io/iso20022/pkg/document"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "omnibus-deposit-network/services/payments-svc/api/payments/v1"
)

const (
	aRouting = "123456780"
	bRouting = "234567898"
	cRouting = "012345672"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func code(err error) int { return int(kerrors.FromError(err).Code) }

func usd(v string) *pb.Money { return &pb.Money{Amount: v, Currency: "USD"} }

func TestBusinessWeek(t *testing.T) {
	s := start(t)
	c := newClients(t, s.base)

	pay := func(token, key, name, routing, account, amount, priority string) *pb.Payment {
		t.Helper()
		r, err := c.pay.CreatePayment(withKey(as(token), key), &pb.CreatePaymentRequest{
			Creditor: &pb.Party{Name: name, RoutingNumber: routing, Account: account}, Amount: usd(amount), Priority: priority})
		if err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
		return r.Payment
	}
	approve := func(id, token string) *pb.Payment {
		t.Helper()
		p, err := c.pay.ApprovePayment(as(token), &pb.ApprovePaymentRequest{Id: id})
		if err != nil {
			t.Fatalf("approve %s as %s: %v", id, token, err)
		}
		return p
	}
	get := func(id, token string) *pb.Payment {
		t.Helper()
		p, err := c.pay.GetPayment(as(token), &pb.GetPaymentRequest{Id: id})
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return p
	}
	waitStatus := func(id, token, status string) *pb.Payment {
		t.Helper()
		var p *pb.Payment
		eventually(t, id+" "+status, 60*time.Second, func() bool {
			p = get(id, token)
			return p.Status == status
		})
		return p
	}
	balance := func(token string) string {
		t.Helper()
		a, err := c.acct.GetAccount(as(token), &pb.GetAccountRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return a.Balance.Amount
	}
	waitBalance := func(token, want string) {
		t.Helper()
		eventually(t, token+" balance "+want, 60*time.Second, func() bool { return balance(token) == want })
	}
	clock := func(ts string) {
		t.Helper()
		at, _ := time.Parse(time.RFC3339, ts)
		if _, err := c.op.SetClock(as("tok-operator-ops"), &pb.SetClockRequest{Time: timestamppb.New(at)}); err != nil {
			t.Fatalf("clock %s: %v", ts, err)
		}
	}
	eq := func(what string, got, want any) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %v, want %v", what, got, want)
		}
	}

	// ── Strangers, and the ERP's webhook ────────────────────────────────
	if _, err := c.acct.GetAccount(context.Background(), &pb.GetAccountRequest{}); code(err) != 401 {
		t.Fatalf("no token: %v", err)
	}
	if _, err := c.acct.GetAccount(as("tok-nobody"), &pb.GetAccountRequest{}); code(err) != 401 {
		t.Fatalf("unknown token: %v", err)
	}
	erp, erpURL := newERP(t)
	if _, err := c.hook.CreateWebhook(as("tok-nw-maker"), &pb.CreateWebhookRequest{Url: erpURL}); code(err) != 403 {
		t.Fatalf("a maker registered a webhook: %v", err)
	}
	if _, err := c.hook.CreateWebhook(as("tok-nw-cfo"), &pb.CreateWebhookRequest{Url: "http://erp.example/x"}); code(err) != 400 {
		t.Fatalf("plain http to a public host was accepted: %v", err)
	}
	hook, err := c.hook.CreateWebhook(as("tok-nw-cfo"), &pb.CreateWebhookRequest{Url: erpURL + "/operator-events"})
	if err != nil || !strings.HasPrefix(hook.Secret, "whsec_") {
		t.Fatalf("webhook: %v %+v", err, hook)
	}
	erp.secret = hook.Secret
	if l, _ := c.hook.ListWebhooks(as("tok-nw-cfo"), &pb.ListWebhooksRequest{}); len(l.Webhooks) != 1 || l.Webhooks[0].Secret != "" {
		t.Fatalf("list shows secrets or misses the endpoint: %+v", l)
	}

	// ── Before paying: who is on the network, and is the payee right? ───
	hdr := nethttp.Header{} // kratos uses it as the request header map too, so it must not be nil
	parts, err := c.pay.ListParticipants(withRequestID(as("tok-nw-maker"), "req-42"), &pb.ListParticipantsRequest{}, khttp.Header(&hdr))
	if err != nil || len(parts.Participants) != 3 {
		t.Fatalf("participants: %v %+v", err, parts)
	}
	eq("Request-Id echoed", hdr.Get("Request-Id"), "req-42")
	chk, err := c.pay.VerifyPayee(as("tok-nw-maker"), &pb.Party{Name: "Contosso Ltd", RoutingNumber: bRouting, Account: "B-4000001"})
	if err != nil || chk.AccountStatus != "OPEN" || chk.NameMatch != "CLOSE_MATCH" || chk.RegisteredName != "Contoso Ltd" {
		t.Fatalf("payee check: %v %+v", err, chk)
	}
	if _, err := c.pay.VerifyPayee(as("tok-nw-maker"), &pb.Party{Name: "X", RoutingNumber: "111111118", Account: "1"}); code(err) != 400 {
		t.Fatalf("a non-member routing number: %v", err)
	}
	if _, err := c.pay.VerifyPayee(as("tok-nw-maker"), &pb.Party{Name: "X", RoutingNumber: "111111119", Account: "1"}); code(err) != 400 {
		t.Fatalf("a bad check digit passed: %v", err)
	}

	// ── Friday 16:30: three netted payments, one cycle ─────────────────
	clock("2026-10-02T16:30:00-04:00")
	nw1 := pay("tok-nw-maker", "po-4471", "Contoso Ltd", bRouting, "B-4000001", "12000000.00", "NORMAL")
	eq("two approvals above $10m", nw1.ApprovalsRequired, int32(2))
	eq("partly approved", approve(nw1.Id, "tok-nw-treasurer").IsoStatus, "PATC")
	eq("queued", approve(nw1.Id, "tok-nw-cfo").Status, "QUEUED_FOR_NETTING")
	co1 := pay("tok-co-maker", "co-1", "Fabrikam Inc", cRouting, "C-5000001", "10000000.00", "NORMAL")
	approve(co1.Id, "tok-co-approver")
	fa1 := pay("tok-fa-maker", "fa-1", "Northwind Corp", aRouting, "A-3000001", "9000000.00", "NORMAL")
	approve(fa1.Id, "tok-fa-approver")
	cyc, err := c.op.RunNettingCycle(as("tok-operator-ops"), &pb.RunNettingCycleRequest{})
	if err != nil || cyc.Discharged != 3 || cyc.Net.Amount != "3000000.00" || cyc.Gross.Amount != "31000000.00" {
		t.Fatalf("netting: %v %+v", err, cyc)
	}
	for id, tok := range map[string]string{nw1.Id: "tok-nw-maker", co1.Id: "tok-co-maker", fa1.Id: "tok-fa-maker"} {
		eq("netted payment credited", waitStatus(id, tok, "SETTLED").IsoStatus != "", true)
	}
	waitBalance("tok-nw-maker", "147000000.00")
	waitBalance("tok-co-maker", "82000000.00")
	waitBalance("tok-fa-maker", "71000000.00")

	// ── A pain.001 file: two transactions accepted, one refused ────────
	xml, err := os.ReadFile("../internal/iso/testdata/pain001_northwind.xml")
	if err != nil {
		t.Fatal(err)
	}
	file, err := c.pay.SubmitPaymentFile(withKey(as("tok-nw-maker"), "file-1005"), &pb.SubmitPaymentFileRequest{Xml: string(xml)})
	if err != nil {
		t.Fatalf("pain.001: %v", err)
	}
	if len(file.Payments) != 2 || len(file.Rejections) != 1 || file.Rejections[0].ReasonCode != "RC01" ||
		!strings.Contains(file.StatusReport.Xml, "<GrpSts>PART</GrpSts>") || file.StatusReport.MessageType != "pain.002.001.11" {
		t.Fatalf("file: %+v", file)
	}
	if file.Payments[1].Priority != "URGENT" || file.Payments[0].EndToEndId != "INV-2026-7781" {
		t.Fatalf("file payments: %+v", file.Payments)
	}
	again, err := c.pay.SubmitPaymentFile(withKey(as("tok-nw-maker"), "file-1005"), &pb.SubmitPaymentFileRequest{Xml: string(xml)})
	if err != nil || !again.DuplicateRequest || again.Id != file.Id {
		t.Fatalf("file replay: %v %+v", err, again)
	}
	if _, err := c.pay.SubmitPaymentFile(withKey(as("tok-nw-maker"), "file-1005-b"), &pb.SubmitPaymentFileRequest{Xml: string(xml)}); code(err) != 409 || kerrors.FromError(err).Reason != "DU01" {
		t.Fatalf("same MsgId under a new key: %v", err)
	}
	if _, err := c.pay.CancelPayment(as("tok-nw-maker"), &pb.CancelPaymentRequest{Id: file.Payments[0].Id}); err != nil {
		t.Fatal(err)
	}
	if p, err := c.pay.DeclinePayment(as("tok-nw-cfo"), &pb.DeclinePaymentRequest{Id: file.Payments[1].Id, Reason: "duplicate of PO-88120"}); err != nil || p.Status != "CANCELLED" {
		t.Fatalf("decline: %v %+v", err, p)
	}
	st, err := c.pay.GetPaymentFileStatusReport(as("tok-nw-maker"), &pb.GetPaymentFileRequest{Id: file.Id})
	if err != nil || strings.Count(st.Xml, "<TxSts>CANC</TxSts>") != 2 || !strings.Contains(st.Xml, "<Cd>RC01</Cd>") {
		t.Fatalf("file pain.002 after cancel and decline: %v\n%s", err, st.GetXml())
	}

	// ── Friday 18:30: a real SDN entry; the funds are blocked ───────────
	clock("2026-10-02T18:30:00-04:00")
	sdn := pay("tok-nw-maker", "po-4480", "Aerocaribbean Airlines", bRouting, "B-7700001", "500000.00", "NORMAL")
	eq("payee check on an unknown account", sdn.PayeeCheck.AccountStatus, "NOT_FOUND")
	approve(sdn.Id, "tok-nw-treasurer")
	eq("held", approve(sdn.Id, "tok-nw-cfo").Status, "ON_HOLD")
	holds, err := c.ops.ListHolds(as("tok-bank-a-compliance"), &pb.ListHoldsRequest{})
	if err != nil || len(holds.Holds) != 1 || !strings.Contains(holds.Holds[0].SanctionsMatch, "SDN #36") || !strings.Contains(holds.Holds[0].SanctionsMatch, "CUBA") {
		t.Fatalf("compliance queue: %v %+v", err, holds)
	}
	eq("the client does not see the list match", get(sdn.Id, "tok-nw-maker").SanctionsMatch, "")
	if _, err := c.ops.ListHolds(as("tok-bank-a-treasury"), &pb.ListHoldsRequest{}); code(err) != 403 {
		t.Fatalf("treasury read the sanctions queue: %v", err)
	}
	blocked, err := c.ops.BlockHold(as("tok-bank-a-compliance"), &pb.HoldDecisionRequest{Id: sdn.Id, Note: "confirmed SDN #36, case 26-0412"})
	if err != nil || blocked.Status != "BLOCKED" || blocked.IsoStatus != "BLCK" {
		t.Fatalf("block: %v %+v", err, blocked)
	}
	eq("OFAC report due (Columbus Day skipped)", blocked.OfacReportDue.AsTime().In(mustET()).Format("2006-01-02"), "2026-10-19")
	eq("funds left the client's account", balance("tok-nw-maker"), "146500000.00")
	if holds, _ = c.ops.ListHolds(as("tok-bank-a-compliance"), &pb.ListHoldsRequest{}); len(holds.Cases) != 1 || len(holds.Holds) != 0 {
		t.Fatalf("cases: %+v", holds)
	}

	// ── Saturday 11:00: the business problem ───────────────────────────
	clock("2026-10-03T11:00:00-04:00")
	req := &pb.CreatePaymentRequest{Creditor: &pb.Party{Name: "Contoso Ltd", RoutingNumber: bRouting, Account: "B-4000001"},
		Amount: usd("25000000.00"), Priority: "URGENT", Remittance: "INV-88213"}
	first, err := c.pay.CreatePayment(withKey(as("tok-nw-maker"), "inv-88213"), req)
	if err != nil {
		t.Fatal(err)
	}
	big := first.Payment
	replay, err := c.pay.CreatePayment(withKey(as("tok-nw-maker"), "inv-88213"), req)
	if err != nil || !replay.DuplicateRequest || replay.Payment.Id != big.Id {
		t.Fatalf("replay: %v %+v", err, replay)
	}
	changed := proto(req)
	changed.Amount = usd("26000000.00")
	if _, err := c.pay.CreatePayment(withKey(as("tok-nw-maker"), "inv-88213"), changed); code(err) != 422 {
		t.Fatalf("key reuse with a different body: %v", err)
	}
	if _, err := c.pay.CreatePayment(as("tok-nw-maker"), req); code(err) != 400 {
		t.Fatalf("no Idempotency-Key: %v", err)
	}
	dupE2E := proto(req)
	dupE2E.EndToEndId = big.EndToEndId
	if _, err := c.pay.CreatePayment(withKey(as("tok-nw-maker"), "inv-88213-b"), dupE2E); code(err) != 409 || kerrors.FromError(err).Reason != "DU04" {
		t.Fatalf("reused endToEndId: %v", err)
	}
	if _, err := c.pay.ApprovePayment(as("tok-nw-maker"), &pb.ApprovePaymentRequest{Id: big.Id}); code(err) != 403 {
		t.Fatalf("the maker approved: %v", err)
	}
	approve(big.Id, "tok-nw-treasurer")
	if _, err := c.pay.ApprovePayment(as("tok-nw-treasurer"), &pb.ApprovePaymentRequest{Id: big.Id}); code(err) != 409 {
		t.Fatalf("an approver counted twice: %v", err)
	}
	p := approve(big.Id, "tok-nw-cfo")
	eq("waits for liquidity", p.Status, "AWAITING_LIQUIDITY")
	if !strings.Contains(p.RouteNote, "Fedwire is closed") || !strings.Contains(p.RouteNote, "RTP") {
		t.Fatalf("route note: %q", p.RouteNote)
	}
	liq, err := c.ops.GetLiquidity(as("tok-bank-a-treasury"), &pb.GetLiquidityRequest{})
	if err != nil || len(liq.AwaitingLiquidity) != 1 || !openAlert(liq, "LIQUIDITY") || liq.FundingInstrument != "Fedwire is closed; FedNow LMT can fund it now" {
		t.Fatalf("treasury view: %v %+v", err, liq)
	}
	fund, err := c.ops.FundOmnibus(as("tok-bank-a-treasury"), &pb.FundOmnibusRequest{Amount: usd("10000000.00")})
	if err != nil || fund.Instrument != "LMT1" || fund.PaymentsReleased != 1 {
		t.Fatalf("weekend funding: %v %+v", err, fund)
	}
	p = waitStatus(big.Id, "tok-nw-maker", "SETTLED")
	eventually(t, "ACCC", 30*time.Second, func() bool { p = get(big.Id, "tok-nw-maker"); return p.IsoStatus == "ACCC" })
	raw, _ := protojson.Marshal(p)
	if regexp.MustCompile(`(?i)A-dT|B-dT|token|mint|omnibus`).Match(raw) {
		t.Fatalf("the client's view shows token mechanics: %s", raw)
	}
	staffView, err := c.ops.GetBankPayment(as("tok-bank-a-treasury"), &pb.GetBankPaymentRequest{Id: big.Id})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := protojson.Marshal(staffView); !strings.Contains(string(raw), "tokenized as A-dT") || !strings.Contains(string(raw), "B-dT minted") {
		t.Fatalf("the bank's staff view lacks the ledger detail: %s", raw)
	}
	for _, e := range p.History {
		t.Logf("%s  %-4s %-19s %s", e.At.AsTime().In(mustET()).Format("Mon 15:04"), e.IsoStatus, e.Status, e.Note)
	}
	waitBalance("tok-nw-maker", "121500000.00")
	waitBalance("tok-co-maker", "107000000.00")
	eventually(t, "low watermark alert", 30*time.Second, func() bool {
		liq, _ = c.ops.GetLiquidity(as("tok-bank-a-treasury"), &pb.GetLiquidityRequest{})
		return openAlert(liq, "LOW_WATERMARK") && !openAlert(liq, "LIQUIDITY")
	})

	// ── Saturday 11:30: a closed account ───────────────────────────────
	clock("2026-10-03T11:30:00-04:00")
	dave := pay("tok-nw-maker", "po-dave", "Dave Ltd", bRouting, "B-2000002", "1000000.00", "URGENT")
	eq("payee check", dave.PayeeCheck.AccountStatus, "CLOSED")
	eq("an extra approval overrides it", dave.ApprovalsRequired, int32(2))
	approve(dave.Id, "tok-nw-treasurer")
	approve(dave.Id, "tok-nw-cfo")
	eq("rejected by the receiving bank", waitStatus(dave.Id, "tok-nw-maker", "REJECTED").ReasonCode, "AC04")
	waitBalance("tok-nw-maker", "121500000.00")

	// ── Monday 09:00: Fedwire open; top-up, and a defund ───────────────
	clock("2026-10-05T09:00:00-04:00")
	if fund, err = c.ops.FundOmnibus(as("tok-bank-a-treasury"), &pb.FundOmnibusRequest{Amount: usd("10000000.00")}); err != nil || fund.Instrument != "BTRC" {
		t.Fatalf("weekday funding: %v %+v", err, fund)
	}
	eventually(t, "watermark alert closed", 30*time.Second, func() bool {
		liq, _ = c.ops.GetLiquidity(as("tok-bank-a-treasury"), &pb.GetLiquidityRequest{})
		return !openAlert(liq, "LOW_WATERMARK")
	})
	df, err := c.ops.RequestDefund(as("tok-bank-b-treasury"), &pb.RequestDefundRequest{Amount: usd("20000000.00")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ops.ApproveDefund(as("tok-bank-b-treasury"), &pb.ApproveDefundRequest{Id: df.Id}); code(err) != 403 {
		t.Fatalf("the requester approved: %v", err)
	}
	if _, err := c.ops.ApproveDefund(as("tok-bank-a-treasury-approver"), &pb.ApproveDefundRequest{Id: df.Id}); code(err) != 404 {
		t.Fatalf("another bank's approver reached it: %v", err)
	}
	if df, err = c.ops.ApproveDefund(as("tok-bank-b-treasury-approver"), &pb.ApproveDefundRequest{Id: df.Id}); err != nil || df.Status != "COMPLETED" {
		t.Fatalf("defund: %v %+v", err, df)
	}

	// ── Monday 09:15: the hourly cycle; one pulled before it ────────────
	clock("2026-10-05T09:15:00-04:00")
	mon := pay("tok-co-maker", "co-2", "Northwind Corp", aRouting, "A-3000001", "2000000.00", "NORMAL")
	approve(mon.Id, "tok-co-approver")
	pulled := pay("tok-co-maker", "co-3", "Fabrikam Inc", cRouting, "C-5000001", "3000000.00", "NORMAL")
	approve(pulled.Id, "tok-co-approver")
	if p, err := c.pay.CancelPayment(as("tok-co-maker"), &pb.CancelPaymentRequest{Id: pulled.Id}); err != nil || p.Status != "CANCELLED" {
		t.Fatalf("cancel before the cycle: %v %+v", err, p)
	}
	waitBalance("tok-co-maker", "105000000.00")
	nv, err := c.op.GetNetwork(as("tok-operator-ops"), &pb.GetNetworkRequest{})
	if err != nil || nv.NextScheduledCycle.AsTime().In(mustET()).Format("15:04") != "10:00" {
		t.Fatalf("next cycle: %v %+v", err, nv.GetNextScheduledCycle())
	}
	clock("2026-10-05T10:00:30-04:00")
	waitStatus(mon.Id, "tok-co-maker", "SETTLED")
	nv, _ = c.op.GetNetwork(as("tok-operator-ops"), &pb.GetNetworkRequest{})
	eq("the latest cycle ran on schedule", nv.Cycles[0].Trigger, "scheduled")
	waitBalance("tok-nw-maker", "123500000.00")

	// ── Close: the Fed and the ledger agree; the statement is camt.053 ──
	rec, err := c.op.Reconcile(as("tok-operator-ops"), &pb.ReconcileRequest{})
	if err != nil || rec.ReconciliationBreak || !rec.InvariantsHold {
		t.Fatalf("reconcile: %v %+v", err, rec)
	}
	eq("Fed joint account", rec.FedBalance.Amount, "90000000.00") // 90 funded, +10 LMT, +10 BTRC, -20 defund
	eq("ledger", rec.LedgerTotal.Amount, "90000000.00")
	stmt, err := c.acct.GetStatement(as("tok-nw-maker"), &pb.GetAccountRequest{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.ParseIso20022Document([]byte(stmt.Xml))
	if err != nil {
		t.Fatalf("camt.053 does not parse: %v", err)
	}
	bals := doc.InspectMessage().(*camt_v08.BankToCustomerStatementV08).Stmt[0].Bal
	eq("camt.053 closing balance", bals[len(bals)-1].Amt.Value, 123_500_000.0)
	if _, err := c.pay.GetPayment(as("tok-co-maker"), &pb.GetPaymentRequest{Id: big.Id}); code(err) != 404 {
		t.Fatalf("another client read Northwind's payment: %v", err)
	}
	if _, err := c.op.GetNetwork(as("tok-bank-a-treasury"), &pb.GetNetworkRequest{}); code(err) != 403 {
		t.Fatalf("a bank read the operator's console: %v", err)
	}

	// ── gRPC: the same services on the other transport ─────────────────
	conn, err := kgrpc.DialInsecure(context.Background(), kgrpc.WithEndpoint(s.grpc))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	gctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer tok-nw-maker")
	acct, err := pb.NewAccountServiceClient(conn).GetAccount(gctx, &pb.GetAccountRequest{})
	if err != nil || acct.Balance.Amount != "123500000.00" {
		t.Fatalf("gRPC: %v %+v", err, acct)
	}

	// ── The ERP heard every status change of the $25m payment, in order ─
	var got []string
	eventually(t, "webhooks", 30*time.Second, func() bool {
		erp.mu.Lock()
		defer erp.mu.Unlock()
		got = got[:0]
		for _, ev := range erp.events {
			if ev.Data["paymentId"] == big.Id {
				got = append(got, ev.Type+"/"+ev.Data["isoStatus"].(string))
			}
		}
		return len(got) >= 9
	})
	want := "payment.awaiting_approval/RCVD payment.awaiting_approval/ACTC payment.awaiting_approval/PATC " +
		"payment.in_process/ACCP payment.awaiting_liquidity/PDNG payment.in_process/ACSP " +
		"payment.pending_receiver/PDNG payment.settled/ACSC payment.credited/ACCC"
	eq("webhooks for the $25m payment", strings.Join(got, " "), want)
	erp.mu.Lock()
	if len(erp.bad) != 0 {
		t.Errorf("signature failures: %v", erp.bad)
	}
	erp.mu.Unlock()
	ds, err := c.hook.ListWebhookDeliveries(as("tok-nw-cfo"), &pb.ListWebhookDeliveriesRequest{Id: hook.Id})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds.Deliveries {
		if d.Status == "FAILED" {
			t.Errorf("failed delivery: %+v", d)
		}
	}
	t.Logf("webhooks: %d deliveries to Northwind's ERP, all signed", len(ds.Deliveries))

	// ── The portal: pages built from the same API, forms with CSRF ──────
	portalChecks(t, s.base, xml)
}

func openAlert(l *pb.Liquidity, kind string) bool {
	if l == nil {
		return false
	}
	for _, a := range l.Alerts {
		if a.Kind == kind && a.Open {
			return true
		}
	}
	return false
}

func proto(r *pb.CreatePaymentRequest) *pb.CreatePaymentRequest {
	cp := &pb.CreatePaymentRequest{DebtorAccount: r.DebtorAccount, Creditor: r.Creditor, Amount: r.Amount,
		Priority: r.Priority, Remittance: r.Remittance, EndToEndId: r.EndToEndId}
	return cp
}

func mustET() *time.Location {
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.UTC
	}
	return l
}
