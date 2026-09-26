import glob
import os
import subprocess

def merge():
    mode_line = "mode: set\n"
    counts = {}

    packages = [
        "cmd/api-gateway",
        "cmd/auth-service",
        "cmd/ledger-service",
        "cmd/logging-service",
        "cmd/wallet-service",
        "pkg/auth",
        "pkg/coordinator",
        "pkg/db",
        "pkg/observability",
        "pkg/tlsutil",
    ]

    os.makedirs("coverage_parts", exist_ok=True)
    for fpath in glob.glob("coverage_parts/*.out"):
        try:
            os.remove(fpath)
        except OSError:
            pass

    def run_test(args):
        i, pkg = args
        out_file = f"coverage_parts/part_{i}.out"
        res = subprocess.run(["go", "test", "-count=1", "-coverprofile", out_file, "-covermode=set", f"./{pkg}"], capture_output=True, text=True)
        if res.returncode != 0:
            print(f"[ERROR] go test failed for ./{pkg}:\n{res.stderr}\n{res.stdout}", flush=True)
        else:
            print(f"[OK] go test passed for ./{pkg}", flush=True)

    from concurrent.futures import ThreadPoolExecutor
    with ThreadPoolExecutor(max_workers=5) as executor:
        list(executor.map(run_test, enumerate(packages)))

    for fpath in glob.glob("coverage_parts/*.out"):
        with open(fpath, "r", encoding="utf-8") as f:
            for line in f:
                if line.startswith("mode:"):
                    continue
                line = line.strip()
                if not line:
                    continue
                parts = line.rsplit(" ", 2)
                if len(parts) == 3:
                    key = parts[0]
                    num_stmt = parts[1]
                    cnt = int(parts[2])
                    if key in counts:
                        counts[key] = (num_stmt, max(counts[key][1], cnt))
                    else:
                        counts[key] = (num_stmt, cnt)

    with open("coverage.out", "w", encoding="utf-8") as f:
        f.write(mode_line)
        for key, (num_stmt, cnt) in counts.items():
            f.write(f"{key} {num_stmt} {cnt}\n")

    print(f"[COVERAGE] Merged {len(counts)} coverage blocks into coverage.out")

if __name__ == "__main__":
    merge()
