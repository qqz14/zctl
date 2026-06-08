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
	SQLExact bool // true = derived from ent AST terminal

	// Redis
	RedisCmd string
}

// ── Logic Review result types ─────────────────────────────────────────────────

// LogicReviewMethod is one Logic entry point with its full IO trace.
type LogicReviewMethod struct {
	PkgPath   string
	TypeName  string
	Method    string
	Module    string    // first subdir under logic/
	SubModule string    // second subdir
	Ops       []IONode
	DBCount   int
	RedisCount int
}

// LogicReviewResult holds all logic methods.
type LogicReviewResult struct {
	Methods []LogicReviewMethod
}

var lastLogicReviewResult *LogicReviewResult

// RunLogicReview reads pre-computed IO subgraphs from the call graph cache.
// All heavy work was done in BuildCallGraph; this is just a map read + formatting pass.
func RunLogicReview(cgCache *CallGraphCache) *Result {
	if cgCache == nil {
		return Skip("call graph not available (build failed or skipped)")
	}

	entries := cgCache.AllLogicEntries()
	if len(entries) == 0 {
		return Pass("no Logic methods found in call graph")
	}

	result := &LogicReviewResult{}
	totalDB, totalRedis := 0, 0

	for _, e := range entries {
		ops := cgCache.IOForLogic(IOKey(e)) // O(1) map lookup
		if len(ops) == 0 {
			continue
		}
		mod, sub := splitLogicPath(e.PkgPath)
		lm := LogicReviewMethod{
			PkgPath:    e.PkgPath,
			TypeName:   e.TypeName,
			Method:     e.Method,
			Module:     mod,
			SubModule:  sub,
			Ops:        ops,
		}
		for _, op := range ops {
			if op.Kind == IOKindDB {
				lm.DBCount++
				totalDB++
			} else {
				lm.RedisCount++
				totalRedis++
			}
		}
		result.Methods = append(result.Methods, lm)
	}

	lastLogicReviewResult = result

	if len(result.Methods) == 0 {
		return Pass("no Logic methods with storage IO found")
	}

	var issues []string
	for _, m := range result.Methods {
		issues = append(issues, fmt.Sprintf("%s.%s DB×%d Redis×%d",
			m.TypeName, m.Method, m.DBCount, m.RedisCount))
	}

	return &Result{
		Level: LevelInfo,
		Summary: fmt.Sprintf("logic review: %d methods, DB×%d Redis×%d",
			len(result.Methods), totalDB, totalRedis),
		Issues: issues,
	}
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
func camelToSnake(s string) string {
	if s == "" {
		return s
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i > 0 && c >= 'A' && c <= 'Z' {
			out = append(out, '_')
		}
		out = append(out, c|0x20)
	}
	return string(out)
}

// daoNameToTable converts "IamUserRoleDao" → "iam_user_role"
func daoNameToTable(daoName string) string {
	s := strings.TrimSuffix(daoName, "Dao")
	s = strings.TrimSuffix(s, "dao")
	return toSnake(s)
}

// toSnake converts CamelCase → snake_case
func toSnake(s string) string {
	return camelToSnake(s)
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
func entTerminalToSQL(terminal, method, table string, fd *ast.FuncDecl) string {
	usedMethods := map[string]bool{}
	if fd != nil && fd.Body != nil {
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				usedMethods[sel.Sel.Name] = true
			}
			return true
		})
	}

	hasWhere := usedMethods["Where"]
	hasLimit := usedMethods["Limit"] || usedMethods["Offset"]
	hasSelect := usedMethods["Select"]
	hasOrder := usedMethods["Order"] || usedMethods["OrderBy"]
	hasUpsert := usedMethods["OnConflictColumns"] || usedMethods["OnConflict"]

	cols := "*"
	if hasSelect {
		cols = "specific_cols"
	}
	where := ""
	if hasWhere {
		where = " WHERE ..."
	}
	order := ""
	if hasOrder {
		order = " ORDER BY ..."
	}
	limit := ""
	if hasLimit {
		limit = " LIMIT n [OFFSET m]"
	}

	switch terminal {
	case "All":
		return fmt.Sprintf("SELECT %s FROM %s%s%s%s", cols, table, where, order, limit)
	case "Only", "First":
		return fmt.Sprintf("SELECT %s FROM %s%s LIMIT 1", cols, table, where)
	case "Count":
		return fmt.Sprintf("SELECT COUNT(*) FROM %s%s", table, where)
	case "Exist", "ExistX", "Exists":
		return fmt.Sprintf("SELECT 1 FROM %s%s LIMIT 1", table, where)
	case "IDs":
		return fmt.Sprintf("SELECT id FROM %s%s%s%s", table, where, order, limit)
	case "Save", "SaveX":
		if strings.HasPrefix(method, "Create") || strings.HasPrefix(method, "Insert") {
			if hasUpsert {
				return fmt.Sprintf("INSERT INTO %s (...) VALUES (...) ON DUPLICATE KEY UPDATE ...", table)
			}
			return fmt.Sprintf("INSERT INTO %s (...) VALUES (...)", table)
		}
		return fmt.Sprintf("UPDATE %s SET ...%s", table, where)
	case "Exec", "ExecX":
		if strings.HasPrefix(method, "Create") {
			if hasUpsert {
				return fmt.Sprintf("INSERT INTO %s (...) VALUES (...) ON DUPLICATE KEY UPDATE ...", table)
			}
			return fmt.Sprintf("INSERT INTO %s (...) VALUES (...)", table)
		}
		if strings.Contains(strings.ToLower(method), "delete") {
			return fmt.Sprintf("UPDATE %s SET deleted_at=NOW()%s  -- soft delete", table, where)
		}
		return fmt.Sprintf("UPDATE %s SET ...%s", table, where)
	default:
		return fmt.Sprintf("-- ent.%s on %s", terminal, table)
	}
}

// methodNameToSQL fallback: derive SQL from DAO method name.
func methodNameToSQL(method, daoName string) string {
	table := daoNameToTable(daoName)
	switch {
	case method == "GetByID" || method == "Get":
		return fmt.Sprintf("SELECT * FROM %s WHERE id=? LIMIT 1", table)
	case strings.HasPrefix(method, "GetBy") || strings.HasPrefix(method, "FindBy"):
		field := toSnake(strings.TrimPrefix(strings.TrimPrefix(method, "GetBy"), "FindBy"))
		return fmt.Sprintf("SELECT * FROM %s WHERE %s=? LIMIT 1", table, field)
	case method == "List":
		return fmt.Sprintf("SELECT * FROM %s WHERE ...  -- pagination?", table)
	case strings.HasPrefix(method, "ListBy"):
		field := toSnake(strings.TrimPrefix(method, "ListBy"))
		return fmt.Sprintf("SELECT * FROM %s WHERE %s IN (...)", table, field)
	case strings.HasPrefix(method, "Count"):
		return fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE ...", table)
	case method == "Create" || method == "Insert":
		return fmt.Sprintf("INSERT INTO %s (...) VALUES (...)", table)
	case strings.HasPrefix(method, "CreateBatch") || strings.HasPrefix(method, "InsertBatch"):
		return fmt.Sprintf("INSERT INTO %s (...) VALUES (...),(...),... -- batch", table)
	case strings.HasPrefix(method, "Update"):
		field := toSnake(strings.TrimPrefix(method, "Update"))
		if field == "by_i_d" || field == "" {
			return fmt.Sprintf("UPDATE %s SET ... WHERE id=?", table)
		}
		return fmt.Sprintf("UPDATE %s SET ... WHERE %s=?", table, field)
	case strings.HasPrefix(method, "Delete") || strings.HasPrefix(method, "SoftDelete"):
		return fmt.Sprintf("UPDATE %s SET deleted_at=NOW() WHERE ...  -- soft delete", table)
	case method == "Exist" || method == "Exists":
		return fmt.Sprintf("SELECT 1 FROM %s WHERE ... LIMIT 1", table)
	case strings.HasPrefix(method, "Upsert"):
		return fmt.Sprintf("INSERT INTO %s ... ON DUPLICATE KEY UPDATE ...", table)
	default:
		return fmt.Sprintf("-- %s on %s", method, table)
	}
}

// redisCmd maps a go-zero Redis client method name to a Redis command string.
func redisCmd(method string) string {
	m := strings.ToLower(strings.TrimSuffix(strings.ToLower(method), "ctx"))
	hints := map[string]string{
		"get": "GET key",
		"set": "SET key value",
		"setex": "SETEX key seconds value",
		"setnx": "SETNX key value",
		"del": "DEL key",
		"exists": "EXISTS key",
		"expire": "EXPIRE key seconds",
		"ttl": "TTL key",
		"persist": "PERSIST key",
		"hget": "HGET key field",
		"hset": "HSET key field value",
		"hmget": "HMGET key field [field ...]",
		"hmset": "HMSET key field value [...]",
		"hgetall": "HGETALL key",
		"hdel": "HDEL key field [field ...]",
		"incr": "INCR key",
		"incrby": "INCRBY key increment",
		"decr": "DECR key",
		"decrby": "DECRBY key decrement",
		"sadd": "SADD key member [member ...]",
		"smembers": "SMEMBERS key",
		"sismember": "SISMEMBER key member",
		"srem": "SREM key member [member ...]",
		"zadd": "ZADD key score member",
		"zrange": "ZRANGE key start stop [WITHSCORES]",
		"zrangebyscore": "ZRANGEBYSCORE key min max",
		"zrem": "ZREM key member",
		"zscore": "ZSCORE key member",
		"zcard": "ZCARD key",
		"lrange": "LRANGE key 0 -1",
		"rpush": "RPUSH key value",
		"lpush": "LPUSH key value",
		"lpop": "LPOP key",
		"rpop": "RPOP key",
		"llen": "LLEN key",
		"mget": "MGET key [key ...]",
		"mset": "MSET key value [key value ...]",
		"getset": "GETSET key value",
		"pipelined": "PIPELINE [multi-command batch]",
		"pipeline": "PIPELINE [multi-command batch]",
		"eval": "EVAL script numkeys [key ...] [arg ...]",
		"lock": "SET key uuid NX PX ms  -- distributed lock",
		"unlock": "DEL key               -- distributed unlock",
		"takedistributedlock": "SET key uuid NX PX ms  -- distributed lock",
		"scan": "SCAN cursor MATCH pattern COUNT n",
		"keys": "KEYS pattern  ⚠️ avoid in production",
	}
	if h, ok := hints[m]; ok {
		return h
	}
	return fmt.Sprintf("Redis.%s(...)", method)
}
