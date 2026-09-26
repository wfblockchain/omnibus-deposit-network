// Command payments-svc is a member bank's payment hub over the omnibus
// network: the corporate payment channel (API, pain.001/pain.002, webhooks),
// the bank's operations console, and the operator's network console.
//
// Kratos pattern: load config → logger → Wire → run.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/go-kratos/kratos/v2"
	"github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/env"
	"github.com/go-kratos/kratos/v2/config/file"
	"github.com/go-kratos/kratos/v2/log"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	khttp "github.com/go-kratos/kratos/v2/transport/http"

	"omnibus-deposit-network/services/payments-svc/internal/buildinfo"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
	"omnibus-deposit-network/services/payments-svc/internal/jobs"
)

var flagConf string

func init() {
	flag.StringVar(&flagConf, "conf", "configs", "config path: a file or a directory of them")
}

func main() {
	flag.Parse()
	level := log.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		level = log.ParseLevel(v)
	}
	logger := log.With(log.NewFilter(log.NewStdLogger(os.Stdout), log.FilterLevel(level)),
		"ts", log.DefaultTimestamp, "caller", log.DefaultCaller, "service.name", "payments-svc")
	h := log.NewHelper(logger)

	bc, err := loadBootstrap(flagConf)
	if err != nil {
		h.Fatalf("%v", err)
	}
	if err := validateProdGuard(bc); err != nil {
		h.Fatalf("refusing to start: %v", err)
	}
	if buildinfo.Demo {
		h.Warn("DEMO BUILD: static bearer tokens, the scenario clock and loopback webhooks are allowed")
	}
	app, cleanup, err := wireApp(bc, logger)
	if err != nil {
		h.Fatalf("wire app: %v", err)
	}
	defer cleanup()
	if err := app.Run(); err != nil {
		h.Fatalf("run: %v", err)
	}
}

// loadBootstrap reads the configuration. Values may reference environment
// variables as ${NAME:default}.
func loadBootstrap(path string) (*conf.Bootstrap, error) {
	c := config.New(config.WithSource(env.NewSource(""), file.NewSource(path)))
	defer c.Close()
	if err := c.Load(); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	var bc conf.Bootstrap
	if err := c.Scan(&bc); err != nil {
		return nil, fmt.Errorf("scan config: %w", err)
	}
	if err := bc.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &bc, nil
}

// demoSecretKey is the notifications key the shipped demo config carries.
const demoSecretKey = "ZGVtby1vbmx5LXdlYmhvb2stc2VjcmV0LWtleS0zMmI="

// validateProdGuard refuses demo settings in a production build: static
// tokens, SQLite, loopback webhook endpoints and the demo encryption key.
func validateProdGuard(bc *conf.Bootstrap) error {
	if buildinfo.Demo {
		return nil
	}
	switch {
	case bc.Auth.Mode != "oidc":
		return fmt.Errorf("auth.mode=%s in a production build; use oidc or rebuild with -tags demo", bc.Auth.Mode)
	case bc.Data.Driver != "postgres":
		return fmt.Errorf("data.driver=%s in a production build; use postgres", bc.Data.Driver)
	case bc.Notifications.AllowLoopbackHTTP:
		return fmt.Errorf("notifications.allow_loopback_http is a demo setting")
	case bc.Notifications.SecretKey == demoSecretKey:
		return fmt.Errorf("notifications.secret_key is the demo key; provide one from the secret store")
	}
	return nil
}

func newApp(logger log.Logger, hs *khttp.Server, gs *kgrpc.Server, js *jobs.Scheduler) *kratos.App {
	return kratos.New(
		kratos.Name("payments-svc"),
		kratos.Logger(logger),
		kratos.Server(hs, gs, js),
	)
}
