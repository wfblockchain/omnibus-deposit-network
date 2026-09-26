package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"

	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
)

func TestStatic(t *testing.T) {
	s, err := NewStatic([]conf.StaticUser{{Token: "tok-a", ID: "a", Name: "A", Role: "maker", Bank: "B", Organization: "o"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(context.Background(), "tok-a")
	if err != nil || p.ID != "a" || p.Role != biz.RoleMaker || p.Org != "o" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := s.Authenticate(context.Background(), "tok-b"); err == nil {
		t.Fatal("unknown token accepted")
	}
	if _, err := NewStatic([]conf.StaticUser{{Token: "x", Role: "superuser"}}); err == nil {
		t.Fatal("unknown role accepted")
	}
}

func sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(claims)
	jws, err := signer.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOIDC(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	const iss, aud = "https://login.bank.example/", "api://payments"
	a := NewOIDCWithKeys(iss, aud, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}})
	base := func() map[string]any {
		return map[string]any{"iss": iss, "aud": aud, "sub": "user-7", "exp": time.Now().Add(time.Hour).Unix(),
			"name": "Tom Reyes", "role": "approver", "bank_id": "BNKAUS30", "org_id": "northwind"}
	}
	p, err := a.Authenticate(context.Background(), sign(t, key, base()))
	if err != nil || p.ID != "user-7" || p.Role != biz.RoleApprover || p.Bank != "BNKAUS30" || p.Org != "northwind" {
		t.Fatalf("valid token: %+v %v", p, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"expired":        func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"wrong audience": func(c map[string]any) { c["aud"] = "api://other" },
		"wrong issuer":   func(c map[string]any) { c["iss"] = "https://evil.example/" },
		"unknown role":   func(c map[string]any) { c["role"] = "admin" },
		"no bank":        func(c map[string]any) { delete(c, "bank_id") },
	} {
		c := base()
		mutate(c)
		if _, err := a.Authenticate(context.Background(), sign(t, key, c)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := a.Authenticate(context.Background(), sign(t, other, base())); err == nil {
		t.Error("a token signed by another key was accepted")
	}
}
