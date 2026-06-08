package checker

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DynamicConfig holds parameters for dynamic analysis.
type DynamicConfig struct {
	// PprofAddr is the pprof HTTP endpoint, e.g. "http://localhost:6060"
	PprofAddr string
	// SlowQueryLogPath is the MySQL slow query log file path on the local machine.
	// Can be empty if only pprof is needed.
	SlowQueryLogPath string
	// ProfileDuration is how long to collect pprof CPU profile (default 30s).
	ProfileDuration time.Duration
	// OutDir is where raw pprof files and flamegraph are written.
	OutDir string
}

// DynamicResult holds results from all dynamic checks.
type DynamicResult struct {
	CPU      *Result
	Heap     *Result
	Goroutine *Result
	SlowQuery *Result
}

// RunDynamic executes dynamic analysis: pprof CPU/heap/goroutine + slow query log.
func RunDynamic(cfg DynamicConfig) *DynamicResult {
	if cfg.ProfileDuration == 0 {
		cfg.ProfileDuration = 30 * time.Second
	}
	dr := &DynamicResult{}

	// ── pprof ──────────────────────────────────────────────────────────────────
	if cfg.PprofAddr != "" {
		dr.CPU = runPprofProfile(cfg, "cpu")
		dr.Heap = runPprofProfile(cfg, "heap")
		dr.Goroutine = runPprofGoroutine(cfg)
	} else {
		dr.CPU = Skip("pprof addr not provided (use --pprof=http://host:port)")
		dr.Heap = Skip("pprof addr not provided")
		dr.Goroutine = Skip("pprof addr not provided")
	}

	// ── slow query log ─────────────────────────────────────────────────────────
	if cfg.SlowQueryLogPath != "" {
		dr.SlowQuery = runSlowQueryAnalysis(cfg.SlowQueryLogPath, cfg.OutDir)
	} else {
		dr.SlowQuery = Skip("slow query log path not provided (use --slow-log=/path/to/slow.log)")
	}

	return dr
}

// ── pprof ─────────────────────────────────────────────────────────────────────

func runPprofProfile(cfg DynamicConfig, kind string) *Result {
	if _, err := exec.LookPath("go"); err != nil {
		return Skip("go tool not found")
	}

	// Check endpoint reachable
	testURL := strings.TrimRight(cfg.PprofAddr, "/") + "/debug/pprof/"
	resp, err := http.Get(testURL) //nolint:noctx
	if err != nil {
		return &Result{
			Level:   LevelSkip,
			Summary: fmt.Sprintf("pprof endpoint unreachable: %s (%v)", testURL, err),
		}
	}
	resp.Body.Close()

	outFile := filepath.Join(cfg.OutDir, kind+".prof")
	var args []string
	switch kind {
	case "cpu":
		args = []string{
			"tool", "pprof", "-top", "-nodecount=20",
			fmt.Sprintf("%s/debug/pprof/profile?seconds=%d", strings.TrimRight(cfg.PprofAddr, "/"),
				int(cfg.ProfileDuration.Seconds())),
		}
	case "heap":
		args = []string{
			"tool", "pprof", "-top", "-nodecount=20",
			strings.TrimRight(cfg.PprofAddr, "/") + "/debug/pprof/heap",
		}
	}

	cmd := exec.Command("go", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()

	raw := out.String()
	_ = os.WriteFile(outFile, []byte(raw), 0o644)

	issues := parsePprofTop(raw, kind)
	if len(issues) == 0 {
		return Pass(fmt.Sprintf("pprof %s: no hotspot detected", kind))
	}
	return &Result{
		Level:   LevelWarn,
		Summary: fmt.Sprintf("pprof %s: %d hotspot(s) — see build/perf/%s.prof", kind, len(issues), kind),
		Issues:  issues,
	}
}

func runPprofGoroutine(cfg DynamicConfig) *Result {
	url := strings.TrimRight(cfg.PprofAddr, "/") + "/debug/pprof/goroutine?debug=1"
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return &Result{Level: LevelSkip, Summary: fmt.Sprintf("goroutine pprof unreachable: %v", err)}
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	raw := buf.String()

	outFile := filepath.Join(cfg.OutDir, "goroutine.txt")
	_ = os.WriteFile(outFile, []byte(raw), 0o644)

	issues, count := parseGoroutineCount(raw)
	if count == 0 {
		return Pass("goroutine: no leak patterns detected")
	}
	return &Result{
		Level:   LevelInfo,
		Summary: fmt.Sprintf("goroutine snapshot: %d goroutine(s) — see build/perf/goroutine.txt", count),
		Issues:  issues,
	}
}

// parsePprofTop extracts top-N lines from `go tool pprof -top` output.
func parsePprofTop(raw, kind string) []string {
	var issues []string
	lines := strings.Split(raw, "\n")
	inTable := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Showing") || strings.HasPrefix(line, "flat") {
			inTable = true
			continue
		}
		if !inTable || line == "" {
			continue
		}
		// Threshold: skip trivial entries (runtime internals)
		if strings.Contains(line, "runtime.") && !strings.Contains(line, "runtime.main") {
			continue
		}
		issues = append(issues, fmt.Sprintf("[pprof-%s] %s", kind, line))
		if len(issues) >= 15 {
			break
		}
	}
	return issues
}

// parseGoroutineCount parses goroutine count from /debug/pprof/goroutine?debug=1 output.
func parseGoroutineCount(raw string) ([]string, int) {
	// Format: "goroutine N [state]:" blocks
	re := regexp.MustCompile(`^goroutine \d+ \[([^\]]+)\]:`)
	stateCount := make(map[string]int)
	total := 0
	for _, line := range strings.Split(raw, "\n") {
		if m := re.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			total++
			stateCount[m[1]]++
		}
	}
	if total == 0 {
		return nil, 0
	}
	var issues []string
	issues = append(issues, fmt.Sprintf("[goroutine] total: %d goroutine(s)", total))
	for state, cnt := range stateCount {
		if cnt > 1 {
			issues = append(issues, fmt.Sprintf("[goroutine] state=%q count=%d", state, cnt))
		}
	}
	return issues, total
}

// ── slow query log ────────────────────────────────────────────────────────────

// SlowQueryEntry is one slow query entry.
type SlowQueryEntry struct {
	QueryTime float64
	LockTime  float64
	RowsSent  int
	RowsExam  int
	SQL       string
}

// RunSlowQueryAnalysis is exported for use by scan.go directly.
func runSlowQueryAnalysis(logPath, outDir string) *Result {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return &Result{
			Level:   LevelSkip,
			Summary: fmt.Sprintf("slow query log not readable: %v", err),
		}
	}

	entries := parseSlowLog(string(data))
	if len(entries) == 0 {
		return Pass("slow query log: no slow queries found")
	}

	// Sort by query time descending (simple selection, avoid importing sort for large slice)
	for i := 0; i < len(entries)-1; i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].QueryTime > entries[i].QueryTime {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}

	// Write full report
	outFile := filepath.Join(outDir, "slow_query.txt")
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "# Query_time=%.3fs  Lock_time=%.3fs  Rows_sent=%d  Rows_examined=%d\n%s\n\n",
			e.QueryTime, e.LockTime, e.RowsSent, e.RowsExam, e.SQL)
	}
	_ = os.WriteFile(outFile, []byte(sb.String()), 0o644)

	// Top 20 for report
	top := entries
	if len(top) > 20 {
		top = top[:20]
	}
	var issues []string
	for i, e := range top {
		sql := e.SQL
		if len(sql) > 120 {
			sql = sql[:120] + "..."
		}
		issues = append(issues, fmt.Sprintf(
			"#%d  query_time=%.3fs rows_examined=%d  %s",
			i+1, e.QueryTime, e.RowsExam, sql))
	}

	return &Result{
		Level:   LevelWarn,
		Summary: fmt.Sprintf("%d slow query(ies) found — top by query_time, see build/perf/slow_query.txt", len(entries)),
		Issues:  issues,
	}
}

// parseSlowLog parses MySQL slow query log text format.
func parseSlowLog(raw string) []SlowQueryEntry {
	var entries []SlowQueryEntry
	lines := strings.Split(raw, "\n")

	var cur SlowQueryEntry
	inQuery := false
	var sqlLines []string

	flush := func() {
		if inQuery && len(sqlLines) > 0 {
			cur.SQL = strings.TrimSpace(strings.Join(sqlLines, " "))
			entries = append(entries, cur)
		}
		cur = SlowQueryEntry{}
		sqlLines = nil
		inQuery = false
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "# Time:") {
			flush()
			continue
		}
		if strings.HasPrefix(line, "# User@Host:") {
			continue
		}
		if strings.HasPrefix(line, "# Query_time:") {
			// # Query_time: 1.234567  Lock_time: 0.000123 Rows_sent: 10  Rows_examined: 50000
			cur.QueryTime = parseFloatField(line, "Query_time:")
			cur.LockTime = parseFloatField(line, "Lock_time:")
			cur.RowsSent = parseIntField(line, "Rows_sent:")
			cur.RowsExam = parseIntField(line, "Rows_examined:")
			inQuery = true
			continue
		}
		if strings.HasPrefix(line, "SET timestamp=") {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if inQuery && strings.TrimSpace(line) != "" {
			sqlLines = append(sqlLines, strings.TrimSpace(line))
		}
	}
	flush()
	return entries
}

func parseFloatField(line, field string) float64 {
	idx := strings.Index(line, field)
	if idx < 0 {
		return 0
	}
	rest := strings.TrimSpace(line[idx+len(field):])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

func parseIntField(line, field string) int {
	idx := strings.Index(line, field)
	if idx < 0 {
		return 0
	}
	rest := strings.TrimSpace(line[idx+len(field):])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.Atoi(fields[0])
	return v
}
