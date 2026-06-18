package checker

// Logic Review — storage-focused code review for Logic entry points.
//
// For each exported Logic method this module:
//   1. Uses the pre-built CHA call graph to trace ALL reachable DB/Redis calls
//   2. Records the complete call chain leading to each IO op
//   3. Derives precise SQL from ent terminal methods via AST
//   4. Records Redis commands from the method name mapping
//   5. Preserves call order (BFS ≈ execution order)

import (
	"fmt"
	"go/ast"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ── Shared IO types (used by callgraph.go and report_html.go) ────────────────

// IOKind is the type of IO operation.
type IOKind string

const (
	IOKindDB    IOKind = "DB"
	IOKindRedis IOKind = "Redis"
)

// IONode is one DB or Redis call reachable from a Logic entry point.
type IONode struct {
	File      string
	ShortFile string
	Line      int

	// CallChain: function names from entry point to this call
	// e.g. ["PassportLogin", "createSessionAndCode", "storeSession"]
	CallChain []string

	Kind     IOKind
	Receiver string // concrete type name, e.g. "iamuserroleOceanBaseDao"
	Method   string // e.g. "List", "SetexCtx"

	// DB
	SQL      string
	SQLExact bool     // true = derived from ent AST terminal or runtime capture
	HookSQLs []string // additional SQL from ent hook cascades (runtime-captured)

	// Redis
	RedisCmd string
	// RedisKeyHint: extracted key prefix from call args, e.g. "passport:auth_session:{sessionID}"
	RedisKeyHint string
	// RedisTTLHint: extracted TTL hint from call args
	RedisTTLHint string

	// Snippet: source code lines surrounding this call (for HTML display)
	Snippet []SourceLine

	// InLoop reports whether this call site lies inside a for/range loop body
	// (anywhere along the call chain's caller file). When true, each invocation
	// represents N executions per Logic call.
	InLoop bool
	// LoopLine is the line number of the enclosing for/range keyword (best effort,
	// innermost loop wins). 0 when InLoop is false.
	LoopLine int
}

// ── Logic Review result types ─────────────────────────────────────────────────

// LogicReviewMethod is one Logic entry point with its full IO trace.
type LogicReviewMethod struct {
	PkgPath    string
	TypeName   string
	Method     string
	Module     string // first subdir under logic/
	SubModule  string // second subdir
	LogicFile  string // short file path of the Logic method definition
	LogicLine  int    // line number of the Logic method definition
	Signature  string // proto-style signature, e.g. "UpdateAPI(in *passport.UpdateAPIReq) (*passport.Empty, error)"
	Ops        []IONode

	// Counts. Each IO op = one IO trip.
	// "Static" = call site NOT inside any for/range loop.
	// "Loop"   = call site inside a for/range loop (each represents N executions).
	// "Hook"   = additional SQL triggered by ent hooks on a mutation op (one per HookSQL).
	DBCount        int // total DB ops = static + loop (does not include hook cascades)
	DBStaticCount  int
	DBLoopCount    int
	DBHookCount    int // total hook-cascade SQL statements
	RedisCount     int
	RedisStaticCount int
	RedisLoopCount   int

	// Pretty-formatted strings ready for HTML, e.g. "3", "2 + 1×N", "5 + 2×N + 3 hook"
	DBSummary    string
	RedisSummary string
}

// LogicReviewResult holds all logic methods.
type LogicReviewResult struct {
	Methods []LogicReviewMethod

	// Aggregated totals across all Logic methods (for top-level summary card).
	TotalDB        int
	TotalDBStatic  int
	TotalDBLoop    int
	TotalDBHook    int
	TotalRedis     int
	TotalRedisStatic int
	TotalRedisLoop   int

	TotalDBSummary    string
	TotalRedisSummary string
}

var lastLogicReviewResult *LogicReviewResult

// LastLogicReviewResult returns the last computed logic review result.
// Used by SQL perf analysis to derive findings without re-scanning.
func LastLogicReviewResult() *LogicReviewResult { return lastLogicReviewResult }

// RunLogicReview reads pre-computed IO subgraphs from the call graph cache.
// SQL is derived entirely from call graph + implIdx AST analysis — no probe, no guessing.
func RunLogicReview(cgCache *CallGraphCache) *Result {
	if cgCache == nil {
		return Skip("call graph not available (build failed or skipped)")
	}

	entries := cgCache.AllLogicEntries()
	if len(entries) == 0 {
		return Pass("no Logic methods found in call graph")
	}

	result := &LogicReviewResult{}

	for _, e := range entries {
		ops := cgCache.IOForLogic(IOKey(e)) // O(1) map lookup
		if len(ops) == 0 {
			continue
		}
		mod, sub := splitLogicPath(e.PkgPath)
		lm := LogicReviewMethod{
			PkgPath:   e.PkgPath,
			TypeName:  e.TypeName,
			Method:    e.Method,
			Module:    mod,
			SubModule: sub,
			LogicFile: e.ShortFile,
			LogicLine: e.Line,
			Signature: e.Signature,
			Ops:       ops,
		}
		for _, op := range ops {
			if op.Kind == IOKindDB {
				lm.DBCount++
				if op.InLoop {
					lm.DBLoopCount++
				} else {
					lm.DBStaticCount++
				}
				lm.DBHookCount += len(op.HookSQLs)
			} else {
				lm.RedisCount++
				if op.InLoop {
					lm.RedisLoopCount++
				} else {
					lm.RedisStaticCount++
				}
			}
		}
		lm.DBSummary = formatIOSummary(lm.DBStaticCount, lm.DBLoopCount, lm.DBHookCount)
		lm.RedisSummary = formatIOSummary(lm.RedisStaticCount, lm.RedisLoopCount, 0)

		result.TotalDB += lm.DBCount
		result.TotalDBStatic += lm.DBStaticCount
		result.TotalDBLoop += lm.DBLoopCount
		result.TotalDBHook += lm.DBHookCount
		result.TotalRedis += lm.RedisCount
		result.TotalRedisStatic += lm.RedisStaticCount
		result.TotalRedisLoop += lm.RedisLoopCount

		result.Methods = append(result.Methods, lm)
	}

	result.TotalDBSummary = formatIOSummary(result.TotalDBStatic, result.TotalDBLoop, result.TotalDBHook)
	result.TotalRedisSummary = formatIOSummary(result.TotalRedisStatic, result.TotalRedisLoop, 0)

	lastLogicReviewResult = result

	if len(result.Methods) == 0 {
		return Pass("no Logic methods with storage IO found")
	}

	var issues []string
	for _, m := range result.Methods {
		issues = append(issues, fmt.Sprintf("%s.%s DB=%s Redis=%s",
			m.TypeName, m.Method, m.DBSummary, m.RedisSummary))
	}

	return &Result{
		Level: LevelInfo,
		Summary: fmt.Sprintf("logic review: %d methods, DB=%s Redis=%s",
			len(result.Methods), result.TotalDBSummary, result.TotalRedisSummary),
		Issues: issues,
	}
}

// formatIOSummary renders counts into a compact human-readable string.
//
// Examples:
//
//	formatIOSummary(3, 0, 0) → "3"
//	formatIOSummary(2, 1, 0) → "2 + 1×N"
//	formatIOSummary(0, 2, 0) → "2×N"
//	formatIOSummary(3, 1, 2) → "3 + 1×N + 2 hook"
//	formatIOSummary(0, 0, 0) → "0"
//
// N denotes the loop iteration count (one per call site, may differ between sites).
func formatIOSummary(staticCnt, loopCnt, hookCnt int) string {
	if staticCnt == 0 && loopCnt == 0 && hookCnt == 0 {
		return "0"
	}
	var parts []string
	if staticCnt > 0 {
		parts = append(parts, fmt.Sprintf("%d", staticCnt))
	}
	if loopCnt > 0 {
		if loopCnt == 1 {
			parts = append(parts, "N")
		} else {
			parts = append(parts, fmt.Sprintf("%d×N", loopCnt))
		}
	}
	if hookCnt > 0 {
		parts = append(parts, fmt.Sprintf("%d hook", hookCnt))
	}
	return strings.Join(parts, " + ")
}


func splitLogicPath(pkgPath string) (mod, sub string) {
	idx := strings.Index(pkgPath, "/logic/")
	if idx < 0 {
		return pkgPath, ""
	}
	rest := pkgPath[idx+len("/logic/"):]
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

// ── IO helper functions (shared with callgraph.go) ────────────────────────────

func isIOVerb(method string) bool {
	verbs := []string{
		"Get", "List", "Find", "Query", "Count", "Exist",
		"Create", "Insert", "Save", "Update", "Delete",
		"Upsert", "Batch", "SoftDelete",
	}
	for _, v := range verbs {
		if strings.HasPrefix(method, v) {
			return true
		}
	}
	return false
}

// implStructToTable converts an impl struct name to a table name.
// It first tries to extract from the impl file name (most accurate for ent projects),
// then falls back to converting the struct name.
//
// e.g. struct "iamuserroleOceanBaseDao", file "iam_user_role_oceanbase.go"
//      → file stem "iam_user_role_oceanbase" → strip "_oceanbase" → "iam_user_role"
func implStructToTable(structName string) string {
	return implStructToTableWithFile(structName, "")
}

func implStructToTableWithFile(structName, filePath string) string {
	// Try file-based extraction first (reliable: ent generates deterministic filenames)
	if filePath != "" {
		base := filepath.Base(filePath)
		// Remove .go extension
		stem := strings.TrimSuffix(base, ".go")
		// Strip common db suffixes: _oceanbase, _mysql, _postgres, _sqlite, _impl
		for _, sfx := range []string{"_oceanbase", "_mysql", "_postgres", "_sqlite", "_impl", "_dao"} {
			stem = strings.TrimSuffix(stem, sfx)
		}
		if stem != "" && stem != "." {
			return stem // already snake_case from filename
		}
	}
	// Fallback: strip Dao suffix from struct name, then convert camel → snake
	s := structName
	for _, suffix := range []string{"OceanBaseDao", "MysqlDao", "PostgresDao", "SqliteDao", "Dao"} {
		if strings.HasSuffix(s, suffix) {
			s = strings.TrimSuffix(s, suffix)
			break
		}
	}
	return camelToSnake(s)
}

// camelToSnake converts a CamelCase string to snake_case, handling consecutive capitals.
// "IamUserRole" → "iam_user_role"
// "iamuserrole" → "iamuserrole" (no capitals to split on — limitation of all-lowercase)
// camelToSnake converts CamelCase / acronyms to snake_case.
//
//	"IamUserRole" → "iam_user_role"
//	"UID"         → "uid"          (all-caps treated as one word)
//	"UIDEQ"       → "uid_eq"       (transition from acronym to next word)
//	"AppCode"     → "app_code"
//	"APIID"       → "api_id"
func camelToSnake(s string) string {
	if s == "" {
		return s
	}
	var out []byte
	n := len(s)
	for i := 0; i < n; i++ {
		c := s[i]
		upper := c >= 'A' && c <= 'Z'
		if i > 0 && upper {
			prev := s[i-1]
			prevUpper := prev >= 'A' && prev <= 'Z'
			// Insert underscore when:
			// 1. transition lower→upper: "appCode" → "app_code"
			// 2. transition upper-seq→upper+lower: "APIId" → "api_id"
			//    i.e. prev is upper, current is upper, next is lower
			nextLower := i+1 < n && s[i+1] >= 'a' && s[i+1] <= 'z'
			if !prevUpper || nextLower {
				out = append(out, '_')
			}
		}
		out = append(out, c | 0x20)
	}
	return string(out)
}



// ── ent schema table map ──────────────────────────────────────────────────────

// buildEntSchemaTableMap parses ent/schema/*.go and returns a map of
// Go type name → actual table name.
//
// For each schema type it checks:
//  1. entsql.Annotation{Table: "xxx"} in Annotations() → use that table name
//  2. No annotation → use default: toSnake(TypeName)
//
// e.g. "IamUserRole" → "iam_user_role"
//      "IamUserRole" with Table:"custom" → "custom"
func buildEntSchemaTableMap(dir string) map[string]string {
	schemaDir := filepath.Join(dir, "ent", "schema")
	tableMap := make(map[string]string)

	fset := token.NewFileSet()
	_ = filepath.WalkDir(schemaDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
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
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				typeName := ts.Name.Name
				// Default table name: snake_case of type name
				defaultTable := camelToSnake(typeName)
				// Try to find entsql.Annotation{Table: "..."} in Annotations()
				table := findAnnotationTable(f, typeName)
				if table == "" {
					table = defaultTable
				}
				tableMap[typeName] = table
			}
		}
		return nil
	})
	return tableMap
}

// findAnnotationTable searches for entsql.Annotation{Table: "..."} in the
// Annotations() method of a given type in the file.
func findAnnotationTable(f *ast.File, typeName string) string {
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Name.Name != "Annotations" || fd.Body == nil {
			continue
		}
		// Check receiver is typeName
		if len(fd.Recv.List) == 0 {
			continue
		}
		recv := fd.Recv.List[0]
		recvName := ""
		switch t := recv.Type.(type) {
		case *ast.Ident:
			recvName = t.Name
		case *ast.StarExpr:
			if id, ok := t.X.(*ast.Ident); ok {
				recvName = id.Name
			}
		}
		if recvName != typeName {
			continue
		}
		// Found Annotations() for this type — scan body for entsql.Annotation{Table: "..."}
		var table string
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if table != "" {
				return false
			}
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			// Check if it's entsql.Annotation or Annotation
			isAnnotation := false
			switch t := cl.Type.(type) {
			case *ast.SelectorExpr:
				isAnnotation = t.Sel.Name == "Annotation"
			case *ast.Ident:
				isAnnotation = t.Name == "Annotation"
			}
			if !isAnnotation {
				return true
			}
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Table" {
					continue
				}
				lit, ok := kv.Value.(*ast.BasicLit)
				if !ok {
					continue
				}
				// Strip surrounding quotes
				table = strings.Trim(lit.Value, `"`)
			}
			return true
		})
		if table != "" {
			return table
		}
	}
	return ""
}

// entTerminalToSQL converts a confirmed ent terminal + method name to a SQL pattern.
// It performs deep AST extraction of WHERE predicates, SET columns, and SELECT columns
// to produce accurate SQL without placeholder "..." tokens.
func entTerminalToSQL(terminal, method, table string, fd *ast.FuncDecl) string {
	if fd == nil || fd.Body == nil {
		return fmt.Sprintf("-- ent.%s on %s", terminal, table)
	}

	info := extractEntChainInfo(fd.Body)

	switch terminal {
	case "All":
		cols := buildSelectCols(info.selectCols)
		where := buildWhereClause(info.whereConds)
		order := buildOrderClause(info.orderCols)
		limit := buildLimitClause(info.hasLimit, info.hasOffset)
		return fmt.Sprintf("SELECT %s FROM %s%s%s%s", cols, table, where, order, limit)

	case "Only", "First":
		cols := buildSelectCols(info.selectCols)
		where := buildWhereClause(info.whereConds)
		return fmt.Sprintf("SELECT %s FROM %s%s LIMIT 1", cols, table, where)

	case "Count":
		where := buildWhereClause(info.whereConds)
		return fmt.Sprintf("SELECT COUNT(*) FROM %s%s", table, where)

	case "Exist", "ExistX", "Exists":
		where := buildWhereClause(info.whereConds)
		return fmt.Sprintf("SELECT 1 FROM %s%s LIMIT 1", table, where)

	case "IDs":
		where := buildWhereClause(info.whereConds)
		order := buildOrderClause(info.orderCols)
		limit := buildLimitClause(info.hasLimit, info.hasOffset)
		return fmt.Sprintf("SELECT id FROM %s%s%s%s", table, where, order, limit)

	case "Save", "SaveX":
		if strings.HasPrefix(method, "Create") || strings.HasPrefix(method, "Insert") {
			cols, vals := buildInsertParts(info.setCols, info.upsertCols)
			if info.hasUpsert {
				upsertSet := buildUpsertSet(info.upsertCols)
				return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
					table, cols, vals, upsertSet)
			}
			return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, cols, vals)
		}
		setCols := buildSetClause(info.setCols)
		where := buildWhereClause(info.whereConds)
		return fmt.Sprintf("UPDATE %s SET %s%s", table, setCols, where)

	case "Exec", "ExecX":
		if strings.HasPrefix(method, "Create") || strings.HasPrefix(method, "Insert") {
			cols, vals := buildInsertParts(info.setCols, info.upsertCols)
			if info.hasUpsert {
				upsertSet := buildUpsertSet(info.upsertCols)
				return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
					table, cols, vals, upsertSet)
			}
			return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, cols, vals)
		}
		if strings.Contains(strings.ToLower(method), "delete") ||
			strings.Contains(strings.ToLower(method), "softdelete") {
			where := buildWhereClause(info.whereConds)
			return fmt.Sprintf("UPDATE %s SET deleted_at = NOW()%s  -- soft delete", table, where)
		}
		setCols := buildSetClause(info.setCols)
		where := buildWhereClause(info.whereConds)
		return fmt.Sprintf("UPDATE %s SET %s%s", table, setCols, where)

	default:
		return fmt.Sprintf("-- ent.%s() on %s", terminal, table)
	}
}

// ── entChainInfo: extracted info from ent method chain AST ───────────────────

type entChainInfo struct {
	whereConds []string // e.g. ["uid = ?", "status IN (?, ?)"]
	setCols    []string // e.g. ["password_hash", "email"]
	selectCols []string // explicitly selected columns
	orderCols  []string // e.g. ["created_at DESC"]
	upsertCols []string // columns in OnConflict update
	hasLimit   bool
	hasOffset  bool
	hasUpsert  bool
}

// extractEntChainInfo walks the AST of a DAO impl body and extracts
// ent method chain metadata: WHERE predicates, SET columns, SELECT columns, etc.
func extractEntChainInfo(body *ast.BlockStmt) entChainInfo {
	var info entChainInfo
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		method := sel.Sel.Name

		switch method {
		case "Where":
			// Each Where() call has predicate args like iamuser.AccountEQ(account)
			for _, arg := range call.Args {
				if cond := extractEntPredicate(arg); cond != "" {
					info.whereConds = append(info.whereConds, cond)
				}
			}
		case "Limit", "Offset":
			if method == "Limit" {
				info.hasLimit = true
			} else {
				info.hasOffset = true
			}
		case "Order", "OrderBy":
			for _, arg := range call.Args {
				if col := extractOrderArg(arg); col != "" {
					info.orderCols = append(info.orderCols, col)
				}
			}
		case "Select":
			for _, arg := range call.Args {
				if col := extractStringLitOrIdent(arg); col != "" {
					info.selectCols = append(info.selectCols, col)
				}
			}
		case "OnConflict", "OnConflictColumns":
			info.hasUpsert = true
		default:
			// SetXxx / SetNillableXxx / ClearXxx calls
			if strings.HasPrefix(method, "Set") {
				field := extractSetFieldName(method)
				if field != "" {
					info.setCols = append(info.setCols, field)
				}
			}
		}
		return true
	})
	return info
}

// extractEntPredicate extracts the WHERE condition from an ent predicate call.
// e.g. iamuser.AccountEQ(account) → "account = ?"
//
//	iamuser.UIDIn(uids...)   → "uid IN (?)"
//	iamuser.StatusEQ(s)      → "status = ?"
func extractEntPredicate(expr ast.Expr) string {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return ""
	}

	// Get function name: could be pkg.FuncName or just FuncName
	funcName := ""
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		funcName = f.Sel.Name // e.g. "AccountEQ", "UIDIn"
	case *ast.Ident:
		funcName = f.Name
	}
	if funcName == "" {
		return ""
	}

	// Handle And/Or combinators
	lowerFn := strings.ToLower(funcName)
	if lowerFn == "and" || lowerFn == "or" || lowerFn == "not" {
		var parts []string
		for _, arg := range call.Args {
			if p := extractEntPredicate(arg); p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		sep := " AND "
		if lowerFn == "or" {
			sep = " OR "
		}
		joined := strings.Join(parts, sep)
		if len(parts) > 1 {
			return "(" + joined + ")"
		}
		return joined
	}

	// Parse field + operator from the ent predicate name
	// e.g. "AccountEQ" → field="account", op="EQ"
	// e.g. "UIDIn"     → field="uid", op="In"
	// e.g. "StatusIn"  → field="status", op="In"
	// e.g. "DeletedAtIsNil" → field="deleted_at", op="IsNil"
	field, op := parseEntPredicateName(funcName)
	if field == "" {
		return ""
	}

	switch op {
	case "EQ", "Eq":
		return field + " = ?"
	case "NEQ", "NEq", "NE":
		return field + " != ?"
	case "GT":
		return field + " > ?"
	case "GTE":
		return field + " >= ?"
	case "LT":
		return field + " < ?"
	case "LTE":
		return field + " <= ?"
	case "In":
		return field + " IN (?)"
	case "NotIn":
		return field + " NOT IN (?)"
	case "Contains", "ContainsFold":
		return field + " LIKE '%?%'"
	case "HasPrefix":
		return field + " LIKE '?%'"
	case "HasSuffix":
		return field + " LIKE '%?'"
	case "IsNil", "IsNull":
		return field + " IS NULL"
	case "NotNil", "NotNull":
		return field + " IS NOT NULL"
	case "Between":
		return field + " BETWEEN ? AND ?"
	default:
		if op != "" {
			return field + " " + op + " ?"
		}
		return field + " = ?"
	}
}

// parseEntPredicateName parses an ent predicate function name into (snakeField, operator).
// e.g. "AccountEQ" → ("account", "EQ")
//
//	"UIDIn"      → ("uid", "In")
//	"DeletedAtIsNil" → ("deleted_at", "IsNil")
//	"UIDEQ"      → ("uid", "EQ")  — all-caps field
func parseEntPredicateName(name string) (field, op string) {
	// Known suffixes in priority order (longest first to avoid partial matches)
	suffixes := []string{
		"ContainsFold", "HasPrefix", "HasSuffix",
		"Contains", "IsNil", "NotNil", "IsNull", "NotNull",
		"NotIn", "Between",
		"NEQ", "GTE", "LTE", "NEq",
		"EQ", "GT", "LT", "In", "Eq", "NE",
	}
	for _, sfx := range suffixes {
		if strings.HasSuffix(name, sfx) {
			rawField := strings.TrimSuffix(name, sfx)
			if rawField == "" {
				continue
			}
			return camelToSnake(rawField), sfx
		}
	}
	// Fallback: whole name as field
	return camelToSnake(name), "EQ"
}

// extractSetFieldName extracts a snake_case column name from a Set method name.
// "SetPasswordHash"   → "password_hash"
// "SetNillableEmail"  → "email"
// "SetUID"            → "uid"
func extractSetFieldName(method string) string {
	s := strings.TrimPrefix(method, "Set")
	if strings.HasPrefix(s, "Nillable") {
		s = strings.TrimPrefix(s, "Nillable")
	}
	if s == "" {
		return ""
	}
	return camelToSnake(s)
}

// extractOrderArg extracts an order column from an Order() argument.
// Handles: ent.Desc("created_at"), ent.Asc("id"), string literal
func extractOrderArg(expr ast.Expr) string {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return extractStringLitOrIdent(expr)
	}
	var funcName string
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		funcName = f.Sel.Name
	case *ast.Ident:
		funcName = f.Name
	}
	dir := ""
	switch strings.ToLower(funcName) {
	case "desc":
		dir = " DESC"
	case "asc":
		dir = " ASC"
	}
	if len(call.Args) > 0 {
		col := extractStringLitOrIdent(call.Args[0])
		if col != "" {
			return col + dir
		}
	}
	return ""
}

// extractStringLitOrIdent extracts a string literal value or identifier name from an AST expression.
func extractStringLitOrIdent(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.BasicLit:
		return strings.Trim(v.Value, `"` + "`")
	case *ast.Ident:
		// Constant-like: iamuser.FieldAccount → extract last part
		return camelToSnake(v.Name)
	case *ast.SelectorExpr:
		return camelToSnake(v.Sel.Name)
	}
	return ""
}

// ── SQL builder helpers ───────────────────────────────────────────────────────

func buildSelectCols(cols []string) string {
	if len(cols) == 0 {
		return "*"
	}
	return strings.Join(cols, ", ")
}

func buildWhereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	// Deduplicate
	seen := make(map[string]bool)
	var unique []string
	for _, c := range conds {
		if !seen[c] {
			seen[c] = true
			unique = append(unique, c)
		}
	}
	return " WHERE " + strings.Join(unique, " AND ")
}

func buildOrderClause(cols []string) string {
	if len(cols) == 0 {
		return ""
	}
	return " ORDER BY " + strings.Join(cols, ", ")
}

func buildLimitClause(hasLimit, hasOffset bool) string {
	if hasLimit && hasOffset {
		return " LIMIT n OFFSET m"
	}
	if hasLimit {
		return " LIMIT n"
	}
	return ""
}

func buildSetClause(cols []string) string {
	if len(cols) == 0 {
		return "..."
	}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = c + " = ?"
	}
	return strings.Join(parts, ", ")
}

func buildInsertParts(setCols, upsertCols []string) (colList, valList string) {
	cols := setCols
	if len(cols) == 0 {
		cols = upsertCols
	}
	if len(cols) == 0 {
		return "...", "..."
	}
	vals := make([]string, len(cols))
	for i := range cols {
		vals[i] = "?"
	}
	return strings.Join(cols, ", "), strings.Join(vals, ", ")
}

func buildUpsertSet(upsertCols []string) string {
	if len(upsertCols) == 0 {
		return "..."
	}
	parts := make([]string, len(upsertCols))
	for i, c := range upsertCols {
		parts[i] = c + " = VALUES(" + c + ")"
	}
	return strings.Join(parts, ", ")
}

// methodNameToSQL fallback: derive SQL from DAO method name.


// redisCmd maps a go-zero Redis client method name to a Redis command string.
func redisCmd(method string) string {
	return redisCmdWithContext(method, "", "")
}

// redisCmdWithContext builds a Redis command string with optional key/TTL hints.
// keyHint: extracted key expression (e.g. "passport:auth_session:{sessionID}")
// ttlHint: extracted TTL expression (e.g. "86400" or "SessionTTLSeconds")
func redisCmdWithContext(method, keyHint, ttlHint string) string {
	m := strings.ToLower(strings.TrimSuffix(strings.ToLower(method), "ctx"))

	key := "{key}"
	if keyHint != "" {
		key = keyHint
	}
	ttl := "{ttl}"
	if ttlHint != "" {
		ttl = ttlHint
	}

	switch m {
	case "get":
		return fmt.Sprintf("GET %s", key)
	case "set":
		return fmt.Sprintf("SET %s {value}", key)
	case "setex":
		return fmt.Sprintf("SETEX %s %s {value}", key, ttl)
	case "setnx":
		return fmt.Sprintf("SETNX %s {value}", key)
	case "del":
		return fmt.Sprintf("DEL %s", key)
	case "exists":
		return fmt.Sprintf("EXISTS %s", key)
	case "expire":
		return fmt.Sprintf("EXPIRE %s %s", key, ttl)
	case "ttl":
		return fmt.Sprintf("TTL %s", key)
	case "persist":
		return fmt.Sprintf("PERSIST %s", key)
	case "hget":
		return fmt.Sprintf("HGET %s {field}", key)
	case "hset":
		return fmt.Sprintf("HSET %s {field} {value}", key)
	case "hmget":
		return fmt.Sprintf("HMGET %s {field} [field ...]", key)
	case "hmset":
		return fmt.Sprintf("HMSET %s {field} {value} [...]", key)
	case "hgetall":
		return fmt.Sprintf("HGETALL %s", key)
	case "hdel":
		return fmt.Sprintf("HDEL %s {field} [field ...]", key)
	case "incr":
		return fmt.Sprintf("INCR %s", key)
	case "incrby":
		return fmt.Sprintf("INCRBY %s {increment}", key)
	case "decr":
		return fmt.Sprintf("DECR %s", key)
	case "decrby":
		return fmt.Sprintf("DECRBY %s {decrement}", key)
	case "sadd":
		return fmt.Sprintf("SADD %s {member} [member ...]", key)
	case "smembers":
		return fmt.Sprintf("SMEMBERS %s", key)
	case "sismember":
		return fmt.Sprintf("SISMEMBER %s {member}", key)
	case "srem":
		return fmt.Sprintf("SREM %s {member} [member ...]", key)
	case "zadd":
		return fmt.Sprintf("ZADD %s {score} {member}", key)
	case "zrange":
		return fmt.Sprintf("ZRANGE %s 0 -1 [WITHSCORES]", key)
	case "zrangebyscore":
		return fmt.Sprintf("ZRANGEBYSCORE %s {min} {max}", key)
	case "zrem":
		return fmt.Sprintf("ZREM %s {member}", key)
	case "zscore":
		return fmt.Sprintf("ZSCORE %s {member}", key)
	case "zcard":
		return fmt.Sprintf("ZCARD %s", key)
	case "lrange":
		return fmt.Sprintf("LRANGE %s 0 -1", key)
	case "rpush":
		return fmt.Sprintf("RPUSH %s {value}", key)
	case "lpush":
		return fmt.Sprintf("LPUSH %s {value}", key)
	case "lpop":
		return fmt.Sprintf("LPOP %s", key)
	case "rpop":
		return fmt.Sprintf("RPOP %s", key)
	case "llen":
		return fmt.Sprintf("LLEN %s", key)
	case "mget":
		return fmt.Sprintf("MGET %s [key ...]", key)
	case "mset":
		return fmt.Sprintf("MSET %s {value} [key value ...]", key)
	case "getset":
		return fmt.Sprintf("GETSET %s {value}", key)
	case "pipelined", "pipeline":
		return "PIPELINE [multi-command batch]"
	case "eval":
		return fmt.Sprintf("EVAL {script} 1 %s [arg ...]", key)
	case "lock", "takedistributedlock":
		return fmt.Sprintf("SET %s {uuid} NX PX %s  -- distributed lock", key, ttl)
	case "unlock":
		return fmt.Sprintf("DEL %s  -- distributed unlock", key)
	case "scan":
		return fmt.Sprintf("SCAN 0 MATCH %s COUNT 100", key)
	case "keys":
		return fmt.Sprintf("KEYS %s  ⚠️ avoid in production", key)
	default:
		return fmt.Sprintf("Redis.%s %s", method, key)
	}
}

// extractRedisArgs scans a DAO/logic function body to find Redis calls matching
// the given method name, and extracts the key and TTL arguments as string hints.
// Returns (keyHint, ttlHint).
//
// Handles patterns like:
//
//	r.GetCtx(ctx, SessionKey(sessionID))
//	r.SetexCtx(ctx, key, value, ttl)
//	r.ExpireCtx(ctx, key, seconds)
func extractRedisArgs(body *ast.BlockStmt, redisMethod string) (keyHint, ttlHint string) {
	if body == nil {
		return "", ""
	}
	targetMethod := strings.ToLower(strings.TrimSuffix(strings.ToLower(redisMethod), "ctx"))

	// Pass 1: build simple variable assignment map for single-assignment vars.
	// e.g. "key := poauth.AuthorizationCodeKey(code)" → varAssigns["key"] = that CallExpr
	varAssigns := buildVarAssignMap(body)

	ast.Inspect(body, func(n ast.Node) bool {
		if keyHint != "" {
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
		m := strings.ToLower(strings.TrimSuffix(strings.ToLower(sel.Sel.Name), "ctx"))
		if m != targetMethod {
			return true
		}

		// go-zero Redis arg layouts (after ctx at args[0]):
		//   GetCtx(ctx, key)                  → key=args[1]
		//   SetexCtx(ctx, key, value, seconds) → key=args[1], ttl=args[3]
		//   ExpireCtx(ctx, key, seconds)       → key=args[1], ttl=args[2]
		//   LockCtx(ctx, key, seconds)         → key=args[1], ttl=args[2]
		args := call.Args
		keyIdx := 1
		ttlIdx := -1
		switch targetMethod {
		case "setex":
			ttlIdx = 3 // (ctx, key, value, seconds)
		case "expire":
			ttlIdx = 2 // (ctx, key, seconds)
		case "lock", "takedistributedlock":
			ttlIdx = 2 // (ctx, key, seconds)
		}

		if keyIdx < len(args) {
			keyHint = resolveRedisKeyExpr(args[keyIdx], varAssigns)
		}
		if ttlIdx >= 0 && ttlIdx < len(args) {
			ttlHint = resolveRedisKeyExpr(args[ttlIdx], varAssigns)
		}
		return false
	})
	return keyHint, ttlHint
}

// buildVarAssignMap builds a map of variable name → assigned expression,
// for simple short-variable declarations: key := expr
func buildVarAssignMap(body *ast.BlockStmt) map[string]ast.Expr {
	m := map[string]ast.Expr{}
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		// Only short decl (:=) with single lhs variable
		if assign.Tok.String() != ":=" {
			return true
		}
		for i, lhs := range assign.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if !ok || i >= len(assign.Rhs) {
				continue
			}
			// Only store if not already defined (first assignment wins)
			if _, exists := m[ident.Name]; !exists {
				m[ident.Name] = assign.Rhs[i]
			}
		}
		return true
	})
	return m
}

// resolveRedisKeyExpr resolves an expression to a key hint string,
// following variable assignments one level deep.
func resolveRedisKeyExpr(expr ast.Expr, varAssigns map[string]ast.Expr) string {
	// If it's a plain identifier, try to resolve to its assigned value
	if ident, ok := expr.(*ast.Ident); ok {
		if resolved, ok := varAssigns[ident.Name]; ok {
			return extractRedisKeyExpr(resolved)
		}
		return "{" + ident.Name + "}"
	}
	return extractRedisKeyExpr(expr)
}

// extractRedisKeyExpr converts an AST expression to a human-readable key hint.
// Handles: string literals, function calls like SessionKey(id), variable names.
func extractRedisKeyExpr(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.BasicLit:
		return strings.Trim(v.Value, `"` + "`")
	case *ast.Ident:
		return "{" + v.Name + "}"
	case *ast.CallExpr:
		// e.g. SessionKey(sessionID) → "SessionKey({sessionID})"
		// e.g. fmt.Sprintf("passport:auth_session:%s", id) → extract format string
		fnName := ""
		switch f := v.Fun.(type) {
		case *ast.Ident:
			fnName = f.Name
		case *ast.SelectorExpr:
			fnName = f.Sel.Name
		}
		if strings.ToLower(fnName) == "sprintf" && len(v.Args) > 0 {
			// Extract the format string
			if lit, ok := v.Args[0].(*ast.BasicLit); ok {
				return strings.Trim(lit.Value, `"` + "`")
			}
		}
		// For key functions like SessionKey(id) → show function name
		if len(v.Args) > 0 {
			argHints := make([]string, 0, len(v.Args))
			for _, arg := range v.Args {
				if h := extractRedisKeyExpr(arg); h != "" {
					argHints = append(argHints, h)
				}
			}
			if fnName != "" && len(argHints) > 0 {
				return fnName + "(" + strings.Join(argHints, ", ") + ")"
			}
			if fnName != "" {
				return fnName + "(...)"
			}
		}
		if fnName != "" {
			return fnName + "(...)"
		}
	case *ast.BinaryExpr:
		// e.g. prefix + id
		left := extractRedisKeyExpr(v.X)
		right := extractRedisKeyExpr(v.Y)
		if left != "" && right != "" {
			return left + " + " + right
		}
	case *ast.SelectorExpr:
		return camelToSnake(v.Sel.Name)
	}
	return ""
}
