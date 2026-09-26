//go:build wireinject

// Wire dependency injection declarations; `make wire` generates wire_gen.go.
package main

import (
	"github.com/go-kratos/kratos/v2"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/wire"

	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
	"omnibus-deposit-network/services/payments-svc/internal/data"
	"omnibus-deposit-network/services/payments-svc/internal/jobs"
	"omnibus-deposit-network/services/payments-svc/internal/notify"
	"omnibus-deposit-network/services/payments-svc/internal/rails"
	"omnibus-deposit-network/services/payments-svc/internal/sanctions"
	"omnibus-deposit-network/services/payments-svc/internal/server"
	"omnibus-deposit-network/services/payments-svc/internal/service"
)

func wireApp(*conf.Bootstrap, log.Logger) (*kratos.App, func(), error) {
	panic(wire.Build(
		// Configuration
		provideServerConf, provideDataConf, provideNetworkConf, provideSanctionsConf,
		provideNotificationsConf, provideJobsConf, provideContext, provideOptions,

		// Screening and the rails
		sanctions.New,
		wire.Bind(new(biz.Screener), new(*sanctions.Screener)),
		provideBankScreener,
		rails.New,
		wire.Bind(new(biz.Rails), new(*rails.Omnibus)),

		// Data
		notify.NewBox,
		data.NewData,
		data.NewRepos,
		provideWebhookRepo,
		wire.Bind(new(server.DBPinger), new(*data.Data)),

		// Business logic
		provideCalendar,
		biz.NewEngine,
		biz.NewPaymentUseCase,
		biz.NewAccountUseCase,
		biz.NewWebhookUseCase,
		biz.NewComplianceUseCase,
		biz.NewTreasuryUseCase,
		biz.NewNetworkUseCase,
		biz.NewProcessor,

		// Services and transports
		provideAuthenticator,
		service.NewPaymentService,
		service.NewAccountService,
		service.NewWebhookService,
		service.NewBankOperationsService,
		service.NewNetworkOperationsService,
		server.NewServices,
		server.NewHTTPServer,
		server.NewGRPCServer,

		// Background work
		notify.NewDispatcher,
		jobs.NewScheduler,

		newApp,
	))
}
