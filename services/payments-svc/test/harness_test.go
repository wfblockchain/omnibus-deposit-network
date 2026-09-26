package test

import (
	"context"
	"fmt"
	"io"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"
	khttp "github.com/go-kratos/kratos/v2/transport/http"

	pb "omnibus-deposit-network/services/payments-svc/api/payments/v1"
	"omnibus-deposit-network/services/payments-svc/internal/notify"
)

// server is a running payments-svc built with -tags demo.
type server struct {
	base, grpc string
	cmd        *exec.Cmd
	log        *os.File
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// start builds and runs the service against a fresh anvil and database.
// PAYMENTS_TEST_POSTGRES, when set, is a Postgres DSN to use instead of
// SQLite.
func start(t *testing.T) *server {
	t.Helper()
	anvil := os.Getenv("ANVIL_BIN")
	if anvil == "" {
		if p, err := exec.LookPath("anvil"); err == nil {
			anvil = p
		}
	}
	if anvil == "" {
		t.Skip("anvil not found: set ANVIL_BIN or put Foundry on PATH")
	}
	svcDir, _ := filepath.Abs("..")
	artifacts := filepath.Join(svcDir, "..", "..", "contracts", "out")
	if _, err := os.Stat(filepath.Join(artifacts, "OmnibusNetting.sol", "OmnibusNetting.json")); err != nil {
		t.Skip("contracts not built: run `forge build` in contracts/")
	}
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/moov-io/watchman").Output()
	if err != nil {
		t.Skipf("watchman module unavailable: %v", err)
	}
	ofacDir := filepath.Join(strings.TrimSpace(string(out)), "pkg", "sources", "ofac", "testdata")

	dir := t.TempDir()
	bin := filepath.Join(dir, "payments-svc")
	build := exec.Command("go", "build", "-tags", "demo", "-o", bin, "./cmd/payments-svc")
	build.Dir = svcDir
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	httpAddr, grpcAddr := freeAddr(t), freeAddr(t)
	driver, dsn := "sqlite", "file:"+filepath.Join(dir, "payments.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	if pg := os.Getenv("PAYMENTS_TEST_POSTGRES"); pg != "" {
		driver, dsn = "postgres", pg
	}
	logf, _ := os.Create(filepath.Join(dir, "service.log"))
	cmd := exec.Command(bin, "-conf", filepath.Join(svcDir, "configs"))
	cmd.Env = append(os.Environ(),
		"HTTP_ADDR="+httpAddr, "GRPC_ADDR="+grpcAddr, "ANVIL_BIN="+anvil, "CONTRACT_ARTIFACTS="+artifacts,
		"OFAC_DIR="+ofacDir, "DATABASE_DRIVER="+driver, "DATABASE_URL="+dsn)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &server{base: "http://" + httpAddr, grpc: grpcAddr, cmd: cmd, log: logf}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
		if t.Failed() {
			b, _ := os.ReadFile(logf.Name())
			lines := strings.Split(string(b), "\n")
			if len(lines) > 60 {
				lines = lines[len(lines)-60:]
			}
			t.Logf("service log (tail):\n%s", strings.Join(lines, "\n"))
		}
	})
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if resp, err := nethttp.Get(s.base + "/ready"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return s
			}
		}
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	b, _ := os.ReadFile(logf.Name())
	t.Fatalf("service did not become ready:\n%s", b)
	return nil
}

// ─── Calling as a person ───

type callKey struct{}

type call struct {
	token, idempotencyKey, requestID string
}

// as is a context that calls as the person holding token.
func as(token string) context.Context {
	return context.WithValue(context.Background(), callKey{}, call{token: token})
}

func withKey(ctx context.Context, key string) context.Context {
	c, _ := ctx.Value(callKey{}).(call)
	c.idempotencyKey = key
	return context.WithValue(ctx, callKey{}, c)
}

func withRequestID(ctx context.Context, id string) context.Context {
	c, _ := ctx.Value(callKey{}).(call)
	c.requestID = id
	return context.WithValue(ctx, callKey{}, c)
}

// headers sets Authorization, Idempotency-Key and Request-Id on outgoing
// calls from the context.
func headers() middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			if tr, ok := transport.FromClientContext(ctx); ok {
				c, _ := ctx.Value(callKey{}).(call)
				if c.token != "" {
					tr.RequestHeader().Set("Authorization", "Bearer "+c.token)
				}
				if c.idempotencyKey != "" {
					tr.RequestHeader().Set("Idempotency-Key", c.idempotencyKey)
				}
				if c.requestID != "" {
					tr.RequestHeader().Set("Request-Id", c.requestID)
				}
			}
			return next(ctx, req)
		}
	}
}

// clients are the generated Kratos HTTP clients.
type clients struct {
	pay  pb.PaymentServiceHTTPClient
	acct pb.AccountServiceHTTPClient
	hook pb.WebhookServiceHTTPClient
	ops  pb.BankOperationsServiceHTTPClient
	op   pb.NetworkOperationsServiceHTTPClient
}

func newClients(t *testing.T, base string) *clients {
	t.Helper()
	conn, err := khttp.NewClient(context.Background(), khttp.WithEndpoint(base), khttp.WithMiddleware(headers()),
		khttp.WithTimeout(60*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &clients{
		pay: pb.NewPaymentServiceHTTPClient(conn), acct: pb.NewAccountServiceHTTPClient(conn),
		hook: pb.NewWebhookServiceHTTPClient(conn), ops: pb.NewBankOperationsServiceHTTPClient(conn),
		op: pb.NewNetworkOperationsServiceHTTPClient(conn),
	}
}

// eventually polls until ok or the timeout; background jobs run every few
// seconds.
func eventually(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ─── An ERP's webhook endpoint ───

type receivedEvent struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

type erp struct {
	mu     sync.Mutex
	secret string
	events []receivedEvent
	ids    map[string]bool
	bad    []error
}

func (e *erp) ServeHTTP(w nethttp.ResponseWriter, r *nethttp.Request) {
	body, _ := io.ReadAll(r.Body)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := notify.Verify(e.secret, r.Header, body, time.Now(), 5*time.Minute); err != nil {
		e.bad = append(e.bad, err)
		w.WriteHeader(nethttp.StatusUnauthorized)
		return
	}
	id := r.Header.Get("webhook-id")
	if e.ids[id] { // at-least-once: deduplicate on webhook-id
		return
	}
	e.ids[id] = true
	var ev receivedEvent
	if err := jsonUnmarshal(body, &ev); err != nil {
		e.bad = append(e.bad, fmt.Errorf("payload: %w", err))
		return
	}
	e.events = append(e.events, ev)
}

func newERP(t *testing.T) (*erp, string) {
	e := &erp{ids: map[string]bool{}}
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return e, srv.URL
}
