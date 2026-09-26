package fedwire

import (
	"math/big"
	"testing"
	"time"

	"omnibus-deposit-network/internal/iso20022"
)

func TestOperatingDay(t *testing.T) {
	cal, err := NewCalendar()
	if err != nil {
		t.Fatal(err)
	}
	at := func(y int, m time.Month, d, h, min int) time.Time {
		return time.Date(y, m, d, h, min, 0, 0, cal.Loc)
	}
	cases := []struct {
		name string
		t    time.Time
		open bool
	}{
		{"Thursday mid-morning", at(2026, 10, 1, 10, 0), true},
		{"Thursday 18:59", at(2026, 10, 1, 18, 59), true},
		{"Thursday 19:00, after the close", at(2026, 10, 1, 19, 0), false},
		{"Thursday 21:00 opens Friday's day", at(2026, 10, 1, 21, 0), true},
		{"Friday 21:00 would open Saturday", at(2026, 10, 2, 21, 0), false},
		{"Saturday", at(2026, 10, 3, 11, 0), false},
		{"Sunday 20:59", at(2026, 10, 4, 20, 59), false},
		{"Sunday 21:00 opens Monday's day", at(2026, 10, 4, 21, 0), true},
		{"Eve of Thanksgiving 21:00", at(2026, 11, 25, 21, 0), false},
		{"Thanksgiving", at(2026, 11, 26, 10, 0), false},
		{"Friday 3 July 2026 (4 July is a Saturday)", at(2026, 7, 3, 10, 0), true},
	}
	for _, c := range cases {
		if got := cal.IsOpen(c.t); got != c.open {
			t.Errorf("%s: open = %v, want %v", c.name, got, c.open)
		}
	}
	if got := cal.NextOpen(at(2026, 10, 2, 19, 30)); !got.Equal(at(2026, 10, 4, 21, 0)) {
		t.Errorf("next open after Friday close = %s, want Sunday 21:00", got)
	}
}

func TestSendRules(t *testing.T) {
	cal, _ := NewCalendar()
	clock := NewManualClock(time.Date(2026, 10, 3, 11, 0, 0, 0, cal.Loc)) // Saturday
	s := New(clock, cal)
	s.OpenAccount("A", iso20022.Agent{ABA: "1"}, iso20022.Dollars(100))
	s.OpenAccount("B", iso20022.Agent{ABA: "2"}, big.NewInt(0))
	var notes []iso20022.Advice
	s.Subscribe("B", func(n iso20022.Advice) { notes = append(notes, n) })

	amt, _ := iso20022.USD(iso20022.Dollars(40))
	m := iso20022.Pacs009{UETR: NewUETR(), Amount: amt, DebtorAcct: "A", CreditorAcct: "B"}
	if st := s.Send(m, iso20022.Dollars(40)); st.TxSts != iso20022.StatusRejected || st.Reason != iso20022.ReasonNotOpen {
		t.Fatalf("weekend transfer: %+v", st)
	}

	clock.Set(time.Date(2026, 10, 5, 10, 0, 0, 0, cal.Loc)) // Monday
	if st := s.Send(m, iso20022.Dollars(40)); st.TxSts != iso20022.StatusAccepted {
		t.Fatalf("Monday transfer: %+v", st)
	}
	if len(notes) != 1 || notes[0].CdtDbtInd != "CRDT" || notes[0].UETR != m.UETR {
		t.Fatalf("camt.054 to creditor: %+v", notes)
	}
	if st := s.Send(m, iso20022.Dollars(61)); st.Reason != iso20022.ReasonInsufficient {
		t.Fatalf("overdraft: %+v", st)
	}
	if s.Statement("B").Units.Cmp(iso20022.Dollars(40)) != 0 {
		t.Fatalf("statement balance wrong")
	}
}

func TestAmounts(t *testing.T) {
	if got := iso20022.FormatDollars(big.NewInt(95_010_958_900_000)); got != "95010958.90" {
		t.Errorf("format: %s", got)
	}
	if _, err := iso20022.USD(big.NewInt(1)); err == nil {
		t.Error("a sub-cent amount must not become a Fedwire amount")
	}
}

func TestAnnounced22x6Calendar(t *testing.T) {
	cal, err := NewCalendar22x6()
	if err != nil {
		t.Fatal(err)
	}
	at := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, cal.Loc) }
	for _, c := range []struct {
		name string
		t    time.Time
		open bool
	}{
		{"Saturday daytime stays closed", at(10, 3, 11), false},
		{"Saturday 21:00 opens Sunday's day", at(10, 3, 21), true},
		{"Sunday daytime", at(10, 4, 11), true},
		{"Thanksgiving, a weekday holiday", at(11, 26, 10), true},
		{"Friday 21:00 would open Saturday", at(10, 2, 21), false},
	} {
		if got := cal.IsOpen(c.t); got != c.open {
			t.Errorf("%s: open = %v, want %v", c.name, got, c.open)
		}
	}
}

func TestLiquidityManagementTransfers(t *testing.T) {
	cal, _ := NewCalendar()
	clock := NewManualClock(time.Date(2026, 10, 3, 11, 0, 0, 0, cal.Loc)) // Saturday
	s := New(clock, cal)
	s.OpenAccount("MASTER", iso20022.Agent{ABA: "1"}, iso20022.Dollars(100))
	s.OpenAccount("JOINT", iso20022.Agent{ABA: "2"}, iso20022.Dollars(10))
	amt, _ := iso20022.USD(iso20022.Dollars(5))
	in := iso20022.Pacs009{UETR: NewUETR(), Amount: amt, DebtorAcct: "MASTER", CreditorAcct: "JOINT", LclInstrm: "LMT1"}
	out := iso20022.Pacs009{UETR: NewUETR(), Amount: amt, DebtorAcct: "JOINT", CreditorAcct: "MASTER", LclInstrm: "LMT1"}

	if st := s.Send(in, iso20022.Dollars(5)); st.TxSts != iso20022.StatusRejected {
		t.Fatal("LMT into a joint account that is not enabled must be refused")
	}
	s.EnableLMT("JOINT")
	if st := s.Send(in, iso20022.Dollars(5)); st.TxSts != iso20022.StatusAccepted {
		t.Fatalf("LMT funding on a Saturday: %+v", st)
	}
	if st := s.Send(out, iso20022.Dollars(5)); st.TxSts != iso20022.StatusRejected {
		t.Fatal("LMT must never defund a joint account")
	}
	clock.Set(time.Date(2026, 10, 5, 10, 0, 0, 0, cal.Loc)) // Monday, Fedwire open
	if st := s.Send(in, iso20022.Dollars(5)); st.TxSts != iso20022.StatusRejected {
		t.Fatal("LMT runs only while Fedwire is closed")
	}
}
