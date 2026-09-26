package iso

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moov-io/iso20022/pkg/camt_v08"
	"github.com/moov-io/iso20022/pkg/document"
	"github.com/moov-io/iso20022/pkg/pain_v11"
)

func TestParsePain001(t *testing.T) {
	b, err := os.ReadFile("testdata/pain001_northwind.xml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePain001(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.MessageID != "NW-20261005-001" || p.Transactions != 3 || p.ControlSum != 475_000_000 {
		t.Fatalf("header: %+v", p)
	}
	if p.Count() != 3 || p.Sum() != p.ControlSum {
		t.Fatalf("count %d sum %d", p.Count(), p.Sum())
	}
	ins := p.Instructions[0]
	if ins.ID != "NW-AP-RUN-1005" || ins.DebtorAccount != "A-3000001" {
		t.Fatalf("instruction: %+v", ins)
	}
	first, second := ins.Transactions[0], ins.Transactions[1]
	if first.EndToEndID != "INV-2026-7781" || first.Amount != 125_000_000 || first.Currency != "USD" ||
		first.CreditorName != "Contoso Ltd" || first.CreditorRouting != "234567898" || first.CreditorAccount != "B-4000001" ||
		first.ServiceLevel != "NURG" || first.InstructionID != "AP-1005-01" || first.Remittance != "INV-2026-7781 steel coil" {
		t.Fatalf("first transaction: %+v", first)
	}
	if second.ServiceLevel != "URGP" {
		t.Fatalf("a transaction-level service level overrides the instruction's: %+v", second)
	}
}

func TestParsePain001RefusesOtherMessages(t *testing.T) {
	b, _ := Pain002(StatusReport{MessageID: "X", At: time.Now(), OrigMessageID: "Y", OrigMessageType: Pain001Type, PaymentInfoID: "Z"})
	if _, err := ParsePain001(b); err == nil || !strings.Contains(err.Error(), "pain.001.001.10") {
		t.Fatalf("a pain.002 was accepted as a pain.001: %v", err)
	}
	if _, err := ParsePain001([]byte("not xml")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestPain002RoundTripsThroughMoov(t *testing.T) {
	at := time.Date(2026, 10, 5, 13, 30, 0, 0, time.UTC)
	b, err := Pain002(StatusReport{
		MessageID: "STS-1", At: at, OrigMessageID: "NW-20261005-001", OrigMessageType: Pain001Type,
		OrigCount: 3, OrigSum: 475_000_000, GroupStatus: "PART", PaymentInfoID: "NW-AP-RUN-1005",
		Transactions: []TxStatus{
			{InstructionID: "AP-1005-01", EndToEndID: "INV-2026-7781", UETR: "3974d5bc-b668-4861-9e80-4f48dbbf1360", Status: "ACTC"},
			{EndToEndID: "INV-2026-7783", Status: "RJCT", Reason: "RC01", Info: "routing number 111111118 is not a network member"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, bad := range []string{"<Prtry></Prtry>", "<Prtry/>", "0001-01-01"} {
		if strings.Contains(s, bad) {
			t.Fatalf("document carries %s:\n%s", bad, s)
		}
	}
	doc, err := document.ParseIso20022Document(b)
	if err != nil {
		t.Fatalf("moov cannot read what we wrote: %v\n%s", err, s)
	}
	m := doc.InspectMessage().(*pain_v11.CustomerPaymentStatusReportV11)
	if string(m.OrgnlGrpInfAndSts.OrgnlMsgId) != "NW-20261005-001" || string(*m.OrgnlGrpInfAndSts.GrpSts) != "PART" {
		t.Fatalf("group: %+v", m.OrgnlGrpInfAndSts)
	}
	txs := m.OrgnlPmtInfAndSts[0].TxInfAndSts
	if len(txs) != 2 || string(*txs[1].TxSts) != "RJCT" || string(txs[1].StsRsnInf[0].Rsn.Cd) != "RC01" ||
		string(*txs[0].OrgnlUETR) != "3974d5bc-b668-4861-9e80-4f48dbbf1360" {
		t.Fatalf("transactions: %+v", txs)
	}
}

func TestCamt053RoundTripsThroughMoov(t *testing.T) {
	from := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(72 * time.Hour)
	b, err := Camt053(Statement{
		MessageID: "STMT-1", StatementID: "A-3000001-20261005", At: to, From: from, To: to,
		Account: "A-3000001", Owner: "Northwind Corp", Routing: "123456780", Currency: "USD",
		Opening: 15_000_000_000, Closing: 12_150_000_000,
		Entries: []Entry{
			{Ref: "PO-1-DR", BookedAt: from.Add(time.Hour), Amount: 2_500_000_000, EndToEndID: "INV-88213"},
			{Ref: "OBL-IN-1", BookedAt: from.Add(2 * time.Hour), Amount: 900_000_000, Credit: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "0001-01-01") || strings.Contains(s, "<Prtry></Prtry>") || strings.Contains(s, "<Dt></Dt>") {
		t.Fatalf("empty choice branches left in:\n%s", s)
	}
	doc, err := document.ParseIso20022Document(b)
	if err != nil {
		t.Fatalf("moov cannot read what we wrote: %v\n%s", err, s)
	}
	m := doc.InspectMessage().(*camt_v08.BankToCustomerStatementV08)
	st := m.Stmt[0]
	if string(st.Acct.Id.Othr.Id) != "A-3000001" || len(st.Ntry) != 2 || len(st.Bal) != 2 {
		t.Fatalf("statement: %+v", st)
	}
	if st.Ntry[0].CdtDbtInd != "DBIT" || st.Ntry[0].Amt.Value != 25_000_000 || st.Ntry[1].CdtDbtInd != "CRDT" {
		t.Fatalf("entries: %+v", st.Ntry)
	}
	if string(*st.Ntry[0].NtryDtls[0].TxDtls[0].Refs.EndToEndId) != "INV-88213" {
		t.Fatal("end-to-end id lost")
	}
	if st.Bal[1].Amt.Value != 121_500_000 || string(st.Bal[1].Tp.CdOrPrtry.Cd) != "CLBD" {
		t.Fatalf("closing balance: %+v", st.Bal[1])
	}
}
