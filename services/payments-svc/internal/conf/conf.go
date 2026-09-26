// Package conf holds payments-svc configuration.
//
// Kratos loads YAML and scans it through JSON, so every field carries a json
// tag, and durations use Duration, which accepts "30s" as well as integer
// nanoseconds.
package conf

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Bootstrap is the root configuration, loaded from configs/.
type Bootstrap struct {
	Server        *Server        `json:"server"`
	Data          *Data          `json:"data"`
	Auth          *Auth          `json:"auth"`
	Network       *Network       `json:"network"`
	Sanctions     *Sanctions     `json:"sanctions"`
	Notifications *Notifications `json:"notifications"`
	Jobs          *Jobs          `json:"jobs"`
	Organizations []Organization `json:"organizations"`
}

// Duration is a time.Duration that loads from "30s" or from nanoseconds.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration: %s", b)
	}
	*d = Duration(n)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Server holds the listeners.
type Server struct {
	HTTP *Listener `json:"http"`
	GRPC *Listener `json:"grpc"`
}

// Listener is one transport's address.
type Listener struct {
	Network string   `json:"network"`
	Addr    string   `json:"addr"`
	Timeout Duration `json:"timeout"`
}

// Data configures the database. Driver is postgres in production; sqlite
// (pure Go) serves tests and local runs.
type Data struct {
	Driver       string `json:"driver"`
	Source       string `json:"source"`
	MaxOpenConns int    `json:"max_open_conns"`
	MaxIdleConns int    `json:"max_idle_conns"`
}

// Auth selects how bearer tokens are verified: "oidc" verifies JWTs from an
// issuer; "static" maps fixed tokens to people and is refused by production
// builds.
type Auth struct {
	Mode        string       `json:"mode"`
	Issuer      string       `json:"issuer"`
	Audience    string       `json:"audience"`
	StaticUsers []StaticUser `json:"static_users"`
}

// StaticUser is a demo identity behind a fixed bearer token.
type StaticUser struct {
	Token        string `json:"token"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	Bank         string `json:"bank"`
	Organization string `json:"organization"`
}

// Network configures the rails: the chain, the Fed simulator and the member
// banks. With RPCURL empty the service starts its own anvil.
type Network struct {
	RPCURL    string        `json:"rpc_url"`
	AnvilBin  string        `json:"anvil_bin"`
	Artifacts string        `json:"artifacts"`
	StartTime string        `json:"start_time"` // RFC 3339; the scenario clock starts here
	Banks     []Bank        `json:"banks"`
	Accounts  []BankAccount `json:"accounts"`
}

// Bank is a member bank of the network.
type Bank struct {
	MemberID           string `json:"member_id"`
	Name               string `json:"name"`
	Ticker             string `json:"ticker"`
	RoutingNumber      string `json:"routing_number"`
	RequiresAcceptance bool   `json:"requires_acceptance"`
	InitialFunding     string `json:"initial_funding"`  // dollars
	LowWatermark       string `json:"low_watermark"`    // dollars of mint capacity
	NormalWatermark    string `json:"normal_watermark"` // dollars of mint capacity
}

// BankAccount is a deposit account opened at start-up.
type BankAccount struct {
	Bank    string `json:"bank"`
	Account string `json:"account"`
	Name    string `json:"name"`
	Opening string `json:"opening"` // dollars
	Closed  bool   `json:"closed"`
}

// Organization is a corporate client and its entitlements.
type Organization struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Bank                string `json:"bank"`
	Account             string `json:"account"`
	PerPaymentLimit     string `json:"per_payment_limit"`
	DailyLimit          string `json:"daily_limit"`
	SecondApprovalAbove string `json:"second_approval_above"`
}

// Sanctions configures OFAC screening. Dir holds the Treasury's sdn.csv,
// add.csv, alt.csv and sdn_comments.csv; with Download set, a missing list
// is fetched from treasury.gov at start-up.
type Sanctions struct {
	OFACDir  string  `json:"ofac_dir"`
	Download bool    `json:"download"`
	MinMatch float64 `json:"min_match"`
}

// Notifications configures webhook delivery. SecretKey (base64, 32 bytes)
// encrypts endpoint signing secrets at rest.
type Notifications struct {
	SecretKey         string `json:"secret_key"`
	AllowLoopbackHTTP bool   `json:"allow_loopback_http"`
}

// Jobs configures background processing.
type Jobs struct {
	TickInterval    Duration `json:"tick_interval"`
	WebhookInterval Duration `json:"webhook_interval"`
	NettingEvery    Duration `json:"netting_every"` // on the scenario clock
}

// Validate checks what the service cannot start without.
func (b *Bootstrap) Validate() error {
	var errs []string
	if b.Server == nil || b.Server.HTTP == nil {
		errs = append(errs, "server.http is required")
	}
	if b.Data == nil || b.Data.Driver == "" || b.Data.Source == "" {
		errs = append(errs, "data.driver and data.source are required")
	} else if b.Data.Driver != "postgres" && b.Data.Driver != "sqlite" {
		errs = append(errs, "data.driver must be postgres or sqlite")
	}
	if b.Auth == nil || (b.Auth.Mode != "oidc" && b.Auth.Mode != "static") {
		errs = append(errs, "auth.mode must be oidc or static")
	} else if b.Auth.Mode == "oidc" && (b.Auth.Issuer == "" || b.Auth.Audience == "") {
		errs = append(errs, "auth.issuer and auth.audience are required for oidc")
	}
	if b.Network == nil || len(b.Network.Banks) == 0 {
		errs = append(errs, "network.banks is required")
	}
	if b.Notifications == nil {
		errs = append(errs, "notifications.secret_key is required")
	} else if k, err := base64.StdEncoding.DecodeString(b.Notifications.SecretKey); err != nil || len(k) != 32 {
		errs = append(errs, "notifications.secret_key must be 32 bytes, base64")
	}
	if b.Sanctions == nil || b.Sanctions.OFACDir == "" {
		errs = append(errs, "sanctions.ofac_dir is required")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
