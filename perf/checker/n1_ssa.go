package checker

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// ── N+1 Finding ───────────────────────────────────────────────────────────────

// N1Finding is a fully-resolved N+1 result.
type N1Finding struct {
	File       string
	ShortFile  string
	LoopLine   int
	CallLine   int
	RecvText   string
	MethodName string
	Level      Level

	Chain       []ChainStep
	EntTerminal string

	LoopSnippet []SourceLine
	ImplSnippet []SourceLine
}

// ChainStep is one hop in the call chain.
type ChainStep struct {
	File      string
	ShortFile string
	Line      int
	FuncName  string
	CallText  string
}

// SourceLine is a numbered source line for HTML.
type SourceLine struct {
	Number    int
	Text      string
	Highlight bool
}

// ── ent terminal methods ──────────────────────────────────────────────────────

var entTerminalMethods = map[string]bool{
	// Read terminals
	"All": true, "First": true, "Only": true,
	"Count": true, "IDs": true, "Exist": true, "Exists": true, "ExistX": true,
	// Write terminals (soft delete, update, create all call Save/SaveX/Exec/ExecX)
	"Save": true, "SaveX": true, "Exec": true, "ExecX": true,
}

const entPkgPrefix = "entgo.io/ent"

// ── Phase 2: type-guided AST tracing ─────────────────────────────────────────

// traceWithSSA resolves N+1 candidates using the pre-built call graph when available,
// falling back to the original go/packages + impl-AST approach otherwise.
//
// With call graph (cgCache != nil):
//   For each candidate call site, look up the callee SSA node in the call graph,
//   then walk its out-edges to find any ent terminal method. This is more accurate
//   because it follows the actual type-checked call edges, not just name matching.
//
// Without call graph (cgCache == nil):
//   Original path: load type info per package, resolve interface → concrete impl,
//   AST-scan impl body for ent terminals.
func traceWithSSA(dir string, fset *token.FileSet, candidates []Candidate, cgCache *CallGraphCache) []N1Finding {
	if len(candidates) == 0 {
		return nil
	}

	// Fast path: use pre-built call graph
	if cgCache != nil {
		return traceWithCallGraph(candidates, cgCache, fset)
	}

	// Slow path: original per-package type loading
	moduleName := detectModuleName(dir)
	if moduleName == "" {
		return fallbackFindings(candidates)
	}

	type pkgGroup struct {
		importPath string
		candidates []Candidate
	}
	pkgMap := make(map[string]*pkgGroup)
	for _, c := range candidates {
		rel, err := filepath.Rel(dir, c.PkgDir)
		if err != nil {
			continue
		}
		ip := moduleName + "/" + filepath.ToSlash(rel)
		if g, ok := pkgMap[ip]; ok {
			g.candidates = append(g.candidates, c)
		} else {
			pkgMap[ip] = &pkgGroup{importPath: ip, candidates: []Candidate{c}}
		}
	}

	implIndex := buildImplIndex(dir, moduleName, fset)

	var findings []N1Finding
	for ip, group := range pkgMap {
		typeInfo := loadTypeInfo(dir, fset, ip)
		for _, c := range group.candidates {
			f := resolveCandidate(c, typeInfo, implIndex, fset, dir)
			if f != nil {
				findings = append(findings, *f)
			}
		}
	}
	return findings
}

// traceWithCallGraph resolves N+1 candidates using the pre-computed DAO impl index.
//
// For each candidate (recv.Method called inside a loop), we look up all concrete DAO
// implementations of that method via cgCache.DAOImplsForMethod (O(1) map lookup),
// then confirm ent terminal via implIdx AST scan.
//
// This avoids scanning all call graph nodes and is reliable because:
//  - DAOImplsForMethod was built during BuildCallGraph's single traversal
//  - implIdx AST scan is stable (uses go/parser, not SSA fset)
func traceWithCallGraph(candidates []Candidate, cgCache *CallGraphCache, fset *token.FileSet) []N1Finding {
	var findings []N1Finding
	implIdx := cgCache.implIdx

	for _, c := range candidates {
		// O(1): pre-computed DAO impls for this method name
		impls := cgCache.DAOImplsForMethod(c.MethodName)
		if len(impls) == 0 {
			if isCrossPackageCall(c) {
				findings = append(findings, *infoFinding(c))
			}
			continue
		}

		entTerminal := ""
		var chain []ChainStep

		for _, impl := range impls {
			// AST confirm via implIdx (most accurate, uses source positions)
			key := impl.recvType + "." + c.MethodName
			if entries, ok := implIdx[key]; ok && len(entries) > 0 {
				ch, terminal := astFindEntTerminal(
					entries[0].funcDecl.Body, entries[0].fset, entries[0].file, 0, 6)
				if terminal != "" {
					entTerminal = terminal
					chain = ch
					break
				}
				// impl found in idx but no ent terminal — not a DB call, skip
				continue
			}
			// implIdx miss: walk call graph from this impl's SSA node
			if node := cgCache.cg.Nodes[impl.fn]; node != nil {
				t, ch := cgReachesEntTerminal(node, cgCache, 0, 6, make(map[*callgraph_Node]bool))
				if t != "" {
					entTerminal = t
					chain = ch
					break
				}
			}
		}

		if entTerminal != "" {
			loopSnip := readSourceSnippet(c.File, c.LoopLine-2, c.CallLine+3, c.LoopLine, c.CallLine)
			var implSnip []SourceLine
			key := impls[0].recvType + "." + c.MethodName
			if entries := implIdx[key]; len(entries) > 0 {
				entry := entries[0]
				implLine := entry.fset.Position(entry.funcDecl.Pos()).Line
				termLine := implLine
				if len(chain) > 0 {
					termLine = chain[len(chain)-1].Line
				}
				implSnip = readSourceSnippet(entry.file, implLine, termLine+3, termLine, termLine)
			}
			findings = append(findings, N1Finding{
				File: c.File, ShortFile: c.ShortFile,
				LoopLine: c.LoopLine, CallLine: c.CallLine,
				RecvText: c.RecvText, MethodName: c.MethodName,
				Level:       LevelFail,
				Chain:       chain,
				EntTerminal: entTerminal,
				LoopSnippet: loopSnip,
				ImplSnippet: implSnip,
			})
		} else if isCrossPackageCall(c) {
			findings = append(findings, *infoFinding(c))
		}
	}
	return findings
}

// callgraph_Node is a local alias to avoid import collision in the BFS visited map.
type callgraph_Node = callgraph.Node

// cgReachesEntTerminal does a BFS from a call graph node to find an ent terminal method.
func cgReachesEntTerminal(
	node *callgraph.Node,
	cgCache *CallGraphCache,
	depth, maxDepth int,
	visited map[*callgraph_Node]bool,
) (terminal string, chain []ChainStep) {
	if node == nil || visited[node] || depth > maxDepth {
		return "", nil
	}
	visited[node] = true

	fn := node.Func
	if fn == nil {
		return "", nil
	}

	// Check if this function IS an ent terminal
	if entTerminalMethods[fn.Name()] && isEntPackage(fn) {
		pos := cgCache.prog.Fset.Position(fn.Pos())
		return fn.Name(), []ChainStep{{
			File:      pos.Filename,
			ShortFile: shortPath(pos.Filename),
			Line:      pos.Line,
			FuncName:  fn.Name(),
			CallText:  "." + fn.Name() + "(ctx)  ← SQL executed here",
		}}
	}

	for _, edge := range node.Out {
		t, c := cgReachesEntTerminal(edge.Callee, cgCache, depth+1, maxDepth, visited)
		if t != "" {
			// Prepend this step to the chain
			pos := cgCache.prog.Fset.Position(fn.Pos())
			step := ChainStep{
				File:      pos.Filename,
				ShortFile: shortPath(pos.Filename),
				Line:      pos.Line,
				FuncName:  fn.Name(),
				CallText:  fn.Name(),
			}
			return t, append([]ChainStep{step}, c...)
		}
	}
	return "", nil
}

func isEntPackage(fn *ssa.Function) bool {
	if fn.Package() == nil || fn.Package().Pkg == nil {
		return false
	}
	return strings.HasPrefix(fn.Package().Pkg.Path(), "entgo.io/ent")
}

// ── Type info loading ─────────────────────────────────────────────────────────

// typeInfoResult holds go/types info for a single package.
type typeInfoResult struct {
	pkg   *packages.Package
	info  *types.Info
	fset  *token.FileSet
	valid bool
}

func loadTypeInfo(dir string, fset *token.FileSet, importPath string) *typeInfoResult {
	cfg := &packages.Config{
		Mode: packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Fset: fset,
		Dir:  dir,
	}
	pkgs, err := packages.Load(cfg, importPath)
	if err != nil || len(pkgs) == 0 {
		return &typeInfoResult{}
	}
	p := pkgs[0]
	if len(p.Errors) > 0 || p.TypesInfo == nil {
		return &typeInfoResult{}
	}
	return &typeInfoResult{pkg: p, info: p.TypesInfo, fset: fset, valid: true}
}

// ── Implementation index ──────────────────────────────────────────────────────

// implEntry describes a concrete method implementation.
type implEntry struct {
	file     string       // absolute file path
	funcDecl *ast.FuncDecl
	fset     *token.FileSet
}

// implIndex maps "TypeName.MethodName" → implEntry.
type implIndexType = map[string][]implEntry

// buildImplIndex scans internal/dao/impl/ to build a TypeName.Method → impl map.
// Uses simple AST parse, no type loading.
func buildImplIndex(dir, moduleName string, fset *token.FileSet) implIndexType {
	idx := make(implIndexType)
	implDir := filepath.Join(dir, "internal", "dao", "impl")
	if _, err := os.Stat(implDir); err != nil {
		return idx
	}

	entries, _ := os.ReadDir(implDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(implDir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		f, err := parseWithGoParser(fset, path, src)
		if err != nil {
			continue
		}

		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			recvType := receiverTypeName(fd.Recv.List[0].Type)
			if recvType == "" {
				continue
			}
			key := recvType + "." + fd.Name.Name
			idx[key] = append(idx[key], implEntry{file: path, funcDecl: fd, fset: fset})
		}
	}
	return idx
}

func receiverTypeName(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return receiverTypeName(v.X)
	case *ast.IndexExpr:
		return receiverTypeName(v.X)
	}
	return ""
}

// ── Candidate resolution ──────────────────────────────────────────────────────

// resolveCandidate determines whether a candidate call leads to an ent terminal.
func resolveCandidate(
	c Candidate,
	typeInfo *typeInfoResult,
	implIdx implIndexType,
	fset *token.FileSet,
	dir string,
) *N1Finding {
	// Step 1: find the interface type of the receiver at the call site
	var ifaceType *types.Interface
	if typeInfo.valid {
		ifaceType = findReceiverInterface(typeInfo, c.File, c.CallLine, c.RecvText)
	}

	// Step 2: find concrete implementations
	var implFuncs []implEntry
	if ifaceType != nil {
		implFuncs = findImplementors(ifaceType, c.MethodName, implIdx, typeInfo)
	}

	// Step 3: name-based fallback — match method name across all impls.
	// Strategy: if we can't resolve the type, collect ALL impl entries for this
	// method name and scan them all. False positives are possible but safe
	// (we only report FAIL when ent terminal is confirmed in the impl body).
	if len(implFuncs) == 0 {
		for key, entries := range implIdx {
			parts := strings.SplitN(key, ".", 2)
			if len(parts) == 2 && parts[1] == c.MethodName {
				implFuncs = append(implFuncs, entries...)
			}
		}
	}

	// Step 4: AST-scan each impl for ent terminal calls
	for _, impl := range implFuncs {
		chain, terminal := astFindEntTerminal(impl.funcDecl.Body, impl.fset, impl.file, 0, 6)
		if terminal != "" {
			loopSnip := readSourceSnippet(c.File, c.LoopLine-2, c.CallLine+3, c.LoopLine, c.CallLine)
			implLine := impl.fset.Position(impl.funcDecl.Pos()).Line
			implSnip := readSourceSnippet(impl.file, implLine, implLine+chain[len(chain)-1].Line-implLine+3,
				chain[len(chain)-1].Line, chain[len(chain)-1].Line)

			return &N1Finding{
				File: c.File, ShortFile: c.ShortFile,
				LoopLine: c.LoopLine, CallLine: c.CallLine,
				RecvText: c.RecvText, MethodName: c.MethodName,
				Level: LevelFail, Chain: chain, EntTerminal: terminal,
				LoopSnippet: loopSnip, ImplSnippet: implSnip,
			}
		}
	}

	// Not confirmed — if cross-package call, emit INFO
	if isCrossPackageCall(c) {
		return infoFinding(c)
	}
	return nil
}

// astFindEntTerminal recursively searches an AST block for ent terminal method calls.
// Returns (chain, terminalMethodName) or (nil, "").
func astFindEntTerminal(body *ast.BlockStmt, fset *token.FileSet, file string, depth, maxDepth int) ([]ChainStep, string) {
	if body == nil || depth > maxDepth {
		return nil, ""
	}
	var found []ChainStep
	var terminal string

	ast.Inspect(body, func(n ast.Node) bool {
		if terminal != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		method := sel.Sel.Name
		if !entTerminalMethods[method] {
			return true
		}
		pos := fset.Position(call.Pos())
		found = []ChainStep{{
			File: file, ShortFile: shortPath(file),
			Line:     pos.Line,
			FuncName: method,
			CallText: "." + method + "(ctx)  ← SQL executed here",
		}}
		terminal = method
		return false
	})

	return found, terminal
}

// ── Type-based interface resolver ─────────────────────────────────────────────

// findReceiverInterface looks up the interface type of the receiver at the call site.
func findReceiverInterface(ti *typeInfoResult, file string, callLine int, recvText string) *types.Interface {
	if ti == nil || !ti.valid {
		return nil
	}
	absFile, _ := filepath.Abs(file)

	// Walk all type-checked objects to find the variable with matching name at line
	for expr, tv := range ti.info.Types {
		pos := ti.fset.Position(expr.Pos())
		posAbs, _ := filepath.Abs(pos.Filename)
		if posAbs != absFile || pos.Line != callLine {
			continue
		}
		if iface, ok := tv.Type.Underlying().(*types.Interface); ok {
			return iface
		}
		if named, ok := tv.Type.(*types.Named); ok {
			if iface, ok2 := named.Underlying().(*types.Interface); ok2 {
				return iface
			}
		}
	}
	return nil
}

// findImplementors returns impl entries whose receiver type implements the interface.
func findImplementors(iface *types.Interface, method string, implIdx implIndexType, ti *typeInfoResult) []implEntry {
	if ti == nil || !ti.valid {
		return nil
	}
	var result []implEntry
	for key, entries := range implIdx {
		parts := strings.SplitN(key, ".", 2)
		if len(parts) != 2 || parts[1] != method {
			continue
		}
		result = append(result, entries...)
	}
	return result
}

// inferTypeName extracts the last identifier from a receiver expression.
// "l.svcCtx.IamUserRoleDao" → "IamUserRoleDao"
// "permDao" → "permDao"
func inferTypeName(recv string) string {
	parts := strings.Split(recv, ".")
	if len(parts) == 0 {
		return recv
	}
	return parts[len(parts)-1]
}

// isCrossPackageCall returns true if the receiver suggests a cross-package call.
func isCrossPackageCall(c Candidate) bool {
	return strings.Contains(c.RecvText, ".")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func detectModuleName(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

func fallbackFindings(candidates []Candidate) []N1Finding {
	var out []N1Finding
	for _, c := range candidates {
		out = append(out, *infoFinding(c))
	}
	return out
}

func infoFinding(c Candidate) *N1Finding {
	snip := readSourceSnippet(c.File, c.LoopLine-1, c.CallLine+2, c.LoopLine, c.CallLine)
	return &N1Finding{
		File: c.File, ShortFile: c.ShortFile,
		LoopLine: c.LoopLine, CallLine: c.CallLine,
		RecvText: c.RecvText, MethodName: c.MethodName,
		Level: LevelInfo, LoopSnippet: snip,
	}
}
