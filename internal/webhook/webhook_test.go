package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

type receiver struct {
	mu     sync.Mutex
	secret string
	fail   int // answer 503 this many times first
	got    []Event
	ids    []string
	bad    []error
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := Verify(r.secret, req.Header, body, time.Now(), 5*time.Minute); err != nil {
		r.bad = append(r.bad, err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.fail > 0 {
		r.fail--
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	var ev Event
	_ = json.Unmarshal(body, &ev)
	r.got = append(r.got, ev)
	r.ids = append(r.ids, req.Header.Get("webhook-id"))
}

func TestSignedOrderedRetriedDelivery(t *testing.T) {
	rcv := &receiver{fail: 1}
	srv := httptest.NewServer(rcv)
	defer srv.Close()

	now := time.Now()
	d := New()
	d.AllowLoopbackHTTP = true
	d.Now = func() time.Time { return now }
	sub, err := d.Subscribe("northwind", srv.URL+"/hooks")
	if err != nil {
		t.Fatal(err)
	}
	rcv.secret = sub.Secret
	d.Publish("northwind", "payment.in_process", "/api/v1/payments/PO-1", now, map[string]string{"status": "IN_PROCESS"})
	d.Publish("northwind", "payment.settled", "/api/v1/payments/PO-1", now, map[string]string{"status": "SETTLED"})
	d.Publish("contoso", "payment.settled", "/api/v1/payments/PO-2", now, nil) // no endpoint: nothing queued

	if n := d.Flush(context.Background()); n != 0 {
		t.Fatalf("first flush delivered %d; the endpoint was down", n)
	}
	if n := d.Flush(context.Background()); n != 0 {
		t.Fatalf("retried before the backoff: %d", n)
	}
	now = now.Add(Backoff[0])
	if n := d.Flush(context.Background()); n != 2 {
		t.Fatalf("after backoff delivered %d, want 2", n)
	}
	if len(rcv.bad) != 0 {
		t.Fatalf("signature failures: %v", rcv.bad)
	}
	if len(rcv.got) != 2 || rcv.got[0].Type != "payment.in_process" || rcv.got[1].Type != "payment.settled" {
		t.Fatalf("order: %+v", rcv.got)
	}
	ds, _ := d.Deliveries("northwind", sub.ID)
	if ds[0].Attempts != 2 || ds[0].Status != "delivered" || ds[0].ID != rcv.ids[0] {
		t.Fatalf("delivery log: %+v", ds[0])
	}
	if _, err := d.Deliveries("contoso", sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another organization read the delivery log: %v", err)
	}
}

func TestVerifyRejectsTamperingAndReplay(t *testing.T) {
	d := New()
	d.AllowLoopbackHTTP = true
	sub, _ := d.Subscribe("o", "http://127.0.0.1:1/x")
	key := d.keys[sub.ID]
	body := []byte(`{"a":1}`)
	now := time.Now()
	h := http.Header{}
	h.Set("webhook-id", "msg_1")
	h.Set("webhook-timestamp", itoa(now.Unix()))
	h.Set("webhook-signature", "v1,"+Sign(key, "msg_1", itoa(now.Unix()), body))
	if err := Verify(sub.Secret, h, body, now, time.Minute); err != nil {
		t.Fatalf("valid delivery refused: %v", err)
	}
	if err := Verify(sub.Secret, h, []byte(`{"a":2}`), now, time.Minute); err == nil {
		t.Fatal("tampered body accepted")
	}
	if err := Verify(sub.Secret, h, body, now.Add(10*time.Minute), time.Minute); err == nil {
		t.Fatal("stale timestamp accepted")
	}
}

func TestSubscribeRefusesUnsafeEndpoints(t *testing.T) {
	d := New()
	for _, u := range []string{"http://example.com/x", "ftp://example.com", "https://user:pw@example.com/", "/relative", "http://127.0.0.1/x"} {
		if _, err := d.Subscribe("o", u); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted", u)
		}
	}
	if _, err := d.Subscribe("o", "https://erp.northwind.example/hooks"); err != nil {
		t.Errorf("https refused: %v", err)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
