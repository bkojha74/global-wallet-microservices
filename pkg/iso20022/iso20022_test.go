package iso20022

import (
	"strings"
	"testing"
	"time"
)

const samplePacs008 = `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08">
  <FIToFICstmrCdtTrf>
    <GrpHdr>
      <MsgId>MSG-FEDNOW-20261113-001</MsgId>
      <CreDtTm>2026-11-13T10:00:00Z</CreDtTm>
      <NbOfTxs>1</NbOfTxs>
      <SttlmInf>
        <SttlmMtd>CLRG</SttlmMtd>
      </SttlmInf>
    </GrpHdr>
    <CdtTrfTxInf>
      <PmtId>
        <EndToEndId>E2E-TXN-987654</EndToEndId>
        <TxId>TXN-CLEARING-12345</TxId>
      </PmtId>
      <IntrBkSttlmAmt Ccy="USD">500.00</IntrBkSttlmAmt>
      <Dbtr>
        <Nm>John Originator</Nm>
      </Dbtr>
      <DbtrAcct>
        <Id>
          <IBAN>DE89370400440532013000</IBAN>
        </Id>
      </DbtrAcct>
      <DbtrAgt>
        <FinInstnId>
          <BICFI>DEUTDEDB</BICFI>
          <Nm>Deutsche Bank AG</Nm>
        </FinInstnId>
      </DbtrAgt>
      <CdtrAgt>
        <FinInstnId>
          <BICFI>CHASUS33</BICFI>
          <Nm>JPMorgan Chase Bank</Nm>
        </FinInstnId>
      </CdtrAgt>
      <Cdtr>
        <Nm>Alice Beneficiary</Nm>
      </Cdtr>
      <CdtrAcct>
        <Id>
          <IBAN>GB29NWBK60161331926819</IBAN>
        </Id>
      </CdtrAcct>
      <RmtInf>
        <Ustrd>Direct invoice settlement #8821</Ustrd>
      </RmtInf>
    </CdtTrfTxInf>
  </FIToFICstmrCdtTrf>
</Document>`

func TestParsePacs008(t *testing.T) {
	doc, err := ParsePacs008([]byte(samplePacs008))
	if err != nil {
		t.Fatalf("ParsePacs008 failed: %v", err)
	}

	if doc.FIToFICstmrCdtTrf.GrpHdr.MsgId != "MSG-FEDNOW-20261113-001" {
		t.Errorf("expected MsgId MSG-FEDNOW-20261113-001, got %s", doc.FIToFICstmrCdtTrf.GrpHdr.MsgId)
	}

	tx := doc.FIToFICstmrCdtTrf.CdtTrfTxInf[0]
	if tx.IntrBkSttlmAmt.Value != 500.00 || tx.IntrBkSttlmAmt.Ccy != "USD" {
		t.Errorf("expected 500.00 USD, got %v %s", tx.IntrBkSttlmAmt.Value, tx.IntrBkSttlmAmt.Ccy)
	}

	if tx.Dbtr.Nm != "John Originator" || tx.Cdtr.Nm != "Alice Beneficiary" {
		t.Errorf("debtor/creditor mismatch: %s -> %s", tx.Dbtr.Nm, tx.Cdtr.Nm)
	}

	if tx.RmtInf == nil || tx.RmtInf.Ustrd != "Direct invoice settlement #8821" {
		t.Errorf("unexpected remittance info: %+v", tx.RmtInf)
	}
}

func TestBuildPacs002(t *testing.T) {
	// Build Accepted Technical Confirmation (ACTC)
	actcBytes, err := BuildPacs002("ORIG-123", "TX-456", StatusAcceptedTechnical, "", "")
	if err != nil {
		t.Fatalf("BuildPacs002 ACTC failed: %v", err)
	}
	actcStr := string(actcBytes)
	if !strings.Contains(actcStr, "<GrpSts>ACTC</GrpSts>") || !strings.Contains(actcStr, "<OrgnlMsgId>ORIG-123</OrgnlMsgId>") {
		t.Fatalf("ACTC missing required fields: %s", actcStr)
	}

	// Build Rejection (RJCT) with AC04 Closed Account
	rjctBytes, err := BuildPacs002("ORIG-123", "TX-456", StatusRejected, ReasonClosedAccount, "Account is frozen")
	if err != nil {
		t.Fatalf("BuildPacs002 RJCT failed: %v", err)
	}
	rjctStr := string(rjctBytes)
	if !strings.Contains(rjctStr, "<TxSts>RJCT</TxSts>") || !strings.Contains(rjctStr, "<Cd>AC04</Cd>") {
		t.Fatalf("RJCT missing reason code: %s", rjctStr)
	}
}

func TestBuildCamt053(t *testing.T) {
	entries := []StatementEntry{
		{
			TxId:        "txn_001",
			Amount:      150.00,
			Currency:    "USD",
			IsCredit:    true,
			BookingDate: time.Now().UTC(),
			Category:    "DEPOSIT",
		},
		{
			TxId:        "txn_002",
			Amount:      50.00,
			Currency:    "USD",
			IsCredit:    false,
			BookingDate: time.Now().UTC(),
			Category:    "FEE",
		},
	}

	camtBytes, err := BuildCamt053("STMT-2026-11-13", "US24CITI1234567890123456", "USD", 1000.00, 1100.00, entries)
	if err != nil {
		t.Fatalf("BuildCamt053 failed: %v", err)
	}

	camtStr := string(camtBytes)
	if !strings.Contains(camtStr, "BkToCstmrStmt") || !strings.Contains(camtStr, "OPBD") || !strings.Contains(camtStr, "CLBD") {
		t.Fatalf("Camt053 missing essential tags: %s", camtStr)
	}
}
