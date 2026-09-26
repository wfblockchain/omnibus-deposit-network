package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Approval is one approver's sign-off on a payment. The unique index makes
// "each approver counts once" a database fact.
type Approval struct {
	ent.Schema
}

func (Approval) Fields() []ent.Field {
	return []ent.Field{
		field.String("order_id").Immutable(),
		field.String("approver_id").Immutable(),
		field.String("approver_name").Immutable(),
		field.Time("at").Immutable(),
	}
}

func (Approval) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("order", PaymentOrder.Type).Ref("approvals").Field("order_id").Unique().Required().Immutable(),
	}
}

func (Approval) Indexes() []ent.Index {
	return []ent.Index{index.Fields("order_id", "approver_id").Unique()}
}
