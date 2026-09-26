package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// WebhookSubscription is an organization's endpoint. The signing secret is
// stored sealed (AES-256-GCM).
type WebhookSubscription struct {
	ent.Schema
}

func (WebhookSubscription) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique().Immutable(),
		field.String("organization_id").Immutable(),
		field.String("url").Immutable(),
		field.Bytes("secret_sealed").Sensitive().Immutable(),
		field.String("created_by").Immutable(),
		field.Time("created_at").Immutable(),
	}
}

func (WebhookSubscription) Indexes() []ent.Index {
	return []ent.Index{index.Fields("organization_id")}
}

// WebhookDelivery is the outbox: one event on its way to one endpoint,
// written in the same transaction as the status change that caused it.
type WebhookDelivery struct {
	ent.Schema
}

func (WebhookDelivery) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique().Immutable().Comment("msg_<UUIDv7>: the webhook-id, ordered"),
		field.String("subscription_id").Immutable(),
		field.String("organization_id").Immutable(),
		field.String("event_type").Immutable(),
		field.Bytes("payload").Immutable(),
		field.String("status").Default("PENDING"),
		field.Int("attempts").Default(0),
		field.Int("last_code").Default(0),
		field.Text("last_error").Default(""),
		field.Time("next_attempt_at"),
		field.Time("created_at").Immutable(),
	}
}

func (WebhookDelivery) Indexes() []ent.Index {
	return []ent.Index{index.Fields("status", "id"), index.Fields("subscription_id", "id")}
}
