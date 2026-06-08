package checker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ── Linter → category mapping ─────────────────────────────────────────────────
//
// Category keys correspond exactly to IssueTab IDs used in report_html.go.
// Each linter maps to ONE leaf tab in the 5-module tree.
//
// Module tree (matches design doc):
//   critical  → panic | errdrop
//   bug       → leak  | concurrency | defect
//   perf      → n1 (zctl custom) | perf-code | escape (go build)
//   security  → code-security | supply-chain | trojan
//   quality   → complexity | style | testing

var linterToTab = map[string]string{
	// ── ⚡ Critical: Panic 风险 ──────────────────────────────────────────────
	"forcetypeassert": "panic", // 强制类型断言 a:=b.(T) → panic
	"nilnil":          "panic", // 同时返回 nil error + nil value
	// staticcheck 的 SA5011(nil deref) 也归 panic，但它是复合 linter
	// 按 severity=error 的 staticcheck issues 归 panic，其余归 quality-style
	"staticcheck": "panic-or-style", // special: split by check ID in buildNavGroups

	// ── ⚡ Critical: Error 处理缺陷 ───────────────────────────────────────────
	"errcheck":    "errdrop", // _ = func() 丢弃 error
	"nilerr":      "errdrop", // 检查了 err!=nil 但 return nil
	"wrapcheck":   "errdrop", // 外部包 error 未 wrap
	"wastedassign":"errdrop", // 赋值后从未使用

	// ── 🐛 Bug: 资源泄漏 ──────────────────────────────────────────────────────
	"bodyclose":    "leak",
	"sqlclosecheck":"leak",
	"rowserrcheck": "leak",

	// ── 🐛 Bug: 并发安全 ──────────────────────────────────────────────────────
	"contextcheck": "concurrency",
	"noctx":        "concurrency",
	"copyloopvar":  "concurrency", // 循环变量被 goroutine/闭包捕获

	// ── 🐛 Bug: 其他缺陷 ──────────────────────────────────────────────────────
	"exhaustive": "defect",
	"unparam":    "defect",
	"govet":      "defect", // 锁拷贝 / printf格式 / 不可达代码

	// ── 🐌 Performance: 代码层性能 ────────────────────────────────────────────
	// N+1 is handled by zctl custom checker, not golangci
	"prealloc":   "perf-code", // slice 未预分配
	"fatcontext": "perf-code", // 循环内 context.WithValue
	"gocritic":   "perf-code", // hugeParam 大结构体值传递
	"perfsprint": "perf-code", // Sprintf 低效
	"unqueryvet": "perf-code", // SELECT * 全列查询

	// ── 🔒 Security: 代码安全 ─────────────────────────────────────────────────
	"gosec": "code-security", // SQL注入/弱随机/硬编码密钥/不安全权限

	// ── 🔒 Security: 代码注入风险 ────────────────────────────────────────────
	"bidichk": "trojan", // 危险 Unicode 双向字符

	// ── 📐 Quality: 复杂度 ────────────────────────────────────────────────────
	"gocyclo":  "complexity",
	"cyclop":   "complexity",
	"gocognit": "complexity",
	"funlen":   "complexity",
	"nestif":   "complexity",
	"maintidx": "complexity",

	// ── 📐 Quality: 代码风格 ──────────────────────────────────────────────────
	"mnd":      "style",
	"goconst":  "style",
	"lll":      "style",
	"misspell": "style",
	"godox":    "style",
	// gofmt is a formatter, handled separately via RunFmt checker

	// ── 📐 Quality: 测试质量 ──────────────────────────────────────────────────
	"testifylint":  "testing",
	"paralleltest": "testing",
}

// staticcheckCritical lists staticcheck check IDs that indicate critical issues (panic risk).
// Others go to quality-style.
var staticcheckCritical = map[string]bool{
	"SA5011": true, // nil pointer dereference
	"SA5012": true, // passing odd-sized slice to function expecting even-sized
	"SA4003": true, // array bounds issue
	"SA1006": true, // ineffectual regexp
}

func tabForLinter(linter string) string {
	if t, ok := linterToTab[linter]; ok {
		return t
	}
	return "style" // default fallback
}

// ── LintResult ────────────────────────────────────────────────────────────────

// LintResult extends Result with per-tab issue breakdowns.
type LintResult struct {
	Result
	// Tabs maps tab ID → []"file:line:col: [linter] message"
	Tabs map[string][]string
}

var lastLintResult *LintResult

// ── RunLint ───────────────────────────────────────────────────────────────────

func RunLint(dir, outDir string) *Result {
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		return Skip("golangci-lint not installed (run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)")
	}

	ensureGolangciYml(dir)

	v2 := isGolangciLintV2()
	jsonOut := filepath.Join(outDir, "lint.json")
	htmlOut := filepath.Join(outDir, "lint.html")
	txtOut := filepath.Join(outDir, "lint.txt")

	if v2 {
		runLintV2(dir, jsonOut, htmlOut, txtOut)
	} else {
		runLintV1Format(dir, "json", jsonOut)
		runLintV1Format(dir, "html", htmlOut)
		runLintV1Format(dir, "colored-line-number", txtOut)
	}

	rawIssues := parseLintJSON(jsonOut)

	// Classify into tabs
	tabs := make(map[string][]string)
	var allErrors, allWarnings []string

	for _, iss := range rawIssues {
		entry := fmt.Sprintf("%s:%d:%d: [%s] %s",
			iss.Pos.Filename, iss.Pos.Line, iss.Pos.Column, iss.FromLinter, iss.Text)

		tab := tabForLinter(iss.FromLinter)

		// Special case: staticcheck split by check ID
		if iss.FromLinter == "staticcheck" {
			tab = classifyStaticcheck(iss.Text)
		}

		tabs[tab] = append(tabs[tab], entry)

		if iss.Severity == "error" {
			allErrors = append(allErrors, entry)
		} else {
			allWarnings = append(allWarnings, entry)
		}
	}

	lastLintResult = &LintResult{Tabs: tabs}

	if len(rawIssues) == 0 {
		r := Pass("no lint issues found")
		lastLintResult.Result = *r
		return r
	}

	var r *Result
	if len(allErrors) > 0 {
		all := append(allErrors, allWarnings...)
		r = Fail(fmt.Sprintf("%d error(s), %d warning(s) — open report.html", len(allErrors), len(allWarnings)), all)
	} else {
		r = Warn(fmt.Sprintf("%d warning(s) — open report.html", len(allWarnings)), append(allErrors, allWarnings...))
	}
	lastLintResult.Result = *r
	return r
}

// classifyStaticcheck maps staticcheck check IDs to tab IDs.
// Text format: "SA5011: ..." or just the message starting with SAnnnn.
func classifyStaticcheck(text string) string {
	for id := range staticcheckCritical {
		// golangci-lint v2 formats: "message (SA5011)" or "SA5011: message"
		if strings.HasPrefix(text, id+":") ||
			strings.Contains(text, "("+id+")") ||
			strings.Contains(text, id) {
			return "panic"
		}
	}
	return "style"
}

// ── Run helpers ───────────────────────────────────────────────────────────────

func isGolangciLintV2() bool {
	out, err := exec.Command("golangci-lint", "--version").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), " version 2.") ||
		strings.Contains(string(out), "v2.")
}

func runLintV2(dir, jsonOut, htmlOut, txtOut string) {
	args := []string{
		"run", "--timeout=120s",
		"--output.json.path=" + jsonOut,
		"--output.html.path=" + htmlOut,
		"--output.text.path=" + txtOut,
		"./...",
	}
	cmd := exec.Command("golangci-lint", args...)
	cmd.Dir = dir
	_ = cmd.Run()
}

func runLintV1Format(dir, format, outFile string) {
	args := []string{"run", "--timeout=120s", "--out-format=" + format, "./..."}
	cmd := exec.Command("golangci-lint", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	_ = os.WriteFile(outFile, out.Bytes(), 0o644)
}

// ── JSON parsing ──────────────────────────────────────────────────────────────

type lintIssue struct {
	Text       string `json:"Text"`
	FromLinter string `json:"FromLinter"`
	Severity   string `json:"Severity"`
	Pos        struct {
		Filename string `json:"Filename"`
		Line     int    `json:"Line"`
		Column   int    `json:"Column"`
	} `json:"Pos"`
}

type lintReport struct {
	Issues []lintIssue `json:"Issues"`
}

func parseLintJSON(jsonPath string) []lintIssue {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil
	}
	var rep lintReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil
	}
	return rep.Issues
}

// ── .golangci.yml ─────────────────────────────────────────────────────────────

func ensureGolangciYml(dir string) {
	ymlPath := filepath.Join(dir, ".golangci.yml")
	if fileExists(ymlPath) {
		return
	}
	_ = os.WriteFile(ymlPath, []byte(defaultGolangciYml), 0o644)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func EnsureGolangciYmlInProject(dir string) { ensureGolangciYml(dir) }

func GolangciYmlContent() string {
	return strings.Replace(defaultGolangciYml,
		"# .golangci.yml — auto-generated by zctl perf scan\n",
		"# .golangci.yml — generated by zctl rpc new\n", 1)
}

// defaultGolangciYml covers all 5 modules in the design doc.
const defaultGolangciYml = `# .golangci.yml — auto-generated by zctl perf scan
# golangci-lint v2 · https://golangci-lint.run/docs/upgrading/v2/
#
# Module coverage:
#   ⚡ Critical  : forcetypeassert nilnil staticcheck errcheck nilerr wrapcheck wastedassign
#   🐛 Bug       : bodyclose sqlclosecheck rowserrcheck contextcheck noctx copyloopvar exhaustive unparam govet
#   🐌 Perf      : prealloc fatcontext gocritic perfsprint unqueryvet  (N+1 handled by zctl)
#   🔒 Security  : gosec bidichk  (CVE handled by govulncheck)
#   📐 Quality   : gocyclo gocognit funlen nestif maintidx mnd goconst lll misspell godox testifylint

version: "2"

run:
  timeout: 120s

formatters:
  enable:
    - gofmt

linters:
  default: none
  enable:
    # ── ⚡ Critical: Panic 风险 ───────────────────────────────────────────────
    - forcetypeassert   # a := b.(T) 无 comma-ok → panic
    - nilnil            # 同时返回 nil error + nil value
    - staticcheck       # SA5011 nil deref 等严重问题

    # ── ⚡ Critical: Error 处理缺陷 ──────────────────────────────────────────
    - errcheck          # _ = func() 丢弃 error
    - nilerr            # 检查了 err!=nil 但 return nil
    - wrapcheck         # 外部包 error 未 wrap，丢失上下文
    - wastedassign      # 赋值后从未使用

    # ── 🐛 Bug: 资源泄漏 ─────────────────────────────────────────────────────
    - bodyclose         # HTTP response.Body 未关闭
    - sqlclosecheck     # sql.Rows / sql.Stmt 未关闭
    - rowserrcheck      # rows.Err() 遍历后未检查

    # ── 🐛 Bug: 并发安全 ─────────────────────────────────────────────────────
    - contextcheck      # context 断链 → goroutine 无法取消
    - noctx             # http.NewRequest 未传 context
    - copyloopvar       # 循环变量被 goroutine/闭包捕获 (Go < 1.22)

    # ── 🐛 Bug: 其他缺陷 ─────────────────────────────────────────────────────
    - exhaustive        # enum switch 非穷举
    - unparam           # 未使用的函数参数
    - govet             # 锁拷贝 / printf格式 / 不可达代码

    # ── 🐌 Performance: 代码层性能 ───────────────────────────────────────────
    # (N+1 由 zctl 自研 AST checker 处理，不在这里)
    - prealloc          # 循环 append → slice 多次扩容
    - fatcontext        # 循环内 context.WithValue → 嵌套context
    - gocritic          # hugeParam 大结构体值传递 + performance tags
    - perfsprint        # fmt.Sprintf 可用更快替代
    - unqueryvet        # SELECT * 全列查询

    # ── 🔒 Security: 代码安全 ────────────────────────────────────────────────
    - gosec             # SQL注入/弱随机数/硬编码密钥/不安全文件权限
    - bidichk           # Trojan Source: 危险 Unicode 双向字符

    # ── 📐 Quality: 复杂度 ───────────────────────────────────────────────────
    - gocyclo           # 圈复杂度 > 10
    - gocognit          # 认知复杂度 > 15
    - funlen            # 函数行数 > 200
    - nestif            # if 嵌套深度 > 5
    - maintidx          # 可维护性指数 < 20

    # ── 📐 Quality: 代码风格 ─────────────────────────────────────────────────
    - mnd               # 魔法数字
    - goconst           # 重复字符串 → 应定义为常量
    - lll               # 行长度 > 120
    - misspell          # 英文拼写错误
    - godox             # TODO/FIXME 残留

    # ── 📐 Quality: 测试质量 ─────────────────────────────────────────────────
    - testifylint       # testify 断言使用规范

  settings:
    govet:
      enable-all: true
    staticcheck:
      checks: ["all", "-SA1019", "-ST1000", "-ST1003", "-ST1020", "-ST1021", "-ST1022"]
    gocritic:
      enabled-tags:
        - performance
        - diagnostic
      disabled-checks:
        - appendAssign
    rowserrcheck:
      packages:
        - database/sql
        - github.com/jmoiron/sqlx
    prealloc:
      simple: true
      range-loops: true
      for-loops: false
    gocyclo:
      min-complexity: 10
    gocognit:
      min-complexity: 15
    funlen:
      lines: 200
      statements: 100
    nestif:
      min-complexity: 5
    maintidx:
      under: 20
    lll:
      line-length: 120
    mnd:
      checks: [argument, case, condition, operation, return]
      ignored-numbers: ["0", "1", "2", "10", "100"]
    goconst:
      min-len: 3
      min-occurrences: 3
    gosec:
      excludes:
        - G115  # integer overflow conversion (too noisy)
    copyloopvar:
      check-alias: true

issues:
  exclude-dirs:
    - ent
    - types
    - migrations
  exclude-rules:
    - path: "_test\\.go"
      linters: [bodyclose, contextcheck, noctx, wrapcheck, errcheck, funlen, gocognit, gocyclo]
    - path: "mock/"
      linters: [govet, gocritic, wrapcheck]
    - path: "cmd/"
      linters: [mnd]
  max-issues-per-linter: 50
  max-same-issues: 10
`
