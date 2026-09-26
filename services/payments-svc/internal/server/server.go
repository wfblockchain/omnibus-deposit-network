// Package server configures payments-svc's HTTP and gRPC transports. Both
// serve the same generated services with the same middleware; HTTP routes
// come from the google.api.http annotations in api/payments/v1.
package server

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	stdhttp "net/http"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/logging"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/middleware/tracing"
	"github.com/go-kratos/kratos/v2/transport"
	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	pb "omnibus-deposit-network/services/payments-svc/api/payments/v1"
	"omnibus-deposit-network/services/payments-svc/internal/auth"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
	"omnibus-deposit-network/services/payments-svc/internal/metrics"
	"omnibus-deposit-network/services/payments-svc/internal/service"
)

//go:embed openapi.yaml
var openapiSpec []byte

// DBPinger checks the database for readiness.
type DBPinger interface {
	PingDB(ctx context.Context) error
}

// Services bundles the generated service implementations.
type Services struct {
	Payments *service.PaymentService
	Accounts *service.AccountService
	Webhooks *service.WebhookService
	Ops      *service.BankOperationsService
	Network  *service.NetworkOperationsService
}

// NewServices bundles the services for the servers.
func NewServices(p *service.PaymentService, a *service.AccountService, w *service.WebhookService,
	o *service.BankOperationsService, n *service.NetworkOperationsService) *Services {
	return &Services{Payments: p, Accounts: a, Webhooks: w, Ops: o, Network: n}
}

// requestID echoes the caller's Request-Id, or assigns one, on every reply,
// so a support call can name the exact request.
func requestID() middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			if tr, ok := transport.FromServerContext(ctx); ok {
				id := tr.RequestHeader().Get("Request-Id")
				if id == "" || len(id) > 64 {
					b := make([]byte, 16)
					_, _ = rand.Read(b)
					id = hex.EncodeToString(b)
				}
				tr.ReplyHeader().Set("Request-Id", id)
			}
			return next(ctx, req)
		}
	}
}

func chain(a auth.Authenticator, logger log.Logger) []middleware.Middleware {
	return []middleware.Middleware{
		recovery.Recovery(),
		tracing.Server(),
		requestID(),
		logging.Server(logger),
		metrics.Server(),
		auth.Server(a, nil),
	}
}

// NewHTTPServer serves the API, health, readiness, metrics and the OpenAPI
// document.
func NewHTTPServer(c *conf.Server, a auth.Authenticator, s *Services, db DBPinger, logger log.Logger) *khttp.Server {
	opts := []khttp.ServerOption{khttp.Middleware(chain(a, logger)...)}
	if c.HTTP != nil {
		if c.HTTP.Network != "" {
			opts = append(opts, khttp.Network(c.HTTP.Network))
		}
		if c.HTTP.Addr != "" {
			opts = append(opts, khttp.Address(c.HTTP.Addr))
		}
		if c.HTTP.Timeout > 0 {
			opts = append(opts, khttp.Timeout(c.HTTP.Timeout.Std()))
		}
	}
	srv := khttp.NewServer(opts...)
	srv.HandleFunc("/health", func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "healthy", "service": "payments-svc"})
	})
	srv.HandleFunc("/ready", func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := db.PingDB(r.Context()); err != nil {
			w.WriteHeader(stdhttp.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "degraded", "database": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready", "service": "payments-svc"})
	})
	srv.Handle("/metrics", promhttp.Handler())
	srv.HandleFunc("/v1/openapi.yaml", func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	})
	pb.RegisterPaymentServiceHTTPServer(srv, s.Payments)
	pb.RegisterAccountServiceHTTPServer(srv, s.Accounts)
	pb.RegisterWebhookServiceHTTPServer(srv, s.Webhooks)
	pb.RegisterBankOperationsServiceHTTPServer(srv, s.Ops)
	pb.RegisterNetworkOperationsServiceHTTPServer(srv, s.Network)
	return srv
}

// NewGRPCServer serves the same services over gRPC.
func NewGRPCServer(c *conf.Server, a auth.Authenticator, s *Services, logger log.Logger) *kgrpc.Server {
	opts := []kgrpc.ServerOption{kgrpc.Middleware(chain(a, logger)...)}
	if c.GRPC != nil {
		if c.GRPC.Network != "" {
			opts = append(opts, kgrpc.Network(c.GRPC.Network))
		}
		if c.GRPC.Addr != "" {
			opts = append(opts, kgrpc.Address(c.GRPC.Addr))
		}
		if c.GRPC.Timeout > 0 {
			opts = append(opts, kgrpc.Timeout(c.GRPC.Timeout.Std()))
		}
	}
	srv := kgrpc.NewServer(opts...)
	pb.RegisterPaymentServiceServer(srv, s.Payments)
	pb.RegisterAccountServiceServer(srv, s.Accounts)
	pb.RegisterWebhookServiceServer(srv, s.Webhooks)
	pb.RegisterBankOperationsServiceServer(srv, s.Ops)
	pb.RegisterNetworkOperationsServiceServer(srv, s.Network)
	return srv
}
