package checker

// CallGraphCache builds a CHA call graph for the target project once and exposes
// query APIs used by N+1 detection and Logic Review.
//
// CHA (Class Hierarchy Analysis) is chosen because:
//   - Fast: only needs type info, no SSA construction
//   - Accurate enough for projects where each DAO interface has exactly one impl
//   - Already available in golang.org/x/tools/go/callgraph/cha
//
// Build cost: ~5-20s for a medium Go project (internal/... only).
// All subsequent analyses share the same cache at zero additional cost.

import (
	"fmt"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// IONode, IOKind, IOKindDB, IOKindRedis are declared in logic_io.go (same package).

// CallGraphCache is the shared, pre-built call graph result.
type CallGraphCache struct {
	// prog is the SSA program (needed for querying)
	prog *ssa.Program

	// cg is the CHA call graph
	cg *callgraph.Graph

	// fset is shared across all packages
	fset *token.FileSet

	// implIdx: TypeName.MethodName → implEntry (for SQL derivation)
	implIdx implIndexType

	// pkgs: all loaded packages
	pkgs []*packages.Package

	// moduleName of the target project
	moduleName string

	// dir is the target project root
	dir string
}

// BuildCallGraph loads the project's internal packages, builds SSA and CHA call graph.
// This is called once; the result is passed to all checkers.
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
		// No special build flags — we need ent packages to be visible
		// so the call graph can contain ent terminal method nodes.
	}

	// Load all packages: internal/... includes dao/impl which calls ent,
	// and ent itself is reachable via NeedDeps.
	// We use "./..." to get the full picture but still scoped to the project dir.
	patterns := []string{"./..."}

	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("packages.Load: %w", err)
	}

	// Collect packages that loaded without errors.
	// Some generated packages (mock, protobuf) may have minor errors — skip only
	// the truly broken ones (Types == nil).
	var goodPkgs []*packages.Package
	packages.Visit(pkgs, func(p *packages.Package) bool {
		if p.Types != nil {
			goodPkgs = append(goodPkgs, p)
		}
		return true
	}, nil)

	if len(goodPkgs) == 0 {
		return nil, fmt.Errorf("no packages loaded successfully (check go build ./... in %s)", dir)
	}

	// Build SSA
	prog, _ := ssautil.AllPackages(goodPkgs, ssa.BuilderMode(0))
	prog.Build()

	// Build CHA call graph
	cg := cha.CallGraph(prog)

	// Build impl index for SQL derivation (reuses existing AST-based builder)
	implIdx := buildImplIndex(dir, moduleName, fset)

	return &CallGraphCache{
		prog:       prog,
		cg:         cg,
		fset:       fset,
		implIdx:    implIdx,
		pkgs:       goodPkgs,
		moduleName: moduleName,
		dir:        dir,
	}, nil
}

// ── Query API ─────────────────────────────────────────────────────────────────

// ReachableIO returns all DB and Redis calls reachable from the given SSA function,
// in call-order (BFS), with their SQL/Redis command derived from ent AST.
// depth limits BFS depth to avoid infinite recursion in cyclic call graphs.
func (c *CallGraphCache) ReachableIO(fn *ssa.Function, depth int) []IONode {
	if fn == nil || depth <= 0 {
		return nil
	}

	var nodes []IONode
	visited := make(map[*callgraph.Node]bool)
	c.bfsIO(c.cg.Nodes[fn], []string{fn.Name()}, visited, depth, &nodes)
	return nodes
}

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

		// Get call site position
		pos := token.NoPos
		if edge.Site != nil {
			pos = edge.Site.Pos()
		}
		position := c.prog.Fset.Position(pos)
		file := position.Filename
		line := position.Line
		short := shortPath(file)

		// Check if this is a DB or Redis call
		if node, ok := c.classifyCall(fn, file, line, short, chain); ok {
			*out = append(*out, node)
			// Don't recurse into DAO impl — we have the SQL already
			continue
		}

		// Recurse into internal business functions (not stdlib, not ent)
		if isInternalFunc(fn, c.moduleName) {
			newChain := append(append([]string{}, chain...), fn.Name())
			c.bfsIO(callee, newChain, visited, depth-1, out)
		}
	}
}

// classifyCall determines if an SSA function is a DB or Redis IO call.
// Returns (IONode, true) if it is, (zero, false) otherwise.
func (c *CallGraphCache) classifyCall(
	fn *ssa.Function,
	file string,
	line int,
	short string,
	chain []string,
) (IONode, bool) {
	if fn.Object() == nil {
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

	recvType := recv.Type()
	typeName := typeBaseName(recvType)
	method := fn.Name()
	recvStr := typeName // e.g. "iamuserroleOceanBaseDao"

	lowerType := strings.ToLower(typeName)

	// ── Redis ──
	// go-zero Redis client type names
	if strings.Contains(lowerType, "redis") || strings.Contains(lowerType, "redisclient") {
		cmd := redisCmd(method)
		return IONode{
			File: file, ShortFile: short, Line: line,
			CallChain: chain,
			Kind:      IOKindRedis,
			Receiver:  typeName, Method: method,
			RedisCmd: cmd,
		}, true
	}

	// ── DB / DAO ──
	// Concrete DAO impl types end with "Dao" (e.g. iamuserroleOceanBaseDao)
	if !strings.HasSuffix(lowerType, "dao") {
		return IONode{}, false
	}
	if !isIOVerb(method) {
		return IONode{}, false
	}

	// Derive SQL from impl AST
	sql, exact := c.deriveSQL(typeName, method)
	return IONode{
		File: file, ShortFile: short, Line: line,
		CallChain: chain,
		Kind:      IOKindDB,
		Receiver:  recvStr, Method: method,
		SQL: sql, SQLExact: exact,
	}, true
}

// deriveSQL finds the impl body for typeName.method and derives SQL from ent terminals.
func (c *CallGraphCache) deriveSQL(typeName, method string) (sql string, exact bool) {
	key := typeName + "." + method
	entries, ok := c.implIdx[key]
	if ok && len(entries) > 0 {
		_, terminal := astFindEntTerminal(entries[0].funcDecl.Body, entries[0].fset, entries[0].file, 0, 4)
		if terminal != "" {
			table := implStructToTable(typeName)
			sql = entTerminalToSQL(terminal, method, table, entries[0].funcDecl)
			return sql, true
		}
	}
	// Fallback: method name heuristic
	return methodNameToSQL(method, typeName), false
}

// FindSSAFunc looks up an SSA function by package+name pattern.
// Used by Logic Review to find the entry point for each Logic method.
func (c *CallGraphCache) FindSSAFunc(pkgPath, funcName string) *ssa.Function {
	for _, p := range c.prog.AllPackages() {
		if p.Pkg == nil {
			continue
		}
		if !strings.HasSuffix(p.Pkg.Path(), pkgPath) && p.Pkg.Path() != pkgPath {
			continue
		}
		if m := p.Members[funcName]; m != nil {
			if f, ok := m.(*ssa.Function); ok {
				return f
			}
		}
	}
	return nil
}

// FindMethodSSAFunc finds an SSA function for a method on a named type.
func (c *CallGraphCache) FindMethodSSAFunc(pkgPath, typeName, methodName string) *ssa.Function {
	for _, p := range c.prog.AllPackages() {
		if p.Pkg == nil {
			continue
		}
		if p.Pkg.Path() != pkgPath && !strings.HasSuffix(p.Pkg.Path(), pkgPath) {
			continue
		}
		// Look for (*TypeName).MethodName or (TypeName).MethodName
		for _, name := range []string{
			"(" + typeName + ")." + methodName,
			"(*" + typeName + ")." + methodName,
		} {
			for _, m := range p.Members {
				if f, ok := m.(*ssa.Function); ok && f.Name() == name {
					return f
				}
			}
		}
		// Also try direct method lookup via type's method set
		if t := p.Pkg.Scope().Lookup(typeName); t != nil {
			if tn, ok := t.(*types.TypeName); ok {
				for _, pkg := range c.prog.AllPackages() {
					if pkg.Pkg == tn.Pkg() {
						sel := types.NewMethodSet(types.NewPointer(tn.Type()))
						for i := 0; i < sel.Len(); i++ {
							obj := sel.At(i).Obj()
							if obj.Name() == methodName {
								if f := pkg.Prog.FuncValue(obj.(*types.Func)); f != nil {
									return f
								}
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// AllLogicFuncs returns all exported methods on *Logic types within internal/logic/.
// SSA stores methods in prog.AllFunctions(), not in package.Members (which only has
// package-level functions). We must iterate all SSA functions and filter by receiver type.
func (c *CallGraphCache) AllLogicFuncs() []LogicFuncEntry {
	var result []LogicFuncEntry
	seen := map[string]bool{}

	for fn := range c.cg.Nodes {
		if fn == nil || fn.Signature == nil {
			continue
		}
		if fn.Package() == nil || fn.Package().Pkg == nil {
			continue
		}
		pkgPath := fn.Package().Pkg.Path()
		if !strings.Contains(pkgPath, "/internal/logic/") {
			continue
		}
		// SSA method name format: "(*TypeName).MethodName" or "(TypeName).MethodName"
		// Extract just the method name (after the last '.')
		rawName := fn.Name()
		methodName := rawName
		if dot := strings.LastIndex(rawName, "."); dot >= 0 {
			methodName = rawName[dot+1:]
		}
		// Must be exported
		if !isExported(methodName) {
			continue
		}
		// Receiver must end with "Logic"
		recv := fn.Signature.Recv()
		if recv == nil {
			continue
		}
		recvType := typeBaseName(recv.Type())
		if !strings.HasSuffix(recvType, "Logic") {
			continue
		}
		// Skip constructors
		if strings.HasPrefix(methodName, "New") {
			continue
		}
		key := pkgPath + "." + recvType + "." + methodName
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, LogicFuncEntry{
			PkgPath:  pkgPath,
			TypeName: recvType,
			Method:   methodName,
			Func:     fn,
		})
	}
	return result
}

// LogicFuncEntry is one exported Logic method found in the call graph.
type LogicFuncEntry struct {
	PkgPath  string
	TypeName string
	Method   string
	Func     *ssa.Function
}

// ── Helpers ───────────────────────────────────────────────────────────────────

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
