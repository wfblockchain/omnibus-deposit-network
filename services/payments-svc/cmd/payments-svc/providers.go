package main

import (
	"context"
	"fmt"

	"omnibus-deposit-network/internal/bank"
	"omnibus-deposit-network/services/payments-svc/internal/auth"
	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
	"omnibus-deposit-network/services/payments-svc/internal/sanctions"
)

func provideServerConf(bc *conf.Bootstrap) *conf.Server               { return bc.Server }
func provideDataConf(bc *conf.Bootstrap) *conf.Data                   { return bc.Data }
func provideNetworkConf(bc *conf.Bootstrap) *conf.Network             { return bc.Network }
func provideSanctionsConf(bc *conf.Bootstrap) *conf.Sanctions         { return bc.Sanctions }
func provideNotificationsConf(bc *conf.Bootstrap) *conf.Notifications { return bc.Notifications }
func provideJobsConf(bc *conf.Bootstrap) *conf.Jobs                   { return bc.Jobs }
func provideContext() context.Context                                 { return context.Background() }
func provideCalendar() biz.Calendar                                   { return biz.FedCalendar{} }
func provideWebhookRepo(r biz.Repos) biz.WebhookRepo                  { return r.Webhooks }

// provideBankScreener lets receiving banks screen inbound payees against the
// same OFAC list.
func provideBankScreener(s *sanctions.Screener) bank.Screener { return s.ForBanks() }

func provideAuthenticator(bc *conf.Bootstrap) (auth.Authenticator, error) {
	if bc.Auth.Mode == "static" {
		return auth.NewStatic(bc.Auth.StaticUsers)
	}
	return auth.NewOIDC(context.Background(), bc.Auth.Issuer, bc.Auth.Audience)
}

// provideOptions turns configuration into the engine's policy.
func provideOptions(bc *conf.Bootstrap) (biz.Options, error) {
	opts := biz.Options{Watermarks: map[string]biz.Watermark{}, AllowLoopbackHTTP: bc.Notifications.AllowLoopbackHTTP}
	if bc.Jobs != nil {
		opts.NettingEvery = bc.Jobs.NettingEvery.Std()
	}
	amount := func(what, v string) (int64, error) {
		if v == "" {
			return 0, nil
		}
		c, err := biz.ParseAmount(v)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", what, err)
		}
		return c, nil
	}
	for _, b := range bc.Network.Banks {
		low, err := amount(b.MemberID+" low_watermark", b.LowWatermark)
		if err != nil {
			return opts, err
		}
		normal, err := amount(b.MemberID+" normal_watermark", b.NormalWatermark)
		if err != nil {
			return opts, err
		}
		opts.Watermarks[b.MemberID] = biz.Watermark{Low: low, Normal: normal}
	}
	for _, o := range bc.Organizations {
		org := biz.Org{ID: o.ID, Name: o.Name, Bank: o.Bank, Account: o.Account}
		var err error
		if org.PerPaymentLimit, err = amount(o.ID+" per_payment_limit", o.PerPaymentLimit); err != nil {
			return opts, err
		}
		if org.DailyLimit, err = amount(o.ID+" daily_limit", o.DailyLimit); err != nil {
			return opts, err
		}
		if org.SecondApprovalAbove, err = amount(o.ID+" second_approval_above", o.SecondApprovalAbove); err != nil {
			return opts, err
		}
		opts.Orgs = append(opts.Orgs, org)
	}
	return opts, nil
}
