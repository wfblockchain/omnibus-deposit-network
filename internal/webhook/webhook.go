// Package webhook pushes payment status changes to the endpoints a client
// registers, the way large banks push them to
// their corporate clients: an event per status change, in a CloudEvents 1.0
// envelope, signed with the Standard Webhooks scheme (webhook-id,
// webhook-timestamp, webhook-signature: v1,<base64 HMAC-SHA256>), and
// retried with backoff until the endpoint answers 2xx.
//
// Delivery is at least once: a receiver deduplicates on webhook-id, and
// rejects a timestamp too far from its own clock to stop replays.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrInvalid reports a subscription request that cannot be accepted.
var ErrInvalid = errors.New("invalid")

// ErrNotFound reports a subscription the caller cannot see.
var ErrNotFound = errors.New("not found")

// Subscription is one registered endpoint of an organization.
type Subscription struct {
	ID      string    `json:"id"`
	Org     string    `json:"org"`
	URL     string    `json:"url"`
	Secret  string    `json:"secret,omitempty"` // returned once, when created
	Created time.Time `json:"createdAt"`
}

// Event is a CloudEvents 1.0 envelope.
type Event struct {
	SpecVersion     string    `json:"specversion"`
	ID              string    `json:"id"`
	Source          string    `json:"source"`
	Type            string    `json:"type"`
	Time            time.Time `json:"time"`
	DataContentType string    `json:"datacontenttype"`
	Data            any       `json:"data"`
}

// Delivery is one event on its way to one subscription.
type Delivery struct {
	ID        string    `json:"id"` // the webhook-id header; stable across retries
	Sub       string    `json:"subscription"`
	Type      string    `json:"type"`
	Status    string    `json:"status"` // pending, delivered, failed
	Attempts  int       `json:"attempts"`
	LastCode  int       `json:"lastResponseCode,omitempty"`
	LastError string    `json:"lastError,omitempty"`
	NextAt    time.Time `json:"nextAttemptAt,omitzero"`

	org  string
	body []byte
}

// Backoff is the wait before each retry; after the last, the delivery fails.
var Backoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 3 * time.Hour}

// Dispatcher holds subscriptions and an outbox of deliveries.
type Dispatcher struct {
	mu     sync.Mutex
	subs   map[string]*Subscription
	keys   map[string][]byte // subscription id → HMAC key
	outbox []*Delivery
	seq    int

	// AllowLoopbackHTTP admits http:// endpoints on loopback addresses, for
	// local demos; every other endpoint must be https.
	AllowLoopbackHTTP bool
	Client            *http.Client
	Now               func() time.Time // wall clock, for retries and signatures
}

// New returns an empty dispatcher.
func New() *Dispatcher {
	return &Dispatcher{
		subs: map[string]*Subscription{}, keys: map[string][]byte{},
		Client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Now: time.Now,
	}
}

// Subscribe registers an endpoint for an organization and returns it with
// its signing secret, which is not shown again.
func (d *Dispatcher) Subscribe(org, endpoint string) (Subscription, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return Subscription{}, fmt.Errorf("%w: url must be absolute, without credentials or fragment", ErrInvalid)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && d.AllowLoopbackHTTP && loopback(u.Hostname()):
	default:
		return Subscription{}, fmt.Errorf("%w: url must be https", ErrInvalid)
	}
	key := make([]byte, 24)
	if _, err := rand.Read(key); err != nil {
		return Subscription{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	s := &Subscription{ID: fmt.Sprintf("WH-%04d", d.seq), Org: org, URL: u.String(), Created: d.Now()}
	d.subs[s.ID], d.keys[s.ID] = s, key
	out := *s
	out.Secret = "whsec_" + base64.StdEncoding.EncodeToString(key)
	return out, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Unsubscribe removes an organization's subscription; pending deliveries
// to it are dropped.
func (d *Dispatcher) Unsubscribe(org, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.subs[id]
	if !ok || s.Org != org {
		return ErrNotFound
	}
	delete(d.subs, id)
	delete(d.keys, id)
	return nil
}

// Subscriptions lists an organization's endpoints, without secrets.
func (d *Dispatcher) Subscriptions(org string) []Subscription {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []Subscription{}
	for i := 1; i <= d.seq; i++ {
		if s, ok := d.subs[fmt.Sprintf("WH-%04d", i)]; ok && s.Org == org {
			out = append(out, *s)
		}
	}
	return out
}

// Publish queues an event for every endpoint of the organization.
func (d *Dispatcher) Publish(org, typ, source string, at time.Time, data any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := 1; i <= d.seq; i++ {
		s, ok := d.subs[fmt.Sprintf("WH-%04d", i)]
		if !ok || s.Org != org {
			continue
		}
		id := "msg_" + randomID()
		body, err := json.Marshal(Event{SpecVersion: "1.0", ID: id, Source: source, Type: typ, Time: at,
			DataContentType: "application/json", Data: data})
		if err != nil {
			continue
		}
		d.outbox = append(d.outbox, &Delivery{ID: id, Sub: s.ID, Type: typ, Status: "pending", NextAt: d.Now(), org: org, body: body})
	}
}

// Deliveries lists an organization's deliveries to one endpoint, newest last.
func (d *Dispatcher) Deliveries(org, sub string) ([]Delivery, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.subs[sub]; !ok || s.Org != org {
		return nil, ErrNotFound
	}
	out := []Delivery{}
	for _, x := range d.outbox {
		if x.Sub == sub {
			out = append(out, *x)
		}
	}
	return out, nil
}

// Flush attempts every delivery that is due, in order, and returns how many
// succeeded. Deliveries to one endpoint go one at a time and stop at the
// first failure, so an endpoint sees its events in order.
func (d *Dispatcher) Flush(ctx context.Context) int {
	d.mu.Lock()
	now := d.Now()
	var due []*Delivery
	blocked := map[string]bool{}
	for _, x := range d.outbox {
		if x.Status != "pending" || blocked[x.Sub] {
			continue
		}
		if _, ok := d.subs[x.Sub]; !ok {
			x.Status, x.LastError, x.NextAt = "failed", "subscription removed", time.Time{}
			continue
		}
		if x.NextAt.After(now) {
			blocked[x.Sub] = true
			continue
		}
		due = append(due, x)
	}
	d.mu.Unlock()

	ok := 0
	failed := map[string]bool{}
	for _, x := range due {
		if failed[x.Sub] {
			continue
		}
		d.mu.Lock()
		s, live := d.subs[x.Sub]
		var endpoint string
		var key []byte
		if live {
			endpoint, key = s.URL, d.keys[x.Sub]
		}
		d.mu.Unlock()
		if !live {
			continue
		}
		code, err := d.send(ctx, endpoint, key, x)
		d.mu.Lock()
		x.Attempts++
		x.LastCode = code
		if err == nil {
			x.Status, x.LastError, x.NextAt = "delivered", "", time.Time{}
			ok++
		} else {
			failed[x.Sub] = true
			x.LastError = err.Error()
			if x.Attempts > len(Backoff) {
				x.Status, x.NextAt = "failed", time.Time{}
			} else {
				x.NextAt = d.Now().Add(Backoff[x.Attempts-1])
			}
		}
		d.mu.Unlock()
	}
	return ok
}

func (d *Dispatcher) send(ctx context.Context, endpoint string, key []byte, x *Delivery) (int, error) {
	ts := strconv.FormatInt(d.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(x.body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("webhook-id", x.ID)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+Sign(key, x.ID, ts, x.body))
	resp, err := d.Client.Do(req)
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

// Run flushes the outbox every interval until ctx ends.
func (d *Dispatcher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.Flush(ctx)
		}
	}
}

// Sign is the Standard Webhooks signature: base64 HMAC-SHA256 over
// "id.timestamp.body" with the secret's key.
func Sign(key []byte, id, ts string, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a delivery the way a receiver should: the signature under
// the secret, and a timestamp within tolerance of now.
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

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
