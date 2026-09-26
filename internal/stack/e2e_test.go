package stack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"omnibus-deposit-network/internal/api"
	"omnibus-deposit-network/internal/demo"
	"omnibus-deposit-network/internal/stack"
	"omnibus-deposit-network/internal/web"
	"omnibus-deposit-network/internal/webhook"
)

// TestBusinessWorkflowThroughTheWeb2Layer runs the business scenario the
// way people would: corporate treasurers and bank and operator operations staff,
// through the REST API and the portals only. It needs anvil and a Foundry
// build, and skips without them.
func TestBusinessWorkflowThroughTheWeb2Layer(t *testing.T) {
	bin := os.Getenv("ANVIL_BIN")
	if bin == "" {
		if p, err := exec.LookPath("anvil"); err == nil {
			bin = p
		}
	}
	if bin == "" {
		t.Skip("anvil not found: set ANVIL_BIN or put Foundry on PATH")
	}
	artifacts := filepath.Join("..", "..", "contracts", "out")
	if _, err := os.Stat(filepath.Join(artifacts, "OmnibusNetting.sol", "OmnibusNetting.json")); err != nil {
		t.Skip("contracts not built: run `forge build` in contracts/")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	rpc, stopAnvil, err := demo.StartAnvil(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	defer stopAnvil()
	st, err := stack.Build(ctx, rpc, artifacts, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", api.Handler(st.Service, st.Hooks))
	mux.Handle("/", web.Handler(st.Service))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &client{t: t, base: srv.URL}
	tick := func() {
		t.Helper()
		if err := st.Service.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	balance := func(token string) string {
		t.Helper()
		_, v := c.json("GET", "/api/v1/account", token, nil, nil)
		return v["balance"].(string)
	}
	at := func(ts string) {
		t.Helper()
		if code, v := c.json("POST", "/api/v1/operator/clock", "tok-operator-ops", map[string]string{"time": ts}, nil); code != 200 {
			t.Fatalf("clock: %d %v", code, v)
		}
	}

	// ── The API refuses strangers ────────────────────────────────────────
	if code, _ := c.json("GET", "/api/v1/payments", "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code := c.get("/api/v1/openapi.yaml", nil); code != 200 {
		t.Fatalf("openapi: %d", code)
	}

	// ── Northwind's ERP registers for status webhooks ─────────────────────
	hooks := &hookReceiver{}
	erp := httptest.NewServer(hooks)
	defer erp.Close()
	if code, _ := c.json("POST", "/api/v1/webhooks", "tok-nw-maker", map[string]string{"url": erp.URL}, nil); code != http.StatusForbidden {
		t.Fatalf("a maker registered a webhook: %d", code)
	}
	code, sub := c.json("POST", "/api/v1/webhooks", "tok-nw-cfo", map[string]string{"url": erp.URL + "/operator-events"}, nil)
	if code != http.StatusCreated || !strings.HasPrefix(sub["secret"].(string), "whsec_") {
		t.Fatalf("webhook: %d %v", code, sub)
	}
	hooks.secret = sub["secret"].(string)

	// ── Before paying: is the payee's bank on the network, and is the
	// payee who we think it is? ────────────────────────────────────────
	_, parts := c.json("GET", "/api/v1/network/participants", "tok-nw-maker", nil, map[string]string{"Request-Id": "req-42"})
	if n := len(parts["participants"].([]any)); n != 3 {
		t.Fatalf("participants: %v", parts)
	}
	if id := c.lastHeader.Get("Request-Id"); id != "req-42" {
		t.Fatalf("Request-Id not echoed: %q", id)
	}
	_, chk := c.json("POST", "/api/v1/payees/verify", "tok-nw-maker",
		map[string]string{"name": "Contosso Ltd", "routingNumber": stack.BankBABA, "account": "B-4000001"}, nil)
	if chk["accountStatus"] != "OPEN" || chk["nameMatch"] != "CLOSE_MATCH" || chk["registeredName"] != "Contoso Ltd" {
		t.Fatalf("payee check: %v", chk)
	}

	// ── Friday 16:30: a corporate uses the portal (HTML forms) ────────────
	at("2026-10-02T16:30:00-04:00")
	maker := c.browser("tok-nw-maker")
	page := maker.page("/corp")
	if !strings.Contains(page, "Northwind Corp") || !strings.Contains(page, "150,000,000.00") {
		t.Fatalf("corporate dashboard missing account:\n%s", page)
	}
	loc := maker.form("/corp/payments", page, url.Values{
		"key": {"web-po-1"}, "name": {"Contoso Ltd"}, "routing": {stack.BankBABA}, "account": {"B-4000001"},
		"amount": {"12000000.00"}, "priority": {"NORMAL"}, "remittance": {"PO-4471"},
	})
	if !strings.HasPrefix(loc, "/orders/PO-") {
		t.Fatalf("create redirected to %q", loc)
	}
	nwPO1 := strings.TrimPrefix(strings.SplitN(loc, "?", 2)[0], "/orders/")
	if p := maker.page("/orders/" + nwPO1); !strings.Contains(p, "AWAITING_APPROVAL") {
		t.Fatalf("new order page:\n%s", p)
	}
	for _, tok := range []string{"tok-nw-treasurer", "tok-nw-cfo"} { // $12m needs two approvers
		b := c.browser(tok)
		b.form("/orders/"+nwPO1+"/approve", b.page("/orders/"+nwPO1), url.Values{})
	}
	c.expectStatus(nwPO1, "tok-nw-maker", "QUEUED_FOR_NETTING")

	// Two more netted payments, via the API, from the other banks' clients.
	coPO := c.create("tok-co-maker", "co-1", "Fabrikam Inc", stack.BankCABA, "C-5000001", "10000000.00", "NORMAL")
	c.approve(coPO, "tok-co-approver", 200)
	faPO := c.create("tok-fa-maker", "fa-1", "Northwind Corp", stack.BankAABA, "A-3000001", "9000000.00", "NORMAL")
	c.approve(faPO, "tok-fa-approver", 200)

	code, cyc := c.json("POST", "/api/v1/operator/netting/run", "tok-operator-ops", nil, nil)
	if code != 200 || cyc["Discharged"].(float64) != 3 {
		t.Fatalf("netting: %d %v", code, cyc)
	}
	tick()
	for tok, id := range map[string]string{"tok-nw-maker": nwPO1, "tok-co-maker": coPO, "tok-fa-maker": faPO} {
		c.expectStatus(id, tok, "SETTLED")
	}
	eq(t, "Northwind after netting", balance("tok-nw-maker"), "147000000.00")
	eq(t, "Contoso after netting", balance("tok-co-maker"), "82000000.00")
	eq(t, "Fabrikam after netting", balance("tok-fa-maker"), "71000000.00")

	// ── Friday 18:30: a sanctions hit is held, reviewed and rejected ──────
	at("2026-10-02T18:30:00-04:00")
	petro := c.create("tok-nw-maker", "po-petrov", "Petrov Trading LLC", stack.BankBABA, "B-2000009", "500000.00", "NORMAL")
	c.approve(petro, "tok-nw-treasurer", 200)
	c.expectStatus(petro, "tok-nw-maker", "ON_HOLD")
	_, holds := c.json("GET", "/api/v1/ops/holds", "tok-bank-a-compliance", nil, nil)
	if n := len(holds["holds"].([]any)); n != 1 {
		t.Fatalf("holds: %d", n)
	}
	if p := c.browser("tok-bank-a-compliance").page("/ops"); !strings.Contains(p, "Sanctions holds") || !strings.Contains(p, petro) {
		t.Fatalf("ops portal does not show the hold:\n%s", p)
	}
	// An SDN has an interest in these funds: OFAC requires blocking, not
	// returning them, and a report within 10 business days.
	c.json("POST", "/api/v1/ops/holds/"+petro+"/block", "tok-bank-a-compliance", map[string]string{"note": "confirmed SDN match"}, nil)
	o := c.expectStatus(petro, "tok-nw-maker", "BLOCKED")
	eq(t, "blocked ISO status", o["isoStatus"], "BLCK")
	eq(t, "OFAC report due (Columbus Day skipped)", o["ofacReportDue"], "2026-10-19T00:00:00-04:00")
	eq(t, "funds left the account for the blocked account", balance("tok-nw-maker"), "146500000.00")
	_, holds = c.json("GET", "/api/v1/ops/holds", "tok-bank-a-compliance", nil, nil)
	if n := len(holds["cases"].([]any)); n != 1 {
		t.Fatalf("sanctions cases: %v", holds)
	}

	// ── Saturday 11:00: the business problem. $25m to a supplier at another
	// bank. Fedwire is closed until Sunday 21:00 and RTP stops at $10m. ────
	at("2026-10-03T11:00:00-04:00")
	req := map[string]any{"creditor": map[string]string{"name": "Contoso Ltd", "routingNumber": stack.BankBABA, "account": "B-4000001"},
		"amount": "25000000.00", "priority": "URGENT", "remittance": "INV-88213"}
	code, first := c.json("POST", "/api/v1/payments", "tok-nw-maker", req, map[string]string{"Idempotency-Key": "inv-88213"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, first)
	}
	big := first["id"].(string)
	code, again := c.json("POST", "/api/v1/payments", "tok-nw-maker", req, map[string]string{"Idempotency-Key": "inv-88213"})
	if code != http.StatusOK || again["id"] != big {
		t.Fatalf("idempotent replay: %d %v", code, again)
	}
	req["amount"] = "26000000.00"
	if code, _ := c.json("POST", "/api/v1/payments", "tok-nw-maker", req, map[string]string{"Idempotency-Key": "inv-88213"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("key reuse with a different body: %d", code)
	}
	req["amount"], req["endToEndId"] = "25000000.00", first["endToEndId"]
	if code, v := c.json("POST", "/api/v1/payments", "tok-nw-maker", req, map[string]string{"Idempotency-Key": "inv-88213-b"}); code != http.StatusConflict ||
		!strings.Contains(v["message"].(string), "DU04") {
		t.Fatalf("duplicate endToEndId under a new key: %d %v", code, v)
	}
	c.approve(big, "tok-nw-maker", http.StatusForbidden) // maker cannot approve
	c.approve(big, "tok-nw-treasurer", 200)
	o = c.expectStatus(big, "tok-nw-maker", "AWAITING_APPROVAL") // 1 of 2
	eq(t, "partly approved", o["isoStatus"], "PATC")
	c.approve(big, "tok-nw-cfo", 200)
	o = c.expectStatus(big, "tok-nw-maker", "AWAITING_LIQUIDITY")
	note := o["routeNote"].(string)
	if !strings.Contains(note, "Fedwire is closed") || !strings.Contains(note, "RTP") {
		t.Fatalf("route note does not explain the business problem: %q", note)
	}

	_, liq := c.json("GET", "/api/v1/ops/liquidity", "tok-bank-a-treasury", nil, nil)
	if len(liq["awaitingLiquidity"].([]any)) != 1 || len(liq["alerts"].([]any)) == 0 {
		t.Fatalf("treasury does not see the waiting payment: %v", liq)
	}
	code, fund := c.json("POST", "/api/v1/ops/funding", "tok-bank-a-treasury", map[string]string{"amount": "10000000.00"}, nil)
	if code != 200 || fund["instrument"] != "LMT1" || fund["ordersReleased"].(float64) != 1 {
		t.Fatalf("weekend funding: %d %v", code, fund)
	}
	c.expectStatus(big, "tok-nw-maker", "PENDING_RECEIVER") // Bank B screens inbound
	tick()
	o = c.expectStatus(big, "tok-nw-maker", "SETTLED")
	eq(t, "ISO status: credited to the payee", o["isoStatus"], "ACCC")
	// The client sees an account and a payment, never the token mechanics;
	// its bank's staff see both.
	if raw, _ := json.Marshal(o); regexp.MustCompile(`(?i)A-dT|B-dT|token|mint|omnibus`).Match(raw) {
		t.Fatalf("the client's view shows token mechanics: %s", raw)
	}
	_, opsView := c.json("GET", "/api/v1/ops/payments", "tok-bank-a-treasury", nil, nil)
	if raw, _ := json.Marshal(opsView); !strings.Contains(string(raw), "tokenized as A-dT") {
		t.Fatalf("the bank's operations view lacks the token detail")
	}
	for _, e := range o["history"].([]any) {
		ev := e.(map[string]any)
		t.Logf("%s  %-4v %-19v %v (%v)", ev["at"], ev["iso"], ev["status"], ev["note"], ev["actor"])
	}
	eq(t, "payer", balance("tok-nw-maker"), "121500000.00")
	eq(t, "payee credited on a Saturday", balance("tok-co-maker"), "107000000.00")
	_, liq = c.json("GET", "/api/v1/ops/liquidity", "tok-bank-a-treasury", nil, nil)
	if a := openAlerts(liq, "LOW_WATERMARK"); a != 1 {
		t.Fatalf("Bank A is below its low watermark but has %d open alerts: %v", a, liq["alerts"])
	}

	// ── Saturday 11:30: the receiving bank rejects; the payer is made whole
	at("2026-10-03T11:30:00-04:00")
	dave := c.create("tok-nw-maker", "po-dave", "Dave Ltd", stack.BankBABA, "B-2000002", "1000000.00", "URGENT")
	c.approve(dave, "tok-nw-treasurer", 200)
	o = c.expectStatus(dave, "tok-nw-maker", "AWAITING_APPROVAL") // the payee check failed: one more approval overrides it
	eq(t, "payee check", o["payeeCheck"].(map[string]any)["accountStatus"], "CLOSED")
	c.approve(dave, "tok-nw-cfo", 200)
	c.expectStatus(dave, "tok-nw-maker", "PENDING_RECEIVER")
	tick()
	o = c.expectStatus(dave, "tok-nw-maker", "REJECTED")
	eq(t, "receiver reason", o["reasonCode"], "AC04")
	eq(t, "refunded", balance("tok-nw-maker"), "121500000.00")

	// ── Monday 09:00: Fedwire is open. Bank A tops up by BTRC, which
	// clears its watermark alert; Bank B takes the weekend's inflow back
	// to its master account, maker-checker ───────────────────────────────
	at("2026-10-05T09:00:00-04:00")
	code, fund = c.json("POST", "/api/v1/ops/funding", "tok-bank-a-treasury", map[string]string{"amount": "10000000.00"}, nil)
	if code != 200 || fund["instrument"] != "BTRC" {
		t.Fatalf("weekday funding: %d %v", code, fund)
	}
	tick()
	_, liq = c.json("GET", "/api/v1/ops/liquidity", "tok-bank-a-treasury", nil, nil)
	if a := openAlerts(liq, "LOW_WATERMARK"); a != 0 {
		t.Fatalf("watermark alert still open after funding: %v", liq["alerts"])
	}
	code, df := c.json("POST", "/api/v1/ops/defunds", "tok-bank-b-treasury", map[string]string{"amount": "20000000.00"}, nil)
	if code != http.StatusCreated {
		t.Fatalf("defund request: %d %v", code, df)
	}
	id := df["id"].(string)
	if code, _ := c.json("POST", "/api/v1/ops/defunds/"+id+"/approve", "tok-bank-b-treasury", nil, nil); code != http.StatusForbidden {
		t.Fatalf("requester approved its own defund: %d", code)
	}
	if code, _ := c.json("POST", "/api/v1/ops/defunds/"+id+"/approve", "tok-bank-a-treasury-approver", nil, nil); code != http.StatusNotFound {
		t.Fatalf("another bank's approver reached Bank B's defund: %d", code)
	}
	_, df = c.json("POST", "/api/v1/ops/defunds/"+id+"/approve", "tok-bank-b-treasury-approver", nil, nil)
	eq(t, "defund", df["status"], "COMPLETED")

	// ── Monday 09:15: a normal payment settles in the 10:00 scheduled cycle
	// with nobody pressing anything ─────────────────────────────────────
	at("2026-10-05T09:15:00-04:00")
	mon := c.create("tok-co-maker", "co-2", "Northwind Corp", stack.BankAABA, "A-3000001", "2000000.00", "NORMAL")
	c.approve(mon, "tok-co-approver", 200)
	c.expectStatus(mon, "tok-co-maker", "QUEUED_FOR_NETTING")
	pulled := c.create("tok-co-maker", "co-3", "Fabrikam Inc", stack.BankCABA, "C-5000001", "3000000.00", "NORMAL")
	c.approve(pulled, "tok-co-approver", 200)
	c.expectStatus(pulled, "tok-co-maker", "QUEUED_FOR_NETTING")
	if code, v := c.json("POST", "/api/v1/payments/"+pulled+"/cancel", "tok-co-maker", nil, nil); code != 200 || v["status"] != "CANCELLED" {
		t.Fatalf("cancel before the cycle: %d %v", code, v)
	}
	eq(t, "Contoso re-credited", balance("tok-co-maker"), "105000000.00")
	_, nv := c.json("GET", "/api/v1/operator/network", "tok-operator-ops", nil, nil)
	eq(t, "next cycle", nv["nextScheduledCycle"], "2026-10-05T10:00:00-04:00")
	at("2026-10-05T10:00:30-04:00")
	c.expectStatus(mon, "tok-co-maker", "SETTLED")
	_, nv = c.json("GET", "/api/v1/operator/network", "tok-operator-ops", nil, nil)
	cycles := nv["cycles"].([]any)
	eq(t, "cycle trigger", cycles[len(cycles)-1].(map[string]any)["Trigger"], "scheduled")
	eq(t, "Northwind after the scheduled cycle", balance("tok-nw-maker"), "123500000.00")

	// ── Close: the Fed and the operator agree; the portals render ──────────────────
	_, rec := c.json("POST", "/api/v1/operator/reconcile", "tok-operator-ops", nil, nil)
	if rec["Break"].(bool) || !rec["Invariants"].(bool) {
		t.Fatalf("reconciliation: %v", rec)
	}
	_, nv = c.json("GET", "/api/v1/operator/network", "tok-operator-ops", nil, nil)
	eq(t, "Fed joint", nv["fedJointAccount"], "90000000.00") // 20+40+30 funded, +10 LMT, +10 BTRC, -20 defund
	eq(t, "ledger", nv["ledgerTotal"], "90000000.00")
	if p := c.browser("tok-operator-ops").page("/operator"); !strings.Contains(p, "Members") || !strings.Contains(p, "books agree") {
		t.Fatalf("the operator console:\n%s", p)
	}
	if p := maker.page("/corp/statement"); !strings.Contains(p, "123,500,000.00") {
		t.Fatalf("statement:\n%s", p)
	}
	if code, _ := c.json("GET", "/api/v1/payments/"+big, "tok-co-maker", nil, nil); code != http.StatusNotFound {
		t.Fatalf("another client could read Northwind's payment: %d", code)
	}

	// ── Northwind's ERP heard every status change of the $25m payment, in
	// order, signed ─────────────────────────────────────────────────────
	st.Hooks.Flush(ctx)
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if len(hooks.bad) != 0 {
		t.Fatalf("webhook signature failures: %v", hooks.bad)
	}
	var got []string
	for _, ev := range hooks.events {
		if ev.Data["paymentId"] == big {
			got = append(got, ev.Type+"/"+ev.Data["isoStatus"].(string))
		}
	}
	want := []string{"payment.awaiting_approval/RCVD", "payment.awaiting_approval/ACTC", "payment.awaiting_approval/PATC",
		"payment.in_process/ACCP", "payment.awaiting_liquidity/PDNG", "payment.in_process/ACSP",
		"payment.pending_receiver/PDNG", "payment.settled/ACSC", "payment.credited/ACCC"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("webhooks for %s:\n got %v\nwant %v", big, got, want)
	}
	for _, ev := range hooks.events {
		if org := ev.Data["paymentId"]; org == nil {
			t.Fatalf("event without a payment: %+v", ev)
		}
	}
	_, ds := c.json("GET", "/api/v1/webhooks/"+sub["id"].(string)+"/deliveries", "tok-nw-cfo", nil, nil)
	for _, d := range ds["deliveries"].([]any) {
		if d.(map[string]any)["status"] != "delivered" {
			t.Fatalf("undelivered: %v", d)
		}
	}
	t.Logf("webhooks: %d events delivered to Northwind's ERP, all signed", len(hooks.events))
}

type hookReceiver struct {
	mu     sync.Mutex
	secret string
	events []struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	bad []error
}

func (h *hookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := webhook.Verify(h.secret, r.Header, body, time.Now(), 5*time.Minute); err != nil {
		h.bad = append(h.bad, err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var ev struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(body, &ev)
	h.events = append(h.events, ev)
}

func openAlerts(liq map[string]any, kind string) int {
	n := 0
	for _, a := range liq["alerts"].([]any) {
		m := a.(map[string]any)
		if m["kind"] == kind && m["open"] == true {
			n++
		}
	}
	return n
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

type client struct {
	t          *testing.T
	base       string
	lastHeader http.Header
}

func (c *client) json(method, path, token string, body any, headers map[string]string) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	c.lastHeader = resp.Header
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (c *client) get(path string, jar http.CookieJar) int {
	resp, err := (&http.Client{Jar: jar}).Get(c.base + path)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (c *client) create(token, key, name, aba, acct, amount, priority string) string {
	c.t.Helper()
	body := map[string]any{"creditor": map[string]string{"name": name, "routingNumber": aba, "account": acct}, "amount": amount, "priority": priority}
	code, v := c.json("POST", "/api/v1/payments", token, body, map[string]string{"Idempotency-Key": key})
	if code != http.StatusCreated {
		c.t.Fatalf("create %s: %d %v", key, code, v)
	}
	return v["id"].(string)
}

func (c *client) approve(id, token string, want int) {
	c.t.Helper()
	if code, v := c.json("POST", "/api/v1/payments/"+id+"/approve", token, nil, nil); code != want {
		c.t.Fatalf("approve %s as %s: %d %v", id, token, code, v)
	}
}

func (c *client) expectStatus(id, token, want string) map[string]any {
	c.t.Helper()
	_, v := c.json("GET", "/api/v1/payments/"+id, token, nil, nil)
	if v["status"] != want {
		c.t.Fatalf("%s: status %v, want %s; history %v", id, v["status"], want, v["history"])
	}
	return v
}

// browser is a signed-in portal session.
type browser struct {
	c  *client
	hc *http.Client
}

func (c *client) browser(token string) *browser {
	c.t.Helper()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.PostForm(c.base+"/login", url.Values{"token": {token}})
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		c.t.Fatalf("login %s: %d", token, resp.StatusCode)
	}
	return &browser{c: c, hc: hc}
}

func (b *browser) page(path string) string {
	b.c.t.Helper()
	resp, err := b.hc.Get(b.c.base + path)
	if err != nil {
		b.c.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		b.c.t.Fatalf("GET %s: %d\n%s", path, resp.StatusCode, body)
	}
	return string(body)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

// form posts a form with the CSRF token taken from a page, and returns
// where the server redirected.
func (b *browser) form(path, page string, v url.Values) string {
	b.c.t.Helper()
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		b.c.t.Fatalf("no CSRF token on the page for %s", path)
	}
	v.Set("csrf", m[1])
	resp, err := b.hc.PostForm(b.c.base+path, v)
	if err != nil {
		b.c.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		b.c.t.Fatalf("POST %s: %d", path, resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if strings.Contains(loc, "err=") {
		u, _ := url.Parse(loc)
		b.c.t.Fatalf("POST %s failed: %s", path, u.Query().Get("err"))
	}
	return loc
}
