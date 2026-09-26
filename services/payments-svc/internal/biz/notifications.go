package biz

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	stderrors "errors"
	"net"
	"net/url"
)

// WebhookUseCase manages an organization's status notification endpoints.
// Delivery itself is internal/notify's job, reading the outbox.
type WebhookUseCase struct{ e *Engine }

// NewWebhookUseCase creates the use case.
func NewWebhookUseCase(e *Engine) *WebhookUseCase { return &WebhookUseCase{e: e} }

func approver(p Principal) error {
	if p.Org == "" || p.Role != RoleApprover {
		return ErrForbidden("an approver of the organization manages its webhooks")
	}
	return nil
}

// Create registers an endpoint and returns its signing secret, whsec_…,
// which is not shown again.
func (uc *WebhookUseCase) Create(ctx context.Context, p Principal, endpoint string) (*Subscription, string, error) {
	if err := approver(p); err != nil {
		return nil, "", err
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, "", ErrInvalid("url must be absolute, without credentials or fragment")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && uc.e.opts.AllowLoopbackHTTP && loopback(u.Hostname()):
	default:
		return nil, "", ErrInvalid("url must be https")
	}
	key := make([]byte, 24)
	if _, err := rand.Read(key); err != nil {
		return nil, "", err
	}
	s := &Subscription{ID: newID(), Org: p.Org, URL: u.String(), Secret: key, CreatedBy: p.ID, CreatedAt: uc.e.now()}
	if err := uc.e.r.Webhooks.CreateSubscription(ctx, s); err != nil {
		return nil, "", err
	}
	return s, "whsec_" + base64.StdEncoding.EncodeToString(key), nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// List lists the organization's endpoints.
func (uc *WebhookUseCase) List(ctx context.Context, p Principal) ([]*Subscription, error) {
	if err := approver(p); err != nil {
		return nil, err
	}
	return uc.e.r.Webhooks.ListSubscriptions(ctx, p.Org)
}

func (uc *WebhookUseCase) own(ctx context.Context, p Principal, id string) error {
	if err := approver(p); err != nil {
		return err
	}
	s, err := uc.e.r.Webhooks.GetSubscription(ctx, id)
	if stderrors.Is(err, ErrNoRows) || (err == nil && s.Org != p.Org) {
		return ErrNotFound("no webhook %s", id)
	}
	return err
}

// Delete removes an endpoint.
func (uc *WebhookUseCase) Delete(ctx context.Context, p Principal, id string) error {
	if err := uc.own(ctx, p, id); err != nil {
		return err
	}
	return uc.e.r.Webhooks.DeleteSubscription(ctx, id)
}

// Deliveries is an endpoint's delivery log.
func (uc *WebhookUseCase) Deliveries(ctx context.Context, p Principal, id string) ([]*Delivery, error) {
	if err := uc.own(ctx, p, id); err != nil {
		return nil, err
	}
	return uc.e.r.Webhooks.ListDeliveries(ctx, id)
}
