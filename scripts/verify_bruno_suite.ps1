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

Write-Host "`n=================== SUMMARY ===================" -ForegroundColor Cyan
$passedCount = ($results | Where-Object { $_.Passed -eq $true }).Count
$totalCount = $results.Count
$summaryColor = if ($passedCount -eq $totalCount) { "Green" } else { "Yellow" }
$failedCount = $totalCount - $passedCount
Write-Host "Total Tests: $totalCount | Passed: $passedCount | Failed: $failedCount" -ForegroundColor $summaryColor
