package iso20022

import (
	"math/big"
	"testing"
)

func TestGroupDollars(t *testing.T) {
	for in, want := range map[string]string{
		"0.00":            "0.00",
		"999.99":          "999.99",
		"1000.00":         "1,000.00",
		"25000000.00":     "25,000,000.00",
		"130010958.90":    "130,010,958.90",
		"-1234567.05":     "-1,234,567.05",
		"100000":          "100,000",
		"95010958900.00":  "95,010,958,900.00",
		"-999.00":         "-999.00",
		"1000000000.01":   "1,000,000,000.01",
		"12345678901.234": "12,345,678,901.234",
	} {
		if got := GroupDollars(in); got != want {
			t.Errorf("GroupDollars(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Readable(new(big.Int).Mul(Dollars(25_000_000), big.NewInt(1))); got != "25,000,000.00" {
		t.Errorf("Readable = %q", got)
	}
}
