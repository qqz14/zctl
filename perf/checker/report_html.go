package checker

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ── Data structures ───────────────────────────────────────────────────────────

// IssueTab is one tab inside a right-pane panel.
type IssueTab struct {
	ID         string
	Label      string
	Level      Level
	Issues     []string
	Note       string        // optional explanatory note for empty tabs
	InlineHTML template.HTML // optional: render custom HTML instead of issue list
	Count      int           // override badge count (used when InlineHTML is set)
	FixHint    *FixHint      // optional: collapsible fix-suggestion box rendered above issue list
}

// FixHint describes a one-click / scripted fix suggestion for a specific tab.
// Rendered as a collapsible <details> box above the issue list.
type FixHint struct {
	Title   string    // 例如 "💡 一键修复建议：fieldalignment"
	Summary string    // 一行简介，描述该工具能修什么
	Steps   []FixStep // 多步操作（安装/执行/验证…）
	Notes   []string  // 提醒事项，例如 "执行前请先 commit / 不可逆"
}

// FixStep is a single executable step in a FixHint.
type FixStep struct {
	Desc    string // 步骤说明，例如 "安装 fieldalignment 工具"
	Command string // 可一键复制执行的命令；空表示纯说明步骤
}

// NavItem is one clickable item in the left nav (maps to one right-pane panel).
type NavItem struct {
	ID      string
	Label   string
	Level   Level    // worst level across all tabs
	Tabs    []IssueTab
	// Special render modes (mutually exclusive with Tabs):
	InlineHTML template.HTML // N+1 inline content
	IframeURL  string        // full-screen iframe (lint raw)
	FlatIssues []string      // simple flat list (no tabs)
	// Count overrides the nav badge count for items without Tabs/FlatIssues (e.g. IframeURL).
	// Leave 0 to auto-compute from Tabs/FlatIssues.
	Count int
}

// NavGroup is a top-level module in the left nav.
type NavGroup struct {
	Icon  string
	Label string
	Items []NavItem
}

// ReportData is the root template data.
type ReportData struct {
	ProjectName string
	GitBranch   string
	GitHash     string
	GitAuthor   string // last committer name+email
	StartTime   string // e.g. "2026-06-10 14:09:00"
	EndTime     string // e.g. "2026-06-10 14:11:23"
	ScanTime    string // alias for StartTime (used in legacy template spots)
	Elapsed     string // e.g. "2分23秒456毫秒"
	Groups      []NavGroup
}

// FormatElapsed converts a duration to "Xm Ys Zms" Chinese readable format.
// e.g. 143456ms → "2分23秒456毫秒"
// e.g.  23456ms → "23秒456毫秒"
// e.g.    456ms → "456毫秒"
func FormatElapsed(d time.Duration) string {
	total := d.Round(time.Millisecond)
	ms := total.Milliseconds()

	minutes := ms / 60000
	ms -= minutes * 60000
	seconds := ms / 1000
	ms -= seconds * 1000

	switch {
	case minutes > 0:
		return fmt.Sprintf("%d分%d秒%d毫秒", minutes, seconds, ms)
	case seconds > 0:
		return fmt.Sprintf("%d秒%d毫秒", seconds, ms)
	default:
		return fmt.Sprintf("%d毫秒", ms)
	}
}

// ── WriteReportHTML ───────────────────────────────────────────────────────────

func WriteReportHTML(
	outDir, projectDir string,
	results map[string]*Result,
	startTime time.Time,
	elapsed time.Duration,
	dr *DynamicResult,
) {
	n1Findings := lastN1Findings
	lintRes := lastLintResult

	projectName := filepath.Base(projectDir)
	detailsDir := filepath.Join(outDir, "details")
	_ = os.MkdirAll(detailsDir, 0o755)

	for _, fname := range []string{"lint.html", "escape.txt", "vuln.txt", "lint.json", "lint.txt"} {
		src := filepath.Join(outDir, fname)
		if data, err := os.ReadFile(src); err == nil {
			_ = os.WriteFile(filepath.Join(detailsDir, fname), data, 0o644)
			_ = os.Remove(src)
		}
	}

	writeN1HTML(filepath.Join(detailsDir, "n1.html"), n1Findings, projectDir)
	n1Inline := renderN1Inline(n1Findings, projectDir)

	gitBranch := gitOutput(projectDir, "rev-parse", "--abbrev-ref", "HEAD")
	gitHash := gitOutput(projectDir, "rev-parse", "--short", "HEAD")
	// "Name <email>" of the most recent commit author
	gitAuthor := gitOutput(projectDir, "log", "-1", "--format=%an <%ae>")

	coverHTMLPath := filepath.Join(detailsDir, "cover.html")
	groups := buildGroups(results, lintRes, n1Findings, n1Inline, dr, coverHTMLPath)

	data := ReportData{
		ProjectName: projectName,
		GitBranch:   gitBranch,
		GitHash:     gitHash,
		GitAuthor:   gitAuthor,
		StartTime:   startTime.Format("2006-01-02 15:04:05"),
		ScanTime:    startTime.Format("2006-01-02 15:04:05"),
		Elapsed:     FormatElapsed(elapsed),
		Groups:      groups,
	}

	tmpl := template.Must(template.New("report").Funcs(buildFuncMap()).Parse(reportHTMLTemplate))
	f, err := os.Create(filepath.Join(outDir, "report.html"))
	if err != nil {
		return
	}
	defer f.Close()
	_ = tmpl.Execute(f, data)
}

// ── buildGroups: strict mapping to design-doc tree ───────────────────────────

func buildGroups(
	results map[string]*Result,
	lr *LintResult,
	n1Findings []N1Finding,
	n1Inline template.HTML,
	dr *DynamicResult,
	coverHTMLPath string, // absolute path to details/cover.html, "" if not generated
) []NavGroup {
	tabs := map[string][]string{}
	buildFailed := false
	if lr != nil {
		tabs = lr.Tabs
		buildFailed = lr.BuildFailed
	}

	// buildFailedNote is injected into every lint-derived tab's Note when the project
	// failed to compile, making it clear the empty result is NOT "no issues found".
	buildFailedNote := ""
	if buildFailed {
		// Pull the first compile error as context hint
		hint := ""
		if errLines := tabs["build-error"]; len(errLines) > 0 {
			for _, l := range errLines {
				if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
					hint = " (" + l + ")"
					break
				}
			}
		}
		buildFailedNote = "⚠️ 此检查项因编译失败未能运行，结果不可信（不代表无问题）" + hint +
			" — 请修复「Panic 风险 > 编译错误」tab 中列出的编译错误后重新扫描"
	}

	// ════════════════════════════════════════════════════════
	// ⚡ Module 1: 严重错误 (Critical)
	// ════════════════════════════════════════════════════════
	// Item 1.1: Panic 风险
	//   Tab: 强制类型断言   forcetypeassert
	//   Tab: Nil 返回值风险  nilnil
	//   Tab: Nil 指针解引用  staticcheck SA5011 (归 panic)
	panicItem := NavItem{
		ID:    "critical-panic",
		Label: "Panic 风险",
		Tabs: []IssueTab{
			{ID: "panic-typeassert", Label: "强制类型断言",
				Issues: tabs["panic"], // forcetypeassert + nilnil + staticcheck-panic merged
			},
		},
	}
	// Split forcetypeassert / nilnil / staticcheck more granularly from tabs["panic"]
	var typeAssertIssues, nilNilIssues, nilDerefIssues []string
	for _, s := range tabs["panic"] {
		switch {
		case strings.Contains(s, "[forcetypeassert]"):
			typeAssertIssues = append(typeAssertIssues, s)
		case strings.Contains(s, "[nilnil]"):
			nilNilIssues = append(nilNilIssues, s)
		default:
			nilDerefIssues = append(nilDerefIssues, s)
		}
	}
	panicItem.Tabs = []IssueTab{
		{ID: "panic-typeassert", Label: "强制类型断言", Level: levelOf(typeAssertIssues),
			Issues: typeAssertIssues,
			Note:   "a := b.(T) 失败直接 panic，应使用 a, ok := b.(T)"},
		{ID: "panic-nilnil", Label: "Nil 返回值风险", Level: levelOf(nilNilIssues),
			Issues: nilNilIssues,
			Note:   "同时返回 nil error + nil value，调用方无法判断成功/失败"},
		{ID: "panic-nilderef", Label: "Nil 指针解引用", Level: levelOf(nilDerefIssues),
			Issues: nilDerefIssues,
			Note:   "map/slice 未初始化直接访问，或 nil pointer dereference (staticcheck SA5011)"},
	}
	panicItem.Level = worstTabLevel(panicItem.Tabs)

	// Item 1.2: Error 处理缺陷
	var errDropIssues, errSwallowIssues, errWrapIssues, errWastedIssues []string
	for _, s := range tabs["errdrop"] {
		switch {
		case strings.Contains(s, "[errcheck]"):
			errDropIssues = append(errDropIssues, s)
		case strings.Contains(s, "[nilerr]"):
			errSwallowIssues = append(errSwallowIssues, s)
		case strings.Contains(s, "[wrapcheck]"):
			errWrapIssues = append(errWrapIssues, s)
		case strings.Contains(s, "[wastedassign]"):
			errWastedIssues = append(errWastedIssues, s)
		}
	}
	errItem := NavItem{
		ID:    "critical-errdrop",
		Label: "Error 处理缺陷",
		Tabs: []IssueTab{
			{ID: "err-drop", Label: "Error 被丢弃", Level: levelOf(errDropIssues),
				Issues: errDropIssues,
				Note:   "_ = func() 丢弃 error，DB 写操作失败时静默无响应"},
			{ID: "err-swallow", Label: "Error 被吞掉", Level: levelOf(errSwallowIssues),
				Issues: errSwallowIssues,
				Note:   "检查了 err != nil 但 return nil，错误被静默吞掉"},
			{ID: "err-wrap", Label: "Error 未包装", Level: levelOf(errWrapIssues),
				Issues: errWrapIssues,
				Note:   "外部包返回的 error 未 wrap，跨层传递时丢失上下文"},
			{ID: "err-wasted", Label: "无效赋值", Level: levelOf(errWastedIssues),
				Issues: errWastedIssues,
				Note:   "赋值后从未使用，可能掩盖逻辑错误"},
		},
	}
	errItem.Level = worstTabLevel(errItem.Tabs)

	// If build failed, append a "编译错误" tab inside panicItem so the user sees
	// compile errors in context — not as a separate mysterious panel.
	if buildFailed {
		panicItem.Tabs = append(panicItem.Tabs, IssueTab{
			ID:     "panic-build-error",
			Label:  "编译错误",
			Level:  LevelFail,
			Issues: tabs["build-error"],
			Note:   "以下包编译失败，导致 golangci-lint 所有 linter 跳过。修复后重新扫描",
		})
		panicItem.Level = worstTabLevel(panicItem.Tabs)
	}

	criticalGroup := NavGroup{
		Icon:  "⚡",
		Label: "严重错误",
		Items: []NavItem{panicItem, errItem},
	}

	// ════════════════════════════════════════════════════════
	// 🐛 Module 2: Bug / 代码缺陷
	// ════════════════════════════════════════════════════════
	// Item 2.1: 资源泄漏
	var leakBodyIssues, leakRowsIssues, leakRowsErrIssues []string
	for _, s := range tabs["leak"] {
		switch {
		case strings.Contains(s, "[bodyclose]"):
			leakBodyIssues = append(leakBodyIssues, s)
		case strings.Contains(s, "[sqlclosecheck]"):
			leakRowsIssues = append(leakRowsIssues, s)
		case strings.Contains(s, "[rowserrcheck]"):
			leakRowsErrIssues = append(leakRowsErrIssues, s)
		}
	}
	leakItem := NavItem{
		ID:    "bug-leak",
		Label: "资源泄漏",
		Tabs: []IssueTab{
			{ID: "leak-body", Label: "HTTP Body 未关闭", Level: levelOf(leakBodyIssues),
				Issues: leakBodyIssues, Note: "缺少 defer resp.Body.Close() → HTTP 连接池耗尽"},
			{ID: "leak-rows", Label: "DB Rows 未关闭", Level: levelOf(leakRowsIssues),
				Issues: leakRowsIssues, Note: "缺少 defer rows.Close() → DB 连接池耗尽"},
			{ID: "leak-rowserr", Label: "rows.Err 未检查", Level: levelOf(leakRowsErrIssues),
				Issues: leakRowsErrIssues, Note: "遍历结束后未检查 rows.Err() → 静默丢失 DB 遍历错误"},
		},
	}
	leakItem.Level = worstTabLevel(leakItem.Tabs)

	// Item 2.2: 并发安全
	var concCtxIssues, concNoctxIssues, concLoopVarIssues []string
	for _, s := range tabs["concurrency"] {
		switch {
		case strings.Contains(s, "[contextcheck]"):
			concCtxIssues = append(concCtxIssues, s)
		case strings.Contains(s, "[noctx]"):
			concNoctxIssues = append(concNoctxIssues, s)
		case strings.Contains(s, "[copyloopvar]"):
			concLoopVarIssues = append(concLoopVarIssues, s)
		}
	}
	// WaitGroup 误用：govet 部分覆盖，加说明
	wgNote := "WaitGroup.Add() 应在 go 语句前调用（当前无独立 linter，govet 检测锁拷贝等）"
	concItem := NavItem{
		ID:    "bug-concurrency",
		Label: "并发安全",
		Tabs: []IssueTab{
			{ID: "conc-ctx", Label: "Context 断链", Level: levelOf(concCtxIssues),
				Issues: concCtxIssues, Note: "goroutine 使用了非继承的 context，无法被取消"},
			{ID: "conc-noctx", Label: "HTTP 无 Context", Level: levelOf(concNoctxIssues),
				Issues: concNoctxIssues, Note: "http.NewRequest 未传 context → 请求无法超时/取消"},
			{ID: "conc-loopvar", Label: "循环变量捕获", Level: levelOf(concLoopVarIssues),
				Issues: concLoopVarIssues, Note: "go func()/闭包捕获了 for 循环迭代变量（Go < 1.22）"},
			{ID: "conc-waitgroup", Label: "WaitGroup 误用", Level: LevelInfo,
				Issues: nil, Note: wgNote},
		},
	}
	concItem.Level = worstTabLevel(concItem.Tabs)

	// Item 2.3: 其他缺陷
	var defectExhIssues, defectUnparamIssues, defectVetIssues []string
	for _, s := range tabs["defect"] {
		switch {
		case strings.Contains(s, "[exhaustive]"):
			defectExhIssues = append(defectExhIssues, s)
		case strings.Contains(s, "[unparam]"):
			defectUnparamIssues = append(defectUnparamIssues, s)
		default: // govet
			defectVetIssues = append(defectVetIssues, s)
		}
	}
	// Note: RunVet output is shown separately in "代码规范 → go vet".
	// We intentionally don't merge it here to avoid duplicating issues.

	defectItem := NavItem{
		ID:    "bug-defect",
		Label: "其他缺陷",
		Tabs: []IssueTab{
			{ID: "defect-enum", Label: "Enum Switch 非穷举", Level: levelOf(defectExhIssues),
				Issues: defectExhIssues, Note: "新增枚举值后 switch 分支未更新"},
			{ID: "defect-unparam", Label: "未使用参数", Level: levelOf(defectUnparamIssues),
				Issues: defectUnparamIssues, Note: "函数参数声明但从未使用"},
			{ID: "defect-vet", Label: "静态检查 (govet)", Level: levelOf(defectVetIssues),
				Issues: defectVetIssues, Note: "锁值传递 / printf 格式错误 / 不可达代码"},
		},
	}
	defectItem.Level = worstTabLevel(defectItem.Tabs)

	bugGroup := NavGroup{
		Icon:  "🐛",
		Label: "Bug / 代码缺陷",
		Items: []NavItem{leakItem, concItem, defectItem},
	}

	// ════════════════════════════════════════════════════════
	// 🐌 Module 3: 性能问题 (Performance) — standalone module
	// ════════════════════════════════════════════════════════
	// Item 3.1: N+1 数据库查询 (inline HTML with tabs)
	var n1ConfirmedIssues, n1ReviewIssues []string
	for _, f := range n1Findings {
		if f.Level == LevelFail {
			n1ConfirmedIssues = append(n1ConfirmedIssues, fmt.Sprintf(
				"%s:%d→%d: [N+1] %s.%s() → %s",
				f.ShortFile, f.LoopLine, f.CallLine, f.RecvText, f.MethodName, f.EntTerminal))
		} else if !isNoiseFinding(f) {
			n1ReviewIssues = append(n1ReviewIssues, fmt.Sprintf(
				"%s:%d→%d: [cross-pkg] %s.%s() — 非DB，人工确认",
				f.ShortFile, f.LoopLine, f.CallLine, f.RecvText, f.MethodName))
		}
	}
	n1Item := NavItem{
		ID:         "perf-n1",
		Label:      "N+1 数据库查询",
		Level:      results["n1"].safeLevel(),
		InlineHTML: n1Inline,
		// Tabs for the header bar (content rendered by InlineHTML's own tabs)
		Tabs: []IssueTab{
			{ID: "n1-confirmed", Label: "确认 N+1", Level: levelOf(n1ConfirmedIssues), Issues: n1ConfirmedIssues,
				Note: "循环内调用链经 impl AST 追踪确认触达 ent 终端方法（All/First/Only/Count/Save 等）"},
			{ID: "n1-review", Label: "人工审核", Level: LevelInfo, Issues: n1ReviewIssues,
				Note: "循环内跨包调用，impl 未发现 ent 终端，可能是缓存/RPC，需人工确认"},
		},
	}

	// Item 3.2: 代码层性能
	var perfSliceIssues, perfCtxIssues, perfHugeIssues, perfSprintIssues, perfSelectIssues []string
	for _, s := range tabs["perf-code"] {
		switch {
		case strings.Contains(s, "[prealloc]"):
			perfSliceIssues = append(perfSliceIssues, s)
		case strings.Contains(s, "[fatcontext]"):
			perfCtxIssues = append(perfCtxIssues, s)
		case strings.Contains(s, "[gocritic]"):
			perfHugeIssues = append(perfHugeIssues, s)
		case strings.Contains(s, "[perfsprint]"):
			perfSprintIssues = append(perfSprintIssues, s)
		case strings.Contains(s, "[unqueryvet]"):
			perfSelectIssues = append(perfSelectIssues, s)
		}
	}
	perfCodeItem := NavItem{
		ID:    "perf-code",
		Label: "代码层性能",
		Tabs: []IssueTab{
			{ID: "perf-slice", Label: "Slice 未预分配", Level: levelOf(perfSliceIssues),
				Issues: perfSliceIssues, Note: "循环内反复 append 触发 slice 多次扩容重分配"},
			{ID: "perf-ctx", Label: "Context 嵌套", Level: levelOf(perfCtxIssues),
				Issues: perfCtxIssues, Note: "循环内 context.WithValue → 嵌套 context 造成内存压力"},
			{ID: "perf-huge", Label: "大结构体值传递", Level: levelOf(perfHugeIssues),
				Issues: perfHugeIssues, Note: "大结构体应传指针而非值，避免栈拷贝开销"},
			{ID: "perf-sprint", Label: "Sprintf 低效", Level: levelOf(perfSprintIssues),
				Issues: perfSprintIssues, Note: "简单拼接用 + 或 strings.Builder，避免 fmt.Sprintf 格式解析"},
			{ID: "perf-select", Label: "SELECT * 查询", Level: levelOf(perfSelectIssues),
				Issues: perfSelectIssues, Note: "全列查询浪费带宽且妨碍索引覆盖扫描优化"},
		},
	}
	perfCodeItem.Level = worstTabLevel(perfCodeItem.Tabs)

	// Item 3.3: SQL 性能 — derived from logic review IONode SQL strings (not file scanning)
	sqlPerfLevel := results["sql-perf"].safeLevel()
	sqlPerfInline := renderSQLPerfFromFindings(lastSQLPerfWarn, lastSQLPerfInfo)
	var sqlNoLimitWarn, sqlNoLimitInfo []string
	for _, f := range lastSQLPerfWarn {
		sqlNoLimitWarn = append(sqlNoLimitWarn, fmt.Sprintf("%s:%d [%s] %s", f.File, f.Line, f.DAO, f.SQL))
	}
	for _, f := range lastSQLPerfInfo {
		sqlNoLimitInfo = append(sqlNoLimitInfo, fmt.Sprintf("%s:%d [%s] %s", f.File, f.Line, f.DAO, f.SQL))
	}
	sqlPerfItem := NavItem{
		ID:         "perf-sql",
		Label:      "SQL 性能",
		Level:      sqlPerfLevel,
		InlineHTML: sqlPerfInline,
		Tabs: []IssueTab{
			{ID: "sql-nolimit", Label: "无分页全表", Level: levelOf(sqlNoLimitWarn), Issues: sqlNoLimitWarn},
			{ID: "sql-bounded", Label: "有WHERE无Limit", Level: LevelInfo, Issues: sqlNoLimitInfo},
		},
	}

	// Item 3.4: 内存逃逸热点
	escapeItem := NavItem{
		ID:         "perf-escape",
		Label:      "内存逃逸热点",
		Level:      results["escape"].safeLevel(),
		FlatIssues: results["escape"].safeIssues(),
	}

	perfGroup := NavGroup{
		Icon:  "🐌",
		Label: "性能问题",
		Items: []NavItem{n1Item, perfCodeItem, sqlPerfItem, escapeItem},
	}

	// ════════════════════════════════════════════════════════
	// 🔒 Module 4: 安全 (Security) — standalone module
	// ════════════════════════════════════════════════════════
	// Item 4.1: 代码安全 (gosec sub-items)
	var secSQLIssues, secRandIssues, secHardcodeIssues, secFilePermIssues, secOtherIssues []string
	for _, s := range tabs["code-security"] {
		lower := strings.ToLower(s)
		switch {
		case strings.Contains(lower, "g201") || strings.Contains(lower, "g202") ||
			strings.Contains(lower, "sql") || strings.Contains(lower, "inject"):
			secSQLIssues = append(secSQLIssues, s)
		case strings.Contains(lower, "g401") || strings.Contains(lower, "g404") ||
			strings.Contains(lower, "rand") || strings.Contains(lower, "weak"):
			secRandIssues = append(secRandIssues, s)
		case strings.Contains(lower, "g101") || strings.Contains(lower, "hardcod") ||
			strings.Contains(lower, "credential") || strings.Contains(lower, "password"):
			secHardcodeIssues = append(secHardcodeIssues, s)
		case strings.Contains(lower, "g306") || strings.Contains(lower, "permission") ||
			strings.Contains(lower, "chmod"):
			secFilePermIssues = append(secFilePermIssues, s)
		default:
			secOtherIssues = append(secOtherIssues, s)
		}
	}
	codeSecItem := NavItem{
		ID:    "sec-code",
		Label: "代码安全",
		Tabs: []IssueTab{
			{ID: "sec-sql", Label: "SQL 注入", Level: levelOf(secSQLIssues),
				Issues: secSQLIssues, Note: "fmt.Sprintf 拼接 SQL → G201/G202"},
			{ID: "sec-rand", Label: "弱随机数", Level: levelOf(secRandIssues),
				Issues: secRandIssues, Note: "math/rand 用于安全场景应换 crypto/rand → G401/G404"},
			{ID: "sec-hardcode", Label: "硬编码密钥", Level: levelOf(secHardcodeIssues),
				Issues: secHardcodeIssues, Note: "密码/密钥写死在代码里 → G101"},
			{ID: "sec-perm", Label: "不安全文件权限", Level: levelOf(secFilePermIssues),
				Issues: secFilePermIssues, Note: "文件权限过宽（如 0777）→ G306"},
			{ID: "sec-other", Label: "其他安全", Level: levelOf(secOtherIssues),
				Issues: secOtherIssues, Note: "路径遍历 (G304)、命令注入 (G204)、SSRF 等其他 gosec 问题"},
		},
	}
	codeSecItem.Level = worstTabLevel(codeSecItem.Tabs)

	// Item 4.2: 供应链安全 (govulncheck)
	supplyItem := NavItem{
		ID:         "sec-supply",
		Label:      "供应链安全",
		Level:      results["vuln"].safeLevel(),
		FlatIssues: results["vuln"].safeIssues(),
	}

	// Item 4.3: 代码注入风险 (bidichk)
	trojanItem := NavItem{
		ID:    "sec-trojan",
		Label: "代码注入风险",
		Level: levelOf(tabs["trojan"]),
		Tabs: []IssueTab{
			{ID: "trojan-bidi", Label: "Trojan Source", Level: levelOf(tabs["trojan"]),
				Issues: tabs["trojan"],
				Note:   "危险 Unicode 双向字符可使代码视觉与实际执行不符（CVE-2021-42574）"},
		},
	}

	secGroup := NavGroup{
		Icon:  "🔒",
		Label: "安全",
		Items: []NavItem{codeSecItem, supplyItem, trojanItem},
	}

	// ════════════════════════════════════════════════════════
	// 📐 Module 5: 代码规范 (Code Quality)
	// ════════════════════════════════════════════════════════
	// Item 5.1: 复杂度
	var cycloIssues, cognitIssues, funlenIssues, nestifIssues, maintidxIssues []string
	for _, s := range tabs["complexity"] {
		switch {
		case strings.Contains(s, "[gocyclo]") || strings.Contains(s, "[cyclop]"):
			cycloIssues = append(cycloIssues, s)
		case strings.Contains(s, "[gocognit]"):
			cognitIssues = append(cognitIssues, s)
		case strings.Contains(s, "[funlen]"):
			funlenIssues = append(funlenIssues, s)
		case strings.Contains(s, "[nestif]"):
			nestifIssues = append(nestifIssues, s)
		case strings.Contains(s, "[maintidx]"):
			maintidxIssues = append(maintidxIssues, s)
		}
	}
	complexItem := NavItem{
		ID:    "quality-complexity",
		Label: "复杂度",
		Tabs: []IssueTab{
			{ID: "cx-cyclo", Label: "圈复杂度 (≤10)", Level: levelOf(cycloIssues),
				Issues: cycloIssues, Note: "函数分支路径数量，超过 10 难以测试"},
			{ID: "cx-cognit", Label: "认知复杂度 (≤15)", Level: levelOf(cognitIssues),
				Issues: cognitIssues, Note: "可读性复杂度，超过 15 难以理解"},
			{ID: "cx-funlen", Label: "函数行数 (≤200)", Level: levelOf(funlenIssues),
				Issues: funlenIssues, Note: "函数超过 200 行应拆分"},
			{ID: "cx-nestif", Label: "嵌套深度 (≤5)", Level: levelOf(nestifIssues),
				Issues: nestifIssues, Note: "if 嵌套超过 5 层，建议提前 return 或拆函数"},
			{ID: "cx-maintidx", Label: "可维护性指数 (≥20)", Level: levelOf(maintidxIssues),
				Issues: maintidxIssues, Note: "综合复杂度+行数+注释率，100=最佳，低于 20 需重构"},
		},
	}
	complexItem.Level = worstTabLevel(complexItem.Tabs)

	// Item 5.2: 代码风格
	var styleFmtIssues, styleMndIssues, styleConstIssues, styleLLLIssues, styleSpellIssues, styleTodoIssues []string
	styleFmtIssues = results["fmt"].safeIssues()
	for _, s := range tabs["style"] {
		switch {
		case strings.Contains(s, "[mnd]"):
			styleMndIssues = append(styleMndIssues, s)
		case strings.Contains(s, "[goconst]"):
			styleConstIssues = append(styleConstIssues, s)
		case strings.Contains(s, "[lll]"):
			styleLLLIssues = append(styleLLLIssues, s)
		case strings.Contains(s, "[misspell]"):
			styleSpellIssues = append(styleSpellIssues, s)
		case strings.Contains(s, "[godox]"):
			styleTodoIssues = append(styleTodoIssues, s)
		}
	}
	styleItem := NavItem{
		ID:    "quality-style",
		Label: "代码风格",
		Tabs: []IssueTab{
			{ID: "sty-fmt", Label: "代码格式化", Level: levelOf(styleFmtIssues),
				Issues: styleFmtIssues, Note: "gofmt 未格式化，运行 gofmt -w . 修复"},
			{ID: "sty-mnd", Label: "魔法数字", Level: levelOf(styleMndIssues),
				Issues: styleMndIssues, Note: "未命名的数字字面量，应定义为常量"},
			{ID: "sty-const", Label: "重复字符串", Level: levelOf(styleConstIssues),
				Issues: styleConstIssues, Note: "同一字符串出现 3+ 次，应定义为常量"},
			{ID: "sty-lll", Label: "行长度 (≤120)", Level: levelOf(styleLLLIssues),
				Issues: styleLLLIssues, Note: "代码行超过 120 字符，影响可读性"},
			{ID: "sty-spell", Label: "拼写错误", Level: levelOf(styleSpellIssues),
				Issues: styleSpellIssues, Note: "英文注释拼写错误"},
			{ID: "sty-todo", Label: "TODO/FIXME 残留", Level: levelOf(styleTodoIssues),
				Issues: styleTodoIssues, Note: "遗留的 TODO/FIXME 注释"},
		},
	}
	styleItem.Level = worstTabLevel(styleItem.Tabs)

	// Item 5.3: 测试质量 (覆盖率汇总表格 + Testify 规范)
	var testTestifyIssues []string
	for _, s := range tabs["testing"] {
		if strings.Contains(s, "[testifylint]") {
			testTestifyIssues = append(testTestifyIssues, s)
		}
	}
	// Overall coverage pct from result summary
	var overallPct float64
	coverLevel := LevelSkip
	if r := results["test"]; r != nil {
		coverLevel = r.Level
		// Extract pct from summary string "coverage XX.X% ..."
		if idx := strings.Index(r.Summary, "coverage "); idx >= 0 {
			parts := strings.Fields(r.Summary[idx+9:])
			if len(parts) > 0 {
				pctStr := strings.TrimSuffix(parts[0], "%")
				if v, err := strconv.ParseFloat(pctStr, 64); err == nil {
					overallPct = v
				}
			}
		}
	}
	// Check if cover.html exists for the "view detail" link
	coverHTMLExists := coverHTMLPath != "" && fileExists(coverHTMLPath)
	// Parse file anchors from cover.html for per-package deep links
	var pkgAnchors map[string]string
	if coverHTMLExists {
		pkgAnchors = parseCoverHTMLAnchors(coverHTMLPath)
	}
	// Build coverage table inline HTML
	coverTableHTML := renderCoverTable(lastCoverPkgs, overallPct, coverHTMLExists, pkgAnchors)

	// Count = total file count across all packages for the coverage tab badge
	coverFileCount := 0
	for _, p := range lastCoverPkgs {
		coverFileCount += p.Files
	}

	testItem := NavItem{
		ID:    "quality-testing",
		Label: "测试质量",
		Tabs: []IssueTab{
			{ID: "test-cover", Label: "覆盖率汇总", Level: coverLevel,
				InlineHTML: coverTableHTML, Count: coverFileCount},
			{ID: "test-testify", Label: "Testify 规范", Level: levelOf(testTestifyIssues),
				Issues: testTestifyIssues, Note: "testify 断言写法错误，如 assert.Equal(t, nil, err) 应改为 assert.NoError"},
		},
	}
	testItem.Level = worstTabLevel(testItem.Tabs)

	// Item 5.4: go vet (raw) + lint raw report
	vetRawItem := NavItem{
		ID:         "quality-vet-raw",
		Label:      "go vet",
		Level:      results["vet"].safeLevel(),
		FlatIssues: results["vet"].safeIssues(),
	}
	lintRawItem := NavItem{
		ID:        "quality-lint-raw",
		Label:     "Lint 原始报告",
		Level:     results["lint"].safeLevel(),
		IframeURL: "details/lint.html",
		Count:     len(results["lint"].safeIssues()),
	}

	qualityGroup := NavGroup{
		Icon:  "📐",
		Label: "代码规范",
		Items: []NavItem{complexItem, styleItem, testItem, vetRawItem, lintRawItem},
	}

	// ════════════════════════════════════════════════════════
	// 🔬 Module 6: 动态分析 (Dynamic) — only when --dynamic flag is set
	// ════════════════════════════════════════════════════════
	var dynamicGroups []NavGroup
	if dr != nil {
		pprofCPUIssues := dr.CPU.safeIssues()
		pprofHeapIssues := dr.Heap.safeIssues()
		goroutineIssues := dr.Goroutine.safeIssues()
		slowQueryIssues := dr.SlowQuery.safeIssues()

		pprofItem := NavItem{
			ID:    "dynamic-pprof",
			Label: "pprof 热点",
			Tabs: []IssueTab{
				{ID: "pprof-cpu", Label: "CPU 热点", Level: dr.CPU.safeLevel(),
					Issues: pprofCPUIssues,
					Note:   "go tool pprof -top，排除 runtime 内部调用。数值越大说明该函数消耗 CPU 越多"},
				{ID: "pprof-heap", Label: "堆内存热点", Level: dr.Heap.safeLevel(),
					Issues: pprofHeapIssues,
					Note:   "堆内存分配 top-N。关注 alloc_space 大的函数，考虑对象复用或减少逃逸"},
				{ID: "pprof-goroutine", Label: "Goroutine 快照", Level: dr.Goroutine.safeLevel(),
					Issues: goroutineIssues,
					Note:   "当前所有 goroutine 状态统计。大量 IO wait 说明下游慢；大量 chan receive 检查有无泄漏"},
			},
		}
		pprofItem.Level = worstTabLevel(pprofItem.Tabs)

		slowQueryItem := NavItem{
			ID:    "dynamic-slow",
			Label: "慢查询",
			Tabs: []IssueTab{
				{ID: "slow-top", Label: "Top 慢 SQL", Level: dr.SlowQuery.safeLevel(),
					Issues: slowQueryIssues,
					Note:   "按 Query_time 降序排列。重点关注 Rows_examined 远大于 Rows_sent 的查询（全表扫），加索引或改写 SQL"},
			},
		}
		slowQueryItem.Level = dr.SlowQuery.safeLevel()

		dynamicGroups = append(dynamicGroups, NavGroup{
			Icon:  "🔬",
			Label: "动态分析",
			Items: []NavItem{pprofItem, slowQueryItem},
		})
	}

	// ════════════════════════════════════════════════════════
	// 🔍 Module 6: 代码逻辑审查 (Logic Review) — standalone module
	// No level badge, no count — this is a code review aid, not a problem list.
	// ════════════════════════════════════════════════════════
	logicReviewInline := renderLogicReviewInline(lastLogicReviewResult)
	logicReviewItem := NavItem{
		ID:         "review-logic",
		Label:      "存储调用分析",
		Level:      LevelInfo,
		InlineHTML: logicReviewInline,
		Count:      0, // no count in nav badge
	}
	logicReviewGroup := NavGroup{
		Icon:  "🔍",
		Label: "代码逻辑审查",
		Items: []NavItem{logicReviewItem},
	}

	all := []NavGroup{criticalGroup, bugGroup, perfGroup, secGroup, qualityGroup, logicReviewGroup}
	all = append(all, dynamicGroups...)

	// Post-process: when lint build failed, inject a visible warning entry into every
	// lint-derived tab that has no real issues — so the tab clearly shows WHY it's
	// empty instead of a misleading "✅ No issues found".
	//
	// We also leave the original Note intact (it explains what the tab normally checks).
	// The injected entry uses a "⚠️" prefix so issueClass() renders it as warn style.
	if buildFailed && buildFailedNote != "" {
		// These tab IDs are purely populated by golangci-lint; they go empty on build fail.
		lintOnlyTabs := map[string]bool{
			"panic-typeassert": true, "panic-nilnil": true, "panic-nilderef": true,
			"err-drop": true, "err-swallow": true, "err-wrap": true, "err-wasted": true,
			"leak-body": true, "leak-rows": true, "leak-rowserr": true,
			"conc-ctx": true, "conc-noctx": true, "conc-loopvar": true,
			"defect-enum": true, "defect-unparam": true, "defect-vet": true,
			"perf-slice": true, "perf-ctx": true, "perf-huge": true, "perf-sprint": true, "perf-select": true,
			"sec-sql": true, "sec-rand": true, "sec-hardcode": true, "sec-perm": true, "sec-other": true,
			"trojan-bidi": true,
			"cx-cyclo": true, "cx-cognit": true, "cx-funlen": true, "cx-nestif": true, "cx-maintidx": true,
			"sty-fmt": true, "sty-mnd": true, "sty-const": true, "sty-lll": true, "sty-spell": true, "sty-todo": true,
			"test-testify": true,
		}
		for gi := range all {
			for ii := range all[gi].Items {
				changed := false
				for ti := range all[gi].Items[ii].Tabs {
					tab := &all[gi].Items[ii].Tabs[ti]
					if !lintOnlyTabs[tab.ID] || tab.InlineHTML != "" {
						continue
					}
					if len(tab.Issues) == 0 {
						tab.Issues = []string{buildFailedNote}
						if tab.Level == LevelPass {
							tab.Level = LevelWarn
							changed = true
						}
					}
				}
				// Re-compute item level so nav badge reflects the injected warn
				if changed {
					all[gi].Items[ii].Level = worstTabLevel(all[gi].Items[ii].Tabs)
				}
			}
		}
	}

	// ── Inject FixHints for static-check tabs ─────────────────────────────
	// Only attach hints when the tab actually has issues — avoid distracting
	// the user with fix instructions on clean tabs.
	for gi := range all {
		for ii := range all[gi].Items {
			for ti := range all[gi].Items[ii].Tabs {
				tab := &all[gi].Items[ii].Tabs[ti]
				if tab.InlineHTML != "" || len(tab.Issues) == 0 {
					continue
				}
				if h := fixHintFor(tab.ID); h != nil {
					tab.FixHint = h
				}
			}
		}
	}

	return all
}

// ── helpers ───────────────────────────────────────────────────────────────────

func levelOf(issues []string) Level {
	if len(issues) == 0 {
		return LevelPass
	}
	return LevelWarn
}

func worstTabLevel(tabs []IssueTab) Level {
	worst := LevelPass
	order := map[Level]int{LevelPass: 0, LevelSkip: 0, LevelInfo: 1, LevelWarn: 2, LevelFail: 3, LevelPanic: 4}
	for _, t := range tabs {
		if order[t.Level] > order[worst] {
			worst = t.Level
		}
	}
	return worst
}

func worstLevel(levels ...Level) Level {
	worst := LevelPass
	order := map[Level]int{LevelPass: 0, LevelSkip: 0, LevelInfo: 1, LevelWarn: 2, LevelFail: 3, LevelPanic: 4}
	for _, l := range levels {
		if order[l] > order[worst] {
			worst = l
		}
	}
	return worst
}

func (r *Result) safeSummary() string {
	if r == nil {
		return "skipped"
	}
	return r.Summary
}

func levelClass(l Level) string {
	switch l {
	case LevelFail:
		return "fail"
	case LevelWarn:
		return "warn"
	case LevelPass:
		return "pass"
	case LevelInfo:
		return "info"
	case LevelPanic:
		return "panic"
	default:
		return "skip"
	}
}

func levelIcon(l Level) string {
	switch l {
	case LevelFail:
		return "❌"
	case LevelWarn:
		return "⚠️"
	case LevelPass:
		return "✅"
	case LevelInfo:
		return "ℹ️"
	case LevelPanic:
		return "💥"
	default:
		return "⊘"
	}
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func renderN1Inline(findings []N1Finding, projectDir string) template.HTML {
	var confirmed, info []N1Finding
	for _, f := range findings {
		if f.Level == LevelFail {
			confirmed = append(confirmed, f)
		} else if !isNoiseFinding(f) {
			info = append(info, f)
		}
	}
	data := n1HTMLData{
		ProjectName: filepath.Base(projectDir),
		Confirmed:   confirmed,
		Info:        info,
		Inline:      true,
	}
	tmpl := template.Must(template.New("n1full").Funcs(template.FuncMap{
		"inc": func(i int) int { return i + 1 },
		"add": func(a, b int) int { return a + b },
		"not": func(v interface{}) bool {
			if v == nil {
				return true
			}
			if x, ok := v.([]N1Finding); ok {
				return len(x) == 0
			}
			return false
		},
	}).Parse(n1HTMLTemplate))
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return template.HTML(fmt.Sprintf("<pre>N+1 render error: %v</pre>", err))
	}
	full := buf.String()
	style := ""
	if s := extractBetween(full, "<style>", "</style>"); s != "" {
		// Scope all selectors to .n1-wrap to prevent leaking generic class
		// names (.tab-panel, .tab-btn, .card, ...) into the parent report.
		style = "<style>" + scopeCSS(s, ".n1-wrap") + "</style>\n"
	}
	return template.HTML(style + extractBetween(full, "<body>", "</body>"))
}

// scopeCSS rewrites every selector in src so that it only matches descendants
// of `prefix`. It splits on top-level "}" (rule boundaries) and on "," inside
// the selector list. Selectors targeting the universal selector "*", "html"
// or "body" are dropped because they would otherwise affect the whole page.
func scopeCSS(src, prefix string) string {
	var out strings.Builder
	depth := 0
	start := 0
	rules := []string{}
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				rules = append(rules, src[start:i+1])
				start = i + 1
			}
		}
	}
	if start < len(src) {
		// trailing whitespace / comments — preserve as-is
		out.WriteString(src[start:])
	}
	for _, rule := range rules {
		brace := strings.Index(rule, "{")
		if brace < 0 {
			out.WriteString(rule)
			continue
		}
		selectorPart := strings.TrimSpace(rule[:brace])
		body := rule[brace:]
		// Keep at-rules (@media, @keyframes, ...) intact — they wrap nested
		// rules which our N+1 template does not use, so a flat copy is fine.
		if strings.HasPrefix(selectorPart, "@") {
			out.WriteString(rule)
			continue
		}
		var newSelectors []string
		for _, sel := range strings.Split(selectorPart, ",") {
			s := strings.TrimSpace(sel)
			if s == "" {
				continue
			}
			// Drop selectors that would leak to the global page.
			if s == "*" || s == "html" || s == "body" || s == "html,body" {
				continue
			}
			newSelectors = append(newSelectors, prefix+" "+s)
		}
		if len(newSelectors) == 0 {
			continue
		}
		out.WriteString(strings.Join(newSelectors, ","))
		out.WriteString(body)
	}
	return out.String()
}

func extractBetween(s, open, close string) string {
	start := strings.Index(s, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.LastIndex(s, close)
	if end < start {
		return ""
	}
	return s[start:end]
}

func makeSection(id, title string, r *Result) NavItem {
	return NavItem{
		ID:         id,
		Label:      title,
		Level:      r.safeLevel(),
		FlatIssues: r.safeIssues(),
	}
}

// ── SQL Perf inline renderer ──────────────────────────────────────────────────

func renderSQLPerfInline(warnIssues, infoIssues []string) template.HTML {
	if len(warnIssues)+len(infoIssues) == 0 {
		return template.HTML(`<div style="padding:32px;text-align:center;color:#060;font-size:.95rem">✅ 未发现无分页全表扫描</div>`)
	}
	var sb strings.Builder
	sb.WriteString(`<style>
.sp-tabs{display:flex;border-bottom:2px solid #ddd;margin-bottom:20px}
.sp-tab-btn{padding:10px 24px;font-size:.93rem;font-weight:600;cursor:pointer;border:none;background:none;color:#888;border-bottom:3px solid transparent;margin-bottom:-2px;transition:all .15s}
.sp-tab-btn:hover{color:#333}
.sp-tab-btn.on-warn{color:#9a6000;border-bottom-color:#c80}
.sp-tab-btn.on-info{color:#0055aa;border-bottom-color:#0055aa}
.sp-panel{display:none;padding:4px 0}
.sp-panel.active{display:block}
.sp-card{background:#fff;border-radius:8px;border:1px solid #e0e0e0;margin-bottom:14px;overflow:hidden}
.sp-card-warn{border-left:4px solid #e08000}
.sp-card-info{border-left:4px solid #0066cc}
.sp-hdr{padding:10px 16px;background:#fafafa;border-bottom:1px solid #eee;font-size:.86rem;font-family:monospace;color:#444}
.sp-body{padding:12px 16px;font-size:.84rem}
.sp-sql{background:#1e1e1e;border-radius:5px;padding:8px 12px;font-family:monospace;color:#d4d4d4;font-size:.82rem;white-space:pre;overflow-x:auto;margin-top:6px}
.sp-empty{color:#666;padding:24px;text-align:center}
</style>
`)
	sb.WriteString(`<div class="sp-tabs" id="sp-tabs">`)
	firstWarnCls := ""
	firstInfoCls := ""
	if len(warnIssues) > 0 {
		firstWarnCls = " on-warn"
	} else {
		firstInfoCls = " on-info"
	}
	fmt.Fprintf(&sb, `<button class="sp-tab-btn%s" onclick="spTab('sp-nolimit',this,'on-warn')">⚠️ 无分页全表 (%d)</button>`, firstWarnCls, len(warnIssues))
	fmt.Fprintf(&sb, `<button class="sp-tab-btn%s" onclick="spTab('sp-bounded',this,'on-info')">ℹ️ 有WHERE无Limit (%d)</button>`, firstInfoCls, len(infoIssues))
	sb.WriteString(`</div>`)

	activeWarn := ""
	if len(warnIssues) > 0 {
		activeWarn = " active"
	}
	activeInfo := ""
	if len(warnIssues) == 0 {
		activeInfo = " active"
	}

	fmt.Fprintf(&sb, `<div id="sp-nolimit" class="sp-panel%s">`, activeWarn)
	if len(warnIssues) == 0 {
		sb.WriteString(`<div class="empty-pass">✅ No issues found</div>`)
	}
	for _, s := range warnIssues {
		loc, hint := splitSQLPerfIssue(s)
		fmt.Fprintf(&sb, `<div class="sp-card sp-card-warn"><div class="sp-hdr">⚠️ %s</div><div class="sp-body">无 .Limit() 且无 WHERE 约束，可能全表扫描<div class="sp-sql">%s</div></div></div>`, loc, htmlEsc(hint))
	}
	sb.WriteString(`</div>`)

	fmt.Fprintf(&sb, `<div id="sp-bounded" class="sp-panel%s">`, activeInfo)
	sb.WriteString(`<p style="font-size:.83rem;color:#666;margin-bottom:12px">有 WHERE 条件但无 .Limit()，结果集有界时可接受，否则建议加 .Limit() 或分页</p>`)
	if len(infoIssues) == 0 {
		sb.WriteString(`<div class="empty-pass">✅ No issues found</div>`)
	}
	for _, s := range infoIssues {
		loc, hint := splitSQLPerfIssue(s)
		fmt.Fprintf(&sb, `<div class="sp-card sp-card-info"><div class="sp-hdr">ℹ️ %s</div><div class="sp-body">有 WHERE 条件无 .Limit()<div class="sp-sql">%s</div></div></div>`, loc, htmlEsc(hint))
	}
	sb.WriteString(`</div>`)

	sb.WriteString(`<script>
function spTab(id,btn,cls){
  ['sp-nolimit','sp-bounded'].forEach(function(x){var e=document.getElementById(x);if(e)e.classList.remove('active');});
  document.getElementById('sp-tabs').querySelectorAll('.sp-tab-btn').forEach(function(b){b.classList.remove('on-warn','on-info');});
  var p=document.getElementById(id);if(p)p.classList.add('active');
  if(btn)btn.classList.add(cls);
}
</script>`)
	return template.HTML(sb.String())
}

func splitSQLPerfIssue(s string) (loc, hint string) {
	idx := strings.Index(s, " — ")
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+3:]
}

// parseCoverHTMLAnchors parses go tool cover -html output and returns a map
// from file path suffix (e.g. "internal/logic/user/user_logic.go") to anchor
// fragment (e.g. "file3"), so table rows can link directly to the right section.
//
// cover.html structure:
//
//	<option value="file0">github.com/foo/bar/internal/logic/user/user_logic.go (N%)</option>
func parseCoverHTMLAnchors(coverHTMLPath string) map[string]string {
	data, err := os.ReadFile(coverHTMLPath)
	if err != nil {
		return nil
	}
	result := map[string]string{}
	content := string(data)
	// Find all: value="fileN">some/path/file.go
	searchStr := `value="`
	idx := 0
	for {
		start := strings.Index(content[idx:], searchStr)
		if start < 0 {
			break
		}
		start += idx + len(searchStr)
		end := strings.Index(content[start:], `"`)
		if end < 0 {
			break
		}
		anchor := content[start : start+end] // e.g. "file3"
		idx = start + end + 1

		// Find the text content between > and <
		gt := strings.Index(content[idx:], ">")
		if gt < 0 {
			break
		}
		gt += idx + 1
		lt := strings.Index(content[gt:], "<")
		if lt < 0 {
			break
		}
		label := strings.TrimSpace(content[gt : gt+lt]) // e.g. "github.com/.../file.go (80.0%)"
		// Strip coverage pct suffix
		if paren := strings.LastIndex(label, " ("); paren >= 0 {
			label = label[:paren]
		}
		idx = gt + lt

		// Map every suffix of the path, so pkg-level matching works
		parts := strings.Split(label, "/")
		for i := range parts {
			suffix := strings.Join(parts[i:], "/")
			if _, exists := result[suffix]; !exists {
				result[suffix] = anchor
			}
		}
	}
	return result
}

// renderCoverTable renders an HTML coverage summary table from per-package stats.
// pkgAnchors maps package path → anchor in details/cover.html (e.g. "file3").
// Clicking a row opens cover.html scrolled to the first file in that package.
func renderCoverTable(pkgs []PkgCoverage, overallPct float64, coverHTMLExists bool, pkgAnchors map[string]string) template.HTML {
	if len(pkgs) == 0 {
		return template.HTML(`<div style="padding:32px;text-align:center;color:#888;font-size:.93rem">⚠️ 暂无覆盖率数据（无测试文件或构建失败）</div>`)
	}

	var sb strings.Builder

	// ── overall banner ─────────────────────────────────────────────────────
	overallColor := "#c00"
	if overallPct >= 80 {
		overallColor = "#1a7f37"
	} else if overallPct >= 60 {
		overallColor = "#b45309"
	}
	totalFiles := 0
	for _, p := range pkgs {
		totalFiles += p.Files
	}
	sb.WriteString(fmt.Sprintf(`
<div style="padding:16px 20px 0">
  <div style="display:flex;align-items:center;gap:16px;margin-bottom:12px">
    <span style="font-size:2rem;font-weight:700;color:%s">%.1f%%</span>
    <span style="color:#555;font-size:.9rem">总体语句覆盖率 · %d 个文件 · %d 个包</span>`, overallColor, overallPct, totalFiles, len(pkgs)))

	if coverHTMLExists {
		sb.WriteString(`    <a href="details/cover.html" target="_blank"
         style="margin-left:auto;padding:5px 14px;border-radius:6px;background:#0969da;color:#fff;
                font-size:.82rem;text-decoration:none;white-space:nowrap">🔍 查看行级覆盖详情</a>`)
	}
	sb.WriteString(`
  </div>`)

	// ── progress bar for overall ────────────────────────────────────────────
	sb.WriteString(fmt.Sprintf(`
  <div style="height:8px;background:#e8e8e8;border-radius:4px;margin-bottom:16px;overflow:hidden">
    <div style="height:100%%;width:%.1f%%;background:%s;border-radius:4px;transition:width .3s"></div>
  </div>`, overallPct, overallColor))

	// ── package table ───────────────────────────────────────────────────────
	sb.WriteString(`
  <table style="width:100%;border-collapse:collapse;font-size:.83rem">
    <thead>
      <tr style="border-bottom:2px solid #e0e0e0;color:#555">
        <th style="text-align:left;padding:6px 8px;font-weight:600">包路径</th>
        <th style="text-align:right;padding:6px 8px;font-weight:600;white-space:nowrap">语句数</th>
        <th style="text-align:right;padding:6px 8px;font-weight:600;white-space:nowrap">覆盖率</th>
        <th style="padding:6px 8px;min-width:120px;font-weight:600">进度</th>
      </tr>
    </thead>
    <tbody>`)

	for i, p := range pkgs {
		rowBg := "#fff"
		if i%2 == 1 {
			rowBg = "#f9f9f9"
		}
		barColor := "#c00"
		textColor := "#c00"
		if p.Pct >= 80 {
			barColor = "#1a7f37"
			textColor = "#1a7f37"
		} else if p.Pct >= 60 {
			barColor = "#b45309"
			textColor = "#b45309"
		}

		// Build link to the first file in this package inside cover.html
		pkgCell := htmlEscape(p.Pkg)
		if coverHTMLExists {
			// Try to find an anchor for any file in this package
			anchor := ""
			for suffix, anch := range pkgAnchors {
				if strings.HasPrefix(suffix, p.Pkg+"/") || suffix == p.Pkg {
					anchor = anch
					break
				}
			}
			if anchor != "" {
				pkgCell = fmt.Sprintf(`<a href="details/cover.html#%s" target="_blank"
              style="color:#0969da;text-decoration:none;font-family:monospace" title="在覆盖率详情中查看">%s</a>`,
					anchor, htmlEscape(p.Pkg))
			}
		}

		sb.WriteString(fmt.Sprintf(`
      <tr style="background:%s;border-bottom:1px solid #ebebeb">
        <td style="padding:6px 8px;word-break:break-all">%s</td>
        <td style="text-align:right;padding:6px 8px;color:#555">%d</td>
        <td style="text-align:right;padding:6px 8px;font-weight:600;color:%s">%.1f%%</td>
        <td style="padding:6px 8px">
          <div style="height:6px;background:#e8e8e8;border-radius:3px;overflow:hidden">
            <div style="height:100%%;width:%.1f%%;background:%s;border-radius:3px"></div>
          </div>
        </td>
      </tr>`,
			rowBg, pkgCell, p.Stmts, textColor, p.Pct, p.Pct, barColor))
	}

	sb.WriteString(`
    </tbody>
  </table>
</div>`)

	return template.HTML(sb.String())
}

// htmlEscape escapes s for safe use in HTML text content.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// renderSQLPerfFromFindings renders the SQL perf inline panel using real SQL strings
// derived from logic review IONodes. The dark box shows the actual SQL.
func renderSQLPerfFromFindings(warn, info []SQLPerfFinding) template.HTML {
	if len(warn)+len(info) == 0 {
		return template.HTML(`<div style="padding:32px;text-align:center;color:#060;font-size:.95rem">✅ 未发现无分页全表扫描</div>`)
	}
	var sb strings.Builder
	// Reuse sp-* CSS already defined in renderSQLPerfInline
	sb.WriteString(`<style>
.sp-tabs{display:flex;border-bottom:2px solid #ddd;margin-bottom:20px}
.sp-tab-btn{padding:10px 24px;font-size:.93rem;font-weight:600;cursor:pointer;border:none;background:none;color:#888;border-bottom:3px solid transparent;margin-bottom:-2px;transition:all .15s}
.sp-tab-btn:hover{color:#333}
.sp-tab-btn.on-warn{color:#9a6000;border-bottom-color:#c80}
.sp-tab-btn.on-info{color:#0055aa;border-bottom-color:#0055aa}
.sp-panel{display:none;padding:4px 0}
.sp-panel.active{display:block}
.sp-card{background:#fff;border-radius:8px;border:1px solid #e0e0e0;margin-bottom:14px;overflow:hidden}
.sp-card-warn{border-left:4px solid #e08000}
.sp-card-info{border-left:4px solid #0066cc}
.sp-hdr{padding:10px 16px;background:#fafafa;border-bottom:1px solid #eee;font-size:.86rem;font-family:monospace;color:#444;display:flex;align-items:center;justify-content:space-between}
.sp-hdr-dao{font-weight:700;color:#333}
.sp-hdr-loc{font-size:.78rem;color:#888}
.sp-body{padding:12px 16px;font-size:.84rem}
.sp-desc{color:#666;margin-bottom:6px;font-size:.82rem}
.sp-sql{background:#1e1e1e;border-radius:5px;padding:8px 12px;font-family:monospace;color:#d4d4d4;font-size:.82rem;white-space:pre;overflow-x:auto}
.sp-empty{color:#666;padding:24px;text-align:center}
</style>
`)
	firstWarnCls := ""
	firstInfoCls := ""
	if len(warn) > 0 {
		firstWarnCls = " on-warn"
	} else {
		firstInfoCls = " on-info"
	}
	sb.WriteString(`<div class="sp-tabs" id="sp-tabs">`)
	fmt.Fprintf(&sb, `<button class="sp-tab-btn%s" onclick="spTab('sp-nolimit',this,'on-warn')">⚠️ 无分页全表 (%d)</button>`, firstWarnCls, len(warn))
	fmt.Fprintf(&sb, `<button class="sp-tab-btn%s" onclick="spTab('sp-bounded',this,'on-info')">ℹ️ 有WHERE无Limit (%d)</button>`, firstInfoCls, len(info))
	sb.WriteString(`</div>`)

	activeWarn := ""
	if len(warn) > 0 {
		activeWarn = " active"
	}
	activeInfo := ""
	if len(warn) == 0 {
		activeInfo = " active"
	}

	// ── 无分页全表 panel ──
	fmt.Fprintf(&sb, `<div id="sp-nolimit" class="sp-panel%s">`, activeWarn)
	if len(warn) == 0 {
		sb.WriteString(`<div class="empty-pass">✅ No issues found</div>`)
	}
	for _, f := range warn {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(&sb,
			`<div class="sp-card sp-card-warn">`+
				`<div class="sp-hdr"><span class="sp-hdr-dao">%s</span><span class="sp-hdr-loc">%s</span></div>`+
				`<div class="sp-body"><div class="sp-desc">无 WHERE 约束，无 LIMIT — 可能全表扫描</div>`+
				`<div class="sp-sql">%s</div></div></div>`,
			htmlEsc(f.DAO), htmlEsc(loc), htmlEsc(f.SQL))
	}
	sb.WriteString(`</div>`)

	// ── 有WHERE无Limit panel ──
	fmt.Fprintf(&sb, `<div id="sp-bounded" class="sp-panel%s">`, activeInfo)
	sb.WriteString(`<p style="font-size:.83rem;color:#666;margin-bottom:12px">有 WHERE 条件但无 LIMIT，结果集有界时可接受，否则建议加 LIMIT 或分页</p>`)
	if len(info) == 0 {
		sb.WriteString(`<div class="empty-pass">✅ No issues found</div>`)
	}
	for _, f := range info {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(&sb,
			`<div class="sp-card sp-card-info">`+
				`<div class="sp-hdr"><span class="sp-hdr-dao">%s</span><span class="sp-hdr-loc">%s</span></div>`+
				`<div class="sp-body"><div class="sp-desc">有 WHERE 条件，无 LIMIT</div>`+
				`<div class="sp-sql">%s</div></div></div>`,
			htmlEsc(f.DAO), htmlEsc(loc), htmlEsc(f.SQL))
	}
	sb.WriteString(`</div>`)

	sb.WriteString(`<script>
function spTab(id,btn,cls){
  ['sp-nolimit','sp-bounded'].forEach(function(x){var e=document.getElementById(x);if(e)e.classList.remove('active');});
  document.getElementById('sp-tabs').querySelectorAll('.sp-tab-btn').forEach(function(b){b.classList.remove('on-warn','on-info');});
  var p=document.getElementById(id);if(p)p.classList.add('active');
  if(btn)btn.classList.add(cls);
}
</script>`)
	return template.HTML(sb.String())
}

func htmlEsc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// ── Logic Review inline renderer ─────────────────────────────────────────────
//
// Layout (参考 SQL 性能 sp-tabs 样式):
//   Top tabs  = first-level subdir under logic/ (e.g. user, oauth, permission)
//   Tab body  = second-level subdir groups (e.g. list, register, auth)
//   Each interface = one collapsible card, expanded by default
//   Card header = fn() + file:line + SQL/Redis badges
//   Card body = numbered IO steps with source snippet (参考 N+1 样式)

func renderLogicReviewInline(r *LogicReviewResult) template.HTML {
	if r == nil || len(r.Methods) == 0 {
		return template.HTML(`<div style="padding:40px;text-align:center;color:#888;font-size:.93rem">
			ℹ️ 暂无数据（call graph 构建失败，或项目无 Logic 层存储调用）</div>`)
	}

	// Group methods: module → subModule → []LogicReviewMethod
	type subGroup struct {
		Name    string
		Methods []LogicReviewMethod
	}
	type topGroup struct {
		Name   string
		Subs   []*subGroup
		subMap map[string]*subGroup
	}
	topMap := make(map[string]*topGroup)
	var topOrder []string
	for _, m := range r.Methods {
		mod := m.Module
		if mod == "" {
			mod = "root"
		}
		sub := m.SubModule
		if sub == "" {
			sub = mod
		}
		if _, ok := topMap[mod]; !ok {
			topMap[mod] = &topGroup{Name: mod, subMap: map[string]*subGroup{}}
			topOrder = append(topOrder, mod)
		}
		tg := topMap[mod]
		if _, ok := tg.subMap[sub]; !ok {
			sg := &subGroup{Name: sub}
			tg.subMap[sub] = sg
			tg.Subs = append(tg.Subs, sg)
		}
		tg.subMap[sub].Methods = append(tg.subMap[sub].Methods, m)
	}

	var sb strings.Builder

	// ── CSS (参考 sp-tabs 样式 + N+1 code-block 样式) ──
	sb.WriteString(`<style>
/* ── top-level tabs (参考 sp-tabs) ── */
.lr-tabs{display:flex;border-bottom:2px solid #ddd;margin-bottom:20px;position:sticky;top:0;z-index:10;background:#fff}
.lr-tab-btn{padding:10px 24px;font-size:.93rem;font-weight:600;cursor:pointer;border:none;background:none;color:#888;border-bottom:3px solid transparent;margin-bottom:-2px;transition:all .15s;text-transform:capitalize}
.lr-tab-btn:hover{color:#333}
.lr-tab-btn.active{color:#141422;border-bottom-color:#141422}
.lr-tab-panel{display:none;padding:4px 0}
.lr-tab-panel.active{display:block}
/* ── submodule section ── */
.lr-sub{margin-bottom:24px}
.lr-sub-title{font-size:.72rem;font-weight:700;text-transform:uppercase;letter-spacing:.1em;color:#7a8aaa;padding:4px 0 8px;border-bottom:1px solid #eef0f5;margin-bottom:10px;display:flex;align-items:center;gap:6px}
/* ── interface card ── */
.lr-card{background:#fff;border:1px solid #e0e3ea;border-radius:8px;margin-bottom:14px;overflow:hidden}
.lr-card-hdr{padding:10px 16px;display:flex;align-items:center;gap:8px;cursor:pointer;user-select:none;background:#f8f9fc;border-bottom:1px solid #eef0f5}
.lr-card-hdr:hover{background:#f0f2f8}
.lr-card-fn{font-weight:700;font-size:.88rem;font-family:monospace;color:#1a1a3a;flex:1}
.lr-card-loc{font-size:.72rem;font-family:monospace;color:#888;flex-shrink:0}
.lr-card-badges{display:flex;gap:5px;flex-shrink:0}
.lr-badge-db{font-size:.68rem;padding:1px 7px;border-radius:8px;background:#e8f0fe;color:#1a56cc;font-weight:700}
.lr-badge-redis{font-size:.68rem;padding:1px 7px;border-radius:8px;background:#fce8e6;color:#c5221f;font-weight:700}
.lr-card-arrow{color:#bbb;font-size:.8rem;transition:transform .15s;flex-shrink:0}
.lr-card-body{display:none;padding:14px 16px 16px}
.lr-card-body.open{display:block}
/* ── IO step ── */
.lr-step{display:flex;gap:0;margin-bottom:16px}
.lr-step:last-child{margin-bottom:0}
.lr-step-num{width:28px;flex-shrink:0;padding-top:4px;color:#bbb;font-size:.72rem;text-align:right;padding-right:8px;font-weight:700}
.lr-step-body{flex:1;min-width:0}
.lr-step-meta{display:flex;align-items:center;gap:6px;margin-bottom:5px;flex-wrap:wrap}
.lr-kind-sql{font-size:.68rem;font-weight:700;padding:2px 7px;border-radius:3px;background:#e8f0fe;color:#1a56cc;flex-shrink:0}
.lr-kind-redis{font-size:.68rem;font-weight:700;padding:2px 7px;border-radius:3px;background:#2a1818;color:#ffb3b0;flex-shrink:0}
.lr-dao{font-family:monospace;font-size:.82rem;color:#444}
.lr-dao strong{color:#222}
.lr-loc{font-size:.72rem;font-family:monospace;color:#888;margin-left:2px}
.lr-inferred{font-size:.65rem;color:#aaa;background:#f5f5f5;padding:1px 5px;border-radius:3px}
.lr-chain{font-size:.68rem;color:#aaa;font-style:italic;margin-left:auto;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:260px}
/* ── SQL block (dark green) ── */
.lr-sql-wrap{margin-top:3px}
.lr-sql{background:#1a1a2e;border-radius:5px;padding:7px 12px;font-family:monospace;color:#a8d8a8;font-size:.82rem;white-space:pre;overflow-x:auto;border-left:3px solid #2e7d32;margin-top:2px}
.lr-sql-hook{border-left-color:#5a7d5a;opacity:.85}
.lr-hook-label{font-size:.68rem;color:#6a9a6a;font-style:italic;margin-top:6px;margin-bottom:1px}
.lr-hook-badge{font-size:.65rem;background:#1a2e1a;color:#6aaa6a;padding:1px 6px;border-radius:3px;margin-left:4px;font-weight:600}
.lr-sql-missing{font-size:.78rem;color:#a05000;background:#fff8f0;border:1px dashed #e08000;border-radius:4px;padding:5px 10px;margin-top:3px}
/* ── Redis block (dark red) ── */
.lr-redis{background:#1a0a0a;border-radius:5px;padding:7px 12px;font-family:monospace;color:#ffb3b0;font-size:.82rem;white-space:pre;overflow-x:auto;margin-top:3px;border-left:3px solid #c5221f}
/* ── Top-level summary card ── */
.lr-summary{display:flex;gap:14px;align-items:stretch;padding:14px 18px;margin-bottom:18px;background:linear-gradient(135deg,#f7f9fc 0%,#eef2f9 100%);border:1px solid #dde3ee;border-radius:10px}
.lr-sum-cell{flex:1;display:flex;flex-direction:column;gap:4px;padding:8px 14px;background:#fff;border-radius:8px;border:1px solid #e5e9f0}
.lr-sum-label{font-size:.7rem;font-weight:700;color:#7a8aaa;text-transform:uppercase;letter-spacing:.08em}
.lr-sum-value{font-size:1.15rem;font-weight:800;font-family:monospace;color:#1a1a3a}
.lr-sum-db .lr-sum-value{color:#1a56cc}
.lr-sum-redis .lr-sum-value{color:#c5221f}
.lr-sum-detail{font-size:.7rem;color:#888;font-family:monospace}
.lr-sum-note{flex:1.4;font-size:.72rem;color:#5a6a85;line-height:1.55;padding:8px 14px;background:#fff;border-radius:8px;border:1px dashed #d4dae6}
.lr-sum-note b{color:#1a1a3a}
.lr-sum-note code{background:#f0f3f8;padding:1px 5px;border-radius:3px;font-family:monospace;font-size:.95em}
/* ── Loop badge on step ── */
.lr-loop-badge{font-size:.65rem;font-weight:700;padding:1px 6px;border-radius:3px;background:#fff3e0;color:#b25600;border:1px solid #ffd9a8;flex-shrink:0}
.lr-loop-badge::before{content:"↻ "}
.lr-card-loop{background:#fff3e0;color:#b25600;border:1px solid #ffd9a8}
</style>
`)

	// Unique prefix to avoid ID collisions when embedded in report.html
	pfx := "lr"

	// ── Top-level storage summary card ──
	// Shows aggregate DB / Redis IO trips across all Logic methods.
	// Format: "static + loop×N + hook" where N denotes loop iteration count.
	dbDetail := fmt.Sprintf("static %d · loop %d × N · hook %d",
		r.TotalDBStatic, r.TotalDBLoop, r.TotalDBHook)
	redisDetail := fmt.Sprintf("static %d · loop %d × N",
		r.TotalRedisStatic, r.TotalRedisLoop)
	sb.WriteString(`<div class="lr-summary">`)
	fmt.Fprintf(&sb,
		`<div class="lr-sum-cell lr-sum-db"><span class="lr-sum-label">DB 调用总次数</span><span class="lr-sum-value">%s</span><span class="lr-sum-detail">%s</span></div>`,
		htmlEsc(r.TotalDBSummary), htmlEsc(dbDetail))
	fmt.Fprintf(&sb,
		`<div class="lr-sum-cell lr-sum-redis"><span class="lr-sum-label">Redis 调用总次数</span><span class="lr-sum-value">%s</span><span class="lr-sum-detail">%s</span></div>`,
		htmlEsc(r.TotalRedisSummary), htmlEsc(redisDetail))
	sb.WriteString(`<div class="lr-sum-note">`)
	sb.WriteString(`<b>计数规则</b>：每个 ent 终端调用（DAO 方法）= 1 次 DB IO；每个 go-zero Redis 命令 = 1 次 Redis IO。`)
	sb.WriteString(`<br>循环内调用记为 <code>N×</code>（N 表示该循环每次 Logic 调用的迭代次数，可能因输入而异）。`)
	sb.WriteString(`<br>ent <b>hook</b> 级联触发的额外 SQL 单独计入 <code>hook</code> 项。`)
	sb.WriteString(`</div>`)
	sb.WriteString(`</div>`)

	// Render top-level module tabs (参考 sp-tabs 样式)
	sb.WriteString(fmt.Sprintf(`<div class="lr-tabs" id="%s-tabs">`, pfx))
	for i, modName := range topOrder {
		activeCls := ""
		if i == 0 {
			activeCls = " active"
		}
		// Count methods in this module
		cnt := 0
		for _, sg := range topMap[modName].Subs {
			cnt += len(sg.Methods)
		}
		fmt.Fprintf(&sb,
			`<button class="lr-tab-btn%s" onclick="lrTab('%s','%s-%s',this)">%s <span style="font-size:.72rem;opacity:.7">(%d)</span></button>`,
			activeCls, pfx, pfx, strings.ReplaceAll(modName, "/", "-"), htmlEsc(modName), cnt)
	}
	sb.WriteString(`</div>`)

	// Render tab panels
	for i, modName := range topOrder {
		tg := topMap[modName]
		tabID := fmt.Sprintf("%s-%s", pfx, strings.ReplaceAll(modName, "/", "-"))
		activeCls := ""
		if i == 0 {
			activeCls = " active"
		}
		fmt.Fprintf(&sb, `<div id="%s" class="lr-tab-panel%s">`, tabID, activeCls)

		for _, sg := range tg.Subs {
			// Sub-module section header
			fmt.Fprintf(&sb, `<div class="lr-sub"><div class="lr-sub-title">📁 %s</div>`,
				htmlEsc(sg.Name))

			for mi, m := range sg.Methods {
				cardID := fmt.Sprintf("%s-%s-%s-%d", pfx,
					strings.ReplaceAll(modName, "/", "-"),
					strings.ReplaceAll(sg.Name, "/", "-"), mi)

				// Card header: signature + file:line + badges + arrow
				fmt.Fprintf(&sb,
					`<div class="lr-card"><div class="lr-card-hdr" onclick="lrToggle('%s')">`,
					cardID)
				// Card title: method name only (clean, no params/returns)
				fmt.Fprintf(&sb, `<span class="lr-card-fn">%s()</span>`, htmlEsc(m.Method))

				// Show file:line of Logic method definition
				if m.LogicFile != "" {
					locStr := m.LogicFile
					if m.LogicLine > 0 {
						locStr = fmt.Sprintf("%s:%d", m.LogicFile, m.LogicLine)
					}
					fmt.Fprintf(&sb, `<span class="lr-card-loc">%s</span>`, htmlEsc(locStr))
				}

				sb.WriteString(`<div class="lr-card-badges">`)
				if m.DBStaticCount > 0 || m.DBLoopCount > 0 || m.DBHookCount > 0 {
					fmt.Fprintf(&sb, `<span class="lr-badge-db" title="static %d · loop %d×N · hook %d">SQL %s</span>`,
						m.DBStaticCount, m.DBLoopCount, m.DBHookCount, htmlEsc(m.DBSummary))
				}
				if m.RedisStaticCount > 0 || m.RedisLoopCount > 0 {
					fmt.Fprintf(&sb, `<span class="lr-badge-redis" title="static %d · loop %d×N">Redis %s</span>`,
						m.RedisStaticCount, m.RedisLoopCount, htmlEsc(m.RedisSummary))
				}
				if m.DBLoopCount > 0 || m.RedisLoopCount > 0 {
					sb.WriteString(`<span class="lr-loop-badge lr-card-loop" title="存在循环内 IO 调用">N+1?</span>`)
				}
				sb.WriteString(`</div>`)
				sb.WriteString(`<span class="lr-card-arrow" id="arr-` + cardID + `">▼</span>`)
				sb.WriteString(`</div>`) // hdr

				// Card body — open by default (lr-card-body.open shown via CSS)
				fmt.Fprintf(&sb, `<div id="%s" class="lr-card-body open">`, cardID)

				if len(m.Ops) == 0 {
					sb.WriteString(`<div style="color:#aaa;font-size:.82rem;padding:4px 0">无存储调用</div>`)
				}
				for si, op := range m.Ops {
					fmt.Fprintf(&sb, `<div class="lr-step"><div class="lr-step-num">%d</div><div class="lr-step-body">`, si+1)

					// Meta line: kind badge + dao.method + file:line + inferred? + call chain
					sb.WriteString(`<div class="lr-step-meta">`)
					if op.Kind == IOKindDB {
						sb.WriteString(`<span class="lr-kind-sql">SQL</span>`)
					} else {
						sb.WriteString(`<span class="lr-kind-redis">Redis</span>`)
					}
					fmt.Fprintf(&sb, `<span class="lr-dao">%s.<strong>%s</strong>()</span>`,
						htmlEsc(op.Receiver), htmlEsc(op.Method))
					// File:line for the call site
					if op.ShortFile != "" && op.Line > 0 {
						fmt.Fprintf(&sb, `<span class="lr-loc">%s:%d</span>`,
							htmlEsc(op.ShortFile), op.Line)
					}
					if op.Kind == IOKindDB && !op.SQLExact {
						sb.WriteString(`<span class="lr-inferred">推断</span>`)
					}
					if op.InLoop {
						loopTitle := "在 for/range 循环内 — 每次 Logic 调用执行 N 次"
						if op.LoopLine > 0 {
							loopTitle = fmt.Sprintf("循环起始 line:%d — 每次 Logic 调用执行 N 次", op.LoopLine)
						}
						fmt.Fprintf(&sb, `<span class="lr-loop-badge" title="%s">×N</span>`, htmlEsc(loopTitle))
					}
					// Show call chain (skip first entry = the Logic method itself)
					if len(op.CallChain) > 1 {
						chainStr := strings.Join(op.CallChain[1:], " → ")
						fmt.Fprintf(&sb, `<span class="lr-chain" title="%s">via %s</span>`,
							htmlEsc(chainStr), htmlEsc(chainStr))
					}
					sb.WriteString(`</div>`) // meta

					// SQL or Redis command
					if op.Kind == IOKindDB {
						if op.SQL == "" {
							// No SQL captured and no ent terminal found in AST — show error, never guess
							sb.WriteString(`<div class="lr-sql-missing">⚠ SQL 未捕获 — impl 未在 implIdx 中 或 无 ent 终端调用</div>`)
						} else {
							sqlLabel := ""
							if len(op.HookSQLs) > 0 {
								sqlLabel = fmt.Sprintf(` <span class="lr-hook-badge">+%d hook SQL</span>`, len(op.HookSQLs))
							}
							fmt.Fprintf(&sb, `<div class="lr-sql-wrap">%s<div class="lr-sql">%s</div>`,
								sqlLabel, htmlEsc(op.SQL))
							for hi, hsql := range op.HookSQLs {
								fmt.Fprintf(&sb,
									`<div class="lr-hook-label">hook cascade #%d</div><div class="lr-sql lr-sql-hook">%s</div>`,
									hi+1, htmlEsc(hsql))
							}
							sb.WriteString(`</div>`) // sql-wrap
						}
					} else {
						fmt.Fprintf(&sb, `<div class="lr-redis">%s</div>`, htmlEsc(op.RedisCmd))
					}

					sb.WriteString(`</div></div>`) // step-body + step
				}
				sb.WriteString(`</div>`) // card-body
				sb.WriteString(`</div>`) // card
			}
			sb.WriteString(`</div>`) // sub
		}
		sb.WriteString(`</div>`) // tab-panel
	}

	// Script
	fmt.Fprintf(&sb, `<script>
function lrTab(pfx, panelId, btn) {
  document.getElementById(pfx+'-tabs').querySelectorAll('.lr-tab-btn').forEach(function(b){b.classList.remove('active');});
  document.querySelectorAll('[id^="'+pfx+'-"]').forEach(function(p){if(p.classList.contains('lr-tab-panel'))p.classList.remove('active');});
  document.getElementById(panelId).classList.add('active');
  btn.classList.add('active');
}
function lrToggle(id) {
  var body = document.getElementById(id);
  if (!body) return;
  body.classList.toggle('open');
  var arr = document.getElementById('arr-'+id);
  if (arr) arr.textContent = body.classList.contains('open') ? '▼' : '▶';
}
</script>`)
	return template.HTML(sb.String())
}


func buildFuncMap() template.FuncMap {
	return template.FuncMap{
		"levelClass":  levelClass,
		"levelIcon":   levelIcon,
		"hasInline":   func(s template.HTML) bool { return s != "" },
		"hasIframe":   func(s string) bool { return s != "" },
		"hasTabs":     func(tabs []IssueTab) bool { return len(tabs) > 0 },
		"hasFlat":     func(issues []string) bool { return len(issues) > 0 },
		"tabLevel":    func(t IssueTab) string { return levelClass(t.Level) },
		"tabIcon":     func(t IssueTab) string { return levelIcon(t.Level) },
		"tabCount": func(t IssueTab) int {
			if t.Count > 0 {
				return t.Count
			}
			return len(t.Issues)
		},
		"navCount": func(item NavItem) int {
			// Explicit override (used by IframeURL items like lint raw report)
			if item.Count > 0 {
				return item.Count
			}
			// For tabbed items: sum all tab counts
			if len(item.Tabs) > 0 {
				total := 0
				for _, t := range item.Tabs {
					total += len(t.Issues)
				}
				return total
			}
			// For flat items
			return len(item.FlatIssues)
		},
		"itemLevel":   func(item NavItem) string { return levelClass(item.Level) },
		"groupLevel":  func(g NavGroup) string {
			var levels []Level
			for _, it := range g.Items {
				levels = append(levels, it.Level)
			}
			return levelClass(worstLevel(levels...))
		},
		"allItems": func(groups []NavGroup) []NavItem {
			var all []NavItem
			for _, g := range groups {
				all = append(all, g.Items...)
			}
			return all
		},
		"issueClass": func(s string) string {
			lower := strings.ToLower(s)
			if strings.Contains(lower, "[n+1]") || strings.Contains(lower, "[forcetypeassert]") ||
				strings.Contains(lower, "[errcheck]") || strings.Contains(lower, "[nilnil]") {
				return "fail"
			}
			if strings.Contains(lower, "warn") || strings.Contains(lower, "⚠") {
				return "warn"
			}
			if strings.Contains(lower, "[cross-pkg]") || strings.Contains(lower, "ℹ") {
				return "info"
			}
			return ""
		},
		"hasSepPrefix": func(s string) bool {
			return strings.HasPrefix(s, "──") || strings.HasPrefix(s, "--")
		},
	}
}

// ── HTML template ─────────────────────────────────────────────────────────────

const reportHTMLTemplate = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>Perf Report · {{.ProjectName}}</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
html,body{height:100%;overflow:hidden}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;display:flex;height:100vh;background:#f0f2f5}

/* ── Left nav ── */
.nav{width:240px;min-width:240px;background:#141422;color:#bbb;display:flex;flex-direction:column;height:100vh;overflow-y:auto;flex-shrink:0;scrollbar-width:thin;scrollbar-color:#333 #141422}
.nav::-webkit-scrollbar{width:4px}.nav::-webkit-scrollbar-track{background:#141422}.nav::-webkit-scrollbar-thumb{background:#333;border-radius:2px}
.nav-scroll-area{flex:1;overflow-y:auto;padding-bottom:32px}
.nav-header{padding:15px 16px 10px;border-bottom:1px solid #252540;flex-shrink:0}
.nav-project{font-size:.98rem;font-weight:700;color:#fff;margin-bottom:2px}
.nav-branch{font-size:.73rem;color:#6a8fff;font-family:monospace;margin-bottom:2px}
.nav-meta{font-size:.7rem;color:#666}
.nav-group-header{display:flex;align-items:center;gap:6px;padding:10px 12px 3px;font-size:.68rem;font-weight:700;text-transform:uppercase;letter-spacing:.1em;color:#3a4a7a;margin-top:2px}
.nav-btn{display:flex;align-items:center;gap:7px;padding:6px 14px 6px 22px;cursor:pointer;font-size:.84rem;color:#999;border-left:3px solid transparent;background:none;border-top:none;border-right:none;border-bottom:none;width:100%;text-align:left;transition:all .12s}
.nav-btn:hover{background:#1e1e38;color:#ddd;border-left-color:#444}
.nav-btn.active{background:#1a1a40;color:#fff;border-left-color:#5a7fff}
.nav-btn.lv-fail{color:#ff8888} .nav-btn.lv-warn{color:#ffcc66} .nav-btn.lv-panic{color:#ff66ff}
.nav-btn.active.lv-fail{background:#2a1010;border-left-color:#d00}
.nav-btn.active.lv-warn{background:#2a2000;border-left-color:#d80}
.nav-btn.active.lv-panic{background:#2a0a2a;border-left-color:#c0c}
.nav-btn.active.lv-pass{background:#0a200a;border-left-color:#080}
.nav-badge{margin-left:auto;font-size:.67rem;padding:1px 5px;border-radius:8px;font-weight:700;flex-shrink:0}
.nb-fail{background:#c00;color:#fff} .nb-warn{background:#9a6000;color:#fff}
.nb-pass{background:#060;color:#fff} .nb-info{background:#0055aa;color:#fff}
.nb-skip{background:#444;color:#aaa}

/* ── Main pane ── */
.main{flex:1;display:flex;flex-direction:column;overflow:hidden}
.panel{display:none;flex:1;min-height:0;flex-direction:column;overflow:hidden}
.panel.active{display:flex}
.panel-scroll{overflow-y:auto;padding:20px 24px;flex:1}
.panel-fullscreen{flex:1;display:flex;flex-direction:column;overflow:hidden}
.panel-fullscreen iframe{flex:1;width:100%;border:none}
.n1-wrap{flex:1;overflow-y:auto;padding-left:4px}

/* ── Panel header + tabs ── */
.panel-hdr{padding:12px 24px 0;background:#fff;border-bottom:1px solid #e5e8ee;flex-shrink:0;overflow:hidden;min-width:0}
.panel-title{font-size:1rem;font-weight:700;margin-bottom:4px}
.panel-title.fail{color:#c00} .panel-title.warn{color:#9a6000}
.panel-title.pass{color:#060} .panel-title.info{color:#0055aa} .panel-title.panic{color:#a00a}
.panel-note{font-size:.8rem;color:#888;margin-bottom:8px}
.tab-bar{display:flex;gap:0;overflow-x:auto;scrollbar-width:thin;scrollbar-color:#ccc transparent}
.tab-bar::-webkit-scrollbar{height:3px}.tab-bar::-webkit-scrollbar-track{background:transparent}.tab-bar::-webkit-scrollbar-thumb{background:#ccc;border-radius:2px}
.tab-btn{padding:7px 16px;font-size:.85rem;font-weight:600;cursor:pointer;border:none;background:none;color:#999;border-bottom:3px solid transparent;transition:all .12s;white-space:nowrap;flex-shrink:0}
.tab-btn:hover{color:#333}
.tab-btn.on{color:#141422;border-bottom-color:#141422}
.tab-btn.on-fail{color:#c00;border-bottom-color:#c00}
.tab-btn.on-warn{color:#9a6000;border-bottom-color:#c80}
.tab-btn.on-pass{color:#060;border-bottom-color:#080}
.tab-btn.on-info{color:#0055aa;border-bottom-color:#0055aa}
.tab-count{font-size:.7rem;padding:1px 5px;border-radius:8px;margin-left:4px;font-weight:700}
.tc-fail{background:#fde;color:#c00} .tc-warn{background:#fff3cc;color:#9a6000}
.tc-pass{background:#dfd;color:#060} .tc-info{background:#e0ecff;color:#0055aa} .tc-panic{background:#f0d0f0;color:#800080}
.tab-panel{display:none;overflow-y:auto;flex:1;min-height:0;padding:14px 24px 18px}
.tab-panel.on{display:block}
.tab-note{font-size:.8rem;color:#888;font-style:italic;padding:6px 0 10px;border-bottom:1px solid #f0f0f0;margin-bottom:10px}

/* ── Fix-suggestion collapsible box (above issue list) ── */
.fix-box{background:#f4f8ff;border:1px solid #c7d8f3;border-radius:6px;margin-bottom:12px;overflow:hidden}
.fix-box>summary{cursor:pointer;padding:8px 14px;font-size:.85rem;font-weight:600;color:#0a4ea0;list-style:none;display:flex;align-items:center;justify-content:space-between;user-select:none}
.fix-box>summary::-webkit-details-marker{display:none}
.fix-box>summary::after{content:"▸";font-size:.7rem;color:#0a4ea0;transition:transform .15s}
.fix-box[open]>summary::after{transform:rotate(90deg)}
.fix-box>summary:hover{background:#e8f0ff}
.fix-body{padding:4px 14px 14px;border-top:1px solid #d8e3f5;background:#fafcff}
.fix-summary{font-size:.82rem;color:#444;line-height:1.55;margin:8px 0 10px}
.fix-steps{margin:0;padding:0;list-style:none}
.fix-steps>li{padding:6px 0;border-bottom:1px dashed #e3e9f3;font-size:.81rem;color:#333}
.fix-steps>li:last-child{border-bottom:none}
.fix-step-desc{margin-bottom:4px;line-height:1.5}
.fix-cmd{display:flex;align-items:center;gap:8px;background:#1e1e1e;border-radius:4px;padding:6px 10px;font-family:'SF Mono',Menlo,Consolas,monospace;font-size:.78rem;color:#d4d4d4;overflow-x:auto}
.fix-cmd code{flex:1;white-space:pre;color:#d4d4d4;background:transparent;padding:0}
.fix-cmd-copy{flex-shrink:0;background:#3a3a3a;color:#fff;border:none;border-radius:3px;padding:2px 8px;font-size:.7rem;cursor:pointer;font-family:inherit;transition:background .12s}
.fix-cmd-copy:hover{background:#555}
.fix-cmd-copy.ok{background:#0a7a2a}
.fix-notes{margin-top:10px;padding:8px 10px;background:#fff7e0;border-left:3px solid #e6a700;border-radius:0 4px 4px 0;font-size:.78rem;color:#6a4a00;line-height:1.55}
.fix-notes>div{margin:2px 0}

/* ── Overview ── */
h1{font-size:1.2rem;margin-bottom:4px}
.meta{font-size:.78rem;color:#888;margin-bottom:16px}
.ov-group{margin-bottom:18px}
.ov-group-title{font-size:.76rem;color:#777;font-weight:700;text-transform:uppercase;letter-spacing:.07em;margin-bottom:6px;display:flex;align-items:center;gap:5px}
.ov-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(160px,1fr));gap:8px}
.ov-card{background:#fff;border-radius:7px;padding:11px 13px;border:1px solid #e0e3ea;cursor:pointer;transition:box-shadow .12s;border-left:4px solid #ccc}
.ov-card:hover{box-shadow:0 2px 8px rgba(0,0,0,.1)}
.ov-card.fail{border-left-color:#c00} .ov-card.warn{border-left-color:#e08000}
.ov-card.pass{border-left-color:#0a0} .ov-card.info{border-left-color:#06c}
.ov-name{font-size:.76rem;color:#888;margin-bottom:3px}
.ov-status{font-size:.9rem;font-weight:700}
.ov-status.fail{color:#c00} .ov-status.warn{color:#9a6000}
.ov-status.pass{color:#060} .ov-status.info{color:#0055aa}
.ov-cnt{font-size:.72rem;color:#aaa;margin-top:2px}

/* ── Issue list ── */
.issue-list{background:#fff;border-radius:6px;border:1px solid #e5e8ee;overflow:hidden}
.issue-item{padding:6px 12px;border-bottom:1px solid #f2f2f2;font-family:monospace;font-size:.78rem;line-height:1.5;word-break:break-all}
.issue-item:last-child{border-bottom:none}
.issue-item.fail{color:#900;background:#fffafb} .issue-item.warn{color:#665500;background:#fffdf0}
.issue-item.info{color:#004;background:#f7f9ff} .issue-item.sep{color:#aaa;font-style:italic}
.empty-pass{background:#f0faf0;border:1px solid #b0d8b0;border-radius:6px;padding:12px;color:#060;text-align:center;font-size:.88rem}
.empty-info{background:#f0f4ff;border:1px solid #b0c8f0;border-radius:6px;padding:12px;color:#0055aa;text-align:center;font-size:.88rem}
</style>
</head>
<body>

<nav class="nav">
  <div class="nav-header">
    <div class="nav-project">{{.ProjectName}}</div>
    {{if .GitBranch}}<div class="nav-branch">⎇ {{.GitBranch}}{{if .GitHash}} · {{.GitHash}}{{end}}</div>{{end}}
    {{if .GitAuthor}}<div class="nav-meta" style="color:#8ab4f8">👤 {{.GitAuthor}}</div>{{end}}
    <div class="nav-meta">🕐 {{.StartTime}}</div>
    <div class="nav-meta" style="color:#8af;font-weight:600">⏱ 耗时 {{.Elapsed}}</div>
  </div>
  <div class="nav-scroll-area">
    <div class="nav-group-header">📊 Overview</div>
    <button class="nav-btn active" id="nav-overview" onclick="goPanel('overview',this,'')">Summary</button>
    {{range .Groups}}
    {{$grp := .}}
    <div class="nav-group-header">{{.Icon}} {{.Label}}</div>
    {{range .Items}}
    {{$item := .}}
    {{$cnt := navCount .}}
    <button class="nav-btn lv-{{levelClass .Level}}" id="nav-{{.ID}}"
      onclick="goPanel('{{.ID}}',this,'lv-{{levelClass .Level}}')">
      {{levelIcon .Level}} {{.Label}}
      {{if gt $cnt 0}}<span class="nav-badge nb-{{levelClass .Level}}">{{$cnt}}</span>{{end}}
    </button>
    {{end}}
    {{end}}
  </div>
</nav>

<div class="main">

  <!-- Overview -->
  <div id="panel-overview" class="panel active">
    <div class="panel-scroll">
      <h1>Perf Scan — {{.ProjectName}}</h1>
      <div class="meta">{{.ScanTime}} · elapsed {{.Elapsed}}</div>
      {{range .Groups}}
      <div class="ov-group">
        <div class="ov-group-title">{{.Icon}} {{.Label}}</div>
        <div class="ov-grid">
          {{range .Items}}
          <div class="ov-card {{levelClass .Level}}" onclick="navTo('{{.ID}}')">
            <div class="ov-name">{{.Label}}</div>
            <div class="ov-status {{levelClass .Level}}">{{levelIcon .Level}} {{levelClass .Level}}</div>
            {{if hasTabs .Tabs}}
            <div class="ov-cnt">
              {{range .Tabs}}{{if gt (tabCount .) 0}}{{tabIcon .}} {{tabCount .}}  {{end}}{{end}}
            </div>
            {{else if gt (len .FlatIssues) 0}}
            <div class="ov-cnt">{{len .FlatIssues}} issue(s)</div>
            {{end}}
          </div>
          {{end}}
        </div>
      </div>
      {{end}}
    </div>
  </div>

  {{range .Groups}}{{range .Items}}
  {{$item := .}}

  {{/* ── Full-screen iframe (lint raw) ── */}}
  {{if hasIframe .IframeURL}}
  <div id="panel-{{.ID}}" class="panel">
    <div class="panel-fullscreen">
      <iframe src="{{.IframeURL}}" title="{{.Label}}"></iframe>
    </div>
  </div>

  {{/* ── N+1 inline (own tab UI) ── */}}
  {{else if hasInline .InlineHTML}}
  <div id="panel-{{.ID}}" class="panel">
    <div class="n1-wrap">{{.InlineHTML}}</div>
  </div>

  {{/* ── Tabbed panel ── */}}
  {{else if hasTabs .Tabs}}
  <div id="panel-{{.ID}}" class="panel">
    <div class="panel-hdr">
      <div class="panel-title {{levelClass .Level}}">{{levelIcon .Level}} {{.Label}}</div>
      <div class="tab-bar" id="tbar-{{.ID}}">
        {{range $i, $tab := .Tabs}}
        <button class="tab-btn {{if eq $i 0}}on-{{tabLevel $tab}}{{end}}"
          onclick="switchTab('{{$item.ID}}','{{$tab.ID}}',this,'on-{{tabLevel $tab}}')">
          {{$tab.Label}}<span class="tab-count tc-{{tabLevel $tab}}">{{tabCount $tab}}</span>
        </button>
        {{end}}
      </div>
    </div>
    {{range $i, $tab := .Tabs}}
    <div id="tp-{{$item.ID}}-{{$tab.ID}}" class="tab-panel {{if eq $i 0}}on{{end}}">
      {{if $tab.Note}}<div class="tab-note">{{$tab.Note}}</div>{{end}}
      {{if $tab.FixHint}}{{template "fixHint" $tab.FixHint}}{{end}}
      {{if $tab.InlineHTML}}{{$tab.InlineHTML}}{{else}}{{template "issueList" $tab.Issues}}{{end}}
    </div>
    {{end}}
  </div>

  {{/* ── Flat list panel ── */}}
  {{else}}
  <div id="panel-{{.ID}}" class="panel">
    <div class="panel-hdr">
      <div class="panel-title {{levelClass .Level}}">{{levelIcon .Level}} {{.Label}}</div>
    </div>
    <div class="panel-scroll" style="padding-top:14px">
      {{template "issueList" .FlatIssues}}
    </div>
  </div>
  {{end}}

  {{end}}{{end}}
</div>

{{define "fixHint"}}
<details class="fix-box">
  <summary>{{.Title}}</summary>
  <div class="fix-body">
    {{if .Summary}}<div class="fix-summary">{{.Summary}}</div>{{end}}
    {{if .Steps}}
    <ol class="fix-steps">
      {{range .Steps}}
      <li>
        {{if .Desc}}<div class="fix-step-desc">{{.Desc}}</div>{{end}}
        {{if .Command}}<div class="fix-cmd"><code>{{.Command}}</code><button class="fix-cmd-copy" onclick="fixCopy(this)">复制</button></div>{{end}}
      </li>
      {{end}}
    </ol>
    {{end}}
    {{if .Notes}}
    <div class="fix-notes">
      {{range .Notes}}<div>⚠️ {{.}}</div>{{end}}
    </div>
    {{end}}
  </div>
</details>
{{end}}

{{define "issueList"}}
{{if eq (len .) 0}}
  <div class="empty-pass">✅ No issues found</div>
{{else}}
  <div class="issue-list">
    {{range .}}
    {{if eq . ""}}{{else if hasSepPrefix .}}
      <div class="issue-item sep">{{.}}</div>
    {{else}}
      <div class="issue-item {{issueClass .}}">{{.}}</div>
    {{end}}
    {{end}}
  </div>
{{end}}
{{end}}

<script>
function goPanel(id, btn, lvCls) {
  document.querySelectorAll('.panel').forEach(p => p.classList.remove('active'));
  document.querySelectorAll('.nav-btn').forEach(b =>
    b.classList.remove('active','lv-fail','lv-warn','lv-pass','lv-info','lv-skip','lv-panic'));
  document.getElementById('panel-' + id).classList.add('active');
  if (btn) { btn.classList.add('active'); if (lvCls) btn.classList.add(lvCls); }
}
function navTo(id) {
  var btn = document.getElementById('nav-' + id);
  var cls = '';
  if (btn) {
    var m = btn.className.match(/lv-(\w+)/);
    if (m) cls = 'lv-' + m[1];
  }
  goPanel(id, btn, cls);
}
function switchTab(panelId, tabId, btn, onCls) {
  document.querySelectorAll('[id^="tp-' + panelId + '-"]').forEach(p => p.classList.remove('on'));
  var bar = document.getElementById('tbar-' + panelId);
  if (bar) bar.querySelectorAll('.tab-btn').forEach(b =>
    b.classList.remove('on','on-fail','on-warn','on-pass','on-info','on-skip'));
  var tp = document.getElementById('tp-' + panelId + '-' + tabId);
  if (tp) tp.classList.add('on');
  if (btn) btn.classList.add(onCls || 'on');
}
function fixCopy(btn) {
  var box = btn.parentElement;
  var code = box && box.querySelector('code');
  if (!code) return;
  var text = code.textContent || '';
  var done = function () {
    var orig = btn.textContent;
    btn.textContent = '已复制';
    btn.classList.add('ok');
    setTimeout(function () { btn.textContent = orig; btn.classList.remove('ok'); }, 1200);
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done).catch(function () {
      var ta = document.createElement('textarea');
      ta.value = text; document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); done(); } catch (e) {}
      document.body.removeChild(ta);
    });
  } else {
    var ta = document.createElement('textarea');
    ta.value = text; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); done(); } catch (e) {}
    document.body.removeChild(ta);
  }
}
</script>
</body>
</html>
`
