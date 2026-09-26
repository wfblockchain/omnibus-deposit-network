package biz

import (
	"time"

	"github.com/moov-io/base"
)

// FedCalendar counts banking days on the Federal Reserve's schedule with
// moov-io/base: weekends and Fed holidays are not banking days.
type FedCalendar struct{}

// AddBusinessDays returns the date n banking days after t, at midnight in
// t's location.
func (FedCalendar) AddBusinessDays(t time.Time, n int) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return base.NewTime(d).AddBankingDay(n).Time
}
