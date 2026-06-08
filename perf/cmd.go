package perf

import (
	"github.com/qqz14/zctl/internal/cobrax"
	"github.com/qqz14/zctl/perf/cli"
	"github.com/spf13/cobra"
)

var (
	// Cmd is the root perf command.
	Cmd = cobrax.NewCommand("perf")

	scanCmd = cobrax.NewCommand("scan", cobrax.WithRunE(cli.PerfScan))
)

func init() {
	Cmd.Short = "Static performance & quality scan"
	Cmd.Long = `Run static analysis on the current Go project to detect:
  - Code style (gofmt)
  - Static correctness (go vet)
  - Resource leaks: HTTP body, sql.Rows, rows.Err(), context (golangci-lint)
  - Performance patterns: slice prealloc, loop context, large-value params
  - CVE vulnerabilities in dependencies (govulncheck)
  - Heap escape hotspots (go build -gcflags="-m=1")
  - N+1 DB query patterns (AST scan)

Outputs a Markdown report to build/perf/REPORT.md.

Example:
  zctl perf scan
  zctl perf scan --dir=/path/to/project
  zctl perf scan --out=./reports/`

	scanCmd.Short = "Run all static checks and output REPORT.md"
	scanCmd.Long = `Execute all static checkers in sequence and write results to build/perf/.

Required tools (auto-detected, skipped if missing):
  golangci-lint  go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
  govulncheck    go install golang.org/x/vuln/cmd/govulncheck@latest

Built-in (no install needed):
  gofmt, go vet, go build -gcflags="-m=1", AST scanner`

	scanFlags := scanCmd.Flags()
	scanFlags.StringVarWithDefaultValue(&cli.VarStringDir, "dir", ".")
	scanFlags.StringVar(&cli.VarStringOut, "out")

	Cmd.AddCommand(scanCmd)

	// Allow calling `zctl perf` directly as an alias for `zctl perf scan`
	Cmd.Command.RunE = func(cmd *cobra.Command, args []string) error {
		return cli.PerfScan(cmd, args)
	}
}
