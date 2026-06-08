package checker

// callgraph.go — One-time CHA call graph build with pre-computed per-Logic-entry subgraph.
//
// Architecture:
//
//   BuildCallGraph(dir)  ~5-20s, called ONCE
//       │
//       ├─ packages.Load("./...")  → SSA build → CHA graph
//       ├─ buildImplIndex          → DAO impl bodies for SQL derivation
//       ├─ buildDAOImplIndex       → methodName → []ssa.Function (for N+1)
//       └─ precomputeLogicSubgraphs
//              ├─ find all (*XxxLogic).Method SSA nodes
//              ├─ BFS from each entry, depth=12
//              ├─ record reachable IO nodes (DB/Redis) with call chain
//              └─ store in cache.logicIOByKey["pkgPath.TypeName.Method"]
//
//   After build, all queries are O(1) map lookups:
//       cache.IOForLogic("pkgPath.TypeName.Method") → []IONode
//       cache.AllLogicEntries()                     → []LogicFuncEntry
//       cache.DAOImplsForMethod("List")             → []daoImplFunc  (N+1 use)

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// IONode, IOKind, IOKindDB, IOKindRedis are declared in logic_io.go (same package).

// ── Cache types ───────────────────────────────────────────────────────────────

// daoImplFunc is a concrete DAO method SSA node with its receiver type name.
type daoImplFunc struct {
	fn       *ssa.Function
	recvType string // e.g. "iamuserroleOceanBaseDao"
}

// LogicFuncEntry is one exported Logic method from the call graph.
type LogicFuncEntry struct {
	PkgPath   string
	TypeName  string // e.g. "GetAllUserListLogic"
	Method    string // e.g. "GetAllUserList"
	Func      *ssa.Function
	ShortFile string // short path of source file
	Line      int    // line of method declaration
	Signature string // SSA-derived signature string
}

// CallGraphCache is the shared, pre-built call graph result.
// All expensive computation happens in BuildCallGraph; all fields are read-only after that.
type CallGraphCache struct {
	prog *ssa.Program
	cg   *callgraph.Graph
	fset *token.FileSet

	moduleName string
	dir        string

	// implIdx: "TypeName.MethodName" → AST impl body (for SQL derivation)
	implIdx implIndexType

	// daoByMethod: methodName → concrete DAO SSA functions (for N+1 Phase 2)
	// Key is the bare method name, e.g. "List", "GetByID"
	daoByMethod map[string][]daoImplFunc

	// logicEntries: all exported Logic methods in call-graph order
	logicEntries []LogicFuncEntry

	// logicIOByKey: pre-computed IO nodes per Logic entry
	// Key format: "pkgPath::TypeName::Method"
	logicIOByKey map[string][]IONode

	// entTableMap: Go schema type name → actual DB table name
	// e.g. "IamUserRole" → "iam_user_role"
	// Built from ent/schema/*.go via AST parsing.
	entTableMap map[string]string
}

// BuildCallGraph loads the project, builds SSA + CHA call graph, and pre-computes
// all per-Logic-entry IO subgraphs in one pass. Called once per scan.
func BuildCallGraph(dir string) (*CallGraphCache, error) {
	moduleName := detectModuleName(dir)
	if moduleName == "" {
		return nil, fmt.Errorf("go.mod not found in %s", dir)
	}

	fset := token.NewFileSet()

	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedTypesSizes |
			packages.NeedImports |
			packages.NeedDeps,
		Fset:  fset,
		Dir:   dir,
		Tests: false, // exclude *_test.go test packages — saves SSA build time
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("packages.Load: %w", err)
	}

	var goodPkgs []*packages.Package
	packages.Visit(pkgs, func(p *packages.Package) bool {
		if p.Types == nil {
			return true
		}
		// Skip mock and test packages early — before SSA build.
		// This is more efficient than filtering during BFS traversal because
		// excluded packages won't have SSA nodes built at all.
		if isMockOrTestPkg(p) {
			return true // visit deps but don't add this pkg
		}
		goodPkgs = append(goodPkgs, p)
		return true
	}, nil)
	if len(goodPkgs) == 0 {
		return nil, fmt.Errorf("no packages loaded (go build ./... passes in %s?)", dir)
	}

	prog, _ := ssautil.AllPackages(goodPkgs, ssa.BuilderMode(0))
	prog.Build()
	cg := cha.CallGraph(prog)

	// AST-based impl index for SQL derivation
	implIdx := buildImplIndex(dir, moduleName, fset)

	// Parse ent/schema/*.go to get accurate table names
	entTableMap := buildEntSchemaTableMap(dir)

	cache := &CallGraphCache{
		prog:         prog,
		cg:           cg,
		fset:         fset,
		moduleName:   moduleName,
		dir:          dir,
		implIdx:      implIdx,
		entTableMap:  entTableMap,
		daoByMethod:  make(map[string][]daoImplFunc),
		logicIOByKey: make(map[string][]IONode),
	}

	// ── Pre-computation pass (single traversal of call graph nodes) ────────────
	// 1. Collect DAO concrete impls (for N+1)
	// 2. Collect Logic entry points
	// 3. Pre-compute IO subgraph for each Logic entry
	cache.precompute()

	return cache, nil
}

// precompute does a single pass over call graph nodes to:
//  1. Build daoByMethod index (for N+1)
//  2. Find all Logic entry functions
//  3. BFS from each Logic entry to collect IO nodes (stored in logicIOByKey)
func (c *CallGraphCache) precompute() {
	// Pass 1: classify all nodes into DAO impls and Logic entries
	seen := map[string]bool{}
	for fn := range c.cg.Nodes {
		if fn == nil || fn.Signature == nil {
			continue
		}
		if fn.Package() == nil || fn.Package().Pkg == nil {
			continue
		}
		pkgPath := fn.Package().Pkg.Path()

		recv := fn.Signature.Recv()
		if recv == nil {
			continue
		}
		recvType := typeBaseName(recv.Type())
		lowerRecv := strings.ToLower(recvType)

		// Extract bare method name from SSA format "(*Type).Method" or "(Type).Method"
		methodName := ssaMethodName(fn.Name())

		// Mock/test packages are excluded at packages.Load time (see isMockOrTestPkg).
		// This check is a defensive fallback only.
		if strings.Contains(pkgPath, "/mock") || strings.Contains(pkgPath, "/mocks") {
			continue
		}

		// ── DAO concrete impl ──
		// Only real impls (not mocks): must be in internal/dao/impl/
		if strings.HasSuffix(lowerRecv, "dao") && isIOVerb(methodName) &&
			strings.Contains(pkgPath, "/dao/impl") {
			c.daoByMethod[methodName] = append(c.daoByMethod[methodName],
				daoImplFunc{fn: fn, recvType: recvType})
		}

		// ── Logic entry ──
		if !strings.Contains(pkgPath, "/internal/logic/") {
			continue
		}
		if !isExported(methodName) || strings.HasPrefix(methodName, "New") {
			continue
		}
		if !strings.HasSuffix(recvType, "Logic") {
			continue
		}
		key := pkgPath + "::" + recvType + "::" + methodName
		if seen[key] {
			continue
		}
		seen[key] = true
		// Extract source position and signature from SSA function
		pos := c.fset.Position(fn.Pos())
		sig := formatSSASignature(fn)
		c.logicEntries = append(c.logicEntries, LogicFuncEntry{
			PkgPath:   pkgPath,
			TypeName:  recvType,
			Method:    methodName,
			Func:      fn,
			ShortFile: shortPath(pos.Filename),
			Line:      pos.Line,
			Signature: sig,
		})
	}

	// Pass 2: BFS from each Logic entry to collect IO nodes
	for _, entry := range c.logicEntries {
		cgNode := c.cg.Nodes[entry.Func]
		if cgNode == nil {
			continue
		}
		visited := make(map[*callgraph.Node]bool)
		var ios []IONode
		c.bfsIO(cgNode, []string{entry.Method}, visited, 12, &ios)
		key := entry.PkgPath + "::" + entry.TypeName + "::" + entry.Method
		c.logicIOByKey[key] = ios
	}
}

// ── Public query API (all O(1) after precompute) ──────────────────────────────

// AllLogicEntries returns pre-computed Logic entry points.
func (c *CallGraphCache) AllLogicEntries() []LogicFuncEntry {
	return c.logicEntries
}

// IOForLogic returns the pre-computed IO nodes for a Logic entry.
// Key format: "pkgPath::TypeName::Method"
func (c *CallGraphCache) IOForLogic(key string) []IONode {
	return c.logicIOByKey[key]
}

// IOKey builds the lookup key for a LogicFuncEntry.
func IOKey(e LogicFuncEntry) string {
	return e.PkgPath + "::" + e.TypeName + "::" + e.Method
}

// DAOImplsForMethod returns all concrete DAO SSA functions with the given method name.
// Used by N+1 Phase 2 to enumerate candidate impls without re-scanning.
func (c *CallGraphCache) DAOImplsForMethod(method string) []daoImplFunc {
	return c.daoByMethod[method]
}

// AllLogicFuncs is kept for backward compat with logic_io.go.
func (c *CallGraphCache) AllLogicFuncs() []LogicFuncEntry {
	return c.logicEntries
}

// ReachableIO is kept for backward compat but now just reads pre-computed data.
func (c *CallGraphCache) ReachableIO(fn *ssa.Function, _ int) []IONode {
	for _, e := range c.logicEntries {
		if e.Func == fn {
			return c.logicIOByKey[IOKey(e)]
		}
	}
	// Not a known Logic entry — do live BFS (rare)
	var ios []IONode
	if node := c.cg.Nodes[fn]; node != nil {
		name := ssaMethodName(fn.Name())
		visited := make(map[*callgraph.Node]bool)
		c.bfsIO(node, []string{name}, visited, 12, &ios)
	}
	return ios
}

// ── BFS engine ────────────────────────────────────────────────────────────────

func (c *CallGraphCache) bfsIO(
	cgNode *callgraph.Node,
	chain []string,
	visited map[*callgraph.Node]bool,
	depth int,
	out *[]IONode,
) {
	if cgNode == nil || visited[cgNode] || depth <= 0 {
		return
	}
	visited[cgNode] = true

	for _, edge := range cgNode.Out {
		callee := edge.Callee
		if callee == nil || callee.Func == nil {
			continue
		}
		fn := callee.Func

		// Defensive: skip any mock packages that slipped through packages.Load filter
		// (e.g. vendored mocks or unusually named packages).
		if fn.Package() != nil && fn.Package().Pkg != nil {
			pkgPath := fn.Package().Pkg.Path()
			if strings.Contains(pkgPath, "/mock") || strings.Contains(pkgPath, "/mocks") {
				continue
			}
		}

		pos := token.NoPos
		if edge.Site != nil {
			pos = edge.Site.Pos()
		}
		position := c.prog.Fset.Position(pos)

		if node, ok := c.classifyCall(fn, position.Filename, position.Line, chain); ok {
			*out = append(*out, node)
			// Don't recurse into DAO/Redis impl — IO node captured
			continue
		}
		if isInternalFunc(fn, c.moduleName) {
			// Use bare method name in chain for readability
			name := ssaMethodName(fn.Name())
			newChain := append(append([]string{}, chain...), name)
			c.bfsIO(callee, newChain, visited, depth-1, out)
		}
	}
}

// classifyCall checks if fn is a DB or Redis call.
func (c *CallGraphCache) classifyCall(
	fn *ssa.Function,
	file string,
	line int,
	chain []string,
) (IONode, bool) {
	if fn.Object() == nil || fn.Signature == nil {
		return IONode{}, false
	}
	sig, ok := fn.Object().Type().(*types.Signature)
	if !ok {
		return IONode{}, false
	}
	recv := sig.Recv()
	if recv == nil {
		return IONode{}, false
	}

	typeName := typeBaseName(recv.Type())
	method := ssaMethodName(fn.Name())
	lower := strings.ToLower(typeName)
	short := shortPath(file)

	// ── Redis ──
	if strings.Contains(lower, "redis") {
		// Try to extract key/TTL from the call site's enclosing function body
		keyHint, ttlHint := c.extractRedisArgsFromCallSite(file, line, method)
		snippet := readSourceSnippet(file, line-1, line+2, line, line)
		return IONode{
			File: file, ShortFile: short, Line: line,
			CallChain:    chain,
			Kind:         IOKindRedis,
			Receiver:     typeName, Method: method,
			RedisCmd:     redisCmdWithContext(method, keyHint, ttlHint),
			RedisKeyHint: keyHint,
			RedisTTLHint: ttlHint,
			Snippet:      snippet,
		}, true
	}

	// ── DB / DAO concrete impl ──
	if !strings.HasSuffix(lower, "dao") || !isIOVerb(method) {
		return IONode{}, false
	}
	sql, exact, snippet := c.deriveSQLWithSnippet(typeName, method)
	return IONode{
		File: file, ShortFile: short, Line: line,
		CallChain: chain,
		Kind:      IOKindDB,
		Receiver:  typeName, Method: method,
		SQL: sql, SQLExact: exact,
		Snippet: snippet,
	}, true
}

// extractRedisArgsFromCallSite parses the source file at file/line,
// finds the enclosing function body, and extracts Redis key/TTL arguments.
// This works for Redis calls in Logic layer, pkg/cache, or any other file.
func (c *CallGraphCache) extractRedisArgsFromCallSite(file string, line int, method string) (keyHint, ttlHint string) {
	if file == "" {
		return "", ""
	}

	// First check implIdx (for DAO impls)
	for _, entries := range c.implIdx {
		for _, e := range entries {
			if e.file != file {
				continue
			}
			if e.funcDecl == nil || e.funcDecl.Body == nil {
				continue
			}
			startLine := e.fset.Position(e.funcDecl.Pos()).Line
			endLine := e.fset.Position(e.funcDecl.End()).Line
			if line >= startLine && line <= endLine {
				return extractRedisArgs(e.funcDecl.Body, method)
			}
		}
	}

	// Fallback: parse the source file directly to find enclosing function
	return extractRedisArgsFromFile(file, line, method, c.fset)
}

// extractRedisArgsFromFile parses a Go source file and finds the function
// enclosing the given line, then extracts Redis call arguments.
// Always uses a fresh FileSet to avoid "file already exists" conflicts with
// the shared call-graph fset.
func extractRedisArgsFromFile(file string, line int, method string, _ *token.FileSet) (keyHint, ttlHint string) {
	// Always use a fresh fset — reusing the shared one causes token.Pos conflicts
	// when the same file was already registered by packages.Load.
	freshFset := token.NewFileSet()

	src, err := os.ReadFile(file)
	if err != nil {
		return "", ""
	}
	f, err := parseWithGoParser(freshFset, file, src)
	if err != nil {
		return "", ""
	}

	// Find the function declaration enclosing the given line.
	// freshFset.Position(fd.Pos()).Line gives the 1-based line within the file,
	// which matches the call-graph's Position(site.Pos()).Line.
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		startLine := freshFset.Position(fd.Pos()).Line
		endLine := freshFset.Position(fd.End()).Line
		if line >= startLine && line <= endLine {
			return extractRedisArgs(fd.Body, method)
		}
	}
	return "", ""
}

// deriveSQL uses implIdx AST to derive SQL from ent terminal methods.
// Returns ("", false) if the impl is not in the index or has no ent terminal —
// callers should show "[SQL 未捕获]" rather than guessing.
func (c *CallGraphCache) deriveSQL(typeName, method string) (string, bool) {
	sql, exact, _ := c.deriveSQLWithSnippet(typeName, method)
	return sql, exact
}

// deriveSQLWithSnippet derives SQL by AST-scanning the impl body for ent terminal calls.
// No fallback guessing: if the impl is missing or has no ent terminal, returns ("", false).
func (c *CallGraphCache) deriveSQLWithSnippet(typeName, method string) (sql string, exact bool, snippet []SourceLine) {
	entries, ok := c.implIdx[typeName+"."+method]
	if !ok || len(entries) == 0 {
		return "", false, nil // impl not in index — no guess
	}
	entry := entries[0]
	_, terminal := astFindEntTerminal(entry.funcDecl.Body, entry.fset, entry.file, 0, 4)
	if terminal == "" {
		return "", false, nil // no ent terminal found — no guess
	}
	table := c.resolveTableName(typeName, entry.file)
	sql = entTerminalToSQL(terminal, method, table, entry.funcDecl)
	return sql, true, nil
}

// resolveTableName finds the actual DB table name for a DAO impl type.
// Priority:
//  1. entTableMap: look up the ent schema type derived from impl struct name
//  2. impl file name heuristic (e.g. iam_user_role_oceanbase.go → iam_user_role)
//  3. camelToSnake(typeName)
func (c *CallGraphCache) resolveTableName(implStructName, implFile string) string {
	// Step 1: derive the ent schema type name from impl struct name
	// "iamuserroleOceanBaseDao" → strip suffix → "iamuserrole" (all-lower)
	// Then find matching key in entTableMap (which uses PascalCase keys like "IamUserRole")
	stripped := implStructName
	for _, sfx := range []string{"OceanBaseDao", "MysqlDao", "PostgresDao", "SqliteDao", "Dao"} {
		if strings.HasSuffix(stripped, sfx) {
			stripped = strings.TrimSuffix(stripped, sfx)
			break
		}
	}
	lowerStripped := strings.ToLower(stripped)
	for schemaType, table := range c.entTableMap {
		if strings.ToLower(schemaType) == lowerStripped {
			return table // ✅ exact match from ent schema
		}
	}

	// Step 2: file name heuristic
	if implFile != "" {
		base := filepath.Base(implFile)
		stem := strings.TrimSuffix(base, ".go")
		for _, sfx := range []string{"_oceanbase", "_mysql", "_postgres", "_sqlite", "_impl", "_dao"} {
			stem = strings.TrimSuffix(stem, sfx)
		}
		if stem != "" {
			return stem
		}
	}

	// Step 3: camelToSnake fallback
	return camelToSnake(stripped)
}

// ── Package filter ────────────────────────────────────────────────────────────

// isMockOrTestPkg returns true if a package should be excluded from the call graph.
//
// Excluded cases:
//  1. Package import path contains mock path segments (/mock, /mocks, /mock_, _mock)
//  2. Package name is "mock" or "mocks"
//  3. All Go files in the package are _test.go files (external test package)
//  4. Any Go file in the package has a "_mock.go" suffix (generated mocks)
func isMockOrTestPkg(p *packages.Package) bool {
	// 1. Check import path segments
	pkgPath := p.PkgPath
	for _, seg := range strings.Split(pkgPath, "/") {
		lower := strings.ToLower(seg)
		if lower == "mock" || lower == "mocks" ||
			strings.HasPrefix(lower, "mock_") || strings.HasSuffix(lower, "_mock") {
			return true
		}
	}

	// 2. Check package name
	pkgName := strings.ToLower(p.Name)
	if pkgName == "mock" || pkgName == "mocks" {
		return true
	}

	// 3 & 4. Check filenames
	if len(p.GoFiles) == 0 {
		return false
	}
	allTest := true
	for _, f := range p.GoFiles {
		base := filepath.Base(f)
		// Generated mock files (mockery, gomock, etc.)
		if strings.HasSuffix(base, "_mock.go") ||
			strings.HasPrefix(base, "mock_") {
			return true
		}
		// Test files
		if !strings.HasSuffix(base, "_test.go") {
			allTest = false
		}
	}
	// All files are _test.go → pure test package
	if allTest && len(p.GoFiles) > 0 {
		return true
	}

	return false
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// ssaMethodName extracts the bare method name from SSA format.
// "(*GetAllUserListLogic).GetAllUserList" → "GetAllUserList"
// "GetAllUserList" → "GetAllUserList"  (package-level func, unchanged)
func ssaMethodName(raw string) string {
	if dot := strings.LastIndex(raw, "."); dot >= 0 {
		return raw[dot+1:]
	}
	return raw
}

func isInternalFunc(fn *ssa.Function, moduleName string) bool {
	if fn.Package() == nil || fn.Package().Pkg == nil {
		return false
	}
	path := fn.Package().Pkg.Path()
	return strings.HasPrefix(path, moduleName+"/internal/") ||
		strings.HasPrefix(path, moduleName+"/pkg/")
}

func typeBaseName(t types.Type) string {
	switch v := t.(type) {
	case *types.Pointer:
		return typeBaseName(v.Elem())
	case *types.Named:
		return v.Obj().Name()
	case *types.Interface:
		return "interface"
	}
	return t.String()
}

func isExported(name string) bool {
	return len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
}

// formatSSASignature builds a readable signature string from an SSA function.
// e.g. "UpdateAPI(in *passport.UpdateAPIReq) (*passport.Empty, error)"
// Falls back to just the method name on parse failure.
func formatSSASignature(fn *ssa.Function) string {
	if fn == nil || fn.Signature == nil {
		return ""
	}
	sig := fn.Signature
	methodName := ssaMethodName(fn.Name())

	// Build params string
	var params []string
	for i := 0; i < sig.Params().Len(); i++ {
		p := sig.Params().At(i)
		name := p.Name()
		typStr := types.TypeString(p.Type(), func(pkg *types.Package) string {
			return pkg.Name() // use short package name
		})
		if name != "" && name != "_" && name != "ctx" {
			params = append(params, name+" "+typStr)
		} else if name == "ctx" {
			// skip ctx for brevity
		} else {
			params = append(params, typStr)
		}
	}

	// Build results string
	var results []string
	for i := 0; i < sig.Results().Len(); i++ {
		r := sig.Results().At(i)
		typStr := types.TypeString(r.Type(), func(pkg *types.Package) string {
			return pkg.Name()
		})
		results = append(results, typStr)
	}

	paramStr := strings.Join(params, ", ")
	var retStr string
	switch len(results) {
	case 0:
		retStr = ""
	case 1:
		retStr = " " + results[0]
	default:
		retStr = " (" + strings.Join(results, ", ") + ")"
	}

	return fmt.Sprintf("%s(%s)%s", methodName, paramStr, retStr)
}
