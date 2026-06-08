package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gookit/color"
	"github.com/qqz14/zctl/perf/checker"
	"github.com/spf13/cobra"
)

var (
	VarStringDir string
	VarStringOut string
)

// ScanResult holds all checker results.
type ScanResult struct {
	Fmt     *checker.Result
	Vet     *checker.Result
	Lint    *checker.Result
	Vuln    *checker.Result
	Escape  *checker.Result
	N1      *checker.Result
	Elapsed time.Duration
}

// PerfScan is the entry for zctl perf scan.
func PerfScan(_ *cobra.Command, _ []string) error {
	dir := VarStringDir
	if dir == "" {
		dir = "."
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("invalid dir: %w", err)
	}
	outDir := VarStringOut
	if outDir == "" {
		outDir = filepath.Join(absDir, "build", "perf")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s failed: %w", outDir, err)
	}

	printBanner(absDir)
	start := time.Now()

	res := &ScanResult{}

	// Step 1: gofmt
	printStep(1, 6, "gofmt (code format)")
	res.Fmt = checker.RunFmt(absDir)
	printResult(res.Fmt)

	// Step 2: go vet
	printStep(2, 6, "go vet (static correctness)")
	res.Vet = checker.RunVet(absDir)
	printResult(res.Vet)

	// Step 3: golangci-lint
	printStep(3, 6, "golangci-lint (resource leak + perf + style)")
	res.Lint = checker.RunLint(absDir, outDir)
	printResult(res.Lint)

	// Step 4: govulncheck
	printStep(4, 6, "govulncheck (CVE scan)")
	res.Vuln = checker.RunVuln(absDir, outDir)
	printResult(res.Vuln)

	// Step 5: escape analysis
	printStep(5, 6, "escape analysis (heap alloc hotspot)")
	res.Escape = checker.RunEscape(absDir, outDir)
	printResult(res.Escape)

	// Step 6: N+1 — two-phase: AST candidates + SSA callgraph trace
	printStep(6, 6, "N+1 query scan (AST + SSA callgraph, partial load)")
	res.N1 = checker.RunN1(absDir, outDir)
	printResult(res.N1)

	res.Elapsed = time.Since(start)

	// Write REPORT.md
	report := buildReport(absDir, res)
	reportPath := filepath.Join(outDir, "REPORT.md")
	if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
		color.Red.Printf("  failed to write report: %v\n", err)
	}

	printSummary(res, outDir)
	return exitCode(res)
}

// ── helpers ──

func printBanner(dir string) {
	fmt.Println()
	color.Style{color.FgCyan, color.Bold}.Println("════════════════════════════════════════════════════════════")
	color.Style{color.FgCyan, color.Bold}.Printf("  zctl perf scan · %s\n", filepath.Base(dir))
	color.Style{color.FgCyan, color.Bold}.Printf("  %s\n", time.Now().Format("2006-01-02 15:04:05"))
	color.Style{color.FgCyan, color.Bold}.Println("════════════════════════════════════════════════════════════")
	fmt.Println()
}

func printStep(n, total int, desc string) {
	color.Style{color.FgBlue, color.Bold}.Printf("[%d/%d] %s ...\n", n, total, desc)
}

func printResult(r *checker.Result) {
	if r == nil {
		return
	}
	switch r.Level {
	case checker.LevelPass:
		color.Green.Printf("  ✅ PASS — %s\n", r.Summary)
	case checker.LevelWarn:
		color.Yellow.Printf("  ⚠️  WARN — %s\n", r.Summary)
		for _, issue := range r.Issues[:min(5, len(r.Issues))] {
			color.Yellow.Printf("     %s\n", issue)
		}
		if len(r.Issues) > 5 {
			color.Yellow.Printf("     ... and %d more (see report)\n", len(r.Issues)-5)
		}
	case checker.LevelFail:
		color.Red.Printf("  ❌ FAIL — %s\n", r.Summary)
		for _, issue := range r.Issues[:min(5, len(r.Issues))] {
			color.Red.Printf("     %s\n", issue)
		}
		if len(r.Issues) > 5 {
			color.Red.Printf("     ... and %d more (see report)\n", len(r.Issues)-5)
		}
	case checker.LevelInfo:
		color.Gray.Printf("  ℹ️  INFO — %s\n", r.Summary)
	case checker.LevelSkip:
		color.Gray.Printf("  ⊘  SKIP — %s\n", r.Summary)
	}
	fmt.Println()
}

func printSummary(res *ScanResult, outDir string) {
	color.Style{color.FgCyan, color.Bold}.Println("════════════════════════════════════════════════════════════")
	fmt.Printf("  %-28s %s\n", "gofmt", badge(res.Fmt))
	fmt.Printf("  %-28s %s\n", "go vet", badge(res.Vet))
	fmt.Printf("  %-28s %s\n", "golangci-lint", badge(res.Lint))
	fmt.Printf("  %-28s %s\n", "govulncheck", badge(res.Vuln))
	fmt.Printf("  %-28s %s\n", "escape analysis", badge(res.Escape))
	fmt.Printf("  %-28s %s\n", "N+1 query scan", badge(res.N1))
	fmt.Printf("  %-28s %s\n", "elapsed", res.Elapsed.Round(time.Millisecond).String())
	fmt.Println()
	color.Gray.Printf("  Report: %s/REPORT.md\n", outDir)
	color.Style{color.FgCyan, color.Bold}.Println("════════════════════════════════════════════════════════════")

	if hasFail(res) {
		fmt.Println()
		color.Style{color.FgRed, color.Bold}.Println("  🚫 RESULT: ISSUES FOUND — review report above")
	} else {
		fmt.Println()
		color.Style{color.FgGreen, color.Bold}.Println("  ✅ RESULT: NO CRITICAL ISSUES")
	}
	fmt.Println()
}

func badge(r *checker.Result) string {
	if r == nil {
		return color.Gray.Sprint("SKIP")
	}
	switch r.Level {
	case checker.LevelPass:
		return color.Green.Sprint("PASS")
	case checker.LevelWarn:
		return color.Yellow.Sprintf("WARN (%d issues)", len(r.Issues))
	case checker.LevelFail:
		return color.Red.Sprintf("FAIL (%d issues)", len(r.Issues))
	case checker.LevelInfo:
		return color.Gray.Sprintf("INFO (%d items)", len(r.Issues))
	default:
		return color.Gray.Sprint("SKIP")
	}
}

func hasFail(res *ScanResult) bool {
	for _, r := range []*checker.Result{res.Fmt, res.Vet, res.Lint, res.Vuln, res.N1} {
		if r != nil && r.Level == checker.LevelFail {
			return true
		}
	}
	return false
}

func exitCode(res *ScanResult) error {
	if hasFail(res) {
		return fmt.Errorf("perf scan found issues — check build/perf/REPORT.md")
	}
	return nil
}

func buildReport(dir string, res *ScanResult) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Perf Static Scan Report — %s\n\n", filepath.Base(dir)))
	sb.WriteString(fmt.Sprintf("- Time   : %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("- Elapsed: %s\n\n", res.Elapsed.Round(time.Millisecond)))

	sb.WriteString("## Summary\n\n")
	sb.WriteString("| 检测项 | 状态 | 问题数 |\n")
	sb.WriteString("|--------|:----:|-------:|\n")
	writeRow(&sb, "gofmt (代码格式)", res.Fmt)
	writeRow(&sb, "go vet (静态正确性)", res.Vet)
	writeRow(&sb, "golangci-lint (资源泄漏/性能/规范)", res.Lint)
	writeRow(&sb, "govulncheck (CVE漏洞)", res.Vuln)
	writeRow(&sb, "escape analysis (堆逃逸热点)", res.Escape)
	writeRow(&sb, "N+1 query scan (SSA callgraph)", res.N1)
	sb.WriteString("\n")

	writeSection(&sb, "gofmt", res.Fmt)
	writeSection(&sb, "go vet", res.Vet)
	writeSection(&sb, "golangci-lint", res.Lint)
	writeSection(&sb, "govulncheck (CVE)", res.Vuln)
	writeSection(&sb, "escape analysis (heap hotspot)", res.Escape)
	writeSection(&sb, "N+1 query scan", res.N1)

	return sb.String()
}

func writeRow(sb *strings.Builder, name string, r *checker.Result) {
	if r == nil {
		sb.WriteString(fmt.Sprintf("| %s | ⊘ SKIP | - |\n", name))
		return
	}
	icon := map[checker.Level]string{
		checker.LevelPass: "✅ PASS",
		checker.LevelWarn: "⚠️ WARN",
		checker.LevelFail: "❌ FAIL",
		checker.LevelInfo: "ℹ️ INFO",
		checker.LevelSkip: "⊘ SKIP",
	}[r.Level]
	sb.WriteString(fmt.Sprintf("| %s | %s | %d |\n", name, icon, len(r.Issues)))
}

func writeSection(sb *strings.Builder, title string, r *checker.Result) {
	if r == nil || len(r.Issues) == 0 {
		return
	}
	sb.WriteString(fmt.Sprintf("## %s\n\n", title))
	sb.WriteString("```\n")
	for _, issue := range r.Issues {
		sb.WriteString(issue + "\n")
	}
	sb.WriteString("```\n\n")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
