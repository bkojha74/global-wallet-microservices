package iso20022

import (
	"encoding/xml"
	"errors"
	"fmt"
	"time"
)

var (
	ErrEmptyXMLPayload       = errors.New("empty XML payload")
	ErrInvalidPacs008Message = errors.New("invalid pacs.008 message")
	ErrInvalidPain001Message = errors.New("invalid pain.001 message")
)

const (
	XMLNSPacs008 = "urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08"
	XMLNSPacs002 = "urn:iso:std:iso:20022:tech:xsd:pacs.002.001.10"
	XMLNSPain001 = "urn:iso:std:iso:20022:tech:xsd:pain.001.001.09"
	XMLNSCamt053 = "urn:iso:std:iso:20022:tech:xsd:camt.053.001.08"
)

// ParsePacs008 unmarshals a raw XML byte slice into a Pacs008Document.
func ParsePacs008(xmlBytes []byte) (*Pacs008Document, error) {
	if len(xmlBytes) == 0 {
		return nil, ErrEmptyXMLPayload
	}

	var doc Pacs008Document
	if err := xml.Unmarshal(xmlBytes, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPacs008Message, err)
	}

	if doc.FIToFICstmrCdtTrf.GrpHdr.MsgId == "" || len(doc.FIToFICstmrCdtTrf.CdtTrfTxInf) == 0 {
		return nil, fmt.Errorf("%w: missing MsgId or transaction items", ErrInvalidPacs008Message)
	}

	return &doc, nil
}

// ParsePain001 unmarshals a raw XML byte slice into a Pain001Document.
func ParsePain001(xmlBytes []byte) (*Pain001Document, error) {
	if len(xmlBytes) == 0 {
		return nil, ErrEmptyXMLPayload
	}

	var doc Pain001Document
	if err := xml.Unmarshal(xmlBytes, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPain001Message, err)
	}

	if doc.CstmrCdtTrfInitn.GrpHdr.MsgId == "" || len(doc.CstmrCdtTrfInitn.PmtInf) == 0 {
		return nil, fmt.Errorf("%w: missing MsgId or payment instructions", ErrInvalidPain001Message)
	}

	return &doc, nil
}

// BuildPacs002 constructs and serializes a pacs.002 Payment Status Report.
func BuildPacs002(originalMsgId, originalTxId, status, reasonCode, reasonDesc string) ([]byte, error) {
	reportMsgId := fmt.Sprintf("STAT-%s-%d", status, time.Now().UnixNano()%1000000)

	doc := Pacs002Document{
		XMLNS: XMLNSPacs002,
		FIToFIPmtStsRpt: FIToFIPmtStsRpt{
			GrpHdr: GroupHeader002{
				MsgId:   reportMsgId,
				CreDtTm: time.Now().UTC(),
			},
			OrgnlGrpInfAndSts: OriginalGroupInfo{
				OrgnlMsgId:   originalMsgId,
				OrgnlMsgNmId: "pacs.008.001.08",
				GrpSts:       status,
			},
			TxInfAndSts: []TxInfoAndStatus{
				{
					OrgnlEndToEndId: originalTxId,
					OrgnlTxId:       originalTxId,
					TxSts:           status,
				},
			},
		},
	}

	if reasonCode != "" {
		doc.FIToFIPmtStsRpt.TxInfAndSts[0].StsRsnInf = &StatusReasonInfo{
			Rsn: &ReasonCode{
				Cd: reasonCode,
			},
			AddtlInf: reasonDesc,
		}
	}

	xmlHeader := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(xmlHeader, body...), nil
}

// StatementEntry represents an audit entry for camt.053 statement generation.
type StatementEntry struct {
	TxId        string
	Amount      float64
	Currency    string
	IsCredit    bool
	BookingDate time.Time
	Category    string
}

// BuildCamt053 constructs an End-of-Day cash statement XML document.
func BuildCamt053(statementId, accountIBAN, currency string, openingBal, closingBal float64, entries []StatementEntry) ([]byte, error) {
	now := time.Now().UTC()
	dateStr := now.Format("2006-01-02")

	opInd := "CRDT"
	if openingBal < 0 {
		opInd = "DBIT"
		openingBal = -openingBal
	}

	clInd := "CRDT"
	if closingBal < 0 {
		clInd = "DBIT"
		closingBal = -closingBal
	}

	var ntryList []StatementNtry
	for _, e := range entries {
		ind := "CRDT"
		amt := e.Amount
		if !e.IsCredit {
			ind = "DBIT"
		}
		if amt < 0 {
			amt = -amt
		}

		bDate := e.BookingDate.Format("2006-01-02")
		ntryList = append(ntryList, StatementNtry{
			Amt: ActiveCurrencyAndAmount{
				Value: amt,
				Ccy:   currency,
			},
			CdtDbtInd: ind,
			Sts:       "BOOK",
			BookgDt:   DateStructure{Dt: bDate},
			ValDt:     DateStructure{Dt: bDate},
			BkTxCd:    BankTxCode{Prtry: ProprietaryCode{Cd: e.Category}},
			NtryDtls: EntryDetails{
				TxDtls: []TransactionDetails{
					{Refs: PaymentIdentification{TxId: e.TxId, EndToEndId: e.TxId}},
				},
			},
		})
	}

	doc := Camt053Document{
		XMLNS: XMLNSCamt053,
		BkToCstmrStmt: BkToCstmrStmt{
			GrpHdr: GroupHeader053{
				MsgId:   fmt.Sprintf("STMT-%s-%s", statementId, dateStr),
				CreDtTm: now,
			},
			Stmt: []Statement{
				{
					Id:      statementId,
					CreDtTm: now,
					Acct: StatementAcct{
						Id:  AccountId{IBAN: accountIBAN},
						Ccy: currency,
					},
					Bal: []BalanceEntry{
						{
							Tp:        BalanceType{CdOrPtry: CodeOrProprietary{Cd: "OPBD"}},
							Amt:       ActiveCurrencyAndAmount{Value: openingBal, Ccy: currency},
							CdtDbtInd: opInd,
							Dt:        DateStructure{Dt: dateStr},
						},
						{
							Tp:        BalanceType{CdOrPtry: CodeOrProprietary{Cd: "CLBD"}},
							Amt:       ActiveCurrencyAndAmount{Value: closingBal, Ccy: currency},
							CdtDbtInd: clInd,
							Dt:        DateStructure{Dt: dateStr},
						},
					},
					Ntry: ntryList,
				},
			},
		},
	}

	xmlHeader := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(xmlHeader, body...), nil
}
