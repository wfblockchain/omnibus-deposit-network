package test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"omnibus-deposit-network/services/payments-portal/portal"
)

type browser struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (b *browser) get(path string) string {
	b.t.Helper()
	resp, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		b.t.Fatalf("GET %s: %d\n%s", path, resp.StatusCode, body)
	}
	if m := csrfRE.FindSubmatch(body); m != nil {
		b.csrf = string(m[1])
	}
	return string(body)
}

// post submits a form and returns the page it redirects to.
func (b *browser) post(path string, form url.Values) string {
	b.t.Helper()
	form.Set("csrf", b.csrf)
	resp, err := b.c.PostForm(b.base+path, form)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		b.t.Fatalf("POST %s: %d\n%s", path, resp.StatusCode, body)
	}
	return string(body)
}

func signIn(t *testing.T, base, name string) *browser {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	b := &browser{t: t, base: base, c: &http.Client{Jar: jar}}
	login := b.get("/login")
	m := regexp.MustCompile(`name="user" value="(\d+)"><button type="submit"><b>` + regexp.QuoteMeta(name) + `</b>`).FindStringSubmatch(login)
	if m == nil {
		t.Fatalf("%s is not offered on the sign-in page", name)
	}
	resp, err := b.c.PostForm(base+"/login", url.Values{"user": {m[1]}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if m := csrfRE.FindSubmatch(body); m != nil {
		b.csrf = string(m[1])
	}
	return b
}

func portalChecks(t *testing.T, api string, pain001 []byte) {
	users := []portal.User{
		{Token: "tok-nw-maker", Name: "Priya Shah, AP specialist", Role: "maker", Bank: "BNKAUS30", Organization: "northwind"},
		{Token: "tok-nw-treasurer", Name: "Tom Reyes, Treasurer", Role: "approver", Bank: "BNKAUS30", Organization: "northwind"},
		{Token: "tok-bank-a-compliance", Name: "Bank A sanctions review", Role: "compliance", Bank: "BNKAUS30"},
		{Token: "tok-bank-a-treasury", Name: "Bank A liquidity desk", Role: "treasury", Bank: "BNKAUS30"},
		{Token: "tok-operator-ops", Name: "the network operations", Role: "operator-ops"},
	}
	p, cleanup, err := portal.New(api, users)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	anon, _ := http.Get(srv.URL + "/corp")
	if anon.Request.URL.Path != "/login" {
		t.Fatalf("an anonymous visitor reached %s", anon.Request.URL.Path)
	}
	anon.Body.Close()

	maker := signIn(t, srv.URL, "Priya Shah, AP specialist")
	corp := maker.get("/corp")
	for _, want := range []string{"Bank A", "Northwind Corp", "123,500,000.00", "Upload file", "INV-88213"} {
		if !strings.Contains(corp, want) {
			t.Fatalf("client portal lacks %q", want)
		}
	}
	if strings.Contains(corp, "A-dT") {
		t.Fatal("client portal shows a ticker")
	}
	// A stale form is refused.
	resp, _ := maker.c.PostForm(srv.URL+"/corp/payments", url.Values{"csrf": {"0"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a form without the session's CSRF token: %d", resp.StatusCode)
	}
	resp.Body.Close()
	page := maker.post("/corp/payments", url.Values{"key": {"portal-1"}, "name": {"Contoso Ltd"}, "routing": {"234567898"},
		"account": {"B-4000001"}, "amount": {"1000000.00"}, "priority": {"NORMAL"}, "remittance": {"PO-PORTAL"}})
	if !strings.Contains(page, "AWAITING_APPROVAL") || !strings.Contains(page, "Payment received") {
		t.Fatalf("new payment page:\n%s", page)
	}
	id := regexp.MustCompile(`Payment id ([0-9a-f-]{36})`).FindStringSubmatch(page)[1]

	approver := signIn(t, srv.URL, "Tom Reyes, Treasurer")
	approver.get("/payments/" + id)
	if page := approver.post("/payments/"+id+"/approve", url.Values{}); !strings.Contains(page, "QUEUED_FOR_NETTING") {
		t.Fatalf("approved payment page:\n%s", page)
	}

	// A pain.001 upload (a new message id and references, so no duplicates).
	file := strings.NewReplacer("NW-20261005-001", "NW-20261005-002", "INV-2026-77", "INV-2026-88").Replace(string(pain001))
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	maker.get("/corp")
	_ = mw.WriteField("csrf", maker.csrf)
	_ = mw.WriteField("key", "portal-file-1")
	fw, _ := mw.CreateFormFile("file", "pain001.xml")
	_, _ = fw.Write([]byte(file))
	mw.Close()
	resp, err = maker.c.Post(srv.URL+"/corp/files", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(out), "File NW-20261005-002: 2 payment(s) awaiting approval, 1 refused.") {
		t.Fatalf("file upload:\n%s", out)
	}
	if st := maker.get("/corp/statement"); !strings.Contains(st, "camt.053.001.08") || !strings.Contains(st, "BkToCstmrStmt") {
		t.Fatal("statement page lacks the camt.053")
	}

	ops := signIn(t, srv.URL, "Bank A sanctions review")
	if page := ops.get("/ops"); !strings.Contains(page, "Sanctions cases") || !strings.Contains(page, "BLOCKED") {
		t.Fatal("compliance console lacks the case")
	}
	treasury := signIn(t, srv.URL, "Bank A liquidity desk")
	if page := treasury.get("/ops"); !strings.Contains(page, "Omnibus position · A-dT") || !strings.Contains(page, "Fund the omnibus") {
		t.Fatal("treasury console")
	}
	op := signIn(t, srv.URL, "the network operations")
	if page := op.get("/operator"); !strings.Contains(page, "books agree") || !strings.Contains(page, "scheduled") {
		t.Fatal("the operator console")
	}
	if page := op.post("/operator/reconcile", url.Values{}); !strings.Contains(page, "Reconciled") {
		t.Fatalf("reconcile from the console:\n%s", page)
	}
}
