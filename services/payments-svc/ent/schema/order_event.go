package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// OrderEvent is one line of a payment's audit trail. Rows are only ever
// inserted.
type OrderEvent struct {
	ent.Schema
}

func (OrderEvent) Fields() []ent.Field {
	return []ent.Field{
		field.String("order_id").Immutable(),
		field.Int("seq").Immutable(),
		field.Time("at").Immutable(),
		field.String("actor").Immutable(),
		field.String("status").Immutable(),
		field.String("iso_status").Immutable(),
		field.Text("note").Immutable(),
		field.Text("detail").Default("").Immutable().Comment("bank and operator staff only"),
	}
}

func (OrderEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("order", PaymentOrder.Type).Ref("events").Field("order_id").Unique().Required().Immutable(),
	}
}

func (OrderEvent) Indexes() []ent.Index {
	return []ent.Index{index.Fields("order_id", "seq").Unique()}
}
