package checker

import (
	"html/template"
	"os"
	"strings"
)

// readSourceSnippet reads lines [start, end] from a file.
// highlightLines are marked with Highlight=true.
func readSourceSnippet(file string, start, end, hlStart, hlEnd int) []SourceLine {
	if file == "" {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	var out []SourceLine
	for i := start; i <= end; i++ {
		text := ""
		if i-1 < len(lines) {
			text = lines[i-1]
		}
		out = append(out, SourceLine{
			Number:    i,
			Text:      text,
			Highlight: i >= hlStart && i <= hlEnd,
		})
	}
	return out
}

// writeN1HTML generates the N+1 HTML report.
func writeN1HTML(path string, findings []N1Finding, projectDir string) {
	var confirmed, info []N1Finding
	for _, f := range findings {
		if f.Level == LevelFail {
			confirmed = append(confirmed, f)
		} else {
			info = append(info, f)
		}
	}

	data := n1HTMLData{
		ProjectDir: projectDir,
		Confirmed:  confirmed,
		Info:       info,
	}

	tmpl := template.Must(template.New("n1").Funcs(template.FuncMap{
		"inc": func(i int) int { return i + 1 },
		"add": func(a, b int) int { return a + b },
		"not": func(v interface{}) bool {
			if v == nil {
				return true
			}
			switch x := v.(type) {
			case []N1Finding:
				return len(x) == 0
			case bool:
				return !x
			}
			return false
		},
	}).Parse(n1HTMLTemplate))

	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	_ = tmpl.Execute(f, data)
}

type n1HTMLData struct {
	ProjectDir string
	Confirmed  []N1Finding
	Info       []N1Finding
}

const n1HTMLTemplate = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>N+1 Query Report</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f5f5f5;color:#222;padding:20px}
h1{font-size:1.5rem;margin-bottom:4px}
.subtitle{color:#666;font-size:.9rem;margin-bottom:24px}
.summary{display:flex;gap:16px;margin-bottom:28px}
.badge{padding:8px 18px;border-radius:8px;font-weight:700;font-size:1rem}
.badge-fail{background:#fde8e8;color:#c00}
.badge-info{background:#e8f4fd;color:#0066cc}
.badge-pass{background:#e8fde8;color:#006600}
h2{font-size:1.1rem;margin:28px 0 12px;padding-bottom:4px;border-bottom:2px solid #ddd}
h2.fail{border-color:#c00;color:#c00}
h2.info{border-color:#0066cc;color:#0066cc}
.card{background:#fff;border-radius:8px;border:1px solid #e0e0e0;margin-bottom:20px;overflow:hidden}
.card-fail{border-left:4px solid #c00}
.card-info{border-left:4px solid #0066cc}
.card-header{padding:12px 16px;background:#fafafa;border-bottom:1px solid #e8e8e8;display:flex;align-items:center;gap:10px}
.icon-fail{color:#c00;font-size:1.2rem}
.icon-info{color:#0066cc;font-size:1.2rem}
.location{font-family:monospace;font-size:.85rem;color:#555}
.card-body{padding:16px}
.section-title{font-size:.8rem;font-weight:700;text-transform:uppercase;color:#888;letter-spacing:.05em;margin:14px 0 6px}
.section-title:first-child{margin-top:0}
.chain{display:flex;flex-direction:column;gap:4px;margin-bottom:4px}
.chain-step{display:flex;align-items:flex-start;gap:8px;font-size:.85rem}
.chain-arrow{color:#999;flex-shrink:0;padding-top:1px}
.chain-loc{font-family:monospace;color:#0066cc;white-space:nowrap}
.chain-text{color:#333}
.chain-terminal{font-family:monospace;color:#c00;font-weight:700}
.code-block{background:#1e1e1e;border-radius:6px;overflow:auto;font-size:.82rem}
.code-table{width:100%;border-collapse:collapse}
.code-table tr td{padding:1px 0}
.line-num{color:#555;text-align:right;padding:0 12px 0 8px;user-select:none;font-family:monospace;white-space:nowrap;width:48px;border-right:1px solid #333}
.line-code{color:#d4d4d4;font-family:monospace;padding:0 12px;white-space:pre;overflow-x:auto}
.line-highlight{background:#3a2000}
.line-highlight .line-num{color:#f90;border-right-color:#f90}
.line-highlight .line-code{color:#ffd080}
.reason-box{background:#fff8e1;border:1px solid #ffe082;border-radius:6px;padding:12px;font-size:.88rem;line-height:1.6}
.reason-box strong{color:#c55a00}
.suggest-box{background:#f0f8ff;border:1px solid #90caf9;border-radius:6px;padding:12px;font-size:.88rem;line-height:1.6}
.suggest-title{font-weight:700;color:#0066cc;margin-bottom:6px}
.suggest-code{background:#1e1e1e;border-radius:4px;padding:10px;margin-top:8px;font-family:monospace;font-size:.8rem;color:#d4d4d4;white-space:pre;overflow-x:auto}
.divider{height:1px;background:#eee;margin:14px 0}
.empty{color:#888;font-style:italic;padding:12px 0}
</style>
</head>
<body>
<h1>N+1 Query Analysis Report</h1>
<p class="subtitle">Project: {{.ProjectDir}}</p>

<div class="summary">
{{if .Confirmed}}
  <div class="badge badge-fail">❌ Confirmed N+1: {{len .Confirmed}}</div>
{{else}}
  <div class="badge badge-pass">✅ No confirmed N+1</div>
{{end}}
{{if .Info}}
  <div class="badge badge-info">ℹ️ Cross-pkg calls (non-DB): {{len .Info}}</div>
{{end}}
</div>

{{if .Confirmed}}
<h2 class="fail">❌ Confirmed Database N+1 Queries</h2>
<p style="font-size:.85rem;color:#666;margin-bottom:16px">
  以下调用已通过 SSA callgraph 追踪确认：循环体内的调用链最终触达 entgo.io/ent 终端方法，每次循环都会执行一条 SQL。
</p>
{{range $i, $f := .Confirmed}}
<div class="card card-fail">
  <div class="card-header">
    <span class="icon-fail">❌</span>
    <div>
      <div style="font-weight:700">N+1 数据库查询 #{{inc $i}}</div>
      <div class="location">{{$f.ShortFile}} &nbsp;·&nbsp; loop:{{$f.LoopLine}} → call:{{$f.CallLine}}</div>
    </div>
  </div>
  <div class="card-body">

    {{if $f.Chain}}
    <div class="section-title">调用链追踪</div>
    <div class="chain">
      <div class="chain-step">
        <span class="chain-arrow">①</span>
        <span class="chain-loc">{{$f.ShortFile}}:{{$f.CallLine}}</span>
        <span class="chain-text">循环内调用 <code>{{$f.RecvText}}.{{$f.MethodName}}()</code></span>
      </div>
      {{range $j, $step := $f.Chain}}
      <div class="chain-step">
        <span class="chain-arrow">{{add $j 2}} ↓</span>
        <span class="chain-loc">{{$step.ShortFile}}:{{$step.Line}}</span>
        {{if $step.CallText}}
          {{if eq $j (add (len $f.Chain) -1)}}
            <span class="chain-terminal">{{$step.CallText}}</span>
            <span style="color:#c00;font-size:.8rem;margin-left:6px">← SQL 在此执行，每次循环触发一次</span>
          {{else}}
            <span class="chain-text"><code>{{$step.FuncName}}</code></span>
          {{end}}
        {{end}}
      </div>
      {{end}}
    </div>
    {{end}}

    {{if $f.LoopSnippet}}
    <div class="section-title">问题代码（循环位置）</div>
    <div class="code-block">
      <table class="code-table">{{range $f.LoopSnippet}}
        <tr class="{{if .Highlight}}line-highlight{{end}}">
          <td class="line-num">{{.Number}}</td>
          <td class="line-code">{{.Text}}</td>
        </tr>{{end}}
      </table>
    </div>
    {{end}}

    {{if $f.ImplSnippet}}
    <div class="section-title">SQL 触发点（ent 终端）</div>
    <div class="code-block">
      <table class="code-table">{{range $f.ImplSnippet}}
        <tr class="{{if .Highlight}}line-highlight{{end}}">
          <td class="line-num">{{.Number}}</td>
          <td class="line-code">{{.Text}}</td>
        </tr>{{end}}
      </table>
    </div>
    {{end}}

    <div class="divider"></div>

    <div class="reason-box">
      <strong>为什么是问题：</strong><br>
      循环体内第 {{$f.CallLine}} 行调用了 <code>{{$f.RecvText}}.{{$f.MethodName}}()</code>，
      该调用链经 SSA 分析确认最终执行了 ent 的 <code>{{$f.EntTerminal}}</code>（SQL 查询）。<br>
      若循环迭代 N 次，则触发 N 次独立 SQL 查询，即 <strong>N+1 问题</strong>。
      大数据量下会导致数据库连接耗尽、延迟显著上升。
    </div>

    <div class="divider"></div>

    <div class="suggest-box">
      <div class="suggest-title">建议修复方式</div>
      <strong>方案 A（推荐）：批量预加载，循环外一次查询</strong>
      <div class="suggest-code">// 修改前（N+1）
for _, item := range items {
    result, _ := dao.QueryByID(ctx, item.ID)  // N 次 SQL
}

// 修改后（1 次 SQL）
ids := extractIDs(items)                     // 收集所有 ID
resultMap, _ := dao.ListByIDs(ctx, ids)      // 1 次批量查询
for _, item := range items {
    result := resultMap[item.ID]             // 内存 map 查找，0 次 SQL
}</div>
      <strong>方案 B：检查该 DAO 是否已有批量方法</strong><br>
      查看 <code>internal/dao/</code> 中对应的接口文件，
      寻找 <code>ListByXxx</code>、<code>GetByXxxIn</code> 等批量方法。若无，需新增。
    </div>

  </div>
</div>
{{end}}
{{end}}

{{if .Info}}
<h2 class="info">ℹ️ Cross-Package Calls in Loops（非 DB，人工确认）</h2>
<p style="font-size:.85rem;color:#666;margin-bottom:16px">
  以下调用在循环内调用了其他包的方法，SSA 分析未发现 ent 终端（非 DB 操作）。
  列出供人工确认，确保无其他副作用。
</p>
{{range $i, $f := .Info}}
<div class="card card-info">
  <div class="card-header">
    <span class="icon-info">ℹ️</span>
    <div>
      <div style="font-weight:700">Cross-pkg call #{{inc $i}}（已确认非DB）</div>
      <div class="location">{{$f.ShortFile}} &nbsp;·&nbsp; loop:{{$f.LoopLine}} → call:{{$f.CallLine}}</div>
    </div>
  </div>
  <div class="card-body">
    <div style="font-size:.88rem;color:#555;margin-bottom:10px">
      循环内调用 <code>{{$f.RecvText}}.{{$f.MethodName}}()</code>，
      SSA 追踪未发现数据库访问（可能是缓存、内存操作或其他外部调用）。
    </div>
    {{if $f.LoopSnippet}}
    <div class="code-block">
      <table class="code-table">{{range $f.LoopSnippet}}
        <tr class="{{if .Highlight}}line-highlight{{end}}">
          <td class="line-num">{{.Number}}</td>
          <td class="line-code">{{.Text}}</td>
        </tr>{{end}}
      </table>
    </div>
    {{end}}
  </div>
</div>
{{end}}
{{end}}

{{if and (not .Confirmed) (not .Info)}}
<div class="card" style="padding:20px;text-align:center;color:#006600">
  ✅ 未发现 N+1 问题，所有循环内调用均已确认为非 DB 操作。
</div>
{{end}}

<p style="margin-top:32px;font-size:.8rem;color:#aaa;text-align:center">
  Generated by zctl perf scan &nbsp;|&nbsp; Two-phase analysis: AST candidate collection + SSA callgraph tracing
</p>
</body>
</html>
`
