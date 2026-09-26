package iso

import (
	"encoding/xml"
	"time"

	"github.com/moov-io/iso20022/pkg/camt_v08"
	"github.com/moov-io/iso20022/pkg/common"
	"github.com/moov-io/iso20022/pkg/document"
	"github.com/moov-io/iso20022/pkg/utils"
)

// Camt053Type is the statement this package writes.
const Camt053Type = "camt.053.001.08"

// Statement is one account's statement.
type Statement struct {
	MessageID   string
	StatementID string
	At          time.Time
	From, To    time.Time
	Account     string
	Owner       string
	Routing     string // the servicing bank's ABA
	Currency    string
	Opening     int64 // cents
	Closing     int64
	Entries     []Entry
}

// Entry is one booked posting.
type Entry struct {
	Ref        string
	BookedAt   time.Time
	Amount     int64 // cents, positive
	Credit     bool
	EndToEndID string
}

// Camt053 writes a bank-to-customer statement.
func Camt053(s Statement) ([]byte, error) {
	d, err := document.NewDocument(utils.DocumentCamt05300108NameSpace)
	if err != nil {
		return nil, err
	}
	m := d.InspectMessage().(*camt_v08.BankToCustomerStatementV08)
	m.GrpHdr = camt_v08.GroupHeader81{MsgId: common.Max35Text(s.MessageID), CreDtTm: common.ISODateTime(s.At.UTC())}

	created := common.ISODateTime(s.At.UTC())
	owner := common.Max140Text(s.Owner)
	ccy := common.ActiveOrHistoricCurrencyCode(s.Currency)
	stmt := camt_v08.AccountStatement9{
		Id:      common.Max35Text(s.StatementID),
		CreDtTm: &created,
		FrToDt:  &camt_v08.DateTimePeriod1{FrDtTm: common.ISODateTime(s.From.UTC()), ToDtTm: common.ISODateTime(s.To.UTC())},
		Acct: &camt_v08.CashAccount39{
			Id:   camt_v08.AccountIdentification4Choice{Othr: camt_v08.GenericAccountIdentification1{Id: common.Max34Text(s.Account)}},
			Ccy:  &ccy,
			Ownr: &camt_v08.PartyIdentification135{Nm: &owner},
			Svcr: &camt_v08.BranchAndFinancialInstitutionIdentification6{FinInstnId: camt_v08.FinancialInstitutionIdentification18{
				ClrSysMmbId: &camt_v08.ClearingSystemMemberIdentification2{
					ClrSysId: &camt_v08.ClearingSystemIdentification2Choice{Cd: "USABA"},
					MmbId:    common.Max35Text(s.Routing),
				},
			}},
		},
		Bal: []camt_v08.CashBalance8{balance("OPBD", s.Opening, s.Currency, s.From), balance("CLBD", s.Closing, s.Currency, s.To)},
	}
	for _, e := range s.Entries {
		ind := common.CreditDebitCode("DBIT")
		if e.Credit {
			ind = "CRDT"
		}
		ref := common.Max35Text(e.Ref)
		booked := camt_v08.DateAndDateTime2Choice{DtTm: common.ISODateTime(e.BookedAt.UTC())}
		ntry := camt_v08.ReportEntry10{
			NtryRef:   &ref,
			Amt:       camt_v08.ActiveOrHistoricCurrencyAndAmount{Value: dollars(e.Amount), Ccy: ccy},
			CdtDbtInd: ind,
			Sts:       camt_v08.EntryStatus1Choice{Cd: "BOOK"},
			BookgDt:   &booked,
			BkTxCd: camt_v08.BankTransactionCodeStructure4{Domn: &camt_v08.BankTransactionCodeStructure5{
				Cd: "PMNT",
				Fmly: camt_v08.BankTransactionCodeStructure6{
					Cd:        familyCode(e.Credit),
					SubFmlyCd: "OTHR",
				},
			}},
		}
		if e.EndToEndID != "" {
			e2e := common.Max35Text(e.EndToEndID)
			ntry.NtryDtls = []camt_v08.EntryDetails9{{TxDtls: []camt_v08.EntryTransaction10{{
				Refs: &camt_v08.TransactionReferences6{EndToEndId: &e2e},
			}}}}
		}
		stmt.Ntry = append(stmt.Ntry, ntry)
	}
	m.Stmt = []camt_v08.AccountStatement9{stmt}

	obj := d.(*document.Iso20022DocumentObject)
	obj.XMLName = xml.Name{Local: "Document"}
	obj.Attrs = []xml.Attr{{Name: xml.Name{Local: "xmlns"}, Value: utils.DocumentCamt05300108NameSpace}}
	return marshal(d)
}

func balance(code string, amount int64, ccy string, at time.Time) camt_v08.CashBalance8 {
	ind := common.CreditDebitCode("CRDT")
	if amount < 0 {
		ind, amount = "DBIT", -amount
	}
	return camt_v08.CashBalance8{
		Tp:        camt_v08.BalanceType13{CdOrPrtry: camt_v08.BalanceType10Choice{Cd: camt_v08.ExternalBalanceType1Code(code)}},
		Amt:       camt_v08.ActiveOrHistoricCurrencyAndAmount{Value: dollars(amount), Ccy: common.ActiveOrHistoricCurrencyCode(ccy)},
		CdtDbtInd: ind,
		Dt:        camt_v08.DateAndDateTime2Choice{DtTm: common.ISODateTime(at.UTC())},
	}
}

// familyCode is the ISO bank transaction code family for a credit transfer
// received or issued.
func familyCode(credit bool) camt_v08.ExternalBankTransactionFamily1Code {
	if credit {
		return "RCDT"
	}
	return "ICDT"
}
