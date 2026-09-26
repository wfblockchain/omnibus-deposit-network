// Package portal is the web front end of payments-svc: server-rendered pages
// for a client's treasury, a bank's operations staff and operator operations. It
// holds no business logic and no data; every page is built from calls to
// payments-svc through the generated Kratos HTTP clients, as the signed-in
// person.
package portal

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	kerrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "omnibus-deposit-network/services/payments-svc/api/payments/v1"
)

//go:embed templates/*.html
var files embed.FS

// User is a demo identity offered on the sign-in page.
type User struct {
	Token, Name, Role, Bank, Organization string
}

type session struct {
	token, csrf string
	who         *pb.Principal
}

// Portal serves the pages.
type Portal struct {
	pay   pb.PaymentServiceHTTPClient
	acct  pb.AccountServiceHTTPClient
	ops   pb.BankOperationsServiceHTTPClient
	op    pb.NetworkOperationsServiceHTTPClient
	users []User
	tpl   *template.Template
	et    *time.Location

	mu       sync.Mutex
	sessions map[string]*session
}

type tokenKey struct{}

// bearer puts the signed-in person's token on every API call.
func bearer() middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			if tr, ok := transport.FromClientContext(ctx); ok {
				if tok, _ := ctx.Value(tokenKey{}).(string); tok != "" {
					tr.RequestHeader().Set("Authorization", "Bearer "+tok)
				}
				if key, _ := ctx.Value(idemKey{}).(string); key != "" {
					tr.RequestHeader().Set("Idempotency-Key", key)
				}
			}
			return next(ctx, req)
		}
	}
}

type idemKey struct{}

// New connects to payments-svc at api.
func New(api string, users []User) (*Portal, func(), error) {
	conn, err := khttp.NewClient(context.Background(), khttp.WithEndpoint(api), khttp.WithMiddleware(bearer()),
		khttp.WithTimeout(90*time.Second))
	if err != nil {
		return nil, nil, err
	}
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		et = time.UTC
	}
	p := &Portal{
		pay: pb.NewPaymentServiceHTTPClient(conn), acct: pb.NewAccountServiceHTTPClient(conn),
		ops: pb.NewBankOperationsServiceHTTPClient(conn), op: pb.NewNetworkOperationsServiceHTTPClient(conn),
		users: users, et: et, sessions: map[string]*session{},
	}
	p.tpl = template.Must(template.New("").Funcs(template.FuncMap{
		"ts":     p.ts,
		"money":  money,
		"short":  short,
		"pill":   pill,
		"canAct": canAct,
		"passed": func(c *pb.PayeeCheck) bool { return c != nil && c.AccountStatus == "OPEN" && c.NameMatch == "MATCH" },
		"last": func(p *pb.Payment) string {
			if len(p.History) == 0 {
				return ""
			}
			return p.History[len(p.History)-1].Note
		},
		"list": func(v ...any) []any { return v },
	}).ParseFS(files, "templates/*.html"))
	sort.Slice(p.users, func(i, j int) bool {
		if p.users[i].Bank != p.users[j].Bank {
			return p.users[i].Bank > p.users[j].Bank
		}
		return p.users[i].Organization > p.users[j].Organization
	})
	return p, func() { conn.Close() }, nil
}

// Handler routes the pages.
func (p *Portal) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", p.home)
	mux.HandleFunc("GET /login", p.loginPage)
	mux.HandleFunc("POST /login", p.login)
	mux.HandleFunc("POST /logout", p.post(p.logout))
	mux.HandleFunc("GET /corp", p.page(p.corp))
	mux.HandleFunc("POST /corp/payments", p.post(p.createPayment))
	mux.HandleFunc("POST /corp/files", p.post(p.uploadFile))
	mux.HandleFunc("GET /corp/statement", p.page(p.statement))
	mux.HandleFunc("GET /payments/{id}", p.page(p.payment))
	mux.HandleFunc("POST /payments/{id}/{action}", p.post(p.paymentAction))
	mux.HandleFunc("GET /ops", p.page(p.opsPage))
	mux.HandleFunc("POST /ops/fund", p.post(p.fund))
	mux.HandleFunc("POST /ops/defunds", p.post(p.requestDefund))
	mux.HandleFunc("POST /ops/defunds/{id}/approve", p.post(p.approveDefund))
	mux.HandleFunc("POST /ops/holds/{id}/{action}", p.post(p.hold))
	mux.HandleFunc("GET /operator", p.page(p.tchPage))
	mux.HandleFunc("POST /operator/netting", p.post(p.runNetting))
	mux.HandleFunc("POST /operator/reconcile", p.post(p.reconcile))
	mux.HandleFunc("POST /operator/clock", p.post(p.clock))
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com")
		h.ServeHTTP(w, r)
	})
}

// ─── Sessions ───

const cookieName = "payments_session"

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (p *Portal) session(r *http.Request) (*session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[c.Value]
	return s, ok
}

func (p *Portal) ctx(r *http.Request, s *session) context.Context {
	return context.WithValue(r.Context(), tokenKey{}, s.token)
}

type view struct {
	Who   *pb.Principal
	CSRF  string
	Title string
	OK    string
	Err   string
	Now   string
	Data  any
	Brand string
}

func (p *Portal) render(w http.ResponseWriter, r *http.Request, s *session, name, title string, data any) {
	v := view{Who: s.who, CSRF: s.csrf, Title: title, OK: r.URL.Query().Get("ok"), Err: r.URL.Query().Get("err"), Data: data,
		Brand: "the operator Network"}
	if s.who.BankId != "" {
		v.Brand = bankName(s.who.BankId)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := p.tpl.ExecuteTemplate(w, name, v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func bankName(id string) string {
	switch id {
	case "BNKAUS30":
		return "Bank A"
	case "BNKBUS30":
		return "Bank B"
	case "BNKCUS30":
		return "Bank C"
	}
	return id
}

// page requires a session.
func (p *Portal) page(h func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := p.session(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r, s)
	}
}

// post requires a session and its CSRF token, then redirects with the
// outcome.
func (p *Portal) post(h func(*http.Request, *session) (string, string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := p.session(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil && err != http.ErrNotMultipart {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.csrf)) != 1 {
			http.Error(w, "stale form: reload the page", http.StatusForbidden)
			return
		}
		to, msg, err := h(r, s)
		q := url.Values{}
		if err != nil {
			q.Set("err", kerrors.FromError(err).Message)
		} else if msg != "" {
			q.Set("ok", msg)
		}
		if len(q) > 0 {
			to += "?" + q.Encode()
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	}
}

func (p *Portal) home(w http.ResponseWriter, r *http.Request) {
	s, ok := p.session(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, landing(s.who), http.StatusSeeOther)
}

func landing(who *pb.Principal) string {
	switch {
	case who.OrganizationId != "":
		return "/corp"
	case who.Role == "operator-ops":
		return "/operator"
	}
	return "/ops"
}

func (p *Portal) loginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = p.tpl.ExecuteTemplate(w, "login", map[string]any{"Users": p.users, "Err": r.URL.Query().Get("err")})
}

// login signs in as one of the demo identities. A production portal would
// run the OpenID Connect authorization code flow here instead.
func (p *Portal) login(w http.ResponseWriter, r *http.Request) {
	i := -1
	fmt.Sscan(r.PostFormValue("user"), &i)
	if i < 0 || i >= len(p.users) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s := &session{token: p.users[i].Token, csrf: random()}
	who, err := p.acct.WhoAmI(p.ctx(r, s), &pb.WhoAmIRequest{})
	if err != nil {
		http.Redirect(w, r, "/login?err="+url.QueryEscape(kerrors.FromError(err).Message), http.StatusSeeOther)
		return
	}
	s.who = who
	id := random()
	p.mu.Lock()
	p.sessions[id] = s
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, landing(who), http.StatusSeeOther)
}

func (p *Portal) logout(r *http.Request, s *session) (string, string, error) {
	if c, err := r.Cookie(cookieName); err == nil {
		p.mu.Lock()
		delete(p.sessions, c.Value)
		p.mu.Unlock()
	}
	return "/login", "", nil
}

// ─── Client treasury ───

func (p *Portal) corp(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := p.ctx(r, s)
	acct, err := p.acct.GetAccount(ctx, &pb.GetAccountRequest{})
	if err != nil {
		p.fail(w, r, s, err)
		return
	}
	list, err := p.pay.ListPayments(ctx, &pb.ListPaymentsRequest{PageSize: 100})
	if err != nil {
		p.fail(w, r, s, err)
		return
	}
	p.render(w, r, s, "corp", acct.OrganizationName, map[string]any{"Account": acct, "Payments": list.Payments, "Key": random()[:24]})
}

func (p *Portal) createPayment(r *http.Request, s *session) (string, string, error) {
	ctx := context.WithValue(p.ctx(r, s), idemKey{}, r.PostFormValue("key"))
	res, err := p.pay.CreatePayment(ctx, &pb.CreatePaymentRequest{
		Creditor: &pb.Party{Name: r.PostFormValue("name"), RoutingNumber: r.PostFormValue("routing"), Account: r.PostFormValue("account")},
		Amount:   &pb.Money{Amount: r.PostFormValue("amount"), Currency: "USD"}, Priority: r.PostFormValue("priority"),
		Remittance: r.PostFormValue("remittance"), EndToEndId: r.PostFormValue("e2e"),
	})
	if err != nil {
		return "/corp", "", err
	}
	return "/payments/" + res.Payment.Id, "Payment received; it needs approval.", nil
}

func (p *Portal) uploadFile(r *http.Request, s *session) (string, string, error) {
	f, _, err := r.FormFile("file")
	if err != nil {
		return "/corp", "", kerrors.BadRequest("NO_FILE", "choose a pain.001 file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		return "/corp", "", err
	}
	ctx := context.WithValue(p.ctx(r, s), idemKey{}, r.PostFormValue("key"))
	res, err := p.pay.SubmitPaymentFile(ctx, &pb.SubmitPaymentFileRequest{Xml: string(b)})
	if err != nil {
		return "/corp", "", err
	}
	return "/corp", fmt.Sprintf("File %s: %d payment(s) awaiting approval, %d refused.", res.MessageId, len(res.Payments), len(res.Rejections)), nil
}

func (p *Portal) statement(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := p.ctx(r, s)
	acct, err := p.acct.GetAccount(ctx, &pb.GetAccountRequest{})
	if err != nil {
		p.fail(w, r, s, err)
		return
	}
	entries, err := p.acct.ListStatementEntries(ctx, &pb.ListStatementEntriesRequest{})
	if err != nil {
		p.fail(w, r, s, err)
		return
	}
	camt, err := p.acct.GetStatement(ctx, &pb.GetAccountRequest{})
	if err != nil {
		p.fail(w, r, s, err)
		return
	}
	p.render(w, r, s, "statement", "Statement", map[string]any{"Account": acct, "Entries": entries.Entries, "Camt": camt.Xml})
}

func (p *Portal) payment(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := p.ctx(r, s)
	id := r.PathValue("id")
	var (
		pay *pb.Payment
		err error
	)
	if s.who.OrganizationId != "" {
		pay, err = p.pay.GetPayment(ctx, &pb.GetPaymentRequest{Id: id})
	} else {
		pay, err = p.ops.GetBankPayment(ctx, &pb.GetBankPaymentRequest{Id: id})
	}
	if err != nil {
		p.fail(w, r, s, err)
		return
	}
	var report string
	if s.who.OrganizationId != "" {
		if d, err := p.pay.GetPaymentStatusReport(ctx, &pb.GetPaymentRequest{Id: id}); err == nil {
			report = d.Xml
		}
	}
	p.render(w, r, s, "payment", "Payment "+short(pay.Id), map[string]any{"P": pay, "Report": report})
}

func (p *Portal) paymentAction(r *http.Request, s *session) (string, string, error) {
	ctx, id := p.ctx(r, s), r.PathValue("id")
	back := "/payments/" + id
	var (
		pay *pb.Payment
		err error
	)
	switch r.PathValue("action") {
	case "approve":
		pay, err = p.pay.ApprovePayment(ctx, &pb.ApprovePaymentRequest{Id: id})
	case "decline":
		pay, err = p.pay.DeclinePayment(ctx, &pb.DeclinePaymentRequest{Id: id, Reason: r.PostFormValue("reason")})
	case "cancel":
		pay, err = p.pay.CancelPayment(ctx, &pb.CancelPaymentRequest{Id: id})
	default:
		return back, "", kerrors.NotFound("NOT_FOUND", "no such action")
	}
	if err != nil {
		return back, "", err
	}
	return back, "Payment is " + pay.Status + ".", nil
}

// ─── Bank operations ───

func (p *Portal) opsPage(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := p.ctx(r, s)
	liq, err := p.ops.GetLiquidity(ctx, &pb.GetLiquidityRequest{})
	if err != nil {
		p.fail(w, r, s, err)
		return
	}
	list, err := p.ops.ListBankPayments(ctx, &pb.ListBankPaymentsRequest{PageSize: 100})
	if err != nil {
		p.fail(w, r, s, err)
		return
	}
	data := map[string]any{"L": liq, "Payments": list.Payments}
	if s.who.Role == "compliance" {
		holds, err := p.ops.ListHolds(ctx, &pb.ListHoldsRequest{})
		if err != nil {
			p.fail(w, r, s, err)
			return
		}
		data["Holds"], data["Cases"] = holds.Holds, holds.Cases
	}
	p.render(w, r, s, "ops", liq.BankName+" operations", data)
}

func (p *Portal) fund(r *http.Request, s *session) (string, string, error) {
	res, err := p.ops.FundOmnibus(p.ctx(r, s), &pb.FundOmnibusRequest{Amount: &pb.Money{Amount: r.PostFormValue("amount"), Currency: "USD"}})
	if err != nil {
		return "/ops", "", err
	}
	if res.Status != "ACSC" {
		return "/ops", "", kerrors.Conflict("FED_REFUSED", "the Fed refused the transfer: "+res.Reason)
	}
	return "/ops", fmt.Sprintf("Funded by %s; %d waiting payment(s) released.", res.Instrument, res.PaymentsReleased), nil
}

func (p *Portal) requestDefund(r *http.Request, s *session) (string, string, error) {
	d, err := p.ops.RequestDefund(p.ctx(r, s), &pb.RequestDefundRequest{Amount: &pb.Money{Amount: r.PostFormValue("amount"), Currency: "USD"}})
	if err != nil {
		return "/ops", "", err
	}
	return "/ops", "Defund " + short(d.Id) + " requested; it needs the treasury approver.", nil
}

func (p *Portal) approveDefund(r *http.Request, s *session) (string, string, error) {
	d, err := p.ops.ApproveDefund(p.ctx(r, s), &pb.ApproveDefundRequest{Id: r.PathValue("id")})
	if err != nil {
		return "/ops", "", err
	}
	return "/ops", "Defund " + short(d.Id) + ": " + d.Status + ".", nil
}

func (p *Portal) hold(r *http.Request, s *session) (string, string, error) {
	ctx := p.ctx(r, s)
	req := &pb.HoldDecisionRequest{Id: r.PathValue("id"), Note: r.PostFormValue("note")}
	var (
		pay *pb.Payment
		err error
	)
	switch r.PathValue("action") {
	case "release":
		pay, err = p.ops.ReleaseHold(ctx, req)
	case "block":
		pay, err = p.ops.BlockHold(ctx, req)
	case "reject":
		pay, err = p.ops.RejectHold(ctx, req)
	default:
		return "/ops", "", kerrors.NotFound("NOT_FOUND", "no such action")
	}
	if err != nil {
		return "/ops", "", err
	}
	msg := "Payment " + short(pay.Id) + " is " + pay.Status + "."
	if pay.OfacReportDue != nil {
		msg += " Report to OFAC by " + pay.OfacReportDue.AsTime().In(p.et).Format("Mon 2 Jan 2006") + "."
	}
	return "/ops", msg, nil
}

// ─── operator operations ───

func (p *Portal) tchPage(w http.ResponseWriter, r *http.Request, s *session) {
	n, err := p.op.GetNetwork(p.ctx(r, s), &pb.GetNetworkRequest{})
	if err != nil {
		p.fail(w, r, s, err)
		return
	}
	p.render(w, r, s, "operator", "Network operations", map[string]any{"N": n})
}

func (p *Portal) runNetting(r *http.Request, s *session) (string, string, error) {
	c, err := p.op.RunNettingCycle(p.ctx(r, s), &pb.RunNettingCycleRequest{})
	if err != nil {
		return "/operator", "", err
	}
	return "/operator", fmt.Sprintf("Cycle: %d settled, %d deferred; $%s gross by moving $%s.", c.Discharged, c.Deferred, money(c.Gross), money(c.Net)), nil
}

func (p *Portal) reconcile(r *http.Request, s *session) (string, string, error) {
	rec, err := p.op.Reconcile(p.ctx(r, s), &pb.ReconcileRequest{})
	if err != nil {
		return "/operator", "", err
	}
	if rec.ReconciliationBreak {
		return "/operator", "", kerrors.Conflict("BREAK", "reconciliation break: the Fed and the ledger differ")
	}
	return "/operator", "Reconciled: the Fed and the ledger both read $" + money(rec.FedBalance) + ".", nil
}

func (p *Portal) clock(r *http.Request, s *session) (string, string, error) {
	ctx := p.ctx(r, s)
	n, err := p.op.GetNetwork(ctx, &pb.GetNetworkRequest{})
	if err != nil {
		return "/operator", "", err
	}
	now := n.Now.AsTime().In(p.et)
	var to time.Time
	switch r.PostFormValue("to") {
	case "hour":
		to = now.Add(time.Hour)
	case "day":
		to = now.AddDate(0, 0, 1)
	case "open":
		to = n.NextFedwireOpen.AsTime().Add(time.Minute)
	case "saturday":
		d := (int(time.Saturday) - int(now.Weekday()) + 7) % 7
		to = time.Date(now.Year(), now.Month(), now.Day()+d, 11, 0, 0, 0, p.et)
	case "monday":
		d := (int(time.Monday) - int(now.Weekday()) + 7) % 7
		if d == 0 {
			d = 7
		}
		to = time.Date(now.Year(), now.Month(), now.Day()+d, 9, 0, 0, 0, p.et)
	default:
		return "/operator", "", kerrors.BadRequest("BAD_CLOCK", "unknown clock move")
	}
	if !to.After(now) {
		to = now.Add(time.Minute)
	}
	res, err := p.op.SetClock(ctx, &pb.SetClockRequest{Time: timestamppb.New(to)})
	if err != nil {
		return "/operator", "", err
	}
	return "/operator", "Clock moved to " + p.ts(res.Now) + ".", nil
}

func (p *Portal) fail(w http.ResponseWriter, r *http.Request, s *session, err error) {
	e := kerrors.FromError(err)
	w.WriteHeader(int(e.Code))
	p.render(w, r, s, "error", "Error", map[string]any{"Code": e.Code, "Message": e.Message})
}

// ─── Template helpers ───

func (p *Portal) ts(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.AsTime().In(p.et).Format("Mon 2 Jan 15:04 MST")
}

// money renders an amount with thousands separators.
func money(m *pb.Money) string {
	if m == nil {
		return ""
	}
	whole, frac, _ := strings.Cut(m.Amount, ".")
	neg := strings.HasPrefix(whole, "-")
	whole = strings.TrimPrefix(whole, "-")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String()
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

func short(id string) string {
	if len(id) > 13 {
		return id[len(id)-12:]
	}
	return id
}

func pill(status string) string {
	switch status {
	case "SETTLED":
		return "ok"
	case "REJECTED", "EXPIRED", "BLOCKED":
		return "bad"
	case "CANCELLED":
		return "mute"
	case "ON_HOLD", "AWAITING_LIQUIDITY":
		return "warn"
	}
	return "info"
}

// canAct mirrors what the API would allow, only to decide which buttons to
// show; the API decides.
func canAct(who *pb.Principal, pay *pb.Payment, action string) bool {
	if who.OrganizationId == "" || who.OrganizationId != pay.OrganizationId {
		return false
	}
	if pay.Status == "QUEUED_FOR_NETTING" {
		return action == "cancel" && (who.Role == "maker" || who.Role == "approver")
	}
	if pay.Status != "AWAITING_APPROVAL" {
		return false
	}
	switch action {
	case "approve", "decline":
		if who.Role != "approver" || who.Id == pay.CreatedBy {
			return false
		}
		for _, a := range pay.Approvals {
			if a == who.Id {
				return false
			}
		}
		return true
	case "cancel":
		return who.Id == pay.CreatedBy
	}
	return false
}
