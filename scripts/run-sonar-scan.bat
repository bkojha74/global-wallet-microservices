@echo off
setlocal

set SONAR_HOST=%SONAR_HOST_URL%
if "%SONAR_HOST%"=="" set SONAR_HOST=http://sonarqube:9000

set TOKEN=%1
if "%TOKEN%"=="" set TOKEN=%SONAR_TOKEN%

echo ==========================================================
echo  Running Dockerized SonarQube Scanner
echo  Target: %SONAR_HOST%
echo ==========================================================

if not "%TOKEN%"=="" goto run_scan

echo [WARNING] No SONAR_TOKEN provided via argument or environment variable.
echo SonarQube requires an authentication token to submit an analysis.
echo.
echo How to get a token:
echo   1. Open http://localhost:9000 in your browser
echo   2. Sign in with 'admin' / 'admin'
echo   3. Go to User Profile -^> Security (or http://localhost:9000/account/security)
echo   4. Generate a Token (e.g. 'local-scan-token')
echo   5. Re-run:
echo      .\scripts\run-sonar-scan.bat YOUR_TOKEN
echo.

:run_scan
echo Generating Go test coverage profile (coverage.out)...
python scripts\merge_coverage.py
if errorlevel 1 (
  echo [WARNING] Failed to generate coverage.out. Continuing with scan...
)

docker run --rm ^
  --network wallet_shared_net ^
  -v "%cd%:/usr/src" ^
  -e SONAR_HOST_URL=%SONAR_HOST% ^
  -e SONAR_TOKEN=%TOKEN% ^
  sonarsource/sonar-scanner-cli:latest

if errorlevel 1 (
  echo.
  echo [ERROR] SonarQube scan failed.
  echo If you got 'Not authorized', provide your user token:
  echo   .\scripts\run-sonar-scan.bat YOUR_TOKEN
  exit /b 1
)

echo.
echo [SUCCESS] SonarQube scan complete!
echo View results at: http://localhost:9000/dashboard?id=global-wallet
