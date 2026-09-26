@echo off
setlocal

echo ==========================================================
echo  Running Dockerized SAST / Security Scan (GoSec)
echo ==========================================================

docker run --rm ^
  -v "%cd%:/app" ^
  -w /app ^
  securego/gosec:latest ^
  -exclude-generated ^
  -severity medium ^
  ./...

if errorlevel 1 (
  echo [WARNING] SAST scan found potential security findings above medium threshold.
) else (
  echo [SUCCESS] No medium or high security issues detected!
)
