package biz

import (
	"testing"
	"time"
)

func TestOFACDueDateSkipsWeekendsAndFedHolidays(t *testing.T) {
	et, _ := time.LoadLocation("America/New_York")
	// Friday 2 Oct 2026, 18:30 ET: ten banking days later skips two weekends
	// and Columbus Day (Mon 12 Oct), landing on Mon 19 Oct.
	got := FedCalendar{}.AddBusinessDays(time.Date(2026, 10, 2, 18, 30, 0, 0, et), 10)
	if want := time.Date(2026, 10, 19, 0, 0, 0, 0, et); !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestParseAmount(t *testing.T) {
	for in, want := range map[string]int64{"25000000": 2_500_000_000, "25,000,000.00": 2_500_000_000, "0.01": 1, "1.5": 150} {
		if got, err := ParseAmount(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "-5", "1.005", "abc", "1e9"} {
		if _, err := ParseAmount(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if Readable(2_500_000_050) != "25,000,000.50" || FormatCents(-150) != "-1.50" {
		t.Fatal("formatting")
	}
}
