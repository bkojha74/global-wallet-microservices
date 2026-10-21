package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
)

var (
	// Version is the current semantic release version of walletctl.
	Version = "1.6.0-rc.1"
	// GitCommit is populated at build time.
	GitCommit = "dev"
	// BuildDate is populated at build time.
	BuildDate = "unknown"
)

const usageText = `walletctl - Unified Operator & Developer CLI for Global Wallet Microservices

Usage:
  walletctl <command> [arguments]

Available Commands:
  health         Inspect multi-region gateway, wallet, ledger, and FX cluster health
  token          Mint offline or remote JWT bearer access tokens with custom scopes
  failover       Inspect active routed target or trigger multi-region failover
  ledger verify  Verify cryptographic SHA-256 hash chains and GAAP balance integrity
  webhook ping   Dispatch signed test webhook payload to verify partner endpoint
  version        Print walletctl CLI version and build information

Use "walletctl <command> --help" for more information about a command.
`

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "walletctl version %s (%s) built %s on %s/%s\n",
		Version, GitCommit, BuildDate, runtime.GOOS, runtime.GOARCH)
}

func runCLI(args []string, out io.Writer, errOut io.Writer) int {
	if len(args) < 2 {
		fmt.Fprint(out, usageText)
		return 0
	}

	cmd := args[1]
	subArgs := args[2:]

	switch cmd {
	case "version", "--version", "-v":
		printVersion(out)
		return 0

	case "help", "--help", "-h":
		fmt.Fprint(out, usageText)
		return 0

	case "health":
		return runHealthCmd(subArgs, out, errOut)

	case "token":
		return runTokenCmd(subArgs, out, errOut)

	case "failover":
		return runFailoverCmd(subArgs, out, errOut)

	case "ledger":
		return runLedgerCmd(subArgs, out, errOut)

	case "webhook":
		return runWebhookCmd(subArgs, out, errOut)

	default:
		fmt.Fprintf(errOut, "Unknown command %q. Run 'walletctl --help' for available commands.\n", cmd)
		return 1
	}
}

func main() {
	exitCode := runCLI(os.Args, os.Stdout, os.Stderr)
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}
