// Package notify delivers the webhook outbox: CloudEvents 1.0 payloads,
// signed per Standard Webhooks (webhook-id, webhook-timestamp,
// webhook-signature: v1,<base64 HMAC-SHA256 of "id.timestamp.body">), in
// order per endpoint, at least once, retried with backoff.
//
// Endpoints are checked when registered (https only), and again when dialed:
// the dialer refuses loopback, private, link-local and other non-public
// addresses, so a hostname that later resolves inward cannot reach the
// bank's own network.
package notify

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-kratos/kratos/v2/log"

	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
)

// ─── Secrets at rest ───

// Box seals webhook secrets with AES-256-GCM.
type Box struct{ aead cipher.AEAD }

// NewBox builds the box from notifications.secret_key.
func NewBox(c *conf.Notifications) (biz.SecretBox, error) {
	key, err := base64.StdEncoding.DecodeString(c.SecretKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("notifications.secret_key must be 32 bytes, base64")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, []byte("webhook-secret")), nil
}

func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed secret too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], []byte("webhook-secret"))
}

// ─── Signatures ───

// Sign is the Standard Webhooks signature over "id.timestamp.body".
func Sign(key []byte, id, ts string, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a delivery the way a receiver should: the signature under
// the whsec_ secret, and a timestamp within tolerance of now.
func Verify(secret string, h http.Header, body []byte, now time.Time, tolerance time.Duration) error {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return fmt.Errorf("bad secret: %w", err)
	}
	id, ts := h.Get("webhook-id"), h.Get("webhook-timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("bad timestamp")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return errors.New("timestamp outside tolerance")
	}
	want := Sign(key, id, ts, body)
	for _, sig := range strings.Fields(h.Get("webhook-signature")) {
		if v, ok := strings.CutPrefix(sig, "v1,"); ok && hmac.Equal([]byte(v), []byte(want)) {
			return nil
		}
	}
	return errors.New("no valid signature")
}

// ─── Delivery ───

// Backoff is the wait before each retry; after the last the delivery fails.
var Backoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 3 * time.Hour}

// Dispatcher drains the outbox.
type Dispatcher struct {
	repo   biz.WebhookRepo
	client *http.Client
	Now    func() time.Time
	log    *log.Helper
}

// NewDispatcher builds the dispatcher. With AllowLoopbackHTTP (demo builds)
// the dialer admits loopback addresses; nothing else non-public, ever.
func NewDispatcher(repo biz.WebhookRepo, c *conf.Notifications, logger log.Logger) *Dispatcher {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: publicOnly(c.AllowLoopbackHTTP)}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second, MaxIdleConnsPerHost: 2}
	return &Dispatcher{
		repo: repo,
		client: &http.Client{Transport: transport, Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Now: time.Now,
		log: log.NewHelper(logger),
	}
}

func publicOnly(allowLoopback bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("webhook dial: %q is not an IP", host)
		}
		if ip.IsLoopback() && allowLoopback {
			return nil
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || ip.Equal(net.ParseIP("169.254.169.254")) {
			return fmt.Errorf("webhook dial: %s is not a public address", ip)
		}
		return nil
	}
}

// Flush attempts every due delivery, oldest first. Deliveries to one
// endpoint go one at a time and stop at its first failure or first
// not-yet-due retry, so an endpoint sees its events in order.
func (d *Dispatcher) Flush(ctx context.Context) (delivered int, err error) {
	pending, err := d.repo.Pending(ctx, 500)
	if err != nil {
		return 0, err
	}
	blocked := map[string]bool{}
	subs := map[string]*biz.Subscription{}
	now := d.Now()
	for _, x := range pending {
		if blocked[x.Subscription] {
			continue
		}
		if x.NextAttempt.After(now) {
			blocked[x.Subscription] = true
			continue
		}
		s, ok := subs[x.Subscription]
		if !ok {
			if s, err = d.repo.GetSubscription(ctx, x.Subscription); err != nil {
				x.Status, x.LastError = "FAILED", "subscription unavailable: "+err.Error()
				_ = d.repo.UpdateDelivery(ctx, x)
				continue
			}
			subs[x.Subscription] = s
		}
		code, sendErr := d.send(ctx, s, x)
		x.Attempts++
		x.LastCode = code
		if sendErr == nil {
			x.Status, x.LastError = "DELIVERED", ""
			delivered++
		} else {
			blocked[x.Subscription] = true
			x.LastError = sendErr.Error()
			if x.Attempts > len(Backoff) {
				x.Status = "FAILED"
			} else {
				x.NextAttempt = d.Now().Add(Backoff[x.Attempts-1])
			}
		}
		if err := d.repo.UpdateDelivery(ctx, x); err != nil {
			return delivered, err
		}
	}
	return delivered, nil
}

func (d *Dispatcher) send(ctx context.Context, s *biz.Subscription, x *biz.Delivery) (int, error) {
	ts := strconv.FormatInt(d.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(x.Payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("webhook-id", x.ID)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+Sign(s.Secret, x.ID, ts, x.Payload))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("endpoint answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
