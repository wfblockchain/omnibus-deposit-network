package iso

import (
	"fmt"
	"strings"
	"time"

	"github.com/moov-io/iso20022/pkg/document"
	"github.com/moov-io/iso20022/pkg/pain_v10"
	"github.com/moov-io/iso20022/pkg/utils"
)

// Pain001Type is the message this package accepts.
const Pain001Type = "pain.001.001.10"

// Pain001 is a customer credit transfer initiation, flattened.
type Pain001 struct {
	MessageID    string
	CreatedAt    time.Time
	Transactions int   // NbOfTxs as declared
	ControlSum   int64 // CtrlSum as declared, cents; 0 when absent
	Instructions []Pain001Instruction
}

// Pain001Instruction is one PmtInf block.
type Pain001Instruction struct {
	ID            string
	DebtorAccount string
	Transactions  []Pain001Tx
}

// Pain001Tx is one credit transfer.
type Pain001Tx struct {
	InstructionID   string
	EndToEndID      string
	Amount          int64 // cents
	Currency        string
	CreditorName    string
	CreditorRouting string // ABA from ClrSysMmbId (USABA)
	CreditorAccount string
	Remittance      string
	ServiceLevel    string // URGP | NURG | SDVA …
}

// ParsePain001 reads a pain.001.001.10 document.
func ParsePain001(b []byte) (*Pain001, error) {
	doc, err := document.ParseIso20022Document(b)
	if err != nil {
		return nil, fmt.Errorf("not an ISO 20022 document: %w", err)
	}
	if ns := doc.NameSpace(); ns != utils.DocumentPain00100110NameSpace {
		return nil, fmt.Errorf("expected %s, got %s", utils.DocumentPain00100110NameSpace, ns)
	}
	m, ok := doc.InspectMessage().(*pain_v10.CustomerCreditTransferInitiationV10)
	if !ok {
		return nil, fmt.Errorf("document is not a customer credit transfer initiation")
	}
	out := &Pain001{MessageID: string(m.GrpHdr.MsgId), CreatedAt: time.Time(m.GrpHdr.CreDtTm)}
	if out.MessageID == "" {
		return nil, fmt.Errorf("GrpHdr/MsgId is required")
	}
	if _, err := fmt.Sscan(string(m.GrpHdr.NbOfTxs), &out.Transactions); err != nil {
		return nil, fmt.Errorf("GrpHdr/NbOfTxs is not a number")
	}
	if m.GrpHdr.CtrlSum != 0 {
		if out.ControlSum, err = cents(m.GrpHdr.CtrlSum); err != nil {
			return nil, fmt.Errorf("GrpHdr/CtrlSum: %w", err)
		}
	}
	for _, p := range m.PmtInf {
		ins := Pain001Instruction{ID: string(p.PmtInfId), DebtorAccount: string(p.DbtrAcct.Id.Othr.Id)}
		pmtSvc := serviceLevel(p.PmtTpInf)
		for _, t := range p.CdtTrfTxInf {
			tx := Pain001Tx{
				EndToEndID:   string(t.PmtId.EndToEndId),
				Currency:     string(t.Amt.InstdAmt.Ccy),
				ServiceLevel: pmtSvc,
			}
			if t.PmtId.InstrId != nil {
				tx.InstructionID = string(*t.PmtId.InstrId)
			}
			if tx.Amount, err = cents(t.Amt.InstdAmt.Value); err != nil {
				return nil, fmt.Errorf("transaction %s: %w", tx.EndToEndID, err)
			}
			if s := serviceLevel(t.PmtTpInf); s != "" {
				tx.ServiceLevel = s
			}
			if t.Cdtr != nil && t.Cdtr.Nm != nil {
				tx.CreditorName = string(*t.Cdtr.Nm)
			}
			if t.CdtrAcct != nil {
				tx.CreditorAccount = string(t.CdtrAcct.Id.Othr.Id)
			}
			if t.CdtrAgt != nil && t.CdtrAgt.FinInstnId.ClrSysMmbId != nil {
				tx.CreditorRouting = strings.TrimPrefix(string(t.CdtrAgt.FinInstnId.ClrSysMmbId.MmbId), "USABA")
			}
			if t.RmtInf != nil {
				var parts []string
				for _, u := range t.RmtInf.Ustrd {
					parts = append(parts, string(u))
				}
				tx.Remittance = strings.Join(parts, " ")
			}
			ins.Transactions = append(ins.Transactions, tx)
		}
		out.Instructions = append(out.Instructions, ins)
	}
	return out, nil
}

func serviceLevel(p *pain_v10.PaymentTypeInformation26) string {
	if p == nil || len(p.SvcLvl) == 0 {
		return ""
	}
	return string(p.SvcLvl[0].Cd)
}

// Count is the number of transactions in the document.
func (p *Pain001) Count() int {
	n := 0
	for _, i := range p.Instructions {
		n += len(i.Transactions)
	}
	return n
}

// Sum is the total of the transactions, in cents.
func (p *Pain001) Sum() int64 {
	var s int64
	for _, i := range p.Instructions {
		for _, t := range i.Transactions {
			s += t.Amount
		}
	}
	return s
}
