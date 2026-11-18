# Comprehensive Bruno Suite Verification Script
$baseUrl = "http://localhost:8080"
$ErrorActionPreference = "Continue"

$results = [System.Collections.Generic.List[PSCustomObject]]::new()

function Record-Test($name, $expected, $actual, $passed, $details) {
    $results.Add([PSCustomObject]@{
        TestName = $name
        Expected = $expected
        Actual   = $actual
        Passed   = $passed
        Details  = $details
    })
    $color = if ($passed) { "Green" } else { "Red" }
    $status = if ($passed) { "PASS" } else { "FAIL" }
    Write-Host "[$status] $name - Expected: $expected, Got: $actual ($details)" -ForegroundColor $color
}

# --- 01. Health & Observability ---
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/healthz" -Method Get
    Record-Test "01-Health-Liveness" "200" "200" ($r.status -eq "UP") "Status: $($r.status)"
} catch {
    Record-Test "01-Health-Liveness" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/readyz" -Method Get
    Record-Test "02-Readiness-Probe" "200" "200" ($r.status -eq "READY") "Status: $($r.status)"
} catch {
    Record-Test "02-Readiness-Probe" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/cluster/status" -Method Get
    Record-Test "03-Cluster-Status" "200" "200" ($null -ne $r.active_target -and $null -ne $r.region) "Active Target: $($r.active_target), Region: $($r.region)"
} catch {
    Record-Test "03-Cluster-Status" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api-docs/openapi.json" -Method Get
    Record-Test "04-OpenAPI-Spec" "200" "200" ($null -ne $r.openapi) "OpenAPI version: $($r.openapi)"
} catch {
    Record-Test "04-OpenAPI-Spec" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

# --- 02. Authentication ---
$aliceToken = ""
$refreshToken = ""
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/auth/login" -Method Post -ContentType "application/json" -Body '{"username":"alice","password":"password123"}'
    $aliceToken = $r.access_token
    $refreshToken = $r.refresh_token
    Record-Test "01-Alice-Login" "200 with token" "200" ($null -ne $aliceToken -and $aliceToken.Length -gt 10) "Token acquired"
} catch {
    Record-Test "01-Alice-Login" "200 with token" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

$adminToken = ""
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/auth/login" -Method Post -ContentType "application/json" -Body '{"username":"admin","password":"admin123"}'
    $adminToken = $r.access_token
    Record-Test "02-Admin-Login" "200 with token" "200" ($null -ne $adminToken -and $adminToken.Length -gt 10) "Token acquired"
} catch {
    Record-Test "02-Admin-Login" "200 with token" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    # Generate mock logout token with sub: "test_revoked_subject"
    $hdr = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes('{"alg":"none","typ":"JWT"}')).TrimEnd('=').Replace('+','-').Replace('/','_')
    $pay = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes('{"iss":"keycloak","sub":"test_revoked_subject","events":{"http://schemas.openid.net/event/backchannel-logout":{}}}')).TrimEnd('=').Replace('+','-').Replace('/','_')
    $mockLogoutToken = "$hdr.$pay."
    
    $r = Invoke-WebRequest -Uri "$baseUrl/api/v1/auth/backchannel-logout" -Method Post -ContentType "application/x-www-form-urlencoded" -Body "logout_token=$mockLogoutToken"
    $cacheCtrl = $r.Headers["Cache-Control"]
    Record-Test "04-Backchannel-Logout" "200 with no-store" "$($r.StatusCode)" ($r.StatusCode -eq 200 -and $cacheCtrl -match "no-store") "Cache-Control: $cacheCtrl"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "04-Backchannel-Logout" "200 with no-store" "$code" $false $_.Message
}

# --- 03. Wallet Lifecycle ---
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $aliceToken" } -Body '{"wallet_id":"alice_usd","currency":"USD","owner_id":"alice"}'
    Record-Test "01-Create-Alice-USD-Wallet" "200 or 409" "200" $true "Wallet created: $($r.wallet_id)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "01-Create-Alice-USD-Wallet" "200 or 409" "$code" ($code -eq 409 -or $code -eq 200) "Status $code"
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $adminToken" } -Body '{"wallet_id":"bob_eur","currency":"EUR","owner_id":"bob"}'
    Record-Test "02-Create-Bob-EUR-Wallet" "200 or 409" "200" $true "Wallet created: $($r.wallet_id)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "02-Create-Bob-EUR-Wallet" "200 or 409" "$code" ($code -eq 409 -or $code -eq 200) "Status $code"
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/alice_usd/balance" -Method Get -Headers @{ Authorization = "Bearer $aliceToken" }
    Record-Test "03-Get-Alice-Balance" "200" "200" ($r.wallet_id -eq "alice_usd") "Balance units: $($r.balances[0].units)"
} catch {
    Record-Test "03-Get-Alice-Balance" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

# --- 04. Deposits & Withdrawals ---
try {
    $depBody = '{"wallet_id":"alice_usd","amount":{"units":50000,"currency":"USD"},"rail":"ACH","reference":"Payroll direct deposit"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/deposit" -Method Post -ContentType "application/json" -Headers @{
        Authorization = "Bearer $aliceToken"
        "Idempotency-Key" = "dep_ach_alice_" + [Guid]::NewGuid().ToString().Substring(0,8)
    } -Body $depBody
    Record-Test "01-Deposit-ACH-Alice" "200 SUCCESS" "$($r.status)" ($r.status -eq "SUCCESS") "Txn: $($r.transaction_id)"
} catch {
    Record-Test "01-Deposit-ACH-Alice" "200 SUCCESS" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $depBody = '{"wallet_id":"bob_eur","amount":{"units":200000,"currency":"EUR"},"rail":"CARD","reference":"Debit card instant top-up"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/deposit" -Method Post -ContentType "application/json" -Headers @{
        Authorization = "Bearer $adminToken"
        "Idempotency-Key" = "dep_card_bob_" + [Guid]::NewGuid().ToString().Substring(0,8)
    } -Body $depBody
    Record-Test "02-Deposit-Card-Bob" "200 SUCCESS" "$($r.status)" ($r.status -eq "SUCCESS") "Txn: $($r.transaction_id)"
} catch {
    Record-Test "02-Deposit-Card-Bob" "200 SUCCESS" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $wthBody = '{"wallet_id":"bob_eur","amount":{"units":10000,"currency":"EUR"},"rail":"SEPA","reference":"SEPA Payout to Bank"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/withdraw" -Method Post -ContentType "application/json" -Headers @{
        Authorization = "Bearer $adminToken"
        "Idempotency-Key" = "wth_sepa_bob_" + [Guid]::NewGuid().ToString().Substring(0,8)
    } -Body $wthBody
    Record-Test "03-Withdraw-SEPA-Bob" "200 SUCCESS" "$($r.status)" ($r.status -eq "SUCCESS") "Txn: $($r.transaction_id)"
} catch {
    Record-Test "03-Withdraw-SEPA-Bob" "200 SUCCESS" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

# --- 05. FX Engine ---
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/fx/currencies" -Method Get
    Record-Test "01-Get-Currencies" "200" "200" ($null -ne $r.currencies) "Currencies count: $($r.currencies.Count)"
} catch {
    Record-Test "01-Get-Currencies" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/fx/rates?base=USD" -Method Get
    Record-Test "02-Get-Live-Rates" "200" "200" ($r.base_currency -eq "USD") "Rates: $($r.rates.PSObject.Properties.Count)"
} catch {
    Record-Test "02-Get-Live-Rates" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

$quoteId = ""
try {
    $qBody = '{"source_currency":"USD","target_currency":"EUR","source_units":10000}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/fx/quotes" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $aliceToken" } -Body $qBody
    $quoteId = $r.quote_id
    Record-Test "03-Lock-Fixed-Quote" "200 LOCKED" "$($r.status)" ($r.status -eq "LOCKED" -and $null -ne $quoteId) "Quote ID: $quoteId"
} catch {
    Record-Test "03-Lock-Fixed-Quote" "200 LOCKED" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $convertBody = '{"quote_id":"' + $quoteId + '"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/fx/convert" -Method Post -ContentType "application/json" -Headers @{
        Authorization = "Bearer $aliceToken"
        "Idempotency-Key" = "fxc_convert_" + [Guid]::NewGuid().ToString().Substring(0,8)
    } -Body $convertBody
    Record-Test "04-Execute-CAS-Convert" "200 or 409" "200" $true "Convert status: $($r.status)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "04-Execute-CAS-Convert" "200 or 409" "$code" ($code -eq 409 -or $code -eq 200) "Status $code"
}

# --- 06. Transfers ---
$sameTxnId = ""
$idemSame = "txn_same_" + [Guid]::NewGuid().ToString().Substring(0,8)
try {
    $txBody = '{"source_wallet_id":"alice_usd","destination_wallet_id":"bob_eur","amount":{"units":25000,"currency":"USD"},"description":"Consulting services payment"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/transfers" -Method Post -ContentType "application/json" -Headers @{
        Authorization = "Bearer $aliceToken"
        "Idempotency-Key" = $idemSame
    } -Body $txBody
    $sameTxnId = $r.transaction_id
    Record-Test "01-Same-Currency-Transfer" "200 SUCCESS" "$($r.status)" ($r.status -eq "SUCCESS") "Txn: $sameTxnId"
} catch {
    Record-Test "01-Same-Currency-Transfer" "200 SUCCESS" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    Start-Sleep -Milliseconds 500
    # Replay transfer with same idempotency key
    $txBody = '{"source_wallet_id":"alice_usd","destination_wallet_id":"bob_eur","amount":{"units":25000,"currency":"USD"},"description":"Consulting services payment"}'
    $rReplay = Invoke-RestMethod -Uri "$baseUrl/api/v1/transfers" -Method Post -ContentType "application/json" -Headers @{
        Authorization = "Bearer $aliceToken"
        "Idempotency-Key" = $idemSame
    } -Body $txBody
    Record-Test "03-Idempotency-Replay-Transfer" "200 (Replay matched ID)" "$($rReplay.transaction_id)" ($rReplay.transaction_id -eq $sameTxnId) "Same txn replayed: $($rReplay.transaction_id)"
} catch {
    Record-Test "03-Idempotency-Replay-Transfer" "200 (Replay matched ID)" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $pacsXml = @"
<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08">
  <FIToFICstmrCdtTrf>
    <GrpHdr>
      <MsgId>MSG-BRUNO-001</MsgId>
      <CreDtTm>2026-11-13T12:00:00Z</CreDtTm>
      <NbOfTxs>1</NbOfTxs>
      <SttlmInf><SttlmMtd>CLRG</SttlmMtd></SttlmInf>
    </GrpHdr>
    <CdtTrfTxInf>
      <PmtId><EndToEndId>E2E-001</EndToEndId><TxId>TX-001</TxId></PmtId>
      <IntrBkSttlmAmt Ccy="EUR">100.00</IntrBkSttlmAmt>
      <DbtrAgt><FinInstnId><BICFI>BARCGB22XXX</BICFI></FinInstnId></DbtrAgt>
      <CdtrAgt><FinInstnId><BICFI>DEUTDEDDFXX</BICFI></FinInstnId></CdtrAgt>
      <DbtrAcct><Id><IBAN>GB29NWBK60161331926819</IBAN></Id></DbtrAcct>
      <CdtrAcct><Id><IBAN>DE89370400440532013000</IBAN></Id></CdtrAcct>
    </CdtTrfTxInf>
  </FIToFICstmrCdtTrf>
</Document>
"@
    $r = Invoke-WebRequest -Uri "$baseUrl/api/v1/clearing/iso20022" -Method Post -ContentType "application/xml" -Body $pacsXml
    Record-Test "04-Ingest-ISO20022-Clearing" "200 XML" "$($r.StatusCode)" ($r.StatusCode -eq 200 -and ($r.Content -match "ACTC" -or $r.Content -match "RJCT")) "Response contains valid pacs.002 XML"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "04-Ingest-ISO20022-Clearing" "200 XML" "$code" $false $_.Message
}

# --- 07. Compliance & KYC ---
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/limits?wallet_id=alice_usd" -Method Get -Headers @{ Authorization = "Bearer $aliceToken" }
    Record-Test "01-Get-Velocity-Limits" "200" "200" ($r.wallet_id -eq "alice_usd" -and $null -ne $r.kyc_tier) "KYC Tier: $($r.kyc_tier)"
} catch {
    Record-Test "01-Get-Velocity-Limits" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $kycBody = '{"wallet_id":"alice_usd","kyc_tier":"TIER_2","verified_by":"Compliance_Officer_Bruno"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/kyc" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $adminToken" } -Body $kycBody
    Record-Test "02-Upgrade-KYC-Tier-Admin" "200 SUCCESS" "$($r.status)" ($r.status -eq "SUCCESS" -and $r.kyc_tier -eq "TIER_2") "New Tier: $($r.kyc_tier)"
} catch {
    Record-Test "02-Upgrade-KYC-Tier-Admin" "200 SUCCESS" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

# --- 08. Ledger & Merkle ---
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/ledger?wallet_id=alice_usd&limit=10" -Method Get -Headers @{ Authorization = "Bearer $aliceToken" }
    Record-Test "01-Get-Ledger-Journal" "200" "200" ($null -ne $r.entries) "Entries count: $($r.entries.Count)"
} catch {
    Record-Test "01-Get-Ledger-Journal" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/ledger/merkle/latest" -Method Get
    Record-Test "02-Get-Latest-Merkle-Root" "200 or 404" "200" $true "Root: $($r.root_hash)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "02-Get-Latest-Merkle-Root" "200 or 404" "$code" ($code -eq 404 -or $code -eq 200) "Status $code"
}

if ($sameTxnId) {
    try {
        $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/ledger?transaction_id=$sameTxnId" -Method Get -Headers @{ Authorization = "Bearer $aliceToken" }
        Record-Test "03-Get-Ledger-By-TxnID-Counterparty" "200" "200" ($null -ne $r.entries -and $r.entries.Count -gt 0) "Entries count: $($r.entries.Count)"
    } catch {
        Record-Test "03-Get-Ledger-By-TxnID-Counterparty" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
    }
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/ledger/trial-balance?currency=USD" -Method Get -Headers @{ Authorization = "Bearer $adminToken" }
    Record-Test "05-Get-Trial-Balance" "200" "200" ($null -ne $r.is_balanced -and $r.currency -eq "USD") "Balanced: $($r.is_balanced), Total Assets: $($r.total_assets)"
} catch {
    Record-Test "05-Get-Trial-Balance" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $eodBody = '{"date":"2026-11-13","currency":"USD","generated_by":"treasury-admin"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/ledger/eod" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $adminToken" } -Body $eodBody
    Record-Test "06-Generate-EOD-Report" "200" "200" ($null -ne $r.report_id -and $r.verified -eq $true) "Report ID: $($r.report_id), Verified: $($r.verified)"
} catch {
    Record-Test "06-Generate-EOD-Report" "200" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

# --- 09. Webhooks ---
try {
    $whBody = '{"target_url":"https://httpbin.org/post","secret":"whsec_super_secret_bruno_test_key_42","events":["transfer.completed","deposit.settled","fraud.flagged"]}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/webhooks/subscriptions" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $aliceToken" } -Body $whBody
    Record-Test "01-Register-Webhook" "200/201 ACTIVE" "$($r.status)" ($r.status -eq "ACTIVE") "Subscription ID: $($r.subscription_id)"
} catch {
    Record-Test "01-Register-Webhook" "200/201 ACTIVE" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $pingBody = '{"target_url":"https://httpbin.org/post","secret":"whsec_super_secret_bruno_test_key_42"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/webhooks/test" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $aliceToken" } -Body $pingBody
    Record-Test "02-Trigger-Test-Ping" "200" "200" $true "Ping executed"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "02-Trigger-Test-Ping" "200" "$code" ($code -eq 200) "Status $code"
}

# --- 10. Disaster Recovery ---
try {
    $drBody = '{"target":"STANDBY"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/cluster/failover" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $adminToken" } -Body $drBody
    Record-Test "01-Failover-To-Standby" "200 STANDBY" "$($r.active_target)" ($r.active_target -eq "STANDBY") "Switched to Standby"
} catch {
    Record-Test "01-Failover-To-Standby" "200 STANDBY" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

try {
    $drBody = '{"target":"PRIMARY"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/cluster/failover" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $adminToken" } -Body $drBody
    Record-Test "02-Restore-Failover-To-Primary" "200 PRIMARY" "$($r.active_target)" ($r.active_target -eq "PRIMARY") "Restored to Primary"
} catch {
    Record-Test "02-Restore-Failover-To-Primary" "200 PRIMARY" $_.Exception.Response.StatusCode.value__ $false $_.Message
}

# --- 11. Security Negative Tests ---
try {
    $unauthBody = '{"source_wallet_id":"alice_usd","destination_wallet_id":"bob_eur","amount":{"units":1000,"currency":"USD"}}'
    Invoke-RestMethod -Uri "$baseUrl/api/v1/transfers" -Method Post -ContentType "application/json" -Body $unauthBody
    Record-Test "01-Reject-Unauthenticated" "401" "200" $false "Should have been rejected"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "01-Reject-Unauthenticated" "401" "$code" ($code -eq 401) "Properly rejected unauthenticated request"
}

try {
    # Alice trying to view Bob's wallet balance
    Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/bob_eur/balance" -Method Get -Headers @{ Authorization = "Bearer $aliceToken" }
    Record-Test "02-Reject-IDOR-Cross-Wallet" "403" "200" $false "Should have been rejected"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "02-Reject-IDOR-Cross-Wallet" "403" "$code" ($code -eq 403) "Properly rejected IDOR / BOLA attempt"
}

try {
    # Missing Idempotency Key on deposit
    $missingIdemBody = '{"wallet_id":"alice_usd","amount":{"units":1000,"currency":"USD"},"rail":"ACH"}'
    Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/deposit" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $aliceToken" } -Body $missingIdemBody
    Record-Test "03-Reject-Missing-Idempotency" "400" "200" $false "Should have been rejected"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "03-Reject-Missing-Idempotency" "400" "$code" ($code -eq 400) "Properly rejected missing idempotency key"
}

try {
    # Reject SSRF Webhook Target
    $ssrfBody = '{"target_url":"http://169.254.169.254/latest/meta-data/","secret":"whsec_attacker_key"}'
    Invoke-RestMethod -Uri "$baseUrl/api/v1/webhooks/subscriptions" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $aliceToken" } -Body $ssrfBody
    Record-Test "04-Reject-SSRF-Webhook" "400" "200" $false "Should have been rejected"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "04-Reject-SSRF-Webhook" "400" "$code" ($code -eq 400) "Properly rejected SSRF internal IP target"
}

# --- 12. Virtual IBANs (BaaS) ---
$viban = ""
try {
    $vibanBody = '{"country_code":"GB","currency":"GBP","label":"Alice Primary vIBAN"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/alice_usd/vibans" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $aliceToken" } -Body $vibanBody
    $viban = $r.iban
    Record-Test "01-Issue-Virtual-IBAN" "201 Created" "201" ($null -ne $viban -and $viban.StartsWith("GB")) "Issued vIBAN: $viban"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "01-Issue-Virtual-IBAN" "201 Created" "$code" $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/alice_usd/vibans" -Method Get -Headers @{ Authorization = "Bearer $aliceToken" }
    Record-Test "02-List-Virtual-IBANs" "200 OK" "200" ($r.count -ge 1) "Found $($r.count) vIBANs"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "02-List-Virtual-IBANs" "200 OK" "$code" $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/wallets/vibans?iban=$viban" -Method Get -Headers @{ Authorization = "Bearer $aliceToken" }
    Record-Test "03-Lookup-Virtual-IBAN" "200 OK" "200" ($r.wallet_id -eq "alice_usd") "Resolved to wallet: $($r.wallet_id)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "03-Lookup-Virtual-IBAN" "200 OK" "$code" $false $_.Message
}

# --- 13. Compliance, Travel Rule & Sanctions ---
try {
    $trBody = @{
        source_wallet_id = "alice_usd"
        destination_wallet_id = "bob_eur"
        amount = 150000
        currency = "USD"
        travel_rule = @{
            originator = @{
                legal_name = "Alice Corp LLC"
                account_number = "alice_usd"
                national_id = "US-EIN-998877"
                country = "US"
            }
            beneficiary = @{
                legal_name = "Bob Trading GmbH"
                account_number = "bob_eur"
                national_id = "DE-HRB-112233"
                country = "DE"
            }
        }
    } | ConvertTo-Json -Depth 5
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/transfers" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $aliceToken" } -Body $trBody
    Record-Test "01-Travel-Rule-Valid-Transfer" "200 OK" "200" ($r.status -eq "SUCCESS") "Transfer tx: $($r.transaction_id)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "01-Travel-Rule-Valid-Transfer" "200 OK" "$code" ($code -eq 200) $_.Message
}

try {
    $sancBody = @{
        source_wallet_id = "alice_usd"
        destination_wallet_id = "bob_eur"
        amount = 200000
        currency = "USD"
        travel_rule = @{
            originator = @{
                legal_name = "EVIL CORP CYBER SYNDICATE"
                account_number = "alice_usd"
                national_id = "RU-CYBER-1001"
                country = "RU"
            }
            beneficiary = @{
                legal_name = "Bob Trading GmbH"
                account_number = "bob_eur"
                country = "DE"
            }
        }
    } | ConvertTo-Json -Depth 5
    Invoke-RestMethod -Uri "$baseUrl/api/v1/transfers" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $aliceToken" } -Body $sancBody
    Record-Test "02-Sanctions-Blocked-Transfer" "403 Forbidden" "200" $false "Should have been blocked"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "02-Sanctions-Blocked-Transfer" "403 Forbidden" "$code" ($code -eq 403) "Properly blocked sanctioned entity"
}

# --- 14. Open Banking PSD2 ---
$obConsentId = ""
try {
    $consentReq = '{"client_tpp_id":"fintech-app-01","wallet_ids":["alice_usd"],"permissions":["ReadAccountsDetail","ReadBalances","CreatePaymentInitiation"]}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/open-banking/consents" -Method Post -ContentType "application/json" -Body $consentReq
    $obConsentId = $r.consent_id
    Record-Test "01-OpenBanking-Create-Consent" "201 Created" "201" ($null -ne $obConsentId -and $r.status -eq "AWAITING_AUTHORISATION") "Consent ID: $obConsentId"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "01-OpenBanking-Create-Consent" "201 Created" "$code" $false $_.Message
}

try {
    $authReq = "{`"consent_id`":`"$obConsentId`",`"user_id`":`"alice`"}"
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/open-banking/consents/authorise" -Method Post -ContentType "application/json" -Headers @{ Authorization = "Bearer $aliceToken" } -Body $authReq
    Record-Test "02-OpenBanking-Authorise-Consent" "200 OK" "200" ($r.status -eq "AUTHORISED") "Consent authorised"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "02-OpenBanking-Authorise-Consent" "200 OK" "$code" $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/open-banking/accounts" -Method Get -Headers @{ "Consent-ID" = $obConsentId }
    Record-Test "03-OpenBanking-AISP-Accounts" "200 OK" "200" ($r.Data.Account -contains "alice_usd") "Accounts: $($r.Data.Account)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "03-OpenBanking-AISP-Accounts" "200 OK" "$code" $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/open-banking/balances?wallet_id=alice_usd" -Method Get -Headers @{ "Consent-ID" = $obConsentId }
    Record-Test "04-OpenBanking-AISP-Balances" "200 OK" "200" ($null -ne $r.Data.Balance) "Balances retrieved"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "04-OpenBanking-AISP-Balances" "200 OK" "$code" $false $_.Message
}

try {
    $pispReq = "{`"instruction_id`":`"inst-ob-100`",`"consent_id`":`"$obConsentId`",`"debtor_wallet_id`":`"alice_usd`",`"creditor_wallet_id`":`"bob_eur`",`"amount`":15.00,`"currency`":`"USD`"}"
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/open-banking/payments" -Method Post -ContentType "application/json" -Headers @{ "Consent-ID" = $obConsentId } -Body $pispReq
    Record-Test "05-OpenBanking-PISP-Payment" "201 Created" "201" ($r.Data.Status -eq "AcceptedSettlementInProcess") "Payment ID: $($r.Data.DomesticPaymentId)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "05-OpenBanking-PISP-Payment" "201 Created" "$code" ($code -eq 201) $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/open-banking/consents?consent_id=$obConsentId" -Method Delete
    Record-Test "06-OpenBanking-Revoke-Consent" "200 OK" "200" ($r.status -eq "REVOKED") "Consent revoked"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "06-OpenBanking-Revoke-Consent" "200 OK" "$code" $false $_.Message
}

# --- 15. Security & Zero-Trust (Step-Up MFA) ---
$stepUpChallengeId = ""
$stepUpNonce = ""
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/auth/step-up/challenge" -Method Post -ContentType "application/json" -Body '{"user_id":"alice","action":"transfer"}'
    $stepUpChallengeId = $r.challenge_id
    $stepUpNonce = $r.nonce
    Record-Test "01-StepUp-Request-Challenge" "200 OK" "200" ($null -ne $stepUpChallengeId -and $stepUpNonce.Length -eq 64) "Challenge ID: $stepUpChallengeId"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "01-StepUp-Request-Challenge" "200 OK" "$code" $false $_.Message
}

$stepUpToken = ""
try {
    # Verify using simulated passkey biometric proof
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/auth/step-up/verify" -Method Post -ContentType "application/json" -Body "{`"challenge_id`":`"$stepUpChallengeId`",`"proof`":`"passkey-biometric-verified`"}"
    $stepUpToken = $r.step_up_token
    Record-Test "02-StepUp-Verify-Challenge" "200 OK" "200" ($r.success -eq $true -and $null -ne $stepUpToken) "Elevated token acquired"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "02-StepUp-Verify-Challenge" "200 OK" "$code" $false $_.Message
}

try {
    $transferReq = '{"source_wallet_id":"alice_usd","destination_wallet_id":"bob_eur","amount":500,"currency":"USD"}'
    $r = Invoke-WebRequest -Uri "$baseUrl/api/v1/transfers" -Method Post -ContentType "application/json" -Headers @{ "X-Require-StepUp" = "true" } -Body $transferReq -SkipHttpErrorCheck
    $code = $r.StatusCode
    Record-Test "03-StepUp-Protected-Mutation-Blocked" "403 Forbidden" "$code" ($code -eq 403) "Correctly blocked mutation without Step-Up header"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "03-StepUp-Protected-Mutation-Blocked" "403 Forbidden" "$code" ($code -eq 403) $_.Message
}

# --- 16. Active-Active Geo-Sharding ---
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/cluster/sharding/regions" -Method Get
    Record-Test "01-Sharding-List-Regions" "200 OK" "200" ($r.count -ge 3 -and $r.regions.Length -ge 3) "Available regions: $($r.count)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "01-Sharding-List-Regions" "200 OK" "$code" $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/cluster/sharding/home?wallet_id=alice_usd" -Method Get
    Record-Test "02-Sharding-Get-Home-Region" "200 OK" "200" ($null -ne $r.home_region.region_id -and $r.home_region.is_healthy -eq $true) "Home Region: $($r.home_region.region_id)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "02-Sharding-Get-Home-Region" "200 OK" "$code" $false $_.Message
}

# --- 17. Multi-Entity Banking & Treasury Operations ---
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/entities" -Method Get
    Record-Test "01-MultiEntity-List-Entities" "200 OK" "200" ($r.count -ge 4 -and $r.entities.Length -ge 4) "Entities: $($r.count)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "01-MultiEntity-List-Entities" "200 OK" "$code" $false $_.Message
}

try {
    $settleReq = '{"source_wallet":"alice_usd","target_wallet":"bob_eur","amount":500,"currency":"EUR"}'
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/entities/settlement" -Method Post -ContentType "application/json" -Body $settleReq
    Record-Test "02-MultiEntity-Omnibus-Settlement" "200 OK" "200" ($r.is_cross_entity -eq $true -and $r.omnibus_postings.Length -eq 4) "Settled 4-leg omnibus"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "02-MultiEntity-Omnibus-Settlement" "200 OK" "$code" $false $_.Message
}

try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/treasury/safeguarding?customer_deposits=50000000&trustee_reserves=51000000" -Method Get
    Record-Test "03-Treasury-Safeguarding-Report" "200 OK" "200" ($r.status -eq "COMPLIANT" -and $r.coverage_ratio -ge 1.0) "Coverage: $($r.coverage_ratio)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "03-Treasury-Safeguarding-Report" "200 OK" "$code" $false $_.Message
}

# --- 18. CQRS Event Sourcing & Point-in-Time Auditing ---
try {
    $r = Invoke-RestMethod -Uri "$baseUrl/api/v1/ledger/events?account_id=alice_usd" -Method Get
    Record-Test "01-CQRS-PointInTime-Balance" "200 OK" "200" ($null -ne $r.account_id -and $null -ne $r.available_balance) "Audit snapshot verified"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    Record-Test "01-CQRS-PointInTime-Balance" "200 OK" "$code" $false $_.Message
}

Write-Host "`n=================== SUMMARY ===================" -ForegroundColor Cyan
$passedCount = ($results | Where-Object { $_.Passed -eq $true }).Count
$totalCount = $results.Count
$summaryColor = if ($passedCount -eq $totalCount) { "Green" } else { "Yellow" }
$failedCount = $totalCount - $passedCount
Write-Host "Total Tests: $totalCount | Passed: $passedCount | Failed: $failedCount" -ForegroundColor $summaryColor
