package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PaymentOrder is a client's payment order and its lifecycle.
type PaymentOrder struct {
	ent.Schema
}

func (PaymentOrder) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique().Immutable().Comment("UUIDv7"),
		field.String("organization_id").NotEmpty().Immutable(),
		field.String("bank_id").NotEmpty().Immutable().Comment("the organization's bank (member id)"),
		field.String("file_id").Optional().Nillable().Immutable().Comment("pain.001 file it came in"),
		field.String("idempotency_key").Optional().Nillable().Immutable(),
		field.String("request_hash").Immutable().Comment("SHA-256 of the request, to tell a replay from a key reuse"),
		field.String("end_to_end_id").NotEmpty().Immutable(),
		field.String("uetr").NotEmpty().Immutable(),
		field.String("debtor_account").NotEmpty().Immutable(),
		field.String("creditor_name").NotEmpty().Immutable(),
		field.String("creditor_routing").NotEmpty().Immutable(),
		field.String("creditor_account").NotEmpty().Immutable(),
		field.Int64("amount").Positive().Immutable().Comment("cents"),
		field.String("priority").Immutable(),
		field.String("remittance").Default("").Immutable(),
		field.String("status"),
		field.String("iso_status"),
		field.String("reason_code").Default(""),
		field.String("route").Default(""),
		field.Text("route_note").Default(""),
		field.String("payee_account_status").Immutable(),
		field.String("payee_name_match").Immutable(),
		field.String("payee_registered_name").Default("").Immutable(),
		field.Int("approvals_required").Immutable(),
		field.String("created_by").Immutable(),
		field.Time("created_at").Immutable(),
		field.Time("updated_at"),
		field.Time("settled_at").Optional().Nillable(),
		field.Time("ofac_report_due").Optional().Nillable(),
		field.String("sanctions_match").Default(""),
		field.String("payment_ref").Default("").Comment("router payment id"),
		field.String("obligation_ref").Default("").Comment("netting obligation id"),
		field.Bool("tokenized").Default(false),
		field.Bool("released").Default(false),
		field.Int("version").Default(0).Comment("optimistic lock"),
	}
}

func (PaymentOrder) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("events", OrderEvent.Type),
		edge.To("approvals", Approval.Type),
	}
}

func (PaymentOrder) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("organization_id", "idempotency_key").Unique(),
		index.Fields("organization_id", "end_to_end_id").Unique(),
		index.Fields("organization_id", "created_at"),
		index.Fields("bank_id", "status"),
		index.Fields("status"),
		index.Fields("file_id"),
	}
}
