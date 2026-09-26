package biz

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseAmount reads a positive dollar amount with at most two decimals
// ("25000000", "25,000,000.00") into cents.
func ParseAmount(v string) (int64, error) {
	v = strings.ReplaceAll(strings.TrimSpace(v), ",", "")
	if v == "" {
		return 0, ErrInvalid("amount is required")
	}
	whole, frac, _ := strings.Cut(v, ".")
	if len(frac) > 2 {
		return 0, ErrInvalid("amount has more than two decimals")
	}
	for len(frac) < 2 {
		frac += "0"
	}
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 || w > 1_000_000_000_000 {
		return 0, ErrInvalid("amount is not a valid dollar amount")
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil || f < 0 {
		return 0, ErrInvalid("amount is not a valid dollar amount")
	}
	cents := w*100 + f
	if cents <= 0 {
		return 0, ErrInvalid("amount must be positive")
	}
	return cents, nil
}

// FormatCents renders cents as a plain decimal ("25000000.00"), as the API
// and ISO messages carry amounts.
func FormatCents(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

// Readable renders cents with thousands separators, for people.
func Readable(c int64) string {
	s := FormatCents(c)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String() + "." + frac
}

// Dollars converts whole dollars to cents.
func Dollars(d int64) int64 { return d * 100 }
