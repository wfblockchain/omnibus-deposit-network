//go:build !demo

package main

import (
	"strings"
	"testing"
)

// The shipped config loads the way the service loads it, and a production
// build refuses it: static tokens, SQLite, loopback webhooks, the demo key.
func TestShippedConfigLoadsAndIsRefusedInProduction(t *testing.T) {
	bc, err := loadBootstrap("../../configs")
	if err != nil {
		t.Fatal(err)
	}
	if len(bc.Network.Banks) != 3 || len(bc.Organizations) != 3 || bc.Jobs.NettingEvery.Std().Hours() != 1 {
		t.Fatalf("config: %+v", bc)
	}
	err = validateProdGuard(bc)
	if err == nil || !strings.Contains(err.Error(), "auth.mode=static") {
		t.Fatalf("a production build accepted the demo config: %v", err)
	}
	bc.Auth.Mode = "oidc"
	if err := validateProdGuard(bc); err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("sqlite accepted: %v", err)
	}
	bc.Data.Driver = "postgres"
	if err := validateProdGuard(bc); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("loopback webhooks accepted: %v", err)
	}
	bc.Notifications.AllowLoopbackHTTP = false
	if err := validateProdGuard(bc); err == nil || !strings.Contains(err.Error(), "demo key") {
		t.Fatalf("demo key accepted: %v", err)
	}
	bc.Notifications.SecretKey = "c29tZS1vdGhlci1rZXktb2YtdGhpcnR5LXR3by1ieXRlcw=="
	if err := validateProdGuard(bc); err != nil {
		t.Fatalf("a production-shaped config refused: %v", err)
	}
}
