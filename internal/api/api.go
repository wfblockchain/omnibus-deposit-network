// Package api is the REST/JSON interface to the workflow: what a corporate
// treasury system, a bank's operations tooling or the operator's consoles call.
//
// Conventions follow bank payments APIs: bearer tokens (standing in for
// OAuth2 client-credentials tokens), an Idempotency-Key header on payment
// creation, pain.002-style ISO status codes on every payment, and errors
// as {"error": code, "message": text} with conventional HTTP statuses.
package api

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"omnibus-deposit-network/internal/webhook"
	"omnibus-deposit-network/internal/workflow"
)

//go:embed openapi.yaml
var openapiSpec []byte

// Handler serves the API under /api/v1/. hooks may be nil, which turns the
// webhook endpoints off.
func Handler(svc *workflow.Service, hooks *webhook.Dispatcher) http.Handler {
	a := &api{svc: svc, hooks: hooks}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	})
	mux.Handle("GET /api/v1/me", a.auth(a.me))

	mux.Handle("GET /api/v1/account", a.auth(a.account))
	mux.Handle("GET /api/v1/account/statement", a.auth(a.statement))
	mux.Handle("GET /api/v1/payments", a.auth(a.listPayments))
	mux.Handle("POST /api/v1/payments", a.auth(a.createPayment))
	mux.Handle("GET /api/v1/payments/{id}", a.auth(a.getPayment))
	mux.Handle("POST /api/v1/payments/{id}/approve", a.auth(a.approve))
	mux.Handle("POST /api/v1/payments/{id}/decline", a.auth(a.decline))
	mux.Handle("POST /api/v1/payments/{id}/cancel", a.auth(a.cancel))
	mux.Handle("POST /api/v1/payees/verify", a.auth(a.verifyPayee))
	mux.Handle("GET /api/v1/network/participants", a.auth(a.participants))
	if hooks != nil {
		mux.Handle("GET /api/v1/webhooks", a.auth(a.listHooks))
		mux.Handle("POST /api/v1/webhooks", a.auth(a.createHook))
		mux.Handle("DELETE /api/v1/webhooks/{id}", a.auth(a.deleteHook))
		mux.Handle("GET /api/v1/webhooks/{id}/deliveries", a.auth(a.hookDeliveries))
	}

	mux.Handle("GET /api/v1/ops/payments", a.auth(a.listPayments))
	mux.Handle("GET /api/v1/ops/holds", a.auth(a.holds))
	mux.Handle("POST /api/v1/ops/holds/{id}/release", a.auth(a.release))
	mux.Handle("POST /api/v1/ops/holds/{id}/reject", a.auth(a.rejectHold))
	mux.Handle("POST /api/v1/ops/holds/{id}/block", a.auth(a.block))
	mux.Handle("GET /api/v1/ops/liquidity", a.auth(a.liquidity))
	mux.Handle("POST /api/v1/ops/funding", a.auth(a.fund))
	mux.Handle("POST /api/v1/ops/defunds", a.auth(a.requestDefund))
	mux.Handle("POST /api/v1/ops/defunds/{id}/approve", a.auth(a.approveDefund))

	mux.Handle("GET /api/v1/operator/network", a.auth(a.network))
	mux.Handle("POST /api/v1/operator/netting/run", a.auth(a.runNetting))
	mux.Handle("POST /api/v1/operator/reconcile", a.auth(a.reconcile))
	mux.Handle("POST /api/v1/operator/clock", a.auth(a.setClock))
	return requestID(mux)
}

// requestID echoes the caller's Request-Id, or assigns one, on every
// response, so a support call can name the exact request.
func requestID(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("Request-Id")
		if id == "" || len(id) > 64 {
			b := make([]byte, 16)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("Request-Id", id)
		h.ServeHTTP(w, r)
	})
}

type api struct {
	svc   *workflow.Service
	hooks *webhook.Dispatcher
}

type handler func(w http.ResponseWriter, r *http.Request, p workflow.Principal)

func (a *api) auth(h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, found := a.svc.Authenticate(strings.TrimSpace(tok))
		if !ok || !found {
			w.Header().Set("WWW-Authenticate", `Bearer realm="omnibus"`)
			writeErr(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		h(w, r, p)
	})
}

func (a *api) me(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	writeJSON(w, http.StatusOK, p)
}

func (a *api) account(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	v, err := a.svc.Account(r.Context(), p)
	respond(w, http.StatusOK, v, err)
}

func (a *api) statement(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	v, err := a.svc.Statement(p)
	respond(w, http.StatusOK, map[string]any{"entries": v}, err)
}

func (a *api) listPayments(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	writeJSON(w, http.StatusOK, map[string]any{"payments": a.svc.Orders(p)})
}

func (a *api) createPayment(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	var req workflow.PaymentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be a JSON payment request")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 64 {
		writeErr(w, http.StatusBadRequest, "idempotency_key_required", "send an Idempotency-Key header of at most 64 characters so a retry cannot pay twice")
		return
	}
	o, created, err := a.svc.Submit(r.Context(), p, req, key)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK // a replay of an earlier request with the same key
	}
	w.Header().Set("Location", "/api/v1/payments/"+o.ID)
	writeJSON(w, status, o)
}

func (a *api) getPayment(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	o, err := a.svc.Order(p, r.PathValue("id"))
	respond(w, http.StatusOK, o, err)
}

func (a *api) approve(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	o, err := a.svc.Approve(r.Context(), p, r.PathValue("id"))
	respond(w, http.StatusOK, o, err)
}

type noteBody struct {
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

func readNote(r *http.Request) noteBody {
	var b noteBody
	_ = json.NewDecoder(r.Body).Decode(&b)
	return b
}

func (a *api) decline(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	o, err := a.svc.Decline(r.Context(), p, r.PathValue("id"), readNote(r).Reason)
	respond(w, http.StatusOK, o, err)
}

func (a *api) cancel(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	o, err := a.svc.Cancel(r.Context(), p, r.PathValue("id"))
	respond(w, http.StatusOK, o, err)
}

func (a *api) holds(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	v, err := a.svc.Holds(p)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	cases, err := a.svc.SanctionsCases(p)
	respond(w, http.StatusOK, map[string]any{"holds": v, "cases": cases}, err)
}

func (a *api) block(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	o, err := a.svc.Block(r.Context(), p, r.PathValue("id"), readNote(r).Note)
	respond(w, http.StatusOK, o, err)
}

func (a *api) release(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	o, err := a.svc.Release(r.Context(), p, r.PathValue("id"), readNote(r).Note)
	respond(w, http.StatusOK, o, err)
}

func (a *api) rejectHold(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	o, err := a.svc.RejectHold(r.Context(), p, r.PathValue("id"), readNote(r).Note)
	respond(w, http.StatusOK, o, err)
}

func (a *api) verifyPayee(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	var party workflow.Party
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&party); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON with name, routingNumber and account")
		return
	}
	v, err := a.svc.VerifyPayee(p, party)
	respond(w, http.StatusOK, v, err)
}

func (a *api) participants(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	v, err := a.svc.Participants(r.Context())
	respond(w, http.StatusOK, map[string]any{"participants": v}, err)
}

// Webhook endpoints belong to an organization and are managed by its
// approvers, the senior entitlement in the demo.
func (a *api) hookOrg(p workflow.Principal) (string, error) {
	if p.Org == "" || p.Role != workflow.RoleApprover {
		return "", fmt.Errorf("%w: an approver of the organization manages its webhooks", workflow.ErrForbidden)
	}
	return p.Org, nil
}

func (a *api) listHooks(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	org, err := a.hookOrg(p)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": a.hooks.Subscriptions(org)})
}

func (a *api) createHook(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	org, err := a.hookOrg(p)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON with a url")
		return
	}
	sub, err := a.hooks.Subscribe(org, body.URL)
	respond(w, http.StatusCreated, sub, err)
}

func (a *api) deleteHook(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	org, err := a.hookOrg(p)
	if err == nil {
		err = a.hooks.Unsubscribe(org, r.PathValue("id"))
	}
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) hookDeliveries(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	org, err := a.hookOrg(p)
	if err != nil {
		respond(w, 0, nil, err)
		return
	}
	ds, err := a.hooks.Deliveries(org, r.PathValue("id"))
	respond(w, http.StatusOK, map[string]any{"deliveries": ds}, err)
}

func (a *api) liquidity(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	v, err := a.svc.Liquidity(r.Context(), p)
	respond(w, http.StatusOK, v, err)
}

type amountBody struct {
	Amount string `json:"amount"`
}

func (a *api) fund(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	var b amountBody
	_ = json.NewDecoder(r.Body).Decode(&b)
	v, err := a.svc.Fund(r.Context(), p, b.Amount)
	respond(w, http.StatusOK, v, err)
}

func (a *api) requestDefund(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	var b amountBody
	_ = json.NewDecoder(r.Body).Decode(&b)
	v, err := a.svc.RequestDefund(r.Context(), p, b.Amount)
	respond(w, http.StatusCreated, v, err)
}

func (a *api) approveDefund(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	v, err := a.svc.ApproveDefund(r.Context(), p, r.PathValue("id"))
	respond(w, http.StatusOK, v, err)
}

func (a *api) network(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	v, err := a.svc.Network(r.Context(), p)
	respond(w, http.StatusOK, v, err)
}

func (a *api) runNetting(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	v, err := a.svc.RunNetting(r.Context(), p)
	respond(w, http.StatusOK, v, err)
}

func (a *api) reconcile(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	v, err := a.svc.Reconcile(r.Context(), p)
	respond(w, http.StatusOK, v, err)
}

func (a *api) setClock(w http.ResponseWriter, r *http.Request, p workflow.Principal) {
	var b struct {
		Time string `json:"time"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	t, err := time.Parse(time.RFC3339, b.Time)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "time must be RFC 3339, e.g. 2026-10-03T11:00:00-04:00")
		return
	}
	err = a.svc.SetClock(r.Context(), p, t)
	respond(w, http.StatusOK, map[string]any{"now": a.svc.Now()}, err)
}

func respond(w http.ResponseWriter, status int, v any, err error) {
	switch {
	case err == nil:
		writeJSON(w, status, v)
	case errors.Is(err, workflow.ErrForbidden):
		writeErr(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, workflow.ErrNotFound), errors.Is(err, webhook.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, workflow.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, workflow.ErrInvalid), errors.Is(err, webhook.ErrInvalid):
		writeErr(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}
