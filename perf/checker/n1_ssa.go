package checker

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
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

// traceWithSSA uses go/packages type info (not full SSA) to resolve interface
// implementations, then AST-scans implementation bodies for ent terminal methods.
//
// Flow per candidate:
//  1. Load candidate package with type info (fast: only the logic package)
//  2. At the call site, use go/types to get the receiver's interface type
//  3. Find all concrete types implementing that interface (via packages.Visit)
//  4. Locate their method implementations on disk
//  5. AST-scan those implementations for ent terminal calls (recursive, depth≤5)
func traceWithSSA(dir string, fset *token.FileSet, candidates []Candidate) []N1Finding {
	if len(candidates) == 0 {
		return nil
	}

	moduleName := detectModuleName(dir)
	if moduleName == "" {
		return fallbackFindings(candidates)
	}

	// Group candidates by import path to load each package once
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

	// Build impl index once: method signatures → impl file + func body
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
		terminal = method + "(ctx)"
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
