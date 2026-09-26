package biz

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"strings"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/uuid"
)

// Watermark is a bank's alert thresholds on its mint capacity, as RTP
// participants set them on their prefunded position: below Low an alert
// opens; at Normal it closes.
type Watermark struct {
	Low, Normal int64
}

// Options are the engine's policy settings.
type Options struct {
	Orgs              []Org
	Watermarks        map[string]Watermark
	NettingEvery      time.Duration // scheduled cycles on the scenario clock; 0 leaves them to operator ops
	AllowLoopbackHTTP bool          // webhook endpoints on http://localhost (demo)
}

// Repos bundles the repositories.
type Repos struct {
	Orders   OrderRepo
	Files    FileRepo
	Webhooks WebhookRepo
	Defunds  DefundRepo
	Alerts   AlertRepo
	Network  NetworkRepo
	Tx       Transaction
}

// Engine is the shared core of the use cases. Every operation that touches
// the rails runs under one lock: the rails send chain transactions from
// shared keys and drive the simulators. Payment state is in the database;
// each state change and the webhook deliveries it causes are written in
// one transaction (a transactional outbox).
type Engine struct {
	mu     sync.Mutex
	r      Repos
	rails  Rails
	screen Screener
	cal    Calendar
	box    SecretBox
	opts   Options
	orgs   map[string]Org
	et     *time.Location
	log    *log.Helper

	nextCycle time.Time
	pending   map[string][]notice // by order id, flushed when the order is saved
}

type notice struct {
	org, typ, source string
	at               time.Time
	data             map[string]any
}

// NewEngine builds the engine.
func NewEngine(r Repos, rails Rails, screen Screener, cal Calendar, box SecretBox, opts Options, logger log.Logger) *Engine {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		et = time.UTC
	}
	e := &Engine{r: r, rails: rails, screen: screen, cal: cal, box: box, opts: opts, et: et,
		orgs: map[string]Org{}, pending: map[string][]notice{}, log: log.NewHelper(logger)}
	for _, o := range opts.Orgs {
		e.orgs[o.ID] = o
	}
	return e
}

func (e *Engine) now() time.Time { return e.rails.Now().In(e.et) }

// Org returns an organization's entitlements.
func (e *Engine) Org(id string) (Org, bool) {
	o, ok := e.orgs[id]
	return o, ok
}

func newID() string { return uuid.Must(uuid.NewV7()).String() }

// ─── Audit trail and notifications ───

// event appends an audit event and, when the status or ISO status changes,
// queues a client notification for when the order is saved.
func (e *Engine) event(o *Order, actor string, st Status, iso, note, detail string) {
	changed := st != o.Status || iso != o.ISO
	o.Status, o.ISO = st, iso
	ev := Event{Seq: len(o.History) + 1, At: e.now(), Actor: actor, Status: st, ISO: iso, Note: note, Detail: detail}
	o.History = append(o.History, ev)
	if !changed {
		return
	}
	typ := "payment." + strings.ToLower(string(st))
	if iso == ISOCredited {
		typ = "payment.credited"
	}
	data := map[string]any{
		"paymentId": o.ID, "endToEndId": o.EndToEndID, "uetr": o.UETR,
		"status": o.Status, "isoStatus": o.ISO,
		"amount": FormatCents(o.Amount), "currency": "USD", "statusUpdatedAt": ev.At, "note": note,
	}
	if o.Reason != "" {
		data["reasonCode"] = o.Reason
	}
	e.pending[o.ID] = append(e.pending[o.ID], notice{org: o.Org, typ: typ, source: "/v1/payments/" + o.ID, at: ev.At, data: data})
}

// cloudEvent is the CloudEvents 1.0 envelope of a webhook delivery.
type cloudEvent struct {
	SpecVersion     string         `json:"specversion"`
	ID              string         `json:"id"`
	Source          string         `json:"source"`
	Type            string         `json:"type"`
	Time            time.Time      `json:"time"`
	DataContentType string         `json:"datacontenttype"`
	Data            map[string]any `json:"data"`
}

// save stores an order (creating it when create is set) together with the
// webhook deliveries its new events cause.
func (e *Engine) save(ctx context.Context, o *Order, create bool) error {
	o.UpdatedAt = e.now()
	notices := e.pending[o.ID]
	err := e.r.Tx.InTx(ctx, func(ctx context.Context) error {
		var err error
		if create {
			err = e.r.Orders.Create(ctx, o)
		} else {
			err = e.r.Orders.Update(ctx, o)
		}
		if err != nil {
			return err
		}
		for _, n := range notices {
			n := n
			if err := e.r.Webhooks.Enqueue(ctx, n.org, n.typ, func(id string) []byte {
				b, _ := json.Marshal(cloudEvent{SpecVersion: "1.0", ID: id, Source: n.source, Type: n.typ, Time: n.at,
					DataContentType: "application/json", Data: n.data})
				return b
			}, n.at); err != nil {
				return err
			}
		}
		return nil
	})
	delete(e.pending, o.ID)
	return err
}

// ─── Access ───

// visible reports whether p may see o: a client its organization's orders,
// bank staff their bank's clients' orders, operator operations everything.
func visible(p Principal, o *Order) bool {
	switch {
	case p.Org != "":
		return o.Org == p.Org
	case p.IsBankStaff():
		return o.Bank == p.Bank
	case p.Role == RoleOperatorOps:
		return true
	}
	return false
}

// load fetches an order the principal may see; anything else is not found.
func (e *Engine) load(ctx context.Context, p Principal, id string) (*Order, error) {
	o, err := e.r.Orders.Get(ctx, id)
	if stderrors.Is(err, ErrNoRows) || (err == nil && !visible(p, o)) {
		return nil, ErrNotFound("no payment %s", id)
	}
	return o, err
}

// ViewFor is an order as p may see it: clients get the account-level story;
// bank and operator staff also see token mechanics and screening detail.
func ViewFor(p Principal, o *Order) *Order {
	cp := *o
	cp.History = append([]Event(nil), o.History...)
	cp.Approvals = append([]Approval(nil), o.Approvals...)
	if p.Org != "" {
		for i := range cp.History {
			cp.History[i].Detail = ""
		}
		cp.SanctionsMatch = ""
	}
	return &cp
}

func viewsFor(p Principal, os []*Order) []*Order {
	out := make([]*Order, 0, len(os))
	for _, o := range os {
		out = append(out, ViewFor(p, o))
	}
	return out
}
