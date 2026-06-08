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
	"go/token"
	"go/types"
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
	PkgPath  string
	TypeName string // e.g. "GetAllUserListLogic"
	Method   string // e.g. "GetAllUserList"
	Func     *ssa.Function
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
		Fset: fset,
		Dir:  dir,
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("packages.Load: %w", err)
	}

	var goodPkgs []*packages.Package
	packages.Visit(pkgs, func(p *packages.Package) bool {
		if p.Types != nil {
			goodPkgs = append(goodPkgs, p)
		}
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

		// Skip mock packages entirely
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
		c.logicEntries = append(c.logicEntries, LogicFuncEntry{
			PkgPath:  pkgPath,
			TypeName: recvType,
			Method:   methodName,
			Func:     fn,
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
		return IONode{
			File: file, ShortFile: short, Line: line,
			CallChain: chain,
			Kind:      IOKindRedis,
			Receiver:  typeName, Method: method,
			RedisCmd: redisCmd(method),
		}, true
	}

	// ── DB / DAO concrete impl ──
	if !strings.HasSuffix(lower, "dao") || !isIOVerb(method) {
		return IONode{}, false
	}
	sql, exact := c.deriveSQL(typeName, method)
	return IONode{
		File: file, ShortFile: short, Line: line,
		CallChain: chain,
		Kind:      IOKindDB,
		Receiver:  typeName, Method: method,
		SQL: sql, SQLExact: exact,
	}, true
}

// deriveSQL: implIdx AST first, method-name heuristic as fallback.
// Table name is resolved from entTableMap (ent schema AST), falling back to filename heuristic.
func (c *CallGraphCache) deriveSQL(typeName, method string) (string, bool) {
	if entries, ok := c.implIdx[typeName+"."+method]; ok && len(entries) > 0 {
		_, terminal := astFindEntTerminal(entries[0].funcDecl.Body, entries[0].fset, entries[0].file, 0, 4)
		if terminal != "" {
			table := c.resolveTableName(typeName, entries[0].file)
			return entTerminalToSQL(terminal, method, table, entries[0].funcDecl), true
		}
	}
	return methodNameToSQL(method, typeName), false
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
