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

// EntFullScanFinding is a .Query()...All() call that may scan without pagination.
type EntFullScanFinding struct {
	ShortFile  string
	Line       int
	FuncName   string
	ChainText  string
	HasWhere   bool // has WHERE constraint (lower risk)
	HasLimit   bool // has Limit either in chain or in variable flow
}

var lastEntFullScanFindings []EntFullScanFinding

// RunEntFullScan scans for ent .Query()...All() patterns without .Limit().
//
// Two detection strategies:
//  1. Inline chain:  d.cli.Query().Where(...).All(ctx)
//     → collect entire chain in one ast.CallExpr walk
//  2. Variable flow: query = d.cli.Query(); ...; query = query.Limit(n); list, _ = query.All(ctx)
//     → track all method names assigned to the same variable within the enclosing function body
//
// Level: WARN when no WHERE constraints; INFO when WHERE is present (bounded result possible).
func RunEntFullScan(dir string) *Result {
	fset := token.NewFileSet()
	var findings []EntFullScanFinding

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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if !strings.Contains(path, "/internal/dao/impl/") &&
			!strings.Contains(path, "/internal/logic/") &&
			!strings.Contains(path, "/internal/service/") &&
			!strings.Contains(path, "/pkg/") {
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
		short := shortPath(path)

		// Scan each function declaration independently for variable-flow analysis
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			funcName := fd.Name.Name
			findings = append(findings, scanFuncForFullScan(fd.Body, fset, short, funcName)...)
		}
		return nil
	})

	lastEntFullScanFindings = findings

	if len(findings) == 0 {
		return Pass("no ent full-table scan detected")
	}

	// Split: WARN (no where) vs INFO (has where but no limit)
	var warnIssues, infoIssues []string
	for _, f := range findings {
		loc := fmt.Sprintf("%s:%d [%s]", f.ShortFile, f.Line, f.FuncName)
		if f.HasWhere {
			infoIssues = append(infoIssues, fmt.Sprintf(
				"%s: [ent-fullscan] %s — .All() without .Limit(), WHERE present (bounded if filter always set)",
				loc, f.ChainText))
		} else {
			warnIssues = append(warnIssues, fmt.Sprintf(
				"%s: [ent-fullscan] %s — .All() without .Limit() and no WHERE, potential full table scan",
				loc, f.ChainText))
		}
	}

	allIssues := append(warnIssues, infoIssues...)
	level := LevelInfo
	if len(warnIssues) > 0 {
		level = LevelWarn
	}
	return &Result{
		Level: level,
		Summary: fmt.Sprintf(
			"%d .All() without .Limit() (%d no-where WARN, %d with-where INFO) — human review",
			len(findings), len(warnIssues), len(infoIssues)),
		Issues: allIssues,
	}
}

// ── Per-function scanner ──────────────────────────────────────────────────────

// queryVarState tracks what methods have been chained onto a query variable.
type queryVarState struct {
	hasLimit bool
	hasWhere bool
	hasQuery bool // confirmed originated from .Query()
}

// scanFuncForFullScan analyses one function body for ent full-scan patterns.
// It builds a variable-flow map: varName → set of methods assigned to it.
func scanFuncForFullScan(body *ast.BlockStmt, fset *token.FileSet, short, funcName string) []EntFullScanFinding {
	// Step 1: collect all assignments "query = query.Method(...)" or
	// "query := d.cli.Query()..." within the function.
	varStates := collectQueryVarStates(body)

	var findings []EntFullScanFinding

	// Step 2: find all .All(ctx) calls in the function
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "All" {
			return true
		}

		pos := fset.Position(call.Pos())

		// Case A: inline chain — d.cli.Query().Where().All()
		chain := collectMethodChain(call)
		if chainHas(chain, "Query") {
			hasLimit := chainHas(chain, "Limit")
			hasWhere := chainHas(chain, "Where")
			if !hasLimit {
				findings = append(findings, EntFullScanFinding{
					ShortFile: short, Line: pos.Line, FuncName: funcName,
					ChainText: renderChainMethods(chain),
					HasWhere:  hasWhere, HasLimit: false,
				})
			}
			return true
		}

		// Case B: variable-based — query.All(ctx) where query was built piece by piece
		recvVar := exprStr(sel.X)
		if state, ok := varStates[recvVar]; ok && state.hasQuery {
			if !state.hasLimit {
				findings = append(findings, EntFullScanFinding{
					ShortFile: short, Line: pos.Line, FuncName: funcName,
					ChainText: recvVar + ".All(ctx)",
					HasWhere:  state.hasWhere, HasLimit: false,
				})
			}
		}
		return true
	})

	return findings
}

// collectQueryVarStates scans a function body and builds a map of
// variable name → methods accumulated (query, where, limit, etc.)
// by tracking assignments like:
//
//	query := d.cli.Query()
//	query = query.Where(...)
//	query = query.Limit(n)
func collectQueryVarStates(body *ast.BlockStmt) map[string]*queryVarState {
	states := make(map[string]*queryVarState)

	ast.Inspect(body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range stmt.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || i >= len(stmt.Rhs) {
					continue
				}
				varName := ident.Name
				// Collect all method names in the RHS chain
				methods := collectMethodChain(stmt.Rhs[i])

				if len(methods) == 0 {
					continue
				}

				// Initialize state if this looks like a query variable
				if _, exists := states[varName]; !exists {
					// Only track if RHS involves .Query() or an existing query var
					if chainHas(methods, "Query") || isKnownQueryVar(stmt.Rhs[i], states) {
						states[varName] = &queryVarState{}
					}
				}

				st := states[varName]
				if st == nil {
					continue
				}

				for _, m := range methods {
					switch m.Method {
					case "Query":
						st.hasQuery = true
					case "Where":
						st.hasWhere = true
					case "Limit", "Offset":
						st.hasLimit = true
					}
				}
			}
		}
		return true
	})

	return states
}

// isKnownQueryVar checks if an expression is a known query variable (for propagation).
// e.g.: query = query.Where(...) — the RHS starts with "query" which is already tracked.
func isKnownQueryVar(expr ast.Expr, states map[string]*queryVarState) bool {
	// Walk the chain to find the innermost receiver
	cur := expr
	for {
		c, ok := cur.(*ast.CallExpr)
		if !ok {
			break
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			break
		}
		cur = sel.X
	}
	// cur is now the root receiver (an Ident like "query")
	if ident, ok := cur.(*ast.Ident); ok {
		_, known := states[ident.Name]
		return known
	}
	return false
}

// ── helpers ───────────────────────────────────────────────────────────────────

func chainHas(chain []methodChainNode, method string) bool {
	for _, n := range chain {
		if n.Method == method {
			return true
		}
	}
	return false
}

// methodChainNode is one segment in a chained call.
type methodChainNode struct {
	Method string
}

// collectMethodChain walks a chained call expression and returns method names
// from outermost to innermost, e.g. All→Order→Where→Query.
func collectMethodChain(call ast.Expr) []methodChainNode {
	var chain []methodChainNode
	cur := call
	for {
		c, ok := cur.(*ast.CallExpr)
		if !ok {
			break
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			break
		}
		chain = append(chain, methodChainNode{Method: sel.Sel.Name})
		cur = sel.X
	}
	return chain
}

func renderChainMethods(chain []methodChainNode) string {
	// chain is [All, Order, Where, Query] → display as Query().Where().Order().All()
	parts := make([]string, len(chain))
	for i, n := range chain {
		parts[len(chain)-1-i] = n.Method + "()"
	}
	if len(parts) > 5 {
		return parts[0] + "." + parts[1] + "...All()"
	}
	return strings.Join(parts, ".")
}

func renderChain(chain []methodChainNode) string {
	return renderChainMethods(chain)
}

// ── SQL perf from logic review ─────────────────────────────────────────────────

// SQLPerfFinding is one SQL statement with a potential full-scan risk,
// derived directly from the logic review IONode SQL strings.
type SQLPerfFinding struct {
	DAO      string // e.g. "iamappOceanBaseDao.List"
	Logic    string // e.g. "GetAllUserList"
	File     string // ShortFile of the DAO call site
	Line     int
	SQL      string
	HasWhere bool // SQL contains a WHERE clause
	HasLimit bool // SQL contains LIMIT
}

// RunSQLPerfFromLogicReview derives SQL performance findings from the already-computed
// logic review IONodes. No file walking — uses the SQL strings already extracted by
// call graph + implIdx AST analysis.
func RunSQLPerfFromLogicReview(r *LogicReviewResult) *Result {
	if r == nil || len(r.Methods) == 0 {
		return Skip("logic review not available")
	}

	seen := map[string]bool{} // deduplicate by DAO.Method
	var warn, info []SQLPerfFinding

	for _, m := range r.Methods {
		for _, op := range m.Ops {
			if op.Kind != IOKindDB || op.SQL == "" {
				continue
			}
			sql := op.SQL
			upper := strings.ToUpper(sql)

			// Only SELECT statements can full-scan; skip INSERT/UPDATE/DELETE
			if !strings.HasPrefix(upper, "SELECT") {
				continue
			}

			hasWhere := strings.Contains(upper, " WHERE ")
			hasLimit := strings.Contains(upper, " LIMIT ")

			// No LIMIT → potential full scan
			if hasLimit {
				continue
			}

			key := op.Receiver + "." + op.Method
			if seen[key] {
				continue
			}
			seen[key] = true

			f := SQLPerfFinding{
				DAO:      key,
				Logic:    m.Method,
				File:     op.ShortFile,
				Line:     op.Line,
				SQL:      sql,
				HasWhere: hasWhere,
				HasLimit: false,
			}
			if hasWhere {
				info = append(info, f)
			} else {
				warn = append(warn, f)
			}
		}
	}

	if len(warn)+len(info) == 0 {
		return Pass("no full-scan risk detected")
	}

	// Build issue strings for nav badge / tabs
	var issues []string
	for _, f := range warn {
		issues = append(issues, fmt.Sprintf("%s:%d [%s] no WHERE no LIMIT — %s",
			f.File, f.Line, f.DAO, f.SQL))
	}
	for _, f := range info {
		issues = append(issues, fmt.Sprintf("%s:%d [%s] WHERE but no LIMIT — %s",
			f.File, f.Line, f.DAO, f.SQL))
	}

	level := LevelInfo
	if len(warn) > 0 {
		level = LevelWarn
	}

	// Store for renderSQLPerfFromFindings
	lastSQLPerfWarn = warn
	lastSQLPerfInfo = info

	return &Result{
		Level:   level,
		Summary: fmt.Sprintf("SQL perf: %d no-WHERE warn, %d has-WHERE info (from logic review)", len(warn), len(info)),
		Issues:  issues,
	}
}

var lastSQLPerfWarn []SQLPerfFinding
var lastSQLPerfInfo []SQLPerfFinding
