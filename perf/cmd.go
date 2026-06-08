package perf

import (
	"time"

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
	Cmd.Short = "Static (+ optional dynamic) performance & quality scan"
	Cmd.Long = `Run static analysis on the current Go project to detect:
  - Code style (gofmt)
  - Static correctness (go vet)
  - Resource leaks, perf patterns, style (golangci-lint)
  - CVE vulnerabilities (govulncheck)
  - Heap escape hotspots (go build -gcflags="-m=1")
  - N+1 DB query patterns (AST + impl trace)
  - ent .All() without .Limit() (potential full-table scan)

With --dynamic, also runs:
  - pprof CPU / heap / goroutine snapshot (service must expose /debug/pprof)
  - MySQL slow query log analysis

Example:
  zctl perf scan
  zctl perf scan --dir=/path/to/project
  zctl perf scan --dynamic --pprof=http://localhost:6060
  zctl perf scan --dynamic --pprof=http://localhost:6060 --slow-log=/var/log/mysql/slow.log
  zctl perf scan --dynamic --pprof=http://localhost:6060 --pprof-window=60s`

	scanCmd.Short = "Run static checks (+ optional dynamic) and output report.html"
	scanCmd.Long = `Execute all static checkers in sequence and write results to build/perf/.

Required tools (auto-detected, skipped if missing):
  golangci-lint  go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
  govulncheck    go install golang.org/x/vuln/cmd/govulncheck@latest

Built-in (no install needed):
  gofmt, go vet, go build -gcflags="-m=1", AST scanner, ent full-scan

Dynamic analysis (only with --dynamic):
  --pprof        pprof HTTP endpoint, e.g. http://localhost:6060
  --slow-log     MySQL slow query log file path
  --pprof-window CPU profile collection window (default 30s)`

	scanFlags := scanCmd.Flags()
	// Static flags
	scanFlags.StringVarWithDefaultValue(&cli.VarStringDir, "dir", ".")
	scanFlags.StringVar(&cli.VarStringOut, "out")

	// Dynamic flags — use cobra native FlagSet for bool/duration support
	rawFlags := scanCmd.Command.Flags()
	rawFlags.BoolVar(&cli.VarBoolDynamic, "dynamic", false,
		"enable dynamic analysis (pprof + slow query log)")
	rawFlags.StringVar(&cli.VarStringPprof, "pprof", "",
		"pprof HTTP endpoint, e.g. http://localhost:6060 (requires --dynamic)")
	rawFlags.StringVar(&cli.VarStringSlowLog, "slow-log", "",
		"MySQL slow query log file path (requires --dynamic)")
	rawFlags.DurationVar(&cli.VarDurationPprofWindow, "pprof-window", 30*time.Second,
		"CPU profile collection window (default 30s, requires --dynamic)")

	Cmd.AddCommand(scanCmd)

	// Allow calling `zctl perf` directly as an alias for `zctl perf scan`
	Cmd.Command.RunE = func(cmd *cobra.Command, args []string) error {
		return cli.PerfScan(cmd, args)
	}
}
