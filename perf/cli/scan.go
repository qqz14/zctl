package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gookit/color"
	"github.com/qqz14/zctl/perf/checker"
	"github.com/spf13/cobra"
)

var (
	VarStringDir string
	VarStringOut string

	// Dynamic analysis flags (only active when --dynamic is set)
	VarBoolDynamic         bool
	VarStringPprof         string
	VarStringSlowLog       string
	VarDurationPprofWindow time.Duration
)

// ScanResult holds all checker results.
type ScanResult struct {
	Fmt         *checker.Result
	Vet         *checker.Result
	Lint        *checker.Result
	Vuln        *checker.Result
	Escape      *checker.Result
	N1          *checker.Result
	EntFullScan *checker.Result
	LogicReview *checker.Result
	Dynamic     *checker.DynamicResult // nil when --dynamic not set
	Elapsed     time.Duration
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

	printBanner(absDir, VarBoolDynamic)
	start := time.Now()

	res := &ScanResult{}

	totalSteps := 8
	if VarBoolDynamic {
		totalSteps = 11
	}

	// Step 0: build call graph once — shared by N+1 and Logic Review
	// This is the most expensive step (~5-20s) but runs only once.
	color.Style{color.FgBlue, color.Bold}.Println("[0/–] building call graph (CHA, internal/... only) ...")
	cgCache, cgErr := checker.BuildCallGraph(absDir)
	if cgErr != nil {
		color.Gray.Printf("  ⊘  call graph skipped: %v\n\n", cgErr)
		cgCache = nil
	} else {
		color.Green.Println("  ✅ call graph ready\n")
	}

	// Step 1: gofmt
	printStep(1, totalSteps, "gofmt (code format)")
	res.Fmt = checker.RunFmt(absDir)
	printResult(res.Fmt)

	// Step 2: go vet
	printStep(2, totalSteps, "go vet (static correctness)")
	res.Vet = checker.RunVet(absDir)
	printResult(res.Vet)

	// Step 3: golangci-lint
	printStep(3, totalSteps, "golangci-lint (resource leak + perf + style)")
	res.Lint = checker.RunLint(absDir, outDir)
	printResult(res.Lint)

	// Step 4: govulncheck
	printStep(4, totalSteps, "govulncheck (CVE scan)")
	res.Vuln = checker.RunVuln(absDir, outDir)
	printResult(res.Vuln)

	// Step 5: escape analysis
	printStep(5, totalSteps, "escape analysis (heap alloc hotspot)")
	res.Escape = checker.RunEscape(absDir, outDir)
	printResult(res.Escape)

	// Step 6: N+1 — Phase1 AST candidates + Phase2 call graph trace
	printStep(6, totalSteps, "N+1 query scan (call graph trace)")
	res.N1 = checker.RunN1(absDir, cgCache)
	printResult(res.N1)

	// Step 7: logic review — per-interface DB/Redis trace via call graph + implIdx AST
	// Must run before SQL perf since SQL perf derives its findings from logic review IONodes.
	printStep(7, totalSteps, "logic review (DB/Redis trace per interface)")
	res.LogicReview = checker.RunLogicReview(cgCache)
	printResult(res.LogicReview)

	// Step 7.5 (no banner): SQL perf derived from logic review results — no extra file scan needed
	res.EntFullScan = checker.RunSQLPerfFromLogicReview(checker.LastLogicReviewResult())

	// Steps 9-11: dynamic analysis (only when --dynamic flag is set)
	if VarBoolDynamic {
		dur := VarDurationPprofWindow
		if dur == 0 {
			dur = 30 * time.Second
		}
		cfg := checker.DynamicConfig{
			PprofAddr:        VarStringPprof,
			SlowQueryLogPath: VarStringSlowLog,
			ProfileDuration:  dur,
			OutDir:           outDir,
		}

		printStep(8, totalSteps, fmt.Sprintf("pprof CPU profile (%s)", dur))
		printStep(9, totalSteps, "pprof heap + goroutine snapshot")
		printStep(10, totalSteps, "slow query log analysis")

		color.Gray.Println("  ⏳ collecting pprof data, please wait...")
		res.Dynamic = checker.RunDynamic(cfg)
		printResult(res.Dynamic.CPU)
		printResult(res.Dynamic.Heap)
		printResult(res.Dynamic.Goroutine)
		printResult(res.Dynamic.SlowQuery)
	}

	res.Elapsed = time.Since(start)

	// Write unified report.html
	checker.WriteReportHTML(outDir, absDir, map[string]*checker.Result{
		"fmt":      res.Fmt,
		"vet":      res.Vet,
		"lint":     res.Lint,
		"vuln":     res.Vuln,
		"escape":   res.Escape,
		"n1":       res.N1,
		"sql-perf": res.EntFullScan,
		"dynamic":  dynamicSummaryResult(res.Dynamic),
	}, res.Elapsed, res.Dynamic)

	printSummary(res, outDir)
	return exitCode(res)
}

// dynamicSummaryResult returns a synthetic Result for the dynamic section header (used in nav).
func dynamicSummaryResult(dr *checker.DynamicResult) *checker.Result {
	if dr == nil {
		return nil
	}
	// worst level across all sub-results
	worst := checker.LevelPass
	order := map[checker.Level]int{
		checker.LevelPass: 0, checker.LevelSkip: 0,
		checker.LevelInfo: 1, checker.LevelWarn: 2, checker.LevelFail: 3,
	}
	for _, r := range []*checker.Result{dr.CPU, dr.Heap, dr.Goroutine, dr.SlowQuery} {
		if r != nil && order[r.Level] > order[worst] {
			worst = r.Level
		}
	}
	return &checker.Result{Level: worst, Summary: "dynamic analysis"}
}

// ── helpers ──

func printBanner(dir string, dynamic bool) {
	mode := "static"
	if dynamic {
		mode = "static + dynamic"
	}
	fmt.Println()
	color.Style{color.FgCyan, color.Bold}.Println("════════════════════════════════════════════════════════════")
	color.Style{color.FgCyan, color.Bold}.Printf("  zctl perf scan [%s] · %s\n", mode, filepath.Base(dir))
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
	fmt.Printf("  %-28s %s\n", "ent full-scan", badge(res.EntFullScan))
	fmt.Printf("  %-28s %s\n", "logic review", badge(res.LogicReview))
	if res.Dynamic != nil {
		fmt.Printf("  %-28s %s\n", "pprof CPU", badge(res.Dynamic.CPU))
		fmt.Printf("  %-28s %s\n", "pprof heap", badge(res.Dynamic.Heap))
		fmt.Printf("  %-28s %s\n", "goroutine", badge(res.Dynamic.Goroutine))
		fmt.Printf("  %-28s %s\n", "slow query", badge(res.Dynamic.SlowQuery))
	}
	fmt.Printf("  %-28s %s\n", "elapsed", res.Elapsed.Round(time.Millisecond).String())
	fmt.Println()
	color.Gray.Printf("  Report: %s/report.html\n", outDir)
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
		return fmt.Errorf("perf scan found issues — open build/perf/report.html")
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
