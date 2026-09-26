package biz

import (
	"context"
	"strings"
)

// VerifyPayee asks the receiving bank, through the network, whether an
// account is open and whether the name matches its records, as the Fed's
// Payee Name Verification does.
func (uc *PaymentUseCase) VerifyPayee(ctx context.Context, p Principal, payee Party) (PayeeCheck, error) {
	if p.Org == "" {
		return PayeeCheck{}, ErrForbidden("payee checks are for client organizations")
	}
	if err := CheckRouting(payee.Routing); err != nil {
		return PayeeCheck{}, err
	}
	e := uc.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, member := e.rails.BankByRouting(payee.Routing); !member {
		return PayeeCheck{}, ErrInvalid("routing number %s is not a network member", payee.Routing)
	}
	return e.verifyPayee(payee), nil
}

func (e *Engine) verifyPayee(payee Party) PayeeCheck {
	b, ok := e.rails.BankByRouting(payee.Routing)
	if !ok {
		return PayeeCheck{Account: "NOT_FOUND", Name: "NO_MATCH"}
	}
	rec, ok := e.rails.Account(b.MemberID, payee.Account)
	if !ok {
		return PayeeCheck{Account: "NOT_FOUND", Name: "NO_MATCH"}
	}
	out := PayeeCheck{Account: "OPEN", Name: NameMatch(payee.Name, rec.Name)}
	if rec.Closed {
		out.Account = "CLOSED"
	}
	if out.Name == "CLOSE_MATCH" {
		out.Registered = rec.Name
	}
	return out
}

// NameMatch compares a typed payee name with the registered one, ignoring
// case, punctuation and legal-form words: MATCH, CLOSE_MATCH (one or two
// typing slips, or most words shared) or NO_MATCH. It is deterministic on
// purpose: sanctions scoring (watchman's Jaro-Winkler) is tuned for recall
// against a list, not for telling a payer "that is the account's name".
func NameMatch(typed, registered string) string {
	a, b := normName(typed), normName(registered)
	switch {
	case a == "" || b == "":
		return "NO_MATCH"
	case a == b:
		return "MATCH"
	case editDistance(a, b) <= 2:
		return "CLOSE_MATCH"
	}
	aw, bw := strings.Fields(a), strings.Fields(b)
	shared := 0
	for _, x := range aw {
		for _, y := range bw {
			if x == y {
				shared++
				break
			}
		}
	}
	if shared > 0 && 2*shared >= max(len(aw), len(bw)) {
		return "CLOSE_MATCH"
	}
	return "NO_MATCH"
}

var legalForms = map[string]bool{"ltd": true, "limited": true, "inc": true, "incorporated": true, "llc": true,
	"corp": true, "corporation": true, "co": true, "company": true, "plc": true, "lp": true, "llp": true, "the": true}

func normName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	var out []string
	for _, w := range strings.Fields(b.String()) {
		if !legalForms[w] {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
