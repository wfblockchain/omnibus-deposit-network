package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Defund is a bank treasury's maker-checker request to take free position
// back to its master account.
type Defund struct {
	ent.Schema
}

func (Defund) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique().Immutable(),
		field.String("bank_id").Immutable(),
		field.Int64("amount").Positive().Immutable(),
		field.String("requested_by_id").Immutable(),
		field.String("requested_by").Immutable(),
		field.String("approved_by").Default(""),
		field.String("status"),
		field.String("ledger_ref").Immutable(),
		field.Time("created_at").Immutable(),
		field.Time("updated_at"),
	}
}

func (Defund) Indexes() []ent.Index {
	return []ent.Index{index.Fields("bank_id"), index.Fields("status")}
}

// Alert is something a bank's treasury must act on.
type Alert struct {
	ent.Schema
}

func (Alert) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique().Immutable(),
		field.String("bank_id").Immutable(),
		field.String("kind").Immutable(),
		field.Text("message").Immutable(),
		field.Bool("open").Default(true),
		field.Time("at").Immutable(),
		field.Time("closed_at").Optional().Nillable(),
	}
}

func (Alert) Indexes() []ent.Index {
	return []ent.Index{index.Fields("bank_id", "kind", "open")}
}

// NettingCycle is one cycle the operator ran.
type NettingCycle struct {
	ent.Schema
}

func (NettingCycle) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique().Immutable(),
		field.String("cycle_ref").Default("").Immutable(),
		field.Time("at").Immutable(),
		field.String("trigger").Immutable(),
		field.Int("discharged").Immutable(),
		field.Int("deferred").Immutable(),
		field.Int64("gross").Immutable(),
		field.Int64("net").Immutable(),
	}
}

// Reconciliation is one attestation of the Fed's balance against the ledger.
type Reconciliation struct {
	ent.Schema
}

func (Reconciliation) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").Unique().Immutable(),
		field.Time("at").Immutable(),
		field.Int64("fed_balance").Immutable(),
		field.Int64("ledger_total").Immutable(),
		field.Bool("reconciliation_break").Immutable(),
		field.Bool("invariants").Immutable(),
	}
}
