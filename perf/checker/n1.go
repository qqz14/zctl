package checker

import (
	"fmt"
	"go/ast"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ── Public entry ──────────────────────────────────────────────────────────────

// RunN1 runs the two-phase N+1 detector and writes build/perf/n1.html.
//
// Phase 1 (AST, ~100ms):
//   Scan all business .go files for CallExpr inside for/range bodies.
//   No DB/non-DB judgment — collect every call as a candidate.
//
// Phase 2 (go/ssa + callgraph, ~3-8s):
//   For each candidate, load only the packages it touches, build a partial SSA,
//   and do a BFS from the call instruction toward callee functions.
//   A candidate is FAIL iff the call chain reaches any function in
//   "entgo.io/ent" whose name is a terminal method (All/First/Only/Count/IDs/Exist).
//   All other candidates are either INFO (cross-package, non-ent) or silently dropped.
//
// Result levels:
//   FAIL  — confirmed DB query inside loop (ent terminal reachable)
//   INFO  — cross-package call inside loop, no ent terminal found (e.g. cache)
//   PASS  — no candidates or all resolved as non-DB
func RunN1(dir, outDir string) *Result {
	fset := token.NewFileSet()

	// ── Phase 1: AST candidate collection ──
	candidates := collectCandidates(fset, dir)
	if len(candidates) == 0 {
		return Pass("no loop-internal calls detected")
	}

	// ── Phase 2: SSA callgraph trace ──
	findings := traceWithSSA(dir, fset, candidates)

	// ── Phase 3: HTML report ──
	htmlPath := filepath.Join(outDir, "n1.html")
	writeN1HTML(htmlPath, findings, dir)

	var confirmed, info []N1Finding
	for _, f := range findings {
		if f.Level == LevelFail {
			confirmed = append(confirmed, f)
		} else if !isNoiseFinding(f) {
			// Apply the same noise filter as writeN1HTML — confirmed N+1 are NEVER filtered
			info = append(info, f)
		}
	}

	var issues []string
	for _, f := range confirmed {
		issues = append(issues, fmt.Sprintf("%s:%d→%d: [N+1] %s.%s() — ent terminal: %s",
			f.ShortFile, f.LoopLine, f.CallLine, f.RecvText, f.MethodName, f.EntTerminal))
	}
	for _, f := range info {
		issues = append(issues, fmt.Sprintf("%s:%d→%d: [cross-pkg] %s.%s() — non-db, confirm manually",
			f.ShortFile, f.LoopLine, f.CallLine, f.RecvText, f.MethodName))
	}

	if len(confirmed) > 0 {
		return &Result{
			Level: LevelFail,
			Summary: fmt.Sprintf(
				"%d confirmed DB N+1 (ent), %d cross-pkg calls — details: build/perf/n1.html",
				len(confirmed), len(info)),
			Issues: issues,
		}
	}
	if len(info) > 0 {
		return Info(
			fmt.Sprintf("%d cross-pkg calls in loops — non-DB, details: build/perf/n1.html", len(info)),
			issues,
		)
	}
	return Pass("no N+1 patterns — all loop calls resolved as non-DB")
}

// ── Phase 1: Candidate ────────────────────────────────────────────────────────

// Candidate is a call expression found inside a for/range loop body.
type Candidate struct {
	File       string       // absolute file path
	ShortFile  string       // relative to internal/ or pkg/
	LoopLine   int          // line of the for/range keyword
	CallLine   int          // line of the call expression
	RecvText   string       // receiver expression as string, e.g. "l.svcCtx.IamUserRoleDao"
	MethodName string       // method name, e.g. "List"
	PkgDir     string       // directory of the file (used for go/packages load)
	CallPos    token.Pos    // position for SSA matching
}

func collectCandidates(fset *token.FileSet, dir string) []Candidate {
	var candidates []Candidate

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirN1(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || skipFileN1(path) {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		f, err := parseWithGoParser(fset, path, src)
		if err != nil {
			return nil
		}

		shortFile := shortPath(path)
		pkgDir := filepath.Dir(path)

		ast.Inspect(f, func(n ast.Node) bool {
			var body *ast.BlockStmt
			var loopLine int
			switch s := n.(type) {
			case *ast.ForStmt:
				body, loopLine = s.Body, fset.Position(s.For).Line
			case *ast.RangeStmt:
				body, loopLine = s.Body, fset.Position(s.For).Line
			default:
				return true
			}

			// Pre-mark ent builder calls inside append() — not actual DB hits.
			appendBuilders := collectAppendArgCalls(body)

			ast.Inspect(body, func(bn ast.Node) bool {
				call, ok := bn.(*ast.CallExpr)
				if !ok {
					return true
				}
				if appendBuilders[call] {
					return true
				}

				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}

				recv := exprStr(sel.X)
				method := sel.Sel.Name
				callLine := fset.Position(call.Pos()).Line

				// Skip obvious non-calls: stdlib, short local vars
				if skipTrivialCall(recv, method) {
					return true
				}

				candidates = append(candidates, Candidate{
					File:       path,
					ShortFile:  shortFile,
					LoopLine:   loopLine,
					CallLine:   callLine,
					RecvText:   recv,
					MethodName: method,
					PkgDir:     pkgDir,
					CallPos:    call.Pos(),
				})
				return true
			})
			return true
		})
		return nil
	})

	return candidates
}

// ── Filters ───────────────────────────────────────────────────────────────────

func skipDirN1(name string) bool {
	return map[string]bool{
		"ent": true, "types": true, "mock": true,
		"vendor": true, ".git": true, "migrations": true,
		"proto": true, "desc": true, "build": true,
	}[name]
}

func skipFileN1(path string) bool {
	if strings.HasSuffix(path, "_test.go") {
		return true
	}
	// Only scan business logic paths
	return !strings.Contains(path, "/internal/logic/") &&
		!strings.Contains(path, "/internal/dao/impl/") &&
		!strings.Contains(path, "/internal/service/") &&
		!strings.Contains(path, "/pkg/")
}

// skipTrivialCall skips calls that are 100% certain to be non-DB in all possible cases.
// Only pure Go stdlib identifiers and single-char loop vars qualify.
//
// IMPORTANT: Do NOT add log/errcode/enums here — those are filtered later
// in writeN1HTML AFTER Phase 2 confirms they are not DB calls.
// Filtering them here would skip Phase 2 entirely and could mask a DB call
// hidden behind a wrapper with a deceptive name.
func skipTrivialCall(recv, method string) bool {
	lower := strings.ToLower(recv)
	// Pure stdlib packages — these can never wrap a DB call
	trivialRecv := map[string]bool{
		"ctx": true, "context": true, "err": true,
		"strings": true, "strconv": true, "fmt": true,
		"json": true, "bytes": true, "os": true,
		"sort": true, "math": true, "sync": true, "time": true,
		"proto": true, "md": true, "buf": true, "sb": true,
		"w": true, "r": true, "t": true, "c": true, "s": true,
		"b": true, "n": true,
	}
	if trivialRecv[lower] {
		return true
	}
	// Single-char lowercase — loop counter or local err var
	if len(recv) == 1 {
		return true
	}
	_ = method
	return false
}

// ── ent builder pattern (same as before) ─────────────────────────────────────

func collectAppendArgCalls(body *ast.BlockStmt) map[*ast.CallExpr]bool {
	result := make(map[*ast.CallExpr]bool)
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "append" {
			return true
		}
		for _, arg := range call.Args[1:] {
			if argCall, ok := arg.(*ast.CallExpr); ok {
				markCallChain(argCall, result)
			}
		}
		return true
	})
	return result
}

func markCallChain(call *ast.CallExpr, m map[*ast.CallExpr]bool) {
	if call == nil {
		return
	}
	m[call] = true
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if inner, ok := sel.X.(*ast.CallExpr); ok {
			markCallChain(inner, m)
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func exprStr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprStr(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprStr(v.Fun) + "()"
	default:
		return "?"
	}
}

func shortPath(filePath string) string {
	for _, anchor := range []string{"/internal/", "/pkg/"} {
		if idx := strings.Index(filePath, anchor); idx >= 0 {
			return filePath[idx+1:]
		}
	}
	return filepath.Base(filePath)
}
