package checker

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReportData is the data passed to the main report HTML template.
type ReportData struct {
	ProjectName string
	ScanTime    string
	Elapsed     string
	Sections    []ReportSection
}

// ReportSection represents one checker's result in the report.
type ReportSection struct {
	ID       string // anchor id, e.g. "n1", "lint"
	Title    string
	Level    Level
	Summary  string
	Issues   []string
	IframeURL string // if non-empty, a sub-page link is shown instead of inline issues
	HasIframe bool
}

// WriteReportHTML generates the single unified report.html.
// Layout: fixed left nav + right content with sections.
// N+1 and lint sections use iframes to embed their own HTML pages.
func WriteReportHTML(outDir, projectDir string, results map[string]*Result, elapsed time.Duration) {
	projectName := filepath.Base(projectDir)

	sections := []ReportSection{
		makeSection("fmt", "gofmt", results["fmt"]),
		makeSection("vet", "go vet", results["vet"]),
		makeSection("lint", "golangci-lint", results["lint"]),
		makeSection("vuln", "govulncheck (CVE)", results["vuln"]),
		makeSection("escape", "Escape Analysis", results["escape"]),
		makeSection("n1", "N+1 Query Scan", results["n1"]),
	}

	// Mark iframe sections
	for i := range sections {
		switch sections[i].ID {
		case "lint":
			if _, err := os.Stat(filepath.Join(outDir, "lint.html")); err == nil {
				sections[i].IframeURL = "lint.html"
				sections[i].HasIframe = true
			}
		case "n1":
			if _, err := os.Stat(filepath.Join(outDir, "n1.html")); err == nil {
				sections[i].IframeURL = "n1.html"
				sections[i].HasIframe = true
			}
		}
	}

	data := ReportData{
		ProjectName: projectName,
		ScanTime:    time.Now().Format("2006-01-02 15:04:05"),
		Elapsed:     elapsed.Round(time.Millisecond).String(),
		Sections:    sections,
	}

	funcMap := template.FuncMap{
		"levelClass": func(l Level) string {
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
		},
		"levelIcon": func(l Level) string {
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
		},
		"issueCount": func(issues []string) string {
			n := 0
			for _, s := range issues {
				if s != "" && !strings.HasPrefix(s, "──") {
					n++
				}
			}
			return fmt.Sprintf("%d", n)
		},
		"toUpper": strings.ToUpper,
		"hasSepPrefix": func(s string) bool {
			return strings.HasPrefix(s, "──") || strings.HasPrefix(s, "--")
		},
		"issueClass": func(s string) string {
			// Color coding: lines with [N+1] or error keywords → fail, warnings → warn
			lower := strings.ToLower(s)
			if strings.Contains(lower, "[n+1]") || strings.Contains(lower, "❌") ||
				strings.Contains(lower, "error") || strings.Contains(lower, "fail") {
				return "fail"
			}
			if strings.Contains(lower, "warn") || strings.Contains(lower, "⚠") {
				return "warn"
			}
			if strings.Contains(lower, "ℹ") || strings.Contains(lower, "[cross-pkg]") ||
				strings.Contains(lower, "info") {
				return "info"
			}
			return ""
		},
	}

	tmpl := template.Must(template.New("report").Funcs(funcMap).Parse(reportHTMLTemplate))
	f, err := os.Create(filepath.Join(outDir, "report.html"))
	if err != nil {
		return
	}
	defer f.Close()
	_ = tmpl.Execute(f, data)
}

func makeSection(id, title string, r *Result) ReportSection {
	if r == nil {
		return ReportSection{ID: id, Title: title, Level: LevelSkip, Summary: "skipped"}
	}
	return ReportSection{
		ID:      id,
		Title:   title,
		Level:   r.Level,
		Summary: r.Summary,
		Issues:  r.Issues,
	}
}

const reportHTMLTemplate = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>Perf Report · {{.ProjectName}}</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
html,body{height:100%;overflow:hidden}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;display:flex;height:100vh;background:#f5f5f5}

/* ── Left Nav ── */
.nav{width:220px;min-width:220px;background:#1a1a2e;color:#ccc;display:flex;flex-direction:column;height:100vh;overflow-y:auto}
.nav-header{padding:18px 16px 12px;border-bottom:1px solid #2a2a4a}
.nav-project{font-size:1rem;font-weight:700;color:#fff;margin-bottom:2px}
.nav-meta{font-size:.72rem;color:#888}
.nav-section{padding:8px 0 4px 14px;font-size:.7rem;font-weight:700;text-transform:uppercase;letter-spacing:.1em;color:#555;margin-top:8px}
.nav-item{display:flex;align-items:center;gap:8px;padding:8px 16px;cursor:pointer;font-size:.88rem;color:#aaa;border-left:3px solid transparent;transition:all .15s;text-decoration:none}
.nav-item:hover{background:#262650;color:#fff;border-left-color:#555}
.nav-item.active{background:#1e1e4e;color:#fff;border-left-color:#6c8fff}
.nav-item.active-fail{background:#2e1010;color:#ff6b6b;border-left-color:#c00}
.nav-item.active-warn{background:#2e2510;color:#ffd080;border-left-color:#f90}
.nav-badge{margin-left:auto;font-size:.7rem;padding:1px 6px;border-radius:10px;font-weight:700}
.nb-fail{background:#c00;color:#fff}
.nb-warn{background:#f90;color:#000}
.nb-pass{background:#090;color:#fff}
.nb-info{background:#0066cc;color:#fff}
.nb-skip{background:#555;color:#ccc}

/* ── Main Content ── */
.main{flex:1;display:flex;flex-direction:column;overflow:hidden}
.content{flex:1;overflow-y:auto;padding:24px 28px}

/* ── Overview ── */
.overview-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(180px,1fr));gap:12px;margin-bottom:28px}
.ov-card{background:#fff;border-radius:8px;padding:14px 16px;border:1px solid #e0e0e0;cursor:pointer;transition:box-shadow .15s}
.ov-card:hover{box-shadow:0 2px 12px rgba(0,0,0,.1)}
.ov-card.fail{border-left:4px solid #c00}
.ov-card.warn{border-left:4px solid #f90}
.ov-card.pass{border-left:4px solid #090}
.ov-card.info{border-left:4px solid #0066cc}
.ov-card.skip{border-left:4px solid #999}
.ov-title{font-size:.8rem;color:#888;margin-bottom:4px}
.ov-status{font-size:1rem;font-weight:700}
.ov-status.fail{color:#c00}
.ov-status.warn{color:#f90}
.ov-status.pass{color:#090}
.ov-status.info{color:#0066cc}
.ov-status.skip{color:#999}
.ov-count{font-size:.8rem;color:#999;margin-top:2px}

/* ── Section panels ── */
.panel{display:none}
.panel.active{display:block}
.panel-title{font-size:1.1rem;font-weight:700;margin-bottom:4px;display:flex;align-items:center;gap:8px}
.panel-title.fail{color:#c00}
.panel-title.warn{color:#c60}
.panel-title.pass{color:#060}
.panel-title.info{color:#0066cc}
.panel-summary{font-size:.88rem;color:#666;margin-bottom:16px}
.issue-list{background:#fff;border-radius:8px;border:1px solid #e0e0e0;overflow:hidden}
.issue-item{padding:8px 16px;border-bottom:1px solid #f0f0f0;font-family:monospace;font-size:.82rem;line-height:1.5}
.issue-item:last-child{border-bottom:none}
.issue-item.fail{color:#900;background:#fff8f8}
.issue-item.warn{color:#660;background:#fffef0}
.issue-item.info{color:#004;background:#f8f8ff}
.issue-item.sep{color:#888;font-style:italic;background:#fafafa}
.iframe-wrap{width:100%;border-radius:8px;border:1px solid #e0e0e0;overflow:hidden;background:#fff}
.iframe-wrap iframe{width:100%;border:none;display:block}
.open-btn{display:inline-flex;align-items:center;gap:6px;margin-bottom:12px;padding:6px 14px;background:#f0f4ff;border:1px solid #c0d0ff;border-radius:6px;font-size:.85rem;color:#0066cc;cursor:pointer;text-decoration:none}
.open-btn:hover{background:#e0ecff}
h1{font-size:1.3rem;margin-bottom:4px}
.meta{font-size:.82rem;color:#888;margin-bottom:20px}
.pass-msg{background:#e8fde8;border:1px solid #90cca0;border-radius:8px;padding:16px;color:#060;font-weight:600;text-align:center;font-size:.95rem}
</style>
</head>
<body>

<!-- Left nav -->
<nav class="nav">
  <div class="nav-header">
    <div class="nav-project">{{.ProjectName}}</div>
    <div class="nav-meta">{{.ScanTime}}</div>
    <div class="nav-meta">elapsed: {{.Elapsed}}</div>
  </div>
  <div class="nav-section">Overview</div>
  <a class="nav-item active" id="nav-overview" onclick="showPanel('overview', this)">
    📊 Summary
  </a>
  <div class="nav-section">Checks</div>
  {{range .Sections}}
  <a class="nav-item" id="nav-{{.ID}}" onclick="showPanel('{{.ID}}', this)">
    {{levelIcon .Level}} {{.Title}}
    {{if gt (len .Issues) 0}}
      <span class="nav-badge nb-{{levelClass .Level}}">{{issueCount .Issues}}</span>
    {{end}}
  </a>
  {{end}}
</nav>

<!-- Main content -->
<div class="main">
  <div class="content">

    <!-- Overview panel -->
    <div id="panel-overview" class="panel active">
      <h1>Perf Scan Report — {{.ProjectName}}</h1>
      <div class="meta">{{.ScanTime}} &nbsp;·&nbsp; elapsed: {{.Elapsed}}</div>
      <div class="overview-grid">
        {{range .Sections}}
        <div class="ov-card {{levelClass .Level}}" onclick="showPanelById('{{.ID}}')">
          <div class="ov-title">{{.Title}}</div>
          <div class="ov-status {{levelClass .Level}}">{{levelIcon .Level}} {{levelClass .Level | toUpper}}</div>
          {{if gt (len .Issues) 0}}<div class="ov-count">{{issueCount .Issues}} issue(s)</div>{{end}}
        </div>
        {{end}}
      </div>
    </div>

    <!-- Per-check panels -->
    {{range .Sections}}
    <div id="panel-{{.ID}}" class="panel">
      <div class="panel-title {{levelClass .Level}}">
        {{levelIcon .Level}} {{.Title}}
      </div>
      <div class="panel-summary">{{.Summary}}</div>

      {{if .HasIframe}}
      <a class="open-btn" href="{{.IframeURL}}" target="_blank">↗ 在新标签页打开</a>
      <div class="iframe-wrap">
        <iframe src="{{.IframeURL}}" id="iframe-{{.ID}}" onload="resizeIframe(this)"></iframe>
      </div>
      {{else if eq (len .Issues) 0}}
        <div class="pass-msg">✅ No issues found</div>
      {{else}}
      <div class="issue-list">
        {{range .Issues}}
        {{if eq . ""}}
        {{else if hasSepPrefix .}}
        <div class="issue-item sep">{{.}}</div>
        {{else}}
        <div class="issue-item {{issueClass .}}">{{.}}</div>
        {{end}}
        {{end}}
      </div>
      {{end}}
    </div>
    {{end}}

  </div>
</div>

<script>
function showPanel(id, el) {
  document.querySelectorAll('.panel').forEach(p => p.classList.remove('active'));
  document.querySelectorAll('.nav-item').forEach(n => {
    n.classList.remove('active','active-fail','active-warn');
  });
  document.getElementById('panel-' + id).classList.add('active');
  if (el) {
    var cls = 'active';
    var icon = el.textContent.trim()[0];
    if (icon === '❌') cls = 'active-fail';
    else if (icon === '⚠') cls = 'active-warn';
    el.classList.add(cls);
  }
}
function showPanelById(id) {
  var nav = document.getElementById('nav-' + id);
  showPanel(id, nav);
}
function resizeIframe(iframe) {
  try {
    var h = iframe.contentWindow.document.body.scrollHeight;
    iframe.style.height = Math.max(h, 400) + 'px';
  } catch(e) {
    iframe.style.height = '600px';
  }
}
// Make toUpper available in template via JS (not needed — handled in Go template below)
</script>
</body>
</html>
`
