package biz

import "testing"

func TestNameMatch(t *testing.T) {
	for _, c := range []struct{ typed, registered, want string }{
		{"Contoso Ltd", "Contoso Ltd", "MATCH"},
		{"CONTOSO LIMITED", "Contoso Ltd", "MATCH"},
		{"Contoso", "Contoso Ltd.", "MATCH"},
		{"Contosso Ltd", "Contoso Ltd", "CLOSE_MATCH"},
		{"Northwind Traders", "Northwind Corp", "CLOSE_MATCH"},
		{"Dave Ltd", "Contoso Ltd", "NO_MATCH"},
		{"", "Contoso Ltd", "NO_MATCH"},
	} {
		if got := NameMatch(c.typed, c.registered); got != c.want {
			t.Errorf("%q vs %q: %s, want %s", c.typed, c.registered, got, c.want)
		}
	}
}
