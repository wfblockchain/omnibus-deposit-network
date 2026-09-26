// Package web is the browser-facing half of the web2 layer: server-rendered
// portals for a corporate treasury, a member bank's operations staff and
// the network operations. Plain HTML forms over the same workflow service
// the REST API uses; no JavaScript is needed to operate it.
package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"omnibus-deposit-network/internal/iso20022"
	"omnibus-deposit-network/internal/workflow"
)

//go:embed templates/*.html
var files embed.FS

// Handler serves the portals.
func Handler(svc *workflow.Service) http.Handler {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	s := &site{svc: svc, secret: secret}
	s.tpl = template.Must(template.New("").Funcs(template.FuncMap{
		"ts":       s.ts,
		"pill":     pill,
		"lower":    strings.ToLower,
		"approved": approvedBy,
		"canAct":   canAct,
		"add":      func(a, b int) int { return a + b },
		"usd":      func(v *big.Int) string { return iso20022.Readable(v) },
		"money":    iso20022.GroupDollars,
		"dict": func(v view, orders []workflow.Order) map[string]any {
			return map[string]any{"P": v.P, "CSRF": v.CSRF, "Orders": orders}
		},
	}).ParseFS(files, "templates/*.html"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)

	mux.HandleFunc("GET /corp", s.page(s.corp))
	mux.HandleFunc("POST /corp/payments", s.post(s.createPayment))
	mux.HandleFunc("GET /corp/statement", s.page(s.statement))
	mux.HandleFunc("GET /orders/{id}", s.page(s.order))
	mux.HandleFunc("POST /orders/{id}/{action}", s.post(s.orderAction))

	mux.HandleFunc("GET /ops", s.page(s.ops))
	mux.HandleFunc("POST /ops/fund", s.post(s.fund))
	mux.HandleFunc("POST /ops/defunds", s.post(s.requestDefund))
	mux.HandleFunc("POST /ops/defunds/{id}/approve", s.post(s.approveDefund))
	mux.HandleFunc("POST /ops/holds/{id}/{action}", s.post(s.holdAction))

	mux.HandleFunc("GET /operator", s.page(s.op))
	mux.HandleFunc("POST /operator/netting", s.post(s.netting))
	mux.HandleFunc("POST /operator/reconcile", s.post(s.reconcile))
	mux.HandleFunc("POST /operator/clock", s.post(s.clock))
	return mux
}

type site struct {
	svc    *workflow.Service
	secret []byte
	tpl    *template.Template
}

const cookieName = "operator_session"

func (s *site) principal(r *http.Request) (workflow.Principal, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return workflow.Principal{}, false
	}
	p, ok := s.svc.Authenticate(c.Value)
	if ok {
		p.Token = c.Value
	}
	return p, ok
}

func (s *site) csrf(p workflow.Principal) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(p.Token))
	return hex.EncodeToString(m.Sum(nil))
}

// view is what every page template receives.
type view struct {
	P       workflow.Principal
	CSRF    string
	Now     time.Time
	Fedwire bool
	OK      string
	Err     string
	Title   string
	Data    any
	Refresh int
	Brand   string // the bank whose channel this is; the operator for its own staff
}

func (s *site) render(w http.ResponseWriter, r *http.Request, p workflow.Principal, name, title string, data any, refresh int) {
	v := view{P: p, CSRF: s.csrf(p), Now: s.svc.Now(), Fedwire: s.svc.Fed.Calendar().IsOpen(s.svc.Now()),
		OK: r.URL.Query().Get("ok"), Err: r.URL.Query().Get("err"), Title: title, Data: data, Refresh: refresh,
		Brand: "the operator Network"}
	if node, ok := s.svc.Banks[p.Bank]; ok && p.Role != workflow.RoleOperatorOps {
		v.Brand = node.G.Bank.Spec.Name // clients and bank staff work in their bank's channel
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// page wraps a GET handler with the session check.
func (s *site) page(h func(w http.ResponseWriter, r *http.Request, p workflow.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.principal(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r, p)
	}
}

// post wraps a form handler with the session and CSRF checks; the handler
// returns where to go and what to say.
func (s *site) post(h func(r *http.Request, p workflow.Principal) (string, string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.principal(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if err := r.ParseForm(); err != nil || !hmac.Equal([]byte(r.PostFormValue("csrf")), []byte(s.csrf(p))) {
			http.Error(w, "invalid form token; reload the page", http.StatusForbidden)
			return
		}
		to, msg, err := h(r, p)
		q := url.Values{}
		if err != nil {
			q.Set("err", friendly(err))
		} else if msg != "" {
			q.Set("ok", msg)
		}
		if enc := q.Encode(); enc != "" {
			to += "?" + enc
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	}
}

func friendly(err error) string {
	msg := err.Error()
	for _, e := range []error{workflow.ErrForbidden, workflow.ErrInvalid, workflow.ErrConflict, workflow.ErrNotFound} {
		if errors.Is(err, e) {
			msg = strings.TrimPrefix(msg, e.Error()+": ")
		}
	}
	return msg
}

func landing(p workflow.Principal) string {
	switch {
	case p.Org != "":
		return "/corp"
	case p.Role == workflow.RoleOperatorOps:
		return "/operator"
	default:
		return "/ops"
	}
}

/*──────────────────────────────── session ───────────────────────────────*/

func (s *site) home(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.principal(r); ok {
		http.Redirect(w, r, landing(p), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *site) loginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tpl.ExecuteTemplate(w, "login", map[string]any{"Users": s.svc.Users(), "Now": s.svc.Now()})
}

func (s *site) login(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	tok := r.PostFormValue("token")
	p, ok := s.svc.Authenticate(tok)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, landing(p), http.StatusSeeOther)
}

func (s *site) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

/*─────────────────────────────── corporate ──────────────────────────────*/

func (s *site) corp(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	if p.Org == "" {
		http.Redirect(w, r, landing(p), http.StatusSeeOther)
		return
	}
	acct, err := s.svc.Account(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	org, _ := s.svc.OrgOf(p)
	s.render(w, r, p, "corp", org.Name, map[string]any{"Account": acct, "Orders": s.svc.Orders(p)}, 0)
}

func (s *site) createPayment(r *http.Request, p workflow.Principal) (string, string, error) {
	req := workflow.PaymentRequest{
		Creditor: workflow.Party{Name: r.PostFormValue("name"), ABA: r.PostFormValue("routing"), Account: r.PostFormValue("account")},
		Amount:   r.PostFormValue("amount"), Priority: workflow.Priority(r.PostFormValue("priority")), Remittance: r.PostFormValue("remittance"),
	}
	o, _, err := s.svc.Submit(r.Context(), p, req, r.PostFormValue("key"))
	if err != nil {
		return "/corp", "", err
	}
	return "/orders/" + o.ID, "Payment " + o.ID + " created; it now needs approval.", nil
}

func (s *site) statement(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	lines, err := s.svc.Statement(p)
	if err != nil {
		http.Redirect(w, r, landing(p), http.StatusSeeOther)
		return
	}
	acct, _ := s.svc.Account(r.Context(), p)
	s.render(w, r, p, "statement", "Statement", map[string]any{"Lines": lines, "Account": acct}, 0)
}

func (s *site) order(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	o, err := s.svc.Order(p, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	refresh := 0
	if !o.Status.Terminal() && o.Status != workflow.StatusAwaitingApproval && o.Status != workflow.StatusOnHold {
		refresh = 3
	}
	s.render(w, r, p, "order", o.ID, o, refresh)
}

func (s *site) orderAction(r *http.Request, p workflow.Principal) (string, string, error) {
	id := r.PathValue("id")
	back := "/orders/" + id
	var err error
	var msg string
	switch r.PathValue("action") {
	case "approve":
		var o workflow.Order
		o, err = s.svc.Approve(r.Context(), p, id)
		msg = "Approved. Status: " + string(o.Status) + "."
	case "decline":
		_, err = s.svc.Decline(r.Context(), p, id, r.PostFormValue("reason"))
		msg = "Declined."
	case "cancel":
		_, err = s.svc.Cancel(r.Context(), p, id)
		msg = "Cancelled."
	default:
		return back, "", workflow.ErrNotFound
	}
	return back, msg, err
}

/*─────────────────────────────── bank ops ───────────────────────────────*/

func (s *site) ops(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	if p.Org != "" || p.Role == workflow.RoleOperatorOps {
		http.Redirect(w, r, landing(p), http.StatusSeeOther)
		return
	}
	liq, err := s.svc.Liquidity(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	holds, _ := s.svc.Holds(p)
	cases, _ := s.svc.SanctionsCases(p)
	s.render(w, r, p, "ops", liq.BankName+" operations", map[string]any{"L": liq, "Holds": holds, "Cases": cases, "Orders": s.svc.Orders(p)}, 0)
}

func (s *site) fund(r *http.Request, p workflow.Principal) (string, string, error) {
	res, err := s.svc.Fund(r.Context(), p, r.PostFormValue("amount"))
	if err != nil {
		return "/ops", "", err
	}
	if res.Status != "ACSC" {
		return "/ops", "", errors.New("the Fed refused the transfer: " + res.Reason)
	}
	return "/ops", "Funded by " + res.Instrument + "; " + itoa(res.Released) + " waiting order(s) released.", nil
}

func (s *site) requestDefund(r *http.Request, p workflow.Principal) (string, string, error) {
	d, err := s.svc.RequestDefund(r.Context(), p, r.PostFormValue("amount"))
	if err != nil {
		return "/ops", "", err
	}
	return "/ops", "Defund " + d.ID + " requested; it needs the treasury approver.", nil
}

func (s *site) approveDefund(r *http.Request, p workflow.Principal) (string, string, error) {
	d, err := s.svc.ApproveDefund(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return "/ops", "", err
	}
	return "/ops", "Defund " + d.ID + " approved: " + d.Status + ".", nil
}

func (s *site) holdAction(r *http.Request, p workflow.Principal) (string, string, error) {
	id := r.PathValue("id")
	note := r.PostFormValue("note")
	switch r.PathValue("action") {
	case "release":
		o, err := s.svc.Release(r.Context(), p, id, note)
		return "/ops", "Released " + id + ": " + string(o.Status) + ".", err
	case "block":
		o, err := s.svc.Block(r.Context(), p, id, note)
		return "/ops", "Blocked " + id + "; report to OFAC by " + o.OFACReportDue.Format("Mon 2 Jan 2006") + ".", err
	case "reject":
		o, err := s.svc.RejectHold(r.Context(), p, id, note)
		return "/ops", "Rejected " + id + " (RR04); report to OFAC by " + o.OFACReportDue.Format("Mon 2 Jan 2006") + ".", err
	}
	return "/ops", "", workflow.ErrNotFound
}

/*──────────────────────────────── operator ops ───────────────────────────────*/

func (s *site) op(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	if p.Role != workflow.RoleOperatorOps {
		http.Redirect(w, r, landing(p), http.StatusSeeOther)
		return
	}
	nv, err := s.svc.Network(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, p, "operator", "Network operations", map[string]any{"N": nv, "Orders": s.svc.Orders(p)}, 0)
}

func (s *site) netting(r *http.Request, p workflow.Principal) (string, string, error) {
	rep, err := s.svc.RunNetting(r.Context(), p)
	if err != nil {
		return "/operator", "", err
	}
	if rep.Discharged == 0 {
		return "/operator", "Nothing settleable this cycle (" + itoa(rep.Deferred) + " deferred).", nil
	}
	return "/operator", rep.CycleID + " settled " + itoa(rep.Discharged) + " obligations.", nil
}

func (s *site) reconcile(r *http.Request, p workflow.Principal) (string, string, error) {
	rep, err := s.svc.Reconcile(r.Context(), p)
	if err != nil {
		return "/operator", "", err
	}
	if rep.Break {
		return "/operator", "", errors.New("reconciliation break: minting and defunding are halted")
	}
	return "/operator", "Reconciled: the Fed's balance equals the ledger.", nil
}

func (s *site) clock(r *http.Request, p workflow.Principal) (string, string, error) {
	now := s.svc.Now()
	loc := s.svc.Fed.Calendar().Loc
	var t time.Time
	switch r.PostFormValue("to") {
	case "hour":
		t = now.Add(time.Hour)
	case "day":
		t = now.Add(24 * time.Hour)
	case "saturday":
		t = nextWeekday(now.In(loc), time.Saturday, 11)
	case "open":
		t = s.svc.Fed.Calendar().NextOpen(now.Add(time.Minute)).Add(5 * time.Minute)
	case "monday":
		t = nextWeekday(now.In(loc), time.Monday, 9)
	default:
		return "/operator", "", workflow.ErrInvalid
	}
	if err := s.svc.SetClock(r.Context(), p, t); err != nil {
		return "/operator", "", err
	}
	return "/operator", "Clock moved to " + s.ts(t) + ".", nil
}

func nextWeekday(now time.Time, day time.Weekday, hour int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	for t.Weekday() != day || !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

/*──────────────────────────────── helpers ───────────────────────────────*/

func (s *site) ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(s.svc.Fed.Calendar().Loc).Format("Mon 2 Jan 15:04 MST")
}

func pill(st workflow.Status) string {
	switch st {
	case workflow.StatusSettled:
		return "ok"
	case workflow.StatusRejected, workflow.StatusExpired, workflow.StatusBlocked:
		return "bad"
	case workflow.StatusCancelled:
		return "mute"
	case workflow.StatusOnHold, workflow.StatusAwaitingLiquidity:
		return "warn"
	default:
		return "info"
	}
}

func approvedBy(o workflow.Order, id string) bool {
	for _, a := range o.Approvals {
		if a == id {
			return true
		}
	}
	return false
}

// canAct reports which order actions a principal has.
func canAct(p workflow.Principal, o workflow.Order, action string) bool {
	if p.Org != o.Org {
		return false
	}
	switch {
	case o.Status == workflow.StatusQueuedForNetting:
		return action == "cancel" && (p.Role == workflow.RoleMaker || p.Role == workflow.RoleApprover)
	case o.Status != workflow.StatusAwaitingApproval:
		return false
	}
	switch action {
	case "approve", "decline":
		return p.Role == workflow.RoleApprover && p.ID != o.CreatedBy && !approvedBy(o, p.ID)
	case "cancel":
		return p.ID == o.CreatedBy
	}
	return false
}

func itoa(n int) string { return strconv.Itoa(n) }
