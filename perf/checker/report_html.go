package checker

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ── Data structures ───────────────────────────────────────────────────────────

// IssueTab is one tab inside a right-pane panel.
type IssueTab struct {
	ID     string
	Label  string
	Level  Level
	Issues []string
	Note   string // optional explanatory note for empty tabs
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
	ScanTime    string
	Elapsed     string
	Groups      []NavGroup
}

// ── WriteReportHTML ───────────────────────────────────────────────────────────

func WriteReportHTML(
	outDir, projectDir string,
	results map[string]*Result,
	elapsed time.Duration,
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

	groups := buildGroups(results, lintRes, n1Findings, n1Inline)

	data := ReportData{
		ProjectName: projectName,
		GitBranch:   gitBranch,
		GitHash:     gitHash,
		ScanTime:    time.Now().Format("2006-01-02 15:04:05"),
		Elapsed:     elapsed.Round(time.Millisecond).String(),
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
) []NavGroup {
	tabs := map[string][]string{}
	if lr != nil {
		tabs = lr.Tabs
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
	// Also include vet result from RunVet
	defectVetIssues = append(defectVetIssues, results["vet"].safeIssues()...)

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

	// Item 3.3: 内存逃逸热点
	escapeItem := NavItem{
		ID:         "perf-escape",
		Label:      "内存逃逸热点",
		Level:      results["escape"].safeLevel(),
		FlatIssues: results["escape"].safeIssues(),
	}

	perfGroup := NavGroup{
		Icon:  "🐌",
		Label: "性能问题",
		Items: []NavItem{n1Item, perfCodeItem, escapeItem},
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
	// merge "other gosec" into sql for brevity if small
	if len(secOtherIssues) > 0 {
		secSQLIssues = append(secSQLIssues, secOtherIssues...)
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

	// Item 5.3: 测试质量
	var testTestifyIssues []string
	for _, s := range tabs["testing"] {
		if strings.Contains(s, "[testifylint]") {
			testTestifyIssues = append(testTestifyIssues, s)
		}
	}
	// Coverage: parse from results["test"] if available (may be nil in static-only run)
	var coverIssues []string
	if r := results["test"]; r != nil {
		coverIssues = r.safeIssues()
	}
	testItem := NavItem{
		ID:    "quality-testing",
		Label: "测试质量",
		Tabs: []IssueTab{
			{ID: "test-cover", Label: "覆盖率", Level: levelOf(coverIssues),
				Issues: coverIssues, Note: "单测覆盖率统计，运行 make test 获取"},
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
	}

	qualityGroup := NavGroup{
		Icon:  "📐",
		Label: "代码规范",
		Items: []NavItem{complexItem, styleItem, testItem, vetRawItem, lintRawItem},
	}

	return []NavGroup{criticalGroup, bugGroup, perfGroup, secGroup, qualityGroup}
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
	order := map[Level]int{LevelPass: 0, LevelSkip: 0, LevelInfo: 1, LevelWarn: 2, LevelFail: 3}
	for _, t := range tabs {
		if order[t.Level] > order[worst] {
			worst = t.Level
		}
	}
	return worst
}

func worstLevel(levels ...Level) Level {
	worst := LevelPass
	order := map[Level]int{LevelPass: 0, LevelSkip: 0, LevelInfo: 1, LevelWarn: 2, LevelFail: 3}
	for _, l := range levels {
		if order[l] > order[worst] {
			worst = l
		}
	}
	return worst
}

func (r *Result) safeLevel() Level {
	if r == nil {
		return LevelSkip
	}
	return r.Level
}
func (r *Result) safeSummary() string {
	if r == nil {
		return "skipped"
	}
	return r.Summary
}
func (r *Result) safeIssues() []string {
	if r == nil {
		return nil
	}
	return r.Issues
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
		style = "<style>" + s + "</style>\n"
	}
	return template.HTML(style + extractBetween(full, "<body>", "</body>"))
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
		"tabCount":    func(t IssueTab) int { return len(t.Issues) },
		"navCount": func(item NavItem) int {
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
.nav{width:232px;min-width:232px;background:#141422;color:#bbb;display:flex;flex-direction:column;height:100vh;overflow-y:auto;flex-shrink:0}
.nav-header{padding:15px 16px 10px;border-bottom:1px solid #252540}
.nav-project{font-size:.98rem;font-weight:700;color:#fff;margin-bottom:2px}
.nav-branch{font-size:.73rem;color:#6a8fff;font-family:monospace;margin-bottom:2px}
.nav-meta{font-size:.7rem;color:#666}
.nav-group-header{display:flex;align-items:center;gap:6px;padding:10px 12px 3px;font-size:.68rem;font-weight:700;text-transform:uppercase;letter-spacing:.1em;color:#3a4a7a;margin-top:2px}
.nav-btn{display:flex;align-items:center;gap:7px;padding:6px 14px 6px 22px;cursor:pointer;font-size:.84rem;color:#999;border-left:3px solid transparent;background:none;border-top:none;border-right:none;border-bottom:none;width:100%;text-align:left;transition:all .12s}
.nav-btn:hover{background:#1e1e38;color:#ddd;border-left-color:#444}
.nav-btn.active{background:#1a1a40;color:#fff;border-left-color:#5a7fff}
.nav-btn.lv-fail{color:#ff8888} .nav-btn.lv-warn{color:#ffcc66}
.nav-btn.active.lv-fail{background:#2a1010;border-left-color:#d00}
.nav-btn.active.lv-warn{background:#2a2000;border-left-color:#d80}
.nav-btn.active.lv-pass{background:#0a200a;border-left-color:#080}
.nav-badge{margin-left:auto;font-size:.67rem;padding:1px 5px;border-radius:8px;font-weight:700;flex-shrink:0}
.nb-fail{background:#c00;color:#fff} .nb-warn{background:#9a6000;color:#fff}
.nb-pass{background:#060;color:#fff} .nb-info{background:#0055aa;color:#fff}
.nb-skip{background:#444;color:#aaa}

/* ── Main pane ── */
.main{flex:1;display:flex;flex-direction:column;overflow:hidden}
.panel{display:none;height:100%;flex-direction:column}
.panel.active{display:flex}
.panel-scroll{overflow-y:auto;padding:20px 24px;flex:1}
.panel-fullscreen{flex:1;display:flex;flex-direction:column;overflow:hidden}
.panel-fullscreen iframe{flex:1;width:100%;border:none}
.n1-wrap{flex:1;overflow-y:auto;padding-left:4px}

/* ── Panel header + tabs ── */
.panel-hdr{padding:12px 24px 0;background:#fff;border-bottom:1px solid #e5e8ee;flex-shrink:0}
.panel-title{font-size:1rem;font-weight:700;margin-bottom:4px}
.panel-title.fail{color:#c00} .panel-title.warn{color:#9a6000}
.panel-title.pass{color:#060} .panel-title.info{color:#0055aa}
.panel-note{font-size:.8rem;color:#888;margin-bottom:8px}
.tab-bar{display:flex;gap:0;overflow-x:auto}
.tab-btn{padding:7px 16px;font-size:.85rem;font-weight:600;cursor:pointer;border:none;background:none;color:#999;border-bottom:3px solid transparent;transition:all .12s;white-space:nowrap;flex-shrink:0}
.tab-btn:hover{color:#333}
.tab-btn.on{color:#141422;border-bottom-color:#141422}
.tab-btn.on-fail{color:#c00;border-bottom-color:#c00}
.tab-btn.on-warn{color:#9a6000;border-bottom-color:#c80}
.tab-btn.on-pass{color:#060;border-bottom-color:#080}
.tab-btn.on-info{color:#0055aa;border-bottom-color:#0055aa}
.tab-count{font-size:.7rem;padding:1px 5px;border-radius:8px;margin-left:4px;font-weight:700}
.tc-fail{background:#fde;color:#c00} .tc-warn{background:#fff3cc;color:#9a6000}
.tc-pass{background:#dfd;color:#060} .tc-info{background:#e0ecff;color:#0055aa}
.tab-panel{display:none;padding:14px 24px 18px}
.tab-panel.on{display:block}
.tab-note{font-size:.8rem;color:#888;font-style:italic;padding:6px 0 10px;border-bottom:1px solid #f0f0f0;margin-bottom:10px}

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
    <div class="nav-meta">{{.ScanTime}} · {{.Elapsed}}</div>
  </div>
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
      {{template "issueList" $tab.Issues}}
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
    b.classList.remove('active','lv-fail','lv-warn','lv-pass','lv-info','lv-skip'));
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
</script>
</body>
</html>
`
