package notify

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kratos/kratos/v2/log"

	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
)

var key32 = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func TestBoxSealsAndDetectsTampering(t *testing.T) {
	b, err := NewBox(&conf.Notifications{SecretKey: key32})
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := b.Seal([]byte("secret"))
	if strings.Contains(string(sealed), "secret") {
		t.Fatal("plaintext visible")
	}
	if got, err := b.Open(sealed); err != nil || string(got) != "secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	if _, err := NewBox(&conf.Notifications{SecretKey: "c2hvcnQ="}); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestVerify(t *testing.T) {
	key := []byte("k")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(key)
	now := time.Now()
	body := []byte(`{"a":1}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	h := http.Header{}
	h.Set("webhook-id", "msg_1")
	h.Set("webhook-timestamp", ts)
	h.Set("webhook-signature", "v1,"+Sign(key, "msg_1", ts, body))
	if err := Verify(secret, h, body, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if Verify(secret, h, []byte(`{"a":2}`), now, time.Minute) == nil {
		t.Fatal("tampered body accepted")
	}
	if Verify(secret, h, body, now.Add(10*time.Minute), time.Minute) == nil {
		t.Fatal("stale timestamp accepted")
	}
}

// memRepo is an in-memory outbox.
type memRepo struct {
	biz.WebhookRepo
	mu   sync.Mutex
	subs map[string]*biz.Subscription
	out  []*biz.Delivery
}

func (m *memRepo) Pending(context.Context, int) ([]*biz.Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var p []*biz.Delivery
	for _, d := range m.out {
		if d.Status == "PENDING" {
			cp := *d
			p = append(p, &cp)
		}
	}
	return p, nil
}

func (m *memRepo) GetSubscription(_ context.Context, id string) (*biz.Subscription, error) {
	return m.subs[id], nil
}

func (m *memRepo) UpdateDelivery(_ context.Context, d *biz.Delivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.out {
		if m.out[i].ID == d.ID {
			cp := *d
			m.out[i] = &cp
		}
	}
	return nil
}

func TestDispatcherDeliversInOrderAndBacksOff(t *testing.T) {
	var mu sync.Mutex
	var got []string
	fail := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if err := Verify("whsec_"+base64.StdEncoding.EncodeToString([]byte("k")), r.Header, b, time.Now(), time.Minute); err != nil {
			w.WriteHeader(401)
			return
		}
		if fail > 0 {
			fail--
			w.WriteHeader(503)
			return
		}
		got = append(got, r.Header.Get("webhook-id"))
	}))
	defer srv.Close()
	now := time.Now()
	repo := &memRepo{subs: map[string]*biz.Subscription{"s": {ID: "s", URL: srv.URL, Secret: []byte("k")}},
		out: []*biz.Delivery{
			{ID: "msg_1", Subscription: "s", Payload: []byte("{}"), Status: "PENDING", NextAttempt: now},
			{ID: "msg_2", Subscription: "s", Payload: []byte("{}"), Status: "PENDING", NextAttempt: now},
		}}
	d := NewDispatcher(repo, &conf.Notifications{AllowLoopbackHTTP: true}, log.DefaultLogger)
	d.Now = func() time.Time { return now }
	if n, _ := d.Flush(context.Background()); n != 0 {
		t.Fatalf("delivered %d while the endpoint was down", n)
	}
	if n, _ := d.Flush(context.Background()); n != 0 {
		t.Fatalf("retried before the backoff and out of order: %d", n)
	}
	now = now.Add(Backoff[0])
	if n, _ := d.Flush(context.Background()); n != 2 {
		t.Fatalf("after backoff delivered %d", n)
	}
	if strings.Join(got, ",") != "msg_1,msg_2" {
		t.Fatalf("order: %v", got)
	}
	if repo.out[0].Attempts != 2 || repo.out[0].Status != "DELIVERED" {
		t.Fatalf("log: %+v", repo.out[0])
	}
}

func TestDialerRefusesNonPublicAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	repo := &memRepo{subs: map[string]*biz.Subscription{"s": {ID: "s", URL: srv.URL, Secret: []byte("k")}},
		out: []*biz.Delivery{{ID: "msg_1", Subscription: "s", Payload: []byte("{}"), Status: "PENDING", NextAttempt: time.Now()}}}
	d := NewDispatcher(repo, &conf.Notifications{AllowLoopbackHTTP: false}, log.DefaultLogger)
	if n, _ := d.Flush(context.Background()); n != 0 {
		t.Fatal("delivered to a loopback address without the demo allowance")
	}
	if !strings.Contains(repo.out[0].LastError, "not a public address") {
		t.Fatalf("error: %q", repo.out[0].LastError)
	}
	for _, ip := range []string{"10.0.0.5:443", "192.168.1.1:443", "169.254.169.254:80", "[::1]:443", "0.0.0.0:443"} {
		if err := publicOnly(false)("tcp", ip, nil); err == nil {
			t.Errorf("%s allowed", ip)
		}
	}
	if err := publicOnly(false)("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("public address refused: %v", err)
	}
}
