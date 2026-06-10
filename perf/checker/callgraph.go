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
	"io/fs"
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

	// entHookMap: table name → list of hook SQL strings triggered on mutation.
	// e.g. "iam_api" → ["UPDATE iam_api_permission SET deleted_at = ? WHERE api_id IN (?)"]
	// Built by scanning internal/dao/hook/*.go for ent chain calls.
	entHookMap map[string][]string

	// entTableMap: Go schema type name → actual DB table name
	entTableMap map[string]string

	// keyFuncCache: function name → resolved key template string.
	// e.g. "SessionKey" → "passport:auth_session:{sessionID}"
	//      "simulatedLoginKey" → "simulated_login:{account}"
	// Built by scanning pkg/** and internal/** for simple string-returning functions.
	keyFuncCache map[string]string

	// ttlConstCache: identifier name → numeric string value.
	// e.g. "SessionTTLSeconds" → "86400"
	ttlConstCache map[string]string

	// ttlFuncCache: method/function name → default int value from function body.
	// e.g. "simulatedLoginTTLSeconds" → "600"  (from positiveOrDefaultInt(config, 600))
	ttlFuncCache map[string]string
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

	// Scan internal/dao/hook/*.go to build hook SQL map
	entHookMap := buildEntHookMap(dir, moduleName, fset, entTableMap)

	// Scan pkg/ and internal/ for Redis key functions and TTL constants/methods
	keyFuncCache, ttlConstCache, ttlFuncCache := buildRedisKeyCache(dir)

	cache := &CallGraphCache{
		prog:          prog,
		cg:            cg,
		fset:          fset,
		moduleName:    moduleName,
		dir:           dir,
		implIdx:       implIdx,
		entTableMap:   entTableMap,
		entHookMap:    entHookMap,
		keyFuncCache:  keyFuncCache,
		ttlConstCache: ttlConstCache,
		ttlFuncCache:  ttlFuncCache,
		daoByMethod:   make(map[string][]daoImplFunc),
		logicIOByKey:  make(map[string][]IONode),
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
	if strings.Contains(lower, "redis") && isRedisVerb(method) {
		keyHint, ttlHint := c.extractRedisArgsFromCallSite(file, line, method)
		// Resolve key function calls to actual templates using keyFuncCache
		keyHint = c.resolveKeyHint(keyHint)
		ttlHint = c.resolveTTLHint(ttlHint)
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
	// Attach hook SQLs only for mutation operations (writes can trigger hooks)
	var hookSQLs []string
	if exact && isMutationMethod(method) {
		hookSQLs = c.hookSQLsFor(typeName, method, file)
	}
	return IONode{
		File: file, ShortFile: short, Line: line,
		CallChain: chain,
		Kind:      IOKindDB,
		Receiver:  typeName, Method: method,
		SQL: sql, SQLExact: exact,
		HookSQLs: hookSQLs,
		Snippet:  snippet,
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
// Hook SQLs (from registered ent hooks on the same table) are returned separately.
func (c *CallGraphCache) deriveSQLWithSnippet(typeName, method string) (sql string, exact bool, snippet []SourceLine) {
	entries, ok := c.implIdx[typeName+"."+method]
	if !ok || len(entries) == 0 {
		return "", false, nil
	}
	entry := entries[0]
	_, terminal := astFindEntTerminal(entry.funcDecl.Body, entry.fset, entry.file, 0, 4)
	if terminal == "" {
		return "", false, nil
	}
	table := c.resolveTableName(typeName, entry.file)
	sql = entTerminalToSQL(terminal, method, table, entry.funcDecl)
	return sql, true, nil
}

// hookSQLsFor returns any hook-triggered SQL statements for the given table+method.
// Only mutation operations (Save/Exec → UPDATE/INSERT/DELETE) can trigger hooks.
func (c *CallGraphCache) hookSQLsFor(typeName, method string, implFile string) []string {
	if c.entHookMap == nil {
		return nil
	}
	table := c.resolveTableName(typeName, implFile)
	return c.entHookMap[table]
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

// ── Redis key / TTL resolution ────────────────────────────────────────────────

// buildRedisKeyCache scans pkg/ and internal/ for:
//  1. Key functions: func FooKey(x T) string { return "prefix:" + x }
//     → keyFuncCache["FooKey"] = "prefix:{x}"
//  2. Numeric TTL constants: const FooTTL = 600
//     → ttlConstCache["FooTTL"] = "600"
//  3. TTL methods: func (l X) fooTTLSeconds() int { return positiveOrDefaultInt(cfg, 600) }
//     → ttlFuncCache["fooTTLSeconds"] = "600"
func buildRedisKeyCache(dir string) (keyFuncs, ttlConsts, ttlFuncs map[string]string) {
	keyFuncs = make(map[string]string)
	ttlConsts = make(map[string]string)
	ttlFuncs = make(map[string]string)

	fset := token.NewFileSet()
	scanDirs := []string{
		filepath.Join(dir, "pkg"),
		filepath.Join(dir, "internal", "logic"),
	}

	for _, scanDir := range scanDirs {
		if _, err := os.Stat(scanDir); err != nil {
			continue
		}
		_ = filepath.WalkDir(scanDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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

			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					// Scan const blocks for TTL values
					for _, spec := range d.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, name := range vs.Names {
							if i >= len(vs.Values) {
								continue
							}
							if v := evalConstExpr(vs.Values[i]); v != "" {
								ttlConsts[name.Name] = v
							}
						}
					}
				case *ast.FuncDecl:
					if d.Body == nil || d.Type.Results == nil || d.Type.Results.NumFields() != 1 {
						continue
					}
					resField := d.Type.Results.List[0]
					resTypeName := ""
					if id, ok := resField.Type.(*ast.Ident); ok {
						resTypeName = id.Name
					}

					if resTypeName == "string" {
						// Key function: extract template
						var paramNames []string
						if d.Type.Params != nil {
							for _, param := range d.Type.Params.List {
								for _, pname := range param.Names {
									paramNames = append(paramNames, pname.Name)
								}
							}
						}
						if tmpl := extractKeyTemplate(d.Body, paramNames); tmpl != "" {
							keyFuncs[d.Name.Name] = tmpl
						}
					} else if resTypeName == "int" || resTypeName == "int64" || resTypeName == "int32" {
						// TTL method: find default int literal in body
						// Pattern: positiveOrDefaultInt(config, 600) or return 600
						fnName := d.Name.Name
						lowerName := strings.ToLower(fnName)
						if strings.Contains(lowerName, "ttl") || strings.Contains(lowerName, "second") ||
							strings.Contains(lowerName, "expire") || strings.Contains(lowerName, "timeout") {
							if v := extractDefaultIntFromBodyWithConsts(d.Body, ttlConsts); v != "" {
								ttlFuncs[fnName] = v
							}
						}
					}
				}
			}
			return nil
		})
	}
	return keyFuncs, ttlConsts, ttlFuncs
}

// evalConstExpr evaluates simple constant expressions to a numeric string.
// e.g. 24 * 60 * 60 → "86400", 10 * 60 → "600"
func evalConstExpr(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.BasicLit:
		if v.Kind == token.INT {
			return v.Value
		}
	case *ast.BinaryExpr:
		if v.Op == token.MUL {
			left := evalConstExpr(v.X)
			right := evalConstExpr(v.Y)
			if left != "" && right != "" {
				// Simple integer multiplication
				var l, r int64
				if _, err := fmt.Sscanf(left, "%d", &l); err != nil {
					return ""
				}
				if _, err := fmt.Sscanf(right, "%d", &r); err != nil {
					return ""
				}
				return fmt.Sprintf("%d", l*r)
			}
		}
		if v.Op == token.ADD {
			left := evalConstExpr(v.X)
			right := evalConstExpr(v.Y)
			if left != "" && right != "" {
				var l, r int64
				if _, err := fmt.Sscanf(left, "%d", &l); err != nil {
					return ""
				}
				if _, err := fmt.Sscanf(right, "%d", &r); err != nil {
					return ""
				}
				return fmt.Sprintf("%d", l+r)
			}
		}
	}
	return ""
}

// extractKeyTemplate extracts the key template from a simple key function body.
// e.g. `return "passport:auth_session:" + sessionID` → "passport:auth_session:{sessionID}"
// e.g. `return fmt.Sprintf("passport:rt_map:%d", userID)` → "passport:rt_map:{userID}"
func extractKeyTemplate(body *ast.BlockStmt, paramNames []string) string {
	paramSet := make(map[string]bool)
	for _, p := range paramNames {
		paramSet[p] = true
	}

	var result string
	ast.Inspect(body, func(n ast.Node) bool {
		if result != "" {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		result = exprToKeyTemplate(ret.Results[0], paramSet)
		return false
	})
	return result
}

// exprToKeyTemplate converts a return expression to a key template string.
func exprToKeyTemplate(expr ast.Expr, params map[string]bool) string {
	switch v := expr.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			return strings.Trim(v.Value, `"`)
		}
	case *ast.Ident:
		if params[v.Name] {
			return "{" + v.Name + "}"
		}
		return v.Name
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			left := exprToKeyTemplate(v.X, params)
			right := exprToKeyTemplate(v.Y, params)
			if left != "" && right != "" {
				return left + right
			}
		}
	case *ast.CallExpr:
		fnName := ""
		switch f := v.Fun.(type) {
		case *ast.Ident:
			fnName = f.Name
		case *ast.SelectorExpr:
			fnName = f.Sel.Name
		}
		// fmt.Sprintf("prefix:%s", param)
		if strings.ToLower(fnName) == "sprintf" && len(v.Args) > 0 {
			if lit, ok := v.Args[0].(*ast.BasicLit); ok {
				tmpl := strings.Trim(lit.Value, `"`)
				for i, arg := range v.Args[1:] {
					placeholder := fmt.Sprintf("{arg%d}", i)
					if id, ok := arg.(*ast.Ident); ok && params[id.Name] {
						placeholder = "{" + id.Name + "}"
					}
					for _, verb := range []string{"%s", "%d", "%v", "%q"} {
						if idx := strings.Index(tmpl, verb); idx >= 0 {
							tmpl = tmpl[:idx] + placeholder + tmpl[idx+len(verb):]
							break
						}
					}
				}
				return tmpl
			}
		}
		// String manipulation wrappers: TrimSpace(x), ToLower(x), etc.
		// Treat the inner argument as the key part.
		trimFuncs := map[string]bool{
			"TrimSpace": true, "ToLower": true, "ToUpper": true,
			"Trim": true, "TrimPrefix": true, "TrimSuffix": true,
		}
		if trimFuncs[fnName] && len(v.Args) > 0 {
			return exprToKeyTemplate(v.Args[0], params)
		}
	}
	return ""
}

// extractDefaultIntFromBody finds the best int default in a TTL function body.
// It also returns any const/selector idents for further resolution.
// Handles:
//   - return 600
//   - return positiveOrDefaultInt(cfg, 600)
//   - return positiveOrDefaultInt(cfg, poauth.SessionTTLSeconds)  → "poauth.SessionTTLSeconds" for lookup
func extractDefaultIntFromBody(body *ast.BlockStmt) string {
	return extractDefaultIntFromBodyWithConsts(body, nil)
}

func extractDefaultIntFromBodyWithConsts(body *ast.BlockStmt, ttlConsts map[string]string) string {
	var result string
	ast.Inspect(body, func(n ast.Node) bool {
		if result != "" {
			return false
		}
		// Direct return int literal
		if ret, ok := n.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			if v := evalConstExpr(ret.Results[0]); v != "" {
				result = v
				return false
			}
		}
		// Call like positiveOrDefaultInt(config, arg) — check last args
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		for i := len(call.Args) - 1; i >= 0; i-- {
			arg := call.Args[i]
			// Int literal
			if v := evalConstExpr(arg); v != "" {
				result = v
				return false
			}
			// pkg.Const selector like poauth.SessionTTLSeconds
			if sel, ok := arg.(*ast.SelectorExpr); ok && ttlConsts != nil {
				constName := sel.Sel.Name
				if v, ok := ttlConsts[constName]; ok {
					result = v
					return false
				}
			}
			// Bare ident like SessionTTLSeconds
			if id, ok := arg.(*ast.Ident); ok && ttlConsts != nil {
				if v, ok := ttlConsts[id.Name]; ok {
					result = v
					return false
				}
			}
		}
		return true
	})
	return result
}

// resolveKeyHint resolves a key hint that may be a function call like "SessionKey({sessionID})"
// to the actual template "passport:auth_session:{sessionID}" using keyFuncCache.
func (c *CallGraphCache) resolveKeyHint(hint string) string {
	if hint == "" || c.keyFuncCache == nil {
		return hint
	}
	// Check if hint is "FuncName({arg})" or "FuncName(...)"
	parenIdx := strings.Index(hint, "(")
	if parenIdx < 0 {
		return hint
	}
	funcName := hint[:parenIdx]
	argPart := ""
	if parenIdx < len(hint)-1 {
		argPart = hint[parenIdx+1 : len(hint)-1] // strip outer parens
	}

	tmpl, ok := c.keyFuncCache[funcName]
	if !ok {
		return hint
	}
	// Replace parameter placeholders in template with the actual arg hints
	// e.g. template "passport:auth_session:{sessionID}", arg "{sessionID}" → keep as-is
	// e.g. template "passport:auth_session:{sessionID}", arg "{code}" → replace {sessionID} with {code}
	if argPart != "" && argPart != "..." {
		// Find {param} in template and replace with actual arg
		result := tmpl
		// Simple single-arg case: replace first {x} with argPart
		if start := strings.Index(result, "{"); start >= 0 {
			if end := strings.Index(result[start:], "}"); end >= 0 {
				result = result[:start] + argPart + result[start+end+1:]
			}
		}
		return result
	}
	return tmpl
}

// resolveTTLHint resolves a TTL hint to a numeric string.
//
// Resolution order:
//  1. Direct const name: "SessionTTLSeconds" → ttlConstCache["SessionTTLSeconds"] → "86400s"
//  2. Hint contains const name: "sessionTTLSeconds(...)" contains "SessionTTLSeconds" → "86400s"
//  3. Strip to bare name, try capitalized: "simulatedLoginTTLSeconds" → scan func body for int literal default
//  4. Scan ttlFuncCache for method name → default int in function body
func (c *CallGraphCache) resolveTTLHint(hint string) string {
	if hint == "" {
		return hint
	}
	if c.ttlConstCache != nil {
		if v, ok := c.ttlConstCache[hint]; ok {
			return formatTTL(v)
		}
		for constName, value := range c.ttlConstCache {
			if strings.Contains(hint, constName) {
				return formatTTL(value)
			}
		}
	}

	// Extract bare method/func name from "methodName(...)" or "pkg.methodName(...)"
	bare := hint
	if idx := strings.Index(bare, "("); idx >= 0 {
		bare = bare[:idx]
	}
	if idx := strings.LastIndex(bare, "."); idx >= 0 {
		bare = bare[idx+1:]
	}
	bare = strings.TrimSpace(bare)

	// Look up in ttlFuncCache (maps method name → default int value from function body)
	if c.ttlFuncCache != nil {
		if v, ok := c.ttlFuncCache[bare]; ok {
			return formatTTL(v)
		}
	}

	return hint
}

// formatTTL converts a numeric seconds string to "Ns (human)" e.g. "86400 (24h)".
func formatTTL(seconds string) string {
	var n int64
	if _, err := fmt.Sscanf(seconds, "%d", &n); err != nil || n <= 0 {
		return seconds + "s"
	}
	switch {
	case n%(30*24*3600) == 0:
		return fmt.Sprintf("%ds (%dd)", n, n/86400)
	case n%(24*3600) == 0:
		return fmt.Sprintf("%ds (%dh)", n, n/3600)
	case n%3600 == 0:
		return fmt.Sprintf("%ds (%dh)", n, n/3600)
	case n%60 == 0:
		return fmt.Sprintf("%ds (%dmin)", n, n/60)
	default:
		return fmt.Sprintf("%ds", n)
	}
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

// isRedisVerb returns true if the method name corresponds to a real Redis command.
// Filters out non-command methods like Error, Ping, Close, Dial that happen to
// exist on *redis.Redis but are not IO operations.
func isRedisVerb(method string) bool {
	m := strings.ToLower(strings.TrimSuffix(strings.ToLower(method), "ctx"))
	switch m {
	case "get", "set", "setex", "setnx", "del", "exists", "expire", "ttl", "persist",
		"hget", "hset", "hmget", "hmset", "hgetall", "hdel", "hlen", "hexists",
		"incr", "incrby", "incrbyfloat", "decr", "decrby",
		"sadd", "smembers", "sismember", "srem", "scard",
		"zadd", "zrange", "zrangebyscore", "zrevrange", "zrevrangebyscore",
		"zrem", "zscore", "zcard", "zrank", "zrevrank",
		"lrange", "rpush", "lpush", "lpop", "rpop", "llen", "lindex",
		"mget", "mset", "getset", "getdel",
		"pipelined", "pipeline", "eval", "evalsha",
		"lock", "unlock", "takedistributedlock",
		"scan", "hscan", "sscan", "zscan",
		"pexpire", "pttl", "psetex",
		"keys", "type", "rename", "renamenx", "move":
		return true
	}
	return false
}

// isMutationMethod returns true if the DAO method name is a write operation
// (which can trigger ent hooks).
func isMutationMethod(method string) bool {
	for _, pfx := range []string{"Create", "Insert", "Update", "Delete", "SoftDelete", "Upsert", "Save", "Reset"} {
		if strings.HasPrefix(method, pfx) {
			return true
		}
	}
	return false
}

// buildEntHookMap scans internal/dao/hook/*.go and extracts SQL triggered by ent hooks.
// Returns map: table_name → []SQL strings produced inside hook mutator functions.
//
// Detection: find functions whose name contains "Hook" or are registered via
// enthook.On(...). Walk their bodies for ent terminal calls (same as DAO impl).
func buildEntHookMap(dir, moduleName string, fset *token.FileSet, entTableMap map[string]string) map[string][]string {
	result := make(map[string][]string)
	hookDir := filepath.Join(dir, "internal", "dao", "hook")
	if _, err := os.Stat(hookDir); err != nil {
		return result
	}

	entries, _ := os.ReadDir(hookDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(hookDir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		f, err := parseWithGoParser(fset, path, src)
		if err != nil {
			continue
		}

		// Determine which table this hook file is for, from the file name.
		// e.g. "iam_api_hook.go" → "iam_api"
		stem := strings.TrimSuffix(e.Name(), ".go")
		stem = strings.TrimSuffix(stem, "_hook")
		tableName := stem // already snake_case from filename

		// Scan every function body for ent terminal calls
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			// Only look at hook mutator functions (not Register* or resolve* helpers)
			fnLower := strings.ToLower(fd.Name.Name)
			if strings.HasPrefix(fnLower, "register") || strings.HasPrefix(fnLower, "resolve") {
				continue
			}

			// Find all ent terminal calls in this function body
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if !entTerminalMethods[sel.Sel.Name] {
					return true
				}
				// Found a terminal call — derive the table from the chain
				// Walk the chain to find the ent client call: m.Client().IamAPIPermission.Update()...
				chainTable := extractEntClientTable(call, entTableMap)
				if chainTable == "" {
					chainTable = tableName // fallback: same table as the hook file
				}
				// Derive SQL from the surrounding chain
				info := extractEntChainInfoFromExpr(call)
				sql := entChainInfoToSQL(sel.Sel.Name, chainTable, info)
				if sql != "" {
					result[tableName] = appendUnique(result[tableName], sql)
				}
				return true
			})
		}
	}
	return result
}

// extractEntClientTable walks a call expression chain to find the ent table name.
// e.g. m.Client().IamAPIPermission.Update().Where(...).Save(ctx)
//      → finds "IamAPIPermission" → looks up entTableMap → "iam_api_permission"
func extractEntClientTable(call ast.Expr, entTableMap map[string]string) string {
	// Walk inward through .Save() → .SetX() → .Where() → .Update() → .IamAPIPermission → .Client() → m
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
		// Check if the receiver is a field access like .IamAPIPermission
		if inner, ok := sel.X.(*ast.CallExpr); ok {
			// Could be m.Client().IamAPIPermission — check the selector on Client() result
			if innerSel, ok := inner.Fun.(*ast.SelectorExpr); ok {
				if innerSel.Sel.Name == "Client" {
					// Next level should be .IamAPIPermission
					// Actually the pattern is: m.Client().IamAPIPermission.Update()
					// sel.X = m.Client().IamAPIPermission (SelectorExpr)
					// We need to look at sel.X as a SelectorExpr
				}
			}
		}
		// Check if sel.X is a selector like m.Client().IamAPIPermission
		if sExpr, ok := sel.X.(*ast.SelectorExpr); ok {
			name := sExpr.Sel.Name
			// Check if this is an ent entity name (PascalCase, in entTableMap)
			lowerName := strings.ToLower(name)
			for schemaType, table := range entTableMap {
				if strings.ToLower(schemaType) == lowerName {
					return table
				}
			}
		}
		cur = sel.X
	}
	return ""
}

// extractEntChainInfoFromExpr extracts WHERE/SET/ORDER info from a call expression chain.
func extractEntChainInfoFromExpr(call ast.Expr) entChainInfo {
	// Wrap in a fake block to reuse extractEntChainInfo
	// We do this by walking the chain manually
	var info entChainInfo
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
		method := sel.Sel.Name
		switch method {
		case "Where":
			for _, arg := range c.Args {
				if cond := extractEntPredicate(arg); cond != "" {
					info.whereConds = append(info.whereConds, cond)
				}
			}
		case "Limit":
			info.hasLimit = true
		case "Offset":
			info.hasOffset = true
		case "OnConflict", "OnConflictColumns":
			info.hasUpsert = true
		default:
			if strings.HasPrefix(method, "Set") {
				if field := extractSetFieldName(method); field != "" {
					info.setCols = append(info.setCols, field)
				}
			}
		}
		cur = sel.X
	}
	return info
}

// entChainInfoToSQL produces a SQL string from extracted chain info and terminal method.
func entChainInfoToSQL(terminal, table string, info entChainInfo) string {
	switch terminal {
	case "Save", "SaveX", "Exec", "ExecX":
		where := buildWhereClause(info.whereConds)
		setCols := buildSetClause(info.setCols)
		if setCols == "..." {
			return ""
		}
		return fmt.Sprintf("UPDATE %s SET %s%s", table, setCols, where)
	case "All":
		where := buildWhereClause(info.whereConds)
		return fmt.Sprintf("SELECT * FROM %s%s", table, where)
	case "Count":
		where := buildWhereClause(info.whereConds)
		return fmt.Sprintf("SELECT COUNT(*) FROM %s%s", table, where)
	default:
		return ""
	}
}

func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
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
