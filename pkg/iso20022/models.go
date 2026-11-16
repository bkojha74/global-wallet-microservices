// Package iso20022 provides parsing, serialization, and schema validation
// for ISO 20022 financial messages: pacs.008, pacs.002, pain.001, and camt.053.
package iso20022

import (
	"encoding/xml"
	"time"
)

// Common ISO 20022 Status Codes (pacs.002)
const (
	StatusAcceptedTechnical  = "ACTC" // AcceptedTechnicalValidation
	StatusAcceptedCustomer   = "ACCP" // AcceptedCustomerProfile
	StatusAcceptedSettlement = "ACSP" // AcceptedSettlementInProcess
	StatusPending            = "PDNG" // Pending
	StatusRejected           = "RJCT" // Rejected
)

// Common ISO 20022 Rejection Reason Codes
const (
	ReasonIncorrectAccount  = "AC01" // IncorrectAccountNumber
	ReasonClosedAccount     = "AC04" // ClosedAccountNumber
	ReasonInsufficientFunds = "AM04" // InsufficientFunds
	ReasonDuplicate         = "AM05" // Duplication
	ReasonRegulatoryBlock   = "RR04" // RegulatoryReason / Sanctions block
	ReasonFormatError       = "FF01" // InvalidFileFormat
)

// ─────────────────────────────────────────────────────────────────────────────
// pacs.008.001.08: FI to FI Customer Credit Transfer
// ─────────────────────────────────────────────────────────────────────────────

type Pacs008Document struct {
	XMLName           xml.Name          `xml:"Document"`
	XMLNS             string            `xml:"xmlns,attr,omitempty"`
	FIToFICstmrCdtTrf FIToFICstmrCdtTrf `xml:"FIToFICstmrCdtTrf"`
}

type FIToFICstmrCdtTrf struct {
	GrpHdr      GroupHeader008        `xml:"GrpHdr"`
	CdtTrfTxInf []CreditTransferTx008 `xml:"CdtTrfTxInf"`
}

type GroupHeader008 struct {
	MsgId    string         `xml:"MsgId"`
	CreDtTm  time.Time      `xml:"CreDtTm"`
	NbOfTxs  int            `xml:"NbOfTxs"`
	SttlmInf SettlementInfo `xml:"SttlmInf"`
}

type SettlementInfo struct {
	SttlmMtd string `xml:"SttlmMtd"` // e.g. "CLRG", "INDA"
}

type CreditTransferTx008 struct {
	PmtId          PaymentIdentification         `xml:"PmtId"`
	IntrBkSttlmAmt ActiveCurrencyAndAmount       `xml:"IntrBkSttlmAmt"`
	IntrBkSttlmDt  string                        `xml:"IntrBkSttlmDt,omitempty"` // YYYY-MM-DD
	Dbtr           PartyIdentification           `xml:"Dbtr"`
	DbtrAcct       CashAccount                   `xml:"DbtrAcct"`
	DbtrAgt        BranchAndFinancialInstitution `xml:"DbtrAgt"`
	CdtrAgt        BranchAndFinancialInstitution `xml:"CdtrAgt"`
	Cdtr           PartyIdentification           `xml:"Cdtr"`
	CdtrAcct       CashAccount                   `xml:"CdtrAcct"`
	RmtInf         *RemittanceInformation        `xml:"RmtInf,omitempty"`
}

type PaymentIdentification struct {
	EndToEndId string `xml:"EndToEndId"`
	TxId       string `xml:"TxId,omitempty"`
}

type ActiveCurrencyAndAmount struct {
	Value float64 `xml:",chardata"`
	Ccy   string  `xml:"Ccy,attr"`
}

type PartyIdentification struct {
	Nm string `xml:"Nm"`
}

type CashAccount struct {
	Id AccountId `xml:"Id"`
}

type AccountId struct {
	IBAN string        `xml:"IBAN,omitempty"`
	Othr *OtherAccount `xml:"Othr,omitempty"`
}

type OtherAccount struct {
	Id string `xml:"Id"`
}

type BranchAndFinancialInstitution struct {
	FinInstnId FinancialInstitutionId `xml:"FinInstnId"`
}

type FinancialInstitutionId struct {
	BICFI string `xml:"BICFI,omitempty"`
	Nm    string `xml:"Nm,omitempty"`
}

type RemittanceInformation struct {
	Ustrd string `xml:"Ustrd,omitempty"` // Unstructured reference description
}

// ─────────────────────────────────────────────────────────────────────────────
// pacs.002.001.10: Payment Status Report
// ─────────────────────────────────────────────────────────────────────────────

type Pacs002Document struct {
	XMLName         xml.Name        `xml:"Document"`
	XMLNS           string          `xml:"xmlns,attr"`
	FIToFIPmtStsRpt FIToFIPmtStsRpt `xml:"FIToFIPmtStsRpt"`
}

type FIToFIPmtStsRpt struct {
	GrpHdr            GroupHeader002    `xml:"GrpHdr"`
	OrgnlGrpInfAndSts OriginalGroupInfo `xml:"OrgnlGrpInfAndSts"`
	TxInfAndSts       []TxInfoAndStatus `xml:"TxInfAndSts,omitempty"`
}

type GroupHeader002 struct {
	MsgId   string    `xml:"MsgId"`
	CreDtTm time.Time `xml:"CreDtTm"`
}

type OriginalGroupInfo struct {
	OrgnlMsgId   string `xml:"OrgnlMsgId"`
	OrgnlMsgNmId string `xml:"OrgnlMsgNmId"` // e.g. "pacs.008.001.08"
	GrpSts       string `xml:"GrpSts,omitempty"`
}

type TxInfoAndStatus struct {
	OrgnlEndToEndId string            `xml:"OrgnlEndToEndId,omitempty"`
	OrgnlTxId       string            `xml:"OrgnlTxId,omitempty"`
	TxSts           string            `xml:"TxSts"` // ACTC, RJCT, etc.
	StsRsnInf       *StatusReasonInfo `xml:"StsRsnInf,omitempty"`
}

type StatusReasonInfo struct {
	Rsn      *ReasonCode `xml:"Rsn,omitempty"`
	AddtlInf string      `xml:"AddtlInf,omitempty"`
}

type ReasonCode struct {
	Cd string `xml:"Cd"`
}

// ─────────────────────────────────────────────────────────────────────────────
// pain.001.001.09: Customer Credit Transfer Initiation (Corporate / B2B)
// ─────────────────────────────────────────────────────────────────────────────

type Pain001Document struct {
	XMLName          xml.Name         `xml:"Document"`
	XMLNS            string           `xml:"xmlns,attr,omitempty"`
	CstmrCdtTrfInitn CstmrCdtTrfInitn `xml:"CstmrCdtTrfInitn"`
}

type CstmrCdtTrfInitn struct {
	GrpHdr GroupHeader001 `xml:"GrpHdr"`
	PmtInf []PaymentInfo  `xml:"PmtInf"`
}

type GroupHeader001 struct {
	MsgId    string              `xml:"MsgId"`
	CreDtTm  time.Time           `xml:"CreDtTm"`
	NbOfTxs  int                 `xml:"NbOfTxs"`
	InitgPty PartyIdentification `xml:"InitgPty"`
}

type PaymentInfo struct {
	PmtInfId    string                        `xml:"PmtInfId"`
	PmtMtd      string                        `xml:"PmtMtd"` // "TRF"
	ReqdExctnDt string                        `xml:"ReqdExctnDt"`
	Dbtr        PartyIdentification           `xml:"Dbtr"`
	DbtrAcct    CashAccount                   `xml:"DbtrAcct"`
	DbtrAgt     BranchAndFinancialInstitution `xml:"DbtrAgt"`
	CdtTrfTxInf []CreditTransferTx001         `xml:"CdtTrfTxInf"`
}

type CreditTransferTx001 struct {
	PmtId    PaymentIdentification         `xml:"PmtId"`
	Amt      ActiveCurrencyAndAmount       `xml:"Amt>InstdAmt"`
	CdtrAgt  BranchAndFinancialInstitution `xml:"CdtrAgt"`
	Cdtr     PartyIdentification           `xml:"Cdtr"`
	CdtrAcct CashAccount                   `xml:"CdtrAcct"`
	RmtInf   *RemittanceInformation        `xml:"RmtInf,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// camt.053.001.08: Bank to Customer Statement (EOD Financial Statement)
// ─────────────────────────────────────────────────────────────────────────────

type Camt053Document struct {
	XMLName       xml.Name      `xml:"Document"`
	XMLNS         string        `xml:"xmlns,attr"`
	BkToCstmrStmt BkToCstmrStmt `xml:"BkToCstmrStmt"`
}

type BkToCstmrStmt struct {
	GrpHdr GroupHeader053 `xml:"GrpHdr"`
	Stmt   []Statement    `xml:"Stmt"`
}

type GroupHeader053 struct {
	MsgId   string    `xml:"MsgId"`
	CreDtTm time.Time `xml:"CreDtTm"`
}

type Statement struct {
	Id      string          `xml:"Id"`
	CreDtTm time.Time       `xml:"CreDtTm"`
	Acct    StatementAcct   `xml:"Acct"`
	Bal     []BalanceEntry  `xml:"Bal"`
	Ntry    []StatementNtry `xml:"Ntry,omitempty"`
}

type StatementAcct struct {
	Id  AccountId `xml:"Id"`
	Ccy string    `xml:"Ccy"`
}

type BalanceEntry struct {
	Tp        BalanceType             `xml:"Tp"`
	Amt       ActiveCurrencyAndAmount `xml:"Amt"`
	CdtDbtInd string                  `xml:"CdtDbtInd"` // "CRDT" or "DBIT"
	Dt        DateStructure           `xml:"Dt"`
}

type BalanceType struct {
	CdOrPtry CodeOrProprietary `xml:"CdOrPtry"`
}

type CodeOrProprietary struct {
	Cd string `xml:"Cd"` // "OPBD" (Opening), "CLBD" (Closing)
}

type DateStructure struct {
	Dt string `xml:"Dt"` // YYYY-MM-DD
}

type StatementNtry struct {
	Amt       ActiveCurrencyAndAmount `xml:"Amt"`
	CdtDbtInd string                  `xml:"CdtDbtInd"` // "CRDT" or "DBIT"
	Sts       string                  `xml:"Sts"`       // "BOOK"
	BookgDt   DateStructure           `xml:"BookgDt"`
	ValDt     DateStructure           `xml:"ValDt"`
	BkTxCd    BankTxCode              `xml:"BkTxCd"`
	NtryDtls  EntryDetails            `xml:"NtryDtls"`
}

type BankTxCode struct {
	Prtry ProprietaryCode `xml:"Prtry"`
}

type ProprietaryCode struct {
	Cd string `xml:"Cd"` // e.g. "TRANSFER", "FEE", "DEPOSIT"
}

type EntryDetails struct {
	TxDtls []TransactionDetails `xml:"TxDtls"`
}

type TransactionDetails struct {
	Refs PaymentIdentification `xml:"Refs"`
}
