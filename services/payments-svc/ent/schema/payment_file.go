package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Rejection is a file transaction refused at validation.
type Rejection struct {
	EndToEndID string `json:"endToEndId"`
	Reason     string `json:"reason"`
	Message    string `json:"message"`
}

// PaymentFile is a pain.001 a client sent.
type PaymentFile struct {
	ent.Schema
}

func (PaymentFile) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique().Immutable(),
		field.String("organization_id").Immutable(),
		field.String("message_id").Immutable().Comment("GrpHdr/MsgId"),
		field.String("request_hash").Immutable(),
		field.String("idempotency_key").Immutable(),
		field.Int("transactions").Immutable(),
		field.Int64("control_sum").Immutable(),
		field.String("created_by").Immutable(),
		field.Time("created_at").Immutable(),
		field.JSON("rejections", []Rejection{}).Optional(),
	}
}

func (PaymentFile) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("organization_id", "message_id").Unique(),
		index.Fields("organization_id", "idempotency_key").Unique(),
	}
}
