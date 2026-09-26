// Package iso20022 holds the subset of ISO 20022 messages the omnibus model
// exchanges with Fedwire and with member banks.
//
// Fedwire Funds and RTP both speak ISO 20022, so the backend keeps the same
// vocabulary on the token side: a cross-bank token payment is initiated from
// a pacs.008 and answered with a pacs.002; funding arrives as a pacs.009, a
// settled payment is recalled with a camt.056 and sent back as a new credit,
// and the Fed joint account is reported by a camt.052. The structs carry the
// fields the model uses, with the ISO element names, and marshal to XML for
// audit logs. They are not complete schema implementations.
package iso20022

import (
	"encoding/xml"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// UnitsPerDollar is the token and ledger scale: 6 decimals.
var UnitsPerDollar = big.NewInt(1_000_000)

// unitsPerCent converts ledger units to Fedwire cents.
var unitsPerCent = big.NewInt(10_000)

// Amount is an ISO 20022 active-currency amount.
type Amount struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

// USD formats ledger units as an ISO amount. It errors on sub-cent values,
// which Fedwire cannot carry.
func USD(units *big.Int) (Amount, error) {
	if new(big.Int).Mod(units, unitsPerCent).Sign() != 0 {
		return Amount{}, fmt.Errorf("amount %s is not a whole number of cents", units)
	}
	return Amount{Ccy: "USD", Value: FormatDollars(units)}, nil
}

// FormatDollars renders ledger units as dollars with two decimals.
func FormatDollars(units *big.Int) string {
	cents := new(big.Int).Quo(units, unitsPerCent)
	neg := cents.Sign() < 0
	if neg {
		cents.Neg(cents)
	}
	d, c := new(big.Int).QuoRem(cents, big.NewInt(100), new(big.Int))
	s := fmt.Sprintf("%s.%02d", d, c.Int64())
	if neg {
		return "-" + s
	}
	return s
}

// GroupDollars adds thousands separators to a FormatDollars string, for
// screens and audit notes; messages and the API keep the plain form.
func GroupDollars(s string) string {
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
	if frac != "" {
		b.WriteString("." + frac)
	}
	return sign + b.String()
}

// Readable renders ledger units as grouped dollars, e.g. 25,000,000.00.
func Readable(units *big.Int) string { return GroupDollars(FormatDollars(units)) }

// Dollars converts whole dollars to ledger units.
func Dollars(d int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(d), UnitsPerDollar)
}

// Agent identifies a financial institution by BIC and ABA routing number.
type Agent struct {
	BICFI string `xml:"FinInstnId>BICFI,omitempty"`
	ABA   string `xml:"FinInstnId>ClrSysMmbId>MmbId,omitempty"`
}

// GroupHeader is common to every message.
type GroupHeader struct {
	MsgID   string    `xml:"MsgId"`
	CreDtTm time.Time `xml:"CreDtTm"`
}

// Pacs009 is a financial institution credit transfer: bank-to-bank money
// over Fedwire (local instrument BTRC) or, outside Fedwire hours, a FedNow
// liquidity management transfer into a joint account (LMT1). Funding moves
// a bank's reserves into the joint account; defunding moves them back.
// As with RTP funding, EndToEndId carries the member id and the category
// purpose names the service, so the operator credits the right position.
type Pacs009 struct {
	XMLName    xml.Name    `xml:"FICdtTrf"`
	GrpHdr     GroupHeader `xml:"GrpHdr"`
	EndToEndID string      `xml:"CdtTrfTxInf>PmtId>EndToEndId"`
	UETR       string      `xml:"CdtTrfTxInf>PmtId>UETR"`
	LclInstrm  string      `xml:"CdtTrfTxInf>PmtTpInf>LclInstrm>Prtry"`
	CtgyPurp   string      `xml:"CdtTrfTxInf>PmtTpInf>CtgyPurp>Prtry,omitempty"`
	Amount     Amount      `xml:"CdtTrfTxInf>IntrBkSttlmAmt"`
	SttlmDt    string      `xml:"CdtTrfTxInf>IntrBkSttlmDt"`
	Debtor     Agent       `xml:"CdtTrfTxInf>Dbtr"`
	Creditor   Agent       `xml:"CdtTrfTxInf>Cdtr"`
	// DebtorAcct and CreditorAcct are the Fed accounts (master or joint).
	DebtorAcct   string `xml:"CdtTrfTxInf>DbtrAcct>Id>Othr>Id"`
	CreditorAcct string `xml:"CdtTrfTxInf>CdtrAcct>Id>Othr>Id"`
	Purpose      string `xml:"CdtTrfTxInf>Purp>Prtry,omitempty"`
}

// Pacs008 is a customer credit transfer: what a corporate client's
// instruction becomes at its bank before it is routed as a token payment.
type Pacs008 struct {
	XMLName      xml.Name    `xml:"FIToFICstmrCdtTrf"`
	GrpHdr       GroupHeader `xml:"GrpHdr"`
	EndToEndID   string      `xml:"CdtTrfTxInf>PmtId>EndToEndId"`
	UETR         string      `xml:"CdtTrfTxInf>PmtId>UETR"`
	Amount       Amount      `xml:"CdtTrfTxInf>IntrBkSttlmAmt"`
	DebtorName   string      `xml:"CdtTrfTxInf>Dbtr>Nm"`
	DebtorAcct   string      `xml:"CdtTrfTxInf>DbtrAcct>Id>Othr>Id"`
	DebtorAgent  Agent       `xml:"CdtTrfTxInf>DbtrAgt"`
	CreditorName string      `xml:"CdtTrfTxInf>Cdtr>Nm"`
	CreditorAcct string      `xml:"CdtTrfTxInf>CdtrAcct>Id>Othr>Id"`
	CreditorAgt  Agent       `xml:"CdtTrfTxInf>CdtrAgt"`
	RemitInfo    string      `xml:"CdtTrfTxInf>RmtInf>Ustrd,omitempty"`
}

// Transaction statuses used in pacs.002.
const (
	StatusAccepted = "ACSC" // settlement completed
	StatusPending  = "PDNG"
	StatusRejected = "RJCT"
)

// Reason codes the model uses (ISO 20022 external code set).
const (
	ReasonClosedAccount = "AC04"
	ReasonRegulatory    = "RR04"
	ReasonFraud         = "FRAD"
	ReasonNotOpen       = "NOOP" // proprietary: Fedwire not in operating hours
	ReasonInsufficient  = "AM04"
)

// Pacs002 is a payment status report.
type Pacs002 struct {
	XMLName   xml.Name    `xml:"FIToFIPmtStsRpt"`
	GrpHdr    GroupHeader `xml:"GrpHdr"`
	OrgnlUETR string      `xml:"TxInfAndSts>OrgnlUETR"`
	TxSts     string      `xml:"TxInfAndSts>TxSts"`
	Reason    string      `xml:"TxInfAndSts>StsRsnInf>Rsn>Cd,omitempty"`
}

// Advice is what a Fed account holder learns about one movement on its
// account: for a credit, the receiver's copy of the incoming pacs.009 (the
// "advice from the Prefunded Balance Account Bank" RTP credits positions on);
// for a debit, the confirmation of its own outbound transfer. Interest the
// Fed books arrives as a credit with purpose INTR. It is an internal record,
// not an ISO message of its own.
type Advice struct {
	XMLName     xml.Name    `xml:"FedwireAdvice"`
	GrpHdr      GroupHeader `xml:"GrpHdr"`
	Account     string      `xml:"Ntfctn>Acct>Id>Othr>Id"`
	CdtDbtInd   string      `xml:"Ntfctn>Ntry>CdtDbtInd"` // CRDT or DBIT
	Amount      Amount      `xml:"Ntfctn>Ntry>Amt"`
	BookingDate string      `xml:"Ntfctn>Ntry>BookgDt>Dt"`
	UETR        string      `xml:"Ntfctn>Ntry>NtryDtls>TxDtls>Refs>UETR"`
	EndToEndID  string      `xml:"Ntfctn>Ntry>NtryDtls>TxDtls>Refs>EndToEndId"`
	LclInstrm   string      `xml:"Ntfctn>Ntry>NtryDtls>TxDtls>LclInstrm,omitempty"`
	Counterpart Agent       `xml:"Ntfctn>Ntry>NtryDtls>TxDtls>RltdAgts>DbtrAgt"`
	Purpose     string      `xml:"Ntfctn>Ntry>NtryDtls>TxDtls>Purp>Prtry,omitempty"`
	// Units is the amount in ledger units, carried alongside the ISO amount
	// so services never re-parse decimal strings.
	Units *big.Int `xml:"-"`
}

// Camt052 is a Fedwire Funds account report (requested with a camt.060):
// the balance and activity of a Fed account at a point in time.
type Camt052 struct {
	XMLName    xml.Name    `xml:"BkToCstmrAcctRpt"`
	GrpHdr     GroupHeader `xml:"GrpHdr"`
	StmtID     string      `xml:"Rpt>Id"`
	Account    string      `xml:"Rpt>Acct>Id>Othr>Id"`
	ClosingBal Amount      `xml:"Rpt>Bal>Amt"`
	Entries    int         `xml:"Rpt>TxsSummry>TtlNtries>NbOfNtries"`
	Units      *big.Int    `xml:"-"`
}

// Camt056 is a request for return (cancellation of a settled payment).
type Camt056 struct {
	XMLName   xml.Name    `xml:"FIToFIPmtCxlReq"`
	GrpHdr    GroupHeader `xml:"GrpHdr"`
	OrgnlUETR string      `xml:"Undrlyg>TxInf>OrgnlUETR"`
	Reason    string      `xml:"Undrlyg>TxInf>CxlRsnInf>Rsn>Cd"`
}

// Pacs004 is a Fedwire payment return. RTP has no pacs.004: an RTP return
// is a new pacs.008 that references the original.
type Pacs004 struct {
	XMLName   xml.Name    `xml:"PmtRtr"`
	GrpHdr    GroupHeader `xml:"GrpHdr"`
	OrgnlUETR string      `xml:"TxInf>OrgnlUETR"`
	Amount    Amount      `xml:"TxInf>RtrdIntrBkSttlmAmt"`
	Reason    string      `xml:"TxInf>RtrRsnInf>Rsn>Cd"`
}

// XML renders any message for the audit log.
func XML(m any) string {
	b, err := xml.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Sprintf("<!-- marshal error: %v -->", err)
	}
	return string(b)
}
