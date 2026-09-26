// Package auth authenticates API callers from a bearer token, as a Kratos
// middleware on both transports. Production verifies OpenID Connect access
// tokens (JWTs) from the bank's identity provider; demo builds may map fixed
// tokens to people instead.
//
// Token claims: sub, name, and the custom claims role (one of biz's roles),
// bank_id and org_id.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"

	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
)

// Authenticator resolves a bearer token to a person.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (biz.Principal, error)
}

type ctxKey struct{}

// NewContext carries the principal.
func NewContext(ctx context.Context, p biz.Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the authenticated principal.
func FromContext(ctx context.Context) (biz.Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(biz.Principal)
	return p, ok
}

var roles = map[biz.Role]bool{biz.RoleMaker: true, biz.RoleApprover: true, biz.RoleViewer: true, biz.RoleCompliance: true,
	biz.RoleTreasury: true, biz.RoleTreasuryApprover: true, biz.RoleOperatorOps: true}

// Server authenticates every operation except those in public.
func Server(a Authenticator, public map[string]bool) middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			tr, ok := transport.FromServerContext(ctx)
			if !ok {
				return nil, biz.ErrUnauthenticated("no transport")
			}
			if public[tr.Operation()] {
				return next(ctx, req)
			}
			h := tr.RequestHeader().Get("Authorization")
			tok, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || strings.TrimSpace(tok) == "" {
				tr.ReplyHeader().Set("WWW-Authenticate", `Bearer realm="payments"`)
				return nil, biz.ErrUnauthenticated("a bearer token is required")
			}
			p, err := a.Authenticate(ctx, strings.TrimSpace(tok))
			if err != nil {
				tr.ReplyHeader().Set("WWW-Authenticate", `Bearer realm="payments", error="invalid_token"`)
				return nil, biz.ErrUnauthenticated("the bearer token is not valid")
			}
			return next(NewContext(ctx, p), req)
		}
	}
}

// ─── Static tokens (demo builds) ───

// Static maps fixed tokens to people; tokens are kept as SHA-256 digests and
// compared in constant time.
type Static struct {
	users []staticUser
}

type staticUser struct {
	digest [32]byte
	p      biz.Principal
}

// NewStatic builds the table from configuration.
func NewStatic(users []conf.StaticUser) (*Static, error) {
	s := &Static{}
	for _, u := range users {
		if !roles[biz.Role(u.Role)] {
			return nil, fmt.Errorf("static user %s: unknown role %q", u.ID, u.Role)
		}
		s.users = append(s.users, staticUser{digest: sha256.Sum256([]byte(u.Token)),
			p: biz.Principal{ID: u.ID, Name: u.Name, Role: biz.Role(u.Role), Bank: u.Bank, Org: u.Organization}})
	}
	return s, nil
}

func (s *Static) Authenticate(_ context.Context, token string) (biz.Principal, error) {
	d := sha256.Sum256([]byte(token))
	for _, u := range s.users {
		if subtle.ConstantTimeCompare(d[:], u.digest[:]) == 1 {
			return u.p, nil
		}
	}
	return biz.Principal{}, fmt.Errorf("unknown token")
}

// Fingerprint is a short, non-reversible id for a token, for logs.
func Fingerprint(token string) string {
	d := sha256.Sum256([]byte(token))
	return hex.EncodeToString(d[:4])
}

// ─── OpenID Connect ───

// OIDC verifies JWT access tokens against the issuer's published keys.
type OIDC struct {
	v *oidc.IDTokenVerifier
}

// NewOIDC discovers the issuer and builds a verifier for the audience.
func NewOIDC(ctx context.Context, issuer, audience string) (*OIDC, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", issuer, err)
	}
	return &OIDC{v: provider.Verifier(&oidc.Config{ClientID: audience})}, nil
}

// NewOIDCWithKeys verifies against a fixed key set (tests, air-gapped runs).
func NewOIDCWithKeys(issuer, audience string, keys oidc.KeySet) *OIDC {
	return &OIDC{v: oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: audience})}
}

type claims struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	BankID string `json:"bank_id"`
	OrgID  string `json:"org_id"`
}

func (o *OIDC) Authenticate(ctx context.Context, token string) (biz.Principal, error) {
	t, err := o.v.Verify(ctx, token)
	if err != nil {
		return biz.Principal{}, err
	}
	var c claims
	if err := t.Claims(&c); err != nil {
		return biz.Principal{}, err
	}
	if !roles[biz.Role(c.Role)] {
		return biz.Principal{}, fmt.Errorf("token carries no known role")
	}
	if c.BankID == "" && biz.Role(c.Role) != biz.RoleOperatorOps {
		return biz.Principal{}, fmt.Errorf("token carries no bank_id")
	}
	return biz.Principal{ID: t.Subject, Name: c.Name, Role: biz.Role(c.Role), Bank: c.BankID, Org: c.OrgID}, nil
}
