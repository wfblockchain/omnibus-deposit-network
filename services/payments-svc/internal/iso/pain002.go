package iso

import (
	"encoding/xml"
	"strconv"
	"time"

	"github.com/moov-io/iso20022/pkg/common"
	"github.com/moov-io/iso20022/pkg/document"
	"github.com/moov-io/iso20022/pkg/pain_v11"
	"github.com/moov-io/iso20022/pkg/utils"
)

// Pain002Type is the status report this package writes.
const Pain002Type = "pain.002.001.11"

// TxStatus is one transaction's status in a report.
type TxStatus struct {
	InstructionID string
	EndToEndID    string
	UETR          string
	Status        string // RCVD | ACTC | PATC | ACCP | PDNG | ACSP | ACSC | ACCC | RJCT | CANC | BLCK
	Reason        string // ISO external status reason code, when rejected
	Info          string
}

// StatusReport is a pain.002 about one original message.
type StatusReport struct {
	MessageID       string
	At              time.Time
	OrigMessageID   string
	OrigMessageType string // pain.001.001.10 for a file; "API" for a payment created through the API
	OrigCount       int
	OrigSum         int64  // cents; 0 omits it
	GroupStatus     string // ACTC | PART | RJCT …; empty omits it
	GroupReason     string
	PaymentInfoID   string
	Transactions    []TxStatus
}

// Pain002 writes a customer payment status report.
func Pain002(r StatusReport) ([]byte, error) {
	d, err := document.NewDocument(utils.DocumentPain00200111NameSpace)
	if err != nil {
		return nil, err
	}
	m := d.InspectMessage().(*pain_v11.CustomerPaymentStatusReportV11)
	m.GrpHdr = pain_v11.GroupHeader86{MsgId: common.Max35Text(r.MessageID), CreDtTm: common.ISODateTime(r.At.UTC())}

	nb := common.Max15NumericText(strconv.Itoa(r.OrigCount))
	m.OrgnlGrpInfAndSts = pain_v11.OriginalGroupHeader17{
		OrgnlMsgId:   common.Max35Text(r.OrigMessageID),
		OrgnlMsgNmId: common.Max35Text(r.OrigMessageType),
		OrgnlNbOfTxs: &nb,
		OrgnlCtrlSum: dollars(r.OrigSum),
	}
	if r.GroupStatus != "" {
		gs := pain_v11.ExternalPaymentGroupStatus1Code(r.GroupStatus)
		m.OrgnlGrpInfAndSts.GrpSts = &gs
		if r.GroupReason != "" {
			m.OrgnlGrpInfAndSts.StsRsnInf = []pain_v11.StatusReasonInformation12{reason(r.GroupReason, "")}
		}
	}
	pmt := pain_v11.OriginalPaymentInstruction38{OrgnlPmtInfId: common.Max35Text(r.PaymentInfoID)}
	for _, t := range r.Transactions {
		tx := pain_v11.PaymentTransaction126{}
		if t.InstructionID != "" {
			v := common.Max35Text(t.InstructionID)
			tx.OrgnlInstrId = &v
		}
		e2e := common.Max35Text(t.EndToEndID)
		tx.OrgnlEndToEndId = &e2e
		if t.UETR != "" {
			u := common.UUIDv4Identifier(t.UETR)
			tx.OrgnlUETR = &u
		}
		st := pain_v11.ExternalPaymentTransactionStatus1Code(t.Status)
		tx.TxSts = &st
		if t.Reason != "" || t.Info != "" {
			tx.StsRsnInf = []pain_v11.StatusReasonInformation12{reason(t.Reason, t.Info)}
		}
		pmt.TxInfAndSts = append(pmt.TxInfAndSts, tx)
	}
	m.OrgnlPmtInfAndSts = []pain_v11.OriginalPaymentInstruction38{pmt}

	obj := d.(*document.Iso20022DocumentObject)
	obj.XMLName = xml.Name{Local: "Document"}
	obj.Attrs = []xml.Attr{{Name: xml.Name{Local: "xmlns"}, Value: utils.DocumentPain00200111NameSpace}}
	return marshal(d)
}

func reason(code, info string) pain_v11.StatusReasonInformation12 {
	r := pain_v11.StatusReasonInformation12{}
	if code != "" {
		r.Rsn = &pain_v11.StatusReason6Choice{Cd: pain_v11.ExternalStatusReason1Code(code)}
	}
	if info != "" {
		if len(info) > 105 {
			info = info[:105]
		}
		r.AddtlInf = []common.Max105Text{common.Max105Text(info)}
	}
	return r
}
