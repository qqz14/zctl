package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"

	"github.com/qqz14/zctl/rpc/generator"
	entgen "github.com/qqz14/zctl/rpc/generator/ent"
	"github.com/qqz14/zctl/util/ctx"
	"github.com/qqz14/zctl/util/pathx"
	"github.com/spf13/cobra"
)

var (
	VarStringSQL string
)

// ────────────────────── Parsed SQL structures ──────────────────────

// sqlOp describes a comparison operator in a WHERE condition.
type sqlOp int

const (
	opEQ        sqlOp = iota // =
	opNEQ                    // != / <>
	opGT                     // >
	opGTE                    // >=
	opLT                     // <
	opLTE                    // <=
	opIN                     // IN (?, ?, ...)
	opLike                   // LIKE
	opIsNull                 // IS NULL
	opIsNotNull              // IS NOT NULL
	opBetween                // BETWEEN ? AND ?
)

func (o sqlOp) String() string {
	switch o {
	case opEQ:
		return "EQ"
	case opNEQ:
		return "NEQ"
	case opGT:
		return "GT"
	case opGTE:
		return "GTE"
	case opLT:
		return "LT"
	case opLTE:
		return "LTE"
	case opIN:
		return "In"
	case opLike:
		return "Contains"
	case opIsNull:
		return "IsNil"
	case opIsNotNull:
		return "NotNil"
	case opBetween:
		return "GTE" // BETWEEN a AND b → GTE(a), LTE(b)
	default:
		return "EQ"
	}
}

// condition is a parsed WHERE condition column + operator.
type condition struct {
	Column string
	Op     sqlOp
	// Alias is the table alias (for JOINs). Empty for single-table queries.
	Alias string
	// Optional indicates this condition came from a "(? IS NULL OR col op ?)" pattern.
	// The generated code should use pointer params with nil-check.
	Optional bool
}

// joinClause is a parsed JOIN.
type joinClause struct {
	JoinType string // INNER, LEFT, RIGHT
	Table    string
	Alias    string
	OnLeft   string // left side of ON
	OnRight  string // right side of ON
}

// updateSet is a parsed SET column.
type updateSet struct {
	Column string
}

// queryType describes the kind of SQL query.
type queryType int

const (
	querySelect queryType = iota
	queryInsert
	queryUpdate
	queryDelete
)

// parsedSQL is the full result of SQL parsing.
type parsedSQL struct {
	Type queryType
	// Tables involved (first is the primary table).
	PrimaryTable string
	PrimaryAlias string
	Joins        []joinClause
	// SELECT specific
	SelectColumns []string // raw column list from SELECT; "*" means all
	IsCount       bool     // SELECT COUNT(...)
	IsDistinct    bool     // SELECT DISTINCT
	// WHERE
	Conditions []condition
	// GROUP BY
	GroupBy []string
	// HAVING (raw string, for comment)
	Having string
	// ORDER BY (raw string, for comment)
	OrderBy string
	// LIMIT / OFFSET
	HasLimit  bool
	LimitN    string // could be "?" or a number
	HasOffset bool
	OffsetN   string
	// UPDATE SET
	SetColumns []updateSet
	// INSERT columns
	InsertColumns []string
	// Raw SQL for comment
	RawSQL string
}

// returnKind determines what the method returns.
type returnKind int

const (
	returnList     returnKind = iota // []*ent.Model, error
	returnOne                        // *ent.Model, error
	returnCount                      // int, error
	returnAffected                   // int, error
	returnID                         // int, error
	returnError                      // error
)

// ────────────────────── Entry point ──────────────────────

// DaoFromSQL generates DAO method from SQL statement.
func DaoFromSQL(_ *cobra.Command, _ []string) error {
	if VarStringSQL == "" {
		return errors.New("--sql is required")
	}
	if VarStringSchema == "" {
		VarStringSchema = "./ent/schema"
	}

	abs, err := filepath.Abs(".")
	if err != nil {
		return err
	}

	projectCtx, err := ctx.Prepare(abs)
	if err != nil {
		return err
	}

	parsed, err := parseSQL(VarStringSQL)
	if err != nil {
		return err
	}

	style := VarStringStyle
	if style == "" {
		style = "gozero"
	}

	g := &entgen.GenContext{
		Schema:      VarStringSchema,
		Style:       style,
		ServiceName: VarStringServiceName,
		Overwrite:   VarBoolOverwrite,
	}

	// Load ent schema for type inference
	schemaMap := loadEntSchemas(VarStringSchema)

	if err := genDaoMethod(g, projectCtx, abs, parsed, schemaMap); err != nil {
		return err
	}

	// Install ent infra + sync ServiceContext (idempotent).
	if err := generator.EnsureEntInfra(abs, projectCtx.Path); err != nil {
		fmt.Printf("[zctl] Warning: failed to ensure ent infra: %v\n", err)
	}

	// Refresh zctl-commands.md on every subcommand run
	generator.RefreshCommandsDoc(abs)
	return nil
}

// ────────────────────── SQL Parser ──────────────────────

// parseSQL is the enhanced SQL parser supporting JOIN, GROUP BY, subqueries,
// UPDATE SET, LIMIT/OFFSET, IN, LIKE, BETWEEN, IS NULL, etc.
func parseSQL(rawSQL string) (*parsedSQL, error) {
	sql := strings.TrimSpace(rawSQL)
	if sql == "" {
		return nil, errors.New("empty SQL")
	}
	// Remove trailing semicolon
	sql = strings.TrimRight(sql, "; \t\n")

	p := &parsedSQL{RawSQL: rawSQL}
	upper := strings.ToUpper(sql)

	switch {
	case strings.HasPrefix(upper, "SELECT"):
		p.Type = querySelect
		if err := parseSelect(sql, p); err != nil {
			return nil, err
		}
	case strings.HasPrefix(upper, "INSERT"):
		p.Type = queryInsert
		if err := parseInsert(sql, p); err != nil {
			return nil, err
		}
	case strings.HasPrefix(upper, "UPDATE"):
		p.Type = queryUpdate
		if err := parseUpdate(sql, p); err != nil {
			return nil, err
		}
	case strings.HasPrefix(upper, "DELETE"):
		p.Type = queryDelete
		if err := parseDelete(sql, p); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported SQL type, must start with SELECT/INSERT/UPDATE/DELETE")
	}

	return p, nil
}

// parseSelect handles SELECT ... FROM ... [JOIN ...] [WHERE ...] [GROUP BY ...] [HAVING ...] [ORDER BY ...] [LIMIT ...]
func parseSelect(sql string, p *parsedSQL) error {
	upper := strings.ToUpper(sql)

	// Check COUNT / DISTINCT
	selectRe := regexp.MustCompile(`(?i)^SELECT\s+(.+?)\s+FROM\s+`)
	selectMatch := selectRe.FindStringSubmatch(sql)
	if len(selectMatch) >= 2 {
		selectPart := strings.TrimSpace(selectMatch[1])
		upperSelectPart := strings.ToUpper(selectPart)
		if strings.Contains(upperSelectPart, "COUNT(") || strings.Contains(upperSelectPart, "COUNT (") {
			p.IsCount = true
		}
		if strings.HasPrefix(upperSelectPart, "DISTINCT") {
			p.IsDistinct = true
		}
		p.SelectColumns = splitColumns(selectPart)
	}

	// Primary table
	tableName, tableAlias := extractFromTable(sql)
	if tableName == "" {
		return fmt.Errorf("cannot extract table name from SQL: %s", sql)
	}
	p.PrimaryTable = cleanTableName(tableName)
	p.PrimaryAlias = tableAlias

	// JOINs
	p.Joins = extractJoins(sql)

	// WHERE conditions
	p.Conditions = extractWhereConditions(sql)

	// GROUP BY
	groupByRe := regexp.MustCompile(`(?i)\bGROUP\s+BY\s+(.+?)(?:\s+HAVING|\s+ORDER|\s+LIMIT|\s*$)`)
	if m := groupByRe.FindStringSubmatch(sql); len(m) >= 2 {
		for _, col := range strings.Split(m[1], ",") {
			col = strings.TrimSpace(col)
			if col != "" {
				p.GroupBy = append(p.GroupBy, col)
			}
		}
	}

	// HAVING
	havingRe := regexp.MustCompile(`(?i)\bHAVING\s+(.+?)(?:\s+ORDER|\s+LIMIT|\s*$)`)
	if m := havingRe.FindStringSubmatch(sql); len(m) >= 2 {
		p.Having = strings.TrimSpace(m[1])
	}

	// ORDER BY
	orderByRe := regexp.MustCompile(`(?i)\bORDER\s+BY\s+(.+?)(?:\s+LIMIT|\s*$)`)
	if m := orderByRe.FindStringSubmatch(sql); len(m) >= 2 {
		p.OrderBy = strings.TrimSpace(m[1])
	}

	// LIMIT / OFFSET
	// Support: LIMIT ? OFFSET ?, LIMIT ?, ?, LIMIT N
	limitRe := regexp.MustCompile(`(?i)\bLIMIT\s+(\?|\d+)(?:\s*,\s*(\?|\d+))?(?:\s+OFFSET\s+(\?|\d+))?`)
	if m := limitRe.FindStringSubmatch(sql); len(m) >= 2 {
		p.HasLimit = true
		if m[2] != "" {
			// LIMIT offset, count → LIMIT ?, ?
			p.OffsetN = m[1]
			p.LimitN = m[2]
			p.HasOffset = true
		} else {
			p.LimitN = m[1]
		}
		if m[3] != "" {
			p.OffsetN = m[3]
			p.HasOffset = true
		}
	}

	// Detect single-row: LIMIT 1 or LIMIT ?
	if p.HasLimit && p.LimitN == "1" && !p.IsCount && !p.HasOffset {
		// Will generate GetBy instead of FindBy
	}

	// Handle subquery in FROM as comment only
	if strings.Contains(upper, "(SELECT") {
		// Subquery detected, we use the primary table but add a comment
	}

	return nil
}

// parseInsert handles INSERT INTO table (col1, col2, ...) VALUES (?, ?, ...)
func parseInsert(sql string, p *parsedSQL) error {
	re := regexp.MustCompile(`(?i)INTO\s+` + tableNamePattern() + `\s*\(([^)]+)\)`)
	m := re.FindStringSubmatch(sql)
	if len(m) < 3 {
		return fmt.Errorf("cannot parse INSERT statement: %s", sql)
	}
	p.PrimaryTable = cleanTableName(m[1])
	cols := strings.Split(m[2], ",")
	for _, c := range cols {
		c = strings.TrimSpace(c)
		c = cleanTableName(c) // remove backticks
		if c != "" {
			p.InsertColumns = append(p.InsertColumns, c)
		}
	}
	return nil
}

// parseUpdate handles UPDATE table SET col1=?, col2=? WHERE ...
func parseUpdate(sql string, p *parsedSQL) error {
	// Table name
	re := regexp.MustCompile(`(?i)^UPDATE\s+` + tableNamePattern() + `(?:\s+(?:AS\s+)?(\w+))?\s+SET\b`)
	m := re.FindStringSubmatch(sql)
	if len(m) < 2 {
		return fmt.Errorf("cannot parse UPDATE statement: %s", sql)
	}
	p.PrimaryTable = cleanTableName(m[1])
	if len(m) >= 3 {
		p.PrimaryAlias = m[2]
	}

	// SET columns
	setRe := regexp.MustCompile(`(?i)\bSET\s+(.+?)(?:\s+WHERE\b|\s*$)`)
	if sm := setRe.FindStringSubmatch(sql); len(sm) >= 2 {
		setPart := sm[1]
		// Split by comma, but be careful of nested parentheses
		for _, item := range splitByCommaRespectParens(setPart) {
			eqIdx := strings.Index(item, "=")
			if eqIdx > 0 {
				col := strings.TrimSpace(item[:eqIdx])
				// Remove table alias prefix
				if dotIdx := strings.Index(col, "."); dotIdx >= 0 {
					col = col[dotIdx+1:]
				}
				col = cleanTableName(col)
				p.SetColumns = append(p.SetColumns, updateSet{Column: col})
			}
		}
	}

	// WHERE
	p.Conditions = extractWhereConditions(sql)
	return nil
}

// parseDelete handles DELETE FROM table WHERE ...
func parseDelete(sql string, p *parsedSQL) error {
	re := regexp.MustCompile(`(?i)FROM\s+` + tableNamePattern())
	m := re.FindStringSubmatch(sql)
	if len(m) < 2 {
		return fmt.Errorf("cannot parse DELETE statement: %s", sql)
	}
	p.PrimaryTable = cleanTableName(m[1])
	p.Conditions = extractWhereConditions(sql)
	return nil
}

// ────────────────────── SQL parser helpers ──────────────────────

func tableNamePattern() string {
	return "(`?\\w+`?(?:\\.`?\\w+`?)?)"
}

func cleanTableName(s string) string {
	s = strings.Trim(s, "` \t")
	// Handle schema.table → take table part
	if idx := strings.LastIndex(s, "."); idx >= 0 {
		s = s[idx+1:]
	}
	return strings.Trim(s, "` \t")
}

func splitColumns(s string) []string {
	var cols []string
	for _, c := range splitByCommaRespectParens(s) {
		c = strings.TrimSpace(c)
		if c != "" {
			cols = append(cols, c)
		}
	}
	return cols
}

// splitByCommaRespectParens splits a string by comma, respecting parentheses nesting.
func splitByCommaRespectParens(s string) []string {
	var result []string
	depth := 0
	start := 0
	for i, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				result = append(result, s[start:i])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		result = append(result, s[start:])
	}
	return result
}

// extractFromTable gets the primary table and alias from a FROM clause.
func extractFromTable(sql string) (table, alias string) {
	// Handle subquery in FROM: FROM (SELECT ...) AS alias
	fromRe := regexp.MustCompile(`(?i)\bFROM\s+\(\s*SELECT`)
	if fromRe.MatchString(sql) {
		// Subquery — try to find the alias
		subRe := regexp.MustCompile(`(?i)\bFROM\s+\([^)]+\)\s+(?:AS\s+)?(\w+)`)
		if m := subRe.FindStringSubmatch(sql); len(m) >= 2 {
			return "subquery", m[1]
		}
		return "subquery", ""
	}

	// Standard: FROM table [AS] alias
	re := regexp.MustCompile(`(?i)\bFROM\s+` + tableNamePattern() + `(?:\s+(?:AS\s+)?(\w+))?`)
	m := re.FindStringSubmatch(sql)
	if len(m) < 2 {
		return "", ""
	}
	table = m[1]
	if len(m) >= 3 {
		alias = m[2]
		// Make sure alias is not a keyword
		aliasUpper := strings.ToUpper(alias)
		keywords := []string{"WHERE", "JOIN", "INNER", "LEFT", "RIGHT", "CROSS", "ON", "GROUP", "ORDER", "LIMIT", "HAVING", "UNION", "SET"}
		for _, kw := range keywords {
			if aliasUpper == kw {
				alias = ""
				break
			}
		}
	}
	return table, alias
}

// extractJoins parses JOIN clauses from SQL.
func extractJoins(sql string) []joinClause {
	var joins []joinClause
	joinRe := regexp.MustCompile(`(?i)\b(INNER|LEFT|RIGHT|CROSS)?\s*JOIN\s+` + tableNamePattern() + `(?:\s+(?:AS\s+)?(\w+))?\s+ON\s+(\w+(?:\.\w+)?)\s*=\s*(\w+(?:\.\w+)?)`)
	matches := joinRe.FindAllStringSubmatch(sql, -1)
	for _, m := range matches {
		jt := strings.ToUpper(strings.TrimSpace(m[1]))
		if jt == "" {
			jt = "INNER"
		}
		joins = append(joins, joinClause{
			JoinType: jt,
			Table:    cleanTableName(m[2]),
			Alias:    m[3],
			OnLeft:   m[4],
			OnRight:  m[5],
		})
	}
	return joins
}

// extractWhereConditions parses WHERE clause into conditions.
func extractWhereConditions(sql string) []condition {
	whereRe := regexp.MustCompile(`(?i)\bWHERE\s+(.+?)(?:\s+GROUP\s+BY\b|\s+ORDER\s+BY\b|\s+LIMIT\b|\s+HAVING\b|\s*$)`)
	m := whereRe.FindStringSubmatch(sql)
	if len(m) < 2 {
		return nil
	}
	wherePart := m[1]
	return parseConditions(wherePart)
}

// parseConditions splits WHERE clause into individual conditions.
func parseConditions(wherePart string) []condition {
	var conds []condition

	// ── Pre-process: (? IS NULL OR col op ?) → optional condition ──
	// This pattern means "if the param is nil, skip; otherwise filter by col".
	// We replace the whole group with just the col comparison and mark it optional.
	optRe := regexp.MustCompile(`(?i)\(\s*\?\s+IS\s+NULL\s+OR\s+(\w+(?:\.\w+)?)\s*(!=|<>|>=|<=|>|<|=)\s*\?\s*\)`)
	for _, m := range optRe.FindAllStringSubmatch(wherePart, -1) {
		col, alias := splitColAlias(m[1])
		op := parseOperator(m[2])
		conds = append(conds, condition{Column: col, Op: op, Alias: alias, Optional: true})
	}
	wherePart = optRe.ReplaceAllString(wherePart, "1=1")

	// ── Pre-process: (? IS NULL OR col LIKE CONCAT('%', ?, '%')) ──
	optLikeRe := regexp.MustCompile(`(?i)\(\s*\?\s+IS\s+NULL\s+OR\s+(\w+(?:\.\w+)?)\s+LIKE\s+(?:\?|CONCAT\s*\([^)]*\?\s*[^)]*\))\s*\)`)
	for _, m := range optLikeRe.FindAllStringSubmatch(wherePart, -1) {
		col, alias := splitColAlias(m[1])
		conds = append(conds, condition{Column: col, Op: opLike, Alias: alias, Optional: true})
	}
	wherePart = optLikeRe.ReplaceAllString(wherePart, "1=1")

	// Pattern: col BETWEEN ? AND ?
	betweenRe := regexp.MustCompile(`(?i)(\w+(?:\.\w+)?)\s+BETWEEN\s+\?\s+AND\s+\?`)
	for _, m := range betweenRe.FindAllStringSubmatch(wherePart, -1) {
		col, alias := splitColAlias(m[1])
		conds = append(conds, condition{Column: col, Op: opBetween, Alias: alias})
	}
	// Remove BETWEEN clauses to avoid double-parsing
	wherePart = betweenRe.ReplaceAllString(wherePart, "1=1")

	// Pattern: col IS NOT NULL
	isNotNullRe := regexp.MustCompile(`(?i)(\w+(?:\.\w+)?)\s+IS\s+NOT\s+NULL`)
	for _, m := range isNotNullRe.FindAllStringSubmatch(wherePart, -1) {
		col, alias := splitColAlias(m[1])
		conds = append(conds, condition{Column: col, Op: opIsNotNull, Alias: alias})
	}
	wherePart = isNotNullRe.ReplaceAllString(wherePart, "1=1")

	// Pattern: col IS NULL
	isNullRe := regexp.MustCompile(`(?i)(\w+(?:\.\w+)?)\s+IS\s+NULL`)
	for _, m := range isNullRe.FindAllStringSubmatch(wherePart, -1) {
		col, alias := splitColAlias(m[1])
		conds = append(conds, condition{Column: col, Op: opIsNull, Alias: alias})
	}
	wherePart = isNullRe.ReplaceAllString(wherePart, "1=1")

	// Pattern: col IN (?, ?, ...)
	inRe := regexp.MustCompile(`(?i)(\w+(?:\.\w+)?)\s+IN\s*\(`)
	for _, m := range inRe.FindAllStringSubmatch(wherePart, -1) {
		col, alias := splitColAlias(m[1])
		conds = append(conds, condition{Column: col, Op: opIN, Alias: alias})
	}
	wherePart = inRe.ReplaceAllString(wherePart, "1 IN (")

	// Pattern: col LIKE ? OR col LIKE CONCAT('%', ?, '%')
	likeRe := regexp.MustCompile(`(?i)(\w+(?:\.\w+)?)\s+LIKE\s+(?:\?|CONCAT\s*\([^)]*\?\s*[^)]*\))`)
	for _, m := range likeRe.FindAllStringSubmatch(wherePart, -1) {
		col, alias := splitColAlias(m[1])
		conds = append(conds, condition{Column: col, Op: opLike, Alias: alias})
	}
	wherePart = likeRe.ReplaceAllString(wherePart, "1=1")

	// Pattern: col OP ? (where OP is =, !=, <>, >, >=, <, <=)
	compRe := regexp.MustCompile(`(\w+(?:\.\w+)?)\s*(!=|<>|>=|<=|>|<|=)\s*\?`)
	for _, m := range compRe.FindAllStringSubmatch(wherePart, -1) {
		col, alias := splitColAlias(m[1])
		op := parseOperator(m[2])
		conds = append(conds, condition{Column: col, Op: op, Alias: alias})
	}

	return conds
}

func splitColAlias(col string) (column, alias string) {
	if idx := strings.Index(col, "."); idx >= 0 {
		return col[idx+1:], col[:idx]
	}
	return col, ""
}

func parseOperator(op string) sqlOp {
	switch op {
	case "=":
		return opEQ
	case "!=", "<>":
		return opNEQ
	case ">":
		return opGT
	case ">=":
		return opGTE
	case "<":
		return opLT
	case "<=":
		return opLTE
	default:
		return opEQ
	}
}

// ────────────────────── Ent Schema Loading ──────────────────────

// schemaInfo holds field name → Go type mapping for an ent schema.
type schemaInfo struct {
	Name   string
	Fields map[string]string // snake_case field name → Go type
}

// loadEntSchemas attempts to load ent schemas for type inference.
// Returns map with multiple keys per schema: lowercase name, table name.
// Returns empty map on failure.
func loadEntSchemas(schemaPath string) map[string]*schemaInfo {
	result := make(map[string]*schemaInfo)

	absSchema, err := filepath.Abs(schemaPath)
	if err != nil {
		return result
	}
	if _, err := os.Stat(absSchema); os.IsNotExist(err) {
		return result
	}

	graph, err := entc.LoadGraph(absSchema, &gen.Config{})
	if err != nil {
		fmt.Printf("  ⚠ Could not load ent schemas from %s: %v (type inference disabled)\n", schemaPath, err)
		return result
	}

	for _, n := range graph.Nodes {
		info := &schemaInfo{
			Name:   n.Name,
			Fields: make(map[string]string),
		}
		info.Fields["id"] = "int"
		for _, f := range n.Fields {
			info.Fields[f.Name] = f.Type.String()
		}
		// Store with multiple keys for flexible lookup:
		// 1. lowercase schema name (e.g. "iamuserappcid")
		result[strings.ToLower(n.Name)] = info
		// 2. table name (e.g. "iam_user_app_cid")
		tableName := n.Table()
		if tableName != "" {
			result[tableName] = info
		}
	}

	return result
}

// resolveGoType maps a column name to its Go type using ent schema.
// Falls back to "interface{}" if not found.
func resolveGoType(col string, schema *schemaInfo) string {
	if schema == nil {
		return "interface{}"
	}
	if t, ok := schema.Fields[col]; ok {
		return mapEntTypeToGo(t)
	}
	return "interface{}"
}

func mapEntTypeToGo(entType string) string {
	switch entType {
	case "string":
		return "string"
	case "bool":
		return "bool"
	case "int", "int32", "int16", "int8":
		return "int"
	case "int64":
		return "int64"
	case "uint", "uint32", "uint16", "uint8":
		return "uint32"
	case "uint64":
		return "uint64"
	case "float32":
		return "float32"
	case "float64":
		return "float64"
	case "time.Time":
		return "time.Time"
	case "[]byte":
		return "[]byte"
	default:
		return "interface{}"
	}
}

// ────────────────────── Method name generation ──────────────────────

func generateMethodName(p *parsedSQL) string {
	switch p.Type {
	case querySelect:
		return generateSelectMethodName(p)
	case queryInsert:
		return generateInsertMethodName(p)
	case queryUpdate:
		return generateUpdateMethodName(p)
	case queryDelete:
		return generateDeleteMethodName(p)
	default:
		return "CustomQuery"
	}
}

func generateSelectMethodName(p *parsedSQL) string {
	var prefix string

	if p.IsCount {
		prefix = "CountBy"
	} else if p.HasLimit && p.LimitN == "1" && !p.HasOffset {
		prefix = "GetBy"
	} else if len(p.GroupBy) > 0 {
		prefix = "GroupBy"
	} else {
		prefix = "FindBy"
	}

	condNames := conditionColumnNames(p.Conditions, p)
	if len(condNames) == 0 {
		if prefix == "FindBy" || prefix == "CountBy" || prefix == "GetBy" {
			return strings.TrimSuffix(prefix, "By") + "All"
		}
		return prefix + "All"
	}

	return prefix + strings.Join(condNames, "And")
}

func generateInsertMethodName(p *parsedSQL) string {
	return "Insert" + toCamelCase(p.PrimaryTable)
}

func generateUpdateMethodName(p *parsedSQL) string {
	var setNames []string
	for _, s := range p.SetColumns {
		setNames = append(setNames, toCamelCase(s.Column))
	}

	condNames := conditionColumnNames(p.Conditions, p)

	prefix := "Update"
	if len(setNames) > 0 && len(setNames) <= 3 {
		prefix += strings.Join(setNames, "And")
	}

	if len(condNames) > 0 {
		prefix += "By" + strings.Join(condNames, "And")
	}

	return prefix
}

func generateDeleteMethodName(p *parsedSQL) string {
	condNames := conditionColumnNames(p.Conditions, p)
	if len(condNames) == 0 {
		return "DeleteAll"
	}
	return "DeleteBy" + strings.Join(condNames, "And")
}

func conditionColumnNames(conds []condition, p *parsedSQL) []string {
	var names []string
	for _, c := range conds {
		// Skip JOIN table conditions — they are not part of the primary query
		if p != nil && isJoinTableCondition(c, p) {
			continue
		}
		name := toCamelCase(c.Column)
		// Append operator suffix for non-EQ operators
		switch c.Op {
		case opGT:
			name += "Gt"
		case opGTE:
			name += "Gte"
		case opLT:
			name += "Lt"
		case opLTE:
			name += "Lte"
		case opNEQ:
			name += "Neq"
		case opIN:
			name += "In"
		case opLike:
			name += "Like"
		case opIsNull:
			name += "IsNil"
		case opIsNotNull:
			name += "NotNil"
		case opBetween:
			name += "Between"
		}
		names = append(names, name)
	}
	return names
}

// ────────────────────── Return type determination ──────────────────────

func determineReturnKind(p *parsedSQL) returnKind {
	switch p.Type {
	case querySelect:
		if p.IsCount {
			return returnCount
		}
		if p.HasLimit && p.LimitN == "1" && !p.HasOffset {
			return returnOne
		}
		return returnList
	case queryInsert:
		return returnID
	case queryUpdate:
		return returnAffected
	case queryDelete:
		return returnAffected
	default:
		return returnError
	}
}

// ────────────────────── Code Generation ──────────────────────

func genDaoMethod(g *entgen.GenContext, projectCtx *ctx.ProjectContext, outputDir string, p *parsedSQL, schemaMap map[string]*schemaInfo) error {
	modulePath := projectCtx.Path
	modelName := toCamelCase(p.PrimaryTable)
	modelSnake := toSnakeCase(modelName)

	// Look up schema for type inference
	schema := schemaMap[strings.ToLower(p.PrimaryTable)]
	if schema == nil {
		// Try snake_case → CamelCase lookup
		schema = schemaMap[modelSnake]
	}

	// Use ent schema's actual Name (e.g. "IamUserAppCID") instead of GoPascal result (e.g. "IamUserAppCid")
	if schema != nil && schema.Name != "" {
		modelName = schema.Name
		modelSnake = toSnakeCase(modelName)
	}

	// ent predicate package name: lowercase without underscores (e.g. "iamuserappcid")
	entPkgName := strings.ToLower(strings.ReplaceAll(modelSnake, "_", ""))

	methodName := generateMethodName(p)
	retKind := determineReturnKind(p)

	// Also load schemas for JOIN tables (for type inference on aliased columns)
	joinSchemas := make(map[string]*schemaInfo) // alias → schema
	for _, j := range p.Joins {
		js := schemaMap[strings.ToLower(j.Table)]
		if js == nil {
			js = schemaMap[toSnakeCase(toCamelCase(j.Table))]
		}
		if js != nil {
			if j.Alias != "" {
				joinSchemas[j.Alias] = js
			}
			joinSchemas[j.Table] = js
		}
	}
	// Also map primary alias
	if p.PrimaryAlias != "" && schema != nil {
		joinSchemas[p.PrimaryAlias] = schema
	}

	// Build method params
	params := buildMethodParams(p, schema, joinSchemas)

	// Build return type string
	retStr := buildReturnType(retKind, modelName)

	// Build interface signature
	interfaceSig := fmt.Sprintf("\t%s(%s) %s\n", methodName, params.Signature, retStr)

	// Build implementation body
	implBody := buildImplBody(p, methodName, modelName, entPkgName, modelSnake, params, retKind, modulePath, schema, joinSchemas)

	// ── Write to DAO interface file ──
	daoFileName := modelSnake + "_dao.go"
	daoPath := filepath.Join(outputDir, "internal", "dao", daoFileName)
	appendToInterface(daoPath, methodName, interfaceSig)

	// ── Write to DAO impl file ──
	implFileName := modelSnake + "_oceanbase.go"
	implPath := filepath.Join(outputDir, "internal", "dao", "impl", implFileName)
	appendToImpl(implPath, methodName, implBody)

	// ── Regenerate mock (always overwrite to stay in sync with interface) ──
	regenerateMock(outputDir, modulePath, modelName, modelSnake, daoPath)

	_ = g
	return nil
}

// methodParams holds the generated parameter info.
type methodParams struct {
	Signature string   // full signature: "ctx context.Context, status int, name string"
	Names     []string // just names: ["status", "name"]
	LogFields string   // for logging: "status, name"
}

func buildMethodParams(p *parsedSQL, schema *schemaInfo, joinSchemas map[string]*schemaInfo) methodParams {
	mp := methodParams{Signature: "ctx context.Context"}
	var names []string

	// resolveCondType resolves Go type for a condition, checking alias → joinSchemas first.
	resolveCondType := func(c condition) string {
		if c.Alias != "" {
			if js, ok := joinSchemas[c.Alias]; ok {
				return resolveGoType(c.Column, js)
			}
		}
		return resolveGoType(c.Column, schema)
	}

	// Add condition params (skip JOIN table conditions — they become TODO comments)
	for _, c := range p.Conditions {
		if isJoinTableCondition(c, p) {
			continue
		}
		switch c.Op {
		case opIsNull, opIsNotNull:
			continue // No param needed
		case opBetween:
			// Two params: min, max
			goType := resolveCondType(c)
			minName := c.Column + "Min"
			maxName := c.Column + "Max"
			mp.Signature += fmt.Sprintf(", %s %s, %s %s", minName, goType, maxName, goType)
			names = append(names, minName, maxName)
		case opIN:
			goType := resolveCondType(c)
			paramName := c.Column + "List"
			mp.Signature += fmt.Sprintf(", %s []%s", paramName, goType)
			names = append(names, paramName)
		default:
			goType := resolveCondType(c)
			if c.Optional {
				// Optional params use pointer type with nil-check
				mp.Signature += fmt.Sprintf(", %s *%s", c.Column, goType)
			} else {
				mp.Signature += fmt.Sprintf(", %s %s", c.Column, goType)
			}
			names = append(names, c.Column)
		}
	}

	// For paginated queries: add page, pageSize
	if p.HasLimit && p.HasOffset && p.Type == querySelect && !p.IsCount {
		mp.Signature += ", page int, pageSize int"
		names = append(names, "page", "pageSize")
	}

	mp.Names = names
	mp.LogFields = strings.Join(names, ", ")
	return mp
}

func buildReturnType(kind returnKind, modelName string) string {
	switch kind {
	case returnList:
		return fmt.Sprintf("([]*ent.%s, error)", modelName)
	case returnOne:
		return fmt.Sprintf("(*ent.%s, error)", modelName)
	case returnCount:
		return "(int, error)"
	case returnAffected:
		return "(int, error)"
	case returnID:
		return "(int, error)"
	case returnError:
		return "error"
	default:
		return "error"
	}
}

// ────────────────────── Implementation body generation ──────────────────────

func buildImplBody(p *parsedSQL, methodName, modelName, entPkgName, modelSnake string, params methodParams, retKind returnKind, modulePath string, schema *schemaInfo, joinSchemas map[string]*schemaInfo) string {
	var b strings.Builder
	modelLower := strings.ToLower(strings.ReplaceAll(modelSnake, "_", ""))

	// Function signature
	retStr := buildReturnType(retKind, modelName)
	fmt.Fprintf(&b, "func (d *%sOceanBaseDao) %s(%s) %s {\n", modelLower, methodName, params.Signature, retStr)

	// Logging
	logFields := buildLogFields(params.Names)
	fmt.Fprintf(&b, "\tctxutil.L(ctx).Infow(\"dao.%s.%s\"%s)\n", modelName, methodName, logFields)

	// Raw SQL comment
	fmt.Fprintf(&b, "\t// Raw SQL: %s\n", p.RawSQL)

	switch p.Type {
	case querySelect:
		buildSelectImpl(&b, p, modelName, entPkgName, modelSnake, params, retKind, schema)
	case queryUpdate:
		buildUpdateImpl(&b, p, modelName, entPkgName, modelSnake, params, schema)
	case queryDelete:
		buildDeleteImpl(&b, p, modelName, entPkgName, params, schema)
	case queryInsert:
		buildInsertImpl(&b, p, modelName, modelLower)
	}

	b.WriteString("}\n")
	return b.String()
}

func buildLogFields(names []string) string {
	if len(names) == 0 {
		return ""
	}
	// Use key-value pairs appended to log message string to avoid logx import dependency.
	// Format: ctxutil.L(ctx).Infow("dao.Model.Method key1=%v key2=%v", val1, val2)
	// But Infow doesn't support format args. So we use logx.Field instead,
	// and ensure the import is added to the file.
	var fields []string
	for _, n := range names {
		fields = append(fields, fmt.Sprintf("logx.Field(\"%s\", %s)", n, n))
	}
	return ", " + strings.Join(fields, ", ")
}

// ensureImport adds an import to the file if not already present.
func ensureImport(filePath, importPath string) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	old := string(content)
	if strings.Contains(old, fmt.Sprintf(`"%s"`, importPath)) {
		return
	}
	// Find the import block and add the new import
	importBlockRe := regexp.MustCompile(`(import\s*\()`)
	if importBlockRe.MatchString(old) {
		newContent := importBlockRe.ReplaceAllString(old, fmt.Sprintf("$1\n\t\"%s\"", importPath))
		os.WriteFile(filePath, []byte(newContent), 0644)
	}
}

// entStructReceiverName returns the receiver struct name for the DAO impl.
// e.g. "IamUserAppCID" → "iamuserappcid"
func entStructReceiverName(modelName string) string {
	return strings.ToLower(modelName)
}

func buildSelectImpl(b *strings.Builder, p *parsedSQL, modelName, entPkgName, modelSnake string, params methodParams, retKind returnKind, schema *schemaInfo) {
	modelLower := entStructReceiverName(modelName)

	// Add JOIN TODO at top if present
	if len(p.Joins) > 0 {
		for _, j := range p.Joins {
			fmt.Fprintf(b, "\t// TODO: %s JOIN %s ON %s = %s → use ent edge query (e.g. .Query%s())\n",
				j.JoinType, j.Table, j.OnLeft, j.OnRight, toCamelCase(j.Table))
		}
	}

	if retKind == returnCount {
		// COUNT query
		fmt.Fprintf(b, "\tcount, err := d.cli.Query().\n")
		buildWherePredicates(b, p, entPkgName, schema)
		fmt.Fprintf(b, "\t\tCount(ctx)\n")
		fmt.Fprintf(b, "\tif err != nil {\n")
		fmt.Fprintf(b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.%s failed\", ctxutil.ErrField(err))\n", modelName, "Count")
		fmt.Fprintf(b, "\t\treturn 0, errcode.Wrapf(errcode.DBQueryFailed, \"%s.%s: %%v\", err)\n", modelLower, "Count")
		fmt.Fprintf(b, "\t}\n")
		fmt.Fprintf(b, "\treturn count, nil\n")
		return
	}

	if retKind == returnOne {
		// Single result query
		fmt.Fprintf(b, "\tresult, err := d.cli.Query().\n")
		buildWherePredicates(b, p, entPkgName, schema)
		buildOrderBy(b, p, entPkgName)
		fmt.Fprintf(b, "\t\tFirst(ctx)\n")
		fmt.Fprintf(b, "\tif err != nil {\n")
		fmt.Fprintf(b, "\t\tif ent.IsNotFound(err) {\n")
		fmt.Fprintf(b, "\t\t\treturn nil, nil\n")
		fmt.Fprintf(b, "\t\t}\n")
		fmt.Fprintf(b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.%s failed\", ctxutil.ErrField(err))\n", modelName, "GetBy")
		fmt.Fprintf(b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.%s: %%v\", err)\n", modelLower, "GetBy")
		fmt.Fprintf(b, "\t}\n")
		fmt.Fprintf(b, "\treturn result, nil\n")
		return
	}

	// List query (possibly paginated)
	if p.HasLimit && p.HasOffset {
		// Paginated query
		fmt.Fprintf(b, "\tquery := d.cli.Query().\n")
		buildWherePredicates(b, p, entPkgName, schema)
		fmt.Fprintf(b, "\t\tClone()\n")
		fmt.Fprintf(b, "\tlist, err := query.\n")
		buildOrderBy(b, p, entPkgName)
		fmt.Fprintf(b, "\t\tOffset((page - 1) * pageSize).\n")
		fmt.Fprintf(b, "\t\tLimit(pageSize).\n")
		fmt.Fprintf(b, "\t\tAll(ctx)\n")
	} else {
		// Plain list
		fmt.Fprintf(b, "\tlist, err := d.cli.Query().\n")
		buildWherePredicates(b, p, entPkgName, schema)
		buildOrderBy(b, p, entPkgName)
		if p.HasLimit && p.LimitN != "?" {
			fmt.Fprintf(b, "\t\tLimit(%s).\n", p.LimitN)
		}
		fmt.Fprintf(b, "\t\tAll(ctx)\n")
	}

	fmt.Fprintf(b, "\tif err != nil {\n")
	fmt.Fprintf(b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.%s failed\", ctxutil.ErrField(err))\n", modelName, "Find")
	fmt.Fprintf(b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.%s: %%v\", err)\n", modelLower, "Find")
	fmt.Fprintf(b, "\t}\n")
	fmt.Fprintf(b, "\treturn list, nil\n")
}

func buildUpdateImpl(b *strings.Builder, p *parsedSQL, modelName, entPkgName, modelSnake string, params methodParams, schema *schemaInfo) {
	modelLower := entStructReceiverName(modelName)
	fmt.Fprintf(b, "\taffected, err := d.cli.Update().\n")

	// SET columns
	for _, s := range p.SetColumns {
		camel := toCamelCase(s.Column)
		fmt.Fprintf(b, "\t\tSet%s(/* TODO: pass %s value */).\n", camel, s.Column)
	}

	buildWherePredicates(b, p, entPkgName, schema)
	fmt.Fprintf(b, "\t\tSave(ctx)\n")
	fmt.Fprintf(b, "\tif err != nil {\n")
	fmt.Fprintf(b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.%s failed\", ctxutil.ErrField(err))\n", modelName, "Update")
	fmt.Fprintf(b, "\t\treturn 0, errcode.Wrapf(errcode.DBUpdateFailed, \"%s.%s: %%v\", err)\n", modelLower, "Update")
	fmt.Fprintf(b, "\t}\n")
	fmt.Fprintf(b, "\treturn affected, nil\n")
}

func buildDeleteImpl(b *strings.Builder, p *parsedSQL, modelName, entPkgName string, params methodParams, schema *schemaInfo) {
	modelSnake := toSnakeCase(modelName)
	modelLower := entStructReceiverName(modelName)
	fmt.Fprintf(b, "\taffected, err := d.cli.Delete().\n")
	buildWherePredicates(b, p, entPkgName, schema)
	fmt.Fprintf(b, "\t\tExec(ctx)\n")
	fmt.Fprintf(b, "\tif err != nil {\n")
	fmt.Fprintf(b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.%s failed\", ctxutil.ErrField(err))\n", modelName, "Delete")
	fmt.Fprintf(b, "\t\treturn 0, errcode.Wrapf(errcode.DBDeleteFailed, \"%s.%s: %%v\", err)\n", modelLower, "Delete")
	fmt.Fprintf(b, "\t}\n")
	fmt.Fprintf(b, "\treturn affected, nil\n")
	_ = modelSnake
}

func buildInsertImpl(b *strings.Builder, p *parsedSQL, modelName, modelLower string) {
	fmt.Fprintf(b, "\tresult, err := d.cli.Create().\n")
	for _, col := range p.InsertColumns {
		if isBaseField(col) {
			continue
		}
		camel := toCamelCase(col)
		fmt.Fprintf(b, "\t\tSet%s(/* TODO: pass %s value */).\n", camel, col)
	}
	fmt.Fprintf(b, "\t\tSave(ctx)\n")
	fmt.Fprintf(b, "\tif err != nil {\n")
	fmt.Fprintf(b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.%s failed\", ctxutil.ErrField(err))\n", modelName, "Insert")
	fmt.Fprintf(b, "\t\treturn 0, errcode.Wrapf(errcode.DBInsertFailed, \"%s.%s: %%v\", err)\n", modelLower, "Insert")
	fmt.Fprintf(b, "\t}\n")
	fmt.Fprintf(b, "\treturn result.ID, nil\n")
}

// buildWherePredicates generates ent predicate calls from parsed conditions.
func buildWherePredicates(b *strings.Builder, p *parsedSQL, entPkgName string, schema *schemaInfo) {
	if len(p.Conditions) == 0 {
		return
	}

	// Separate conditions into: primary table normal, primary table optional, join table
	var normalConds, optConds, joinConds []condition
	for _, c := range p.Conditions {
		if isJoinTableCondition(c, p) {
			joinConds = append(joinConds, c)
		} else if c.Optional {
			optConds = append(optConds, c)
		} else {
			normalConds = append(normalConds, c)
		}
	}

	// Write normal predicates (primary table only) in Where()
	if len(normalConds) > 0 {
		fmt.Fprintf(b, "\t\tWhere(\n")
		for _, c := range normalConds {
			predicate := buildEntPredicate(c, entPkgName)
			fmt.Fprintf(b, "\t\t\t%s,\n", predicate)
		}
		fmt.Fprintf(b, "\t\t).\n")
	}

	// Write optional predicates (primary table) with nil-check comments
	if len(optConds) > 0 {
		for _, c := range optConds {
			predicate := buildEntPredicateDeref(c, entPkgName)
			fmt.Fprintf(b, "\t\t// Optional: only apply when %s != nil\n", c.Column)
			fmt.Fprintf(b, "\t\t// Where(%s).\n", predicate)
		}
	}

	// Write join table conditions as TODO comments (cannot use ent predicate for another table)
	if len(joinConds) > 0 {
		for _, c := range joinConds {
			alias := c.Alias
			if alias == "" {
				alias = "?"
			}
			fmt.Fprintf(b, "\t\t// TODO: %s.%s (from JOIN table) → filter via edge query or subquery\n", alias, c.Column)
		}
	}
}

// isJoinTableCondition checks whether a condition belongs to a JOIN table rather than the primary table.
func isJoinTableCondition(c condition, p *parsedSQL) bool {
	if c.Alias == "" {
		return false // no alias → assume primary table
	}
	// If alias matches primary table alias → primary table
	if p.PrimaryAlias != "" && c.Alias == p.PrimaryAlias {
		return false
	}
	// If alias matches a JOIN table alias → join table
	for _, j := range p.Joins {
		if c.Alias == j.Alias {
			return true
		}
	}
	return false
}

// buildEntPredicateDeref generates a predicate with dereferenced pointer param.
func buildEntPredicateDeref(c condition, entPkgName string) string {
	camel := toCamelCase(c.Column)
	pkg := entPkgName

	switch c.Op {
	case opEQ:
		return fmt.Sprintf("%s.%sEQ(*%s)", pkg, camel, c.Column)
	case opNEQ:
		return fmt.Sprintf("%s.%sNEQ(*%s)", pkg, camel, c.Column)
	case opGT:
		return fmt.Sprintf("%s.%sGT(*%s)", pkg, camel, c.Column)
	case opGTE:
		return fmt.Sprintf("%s.%sGTE(*%s)", pkg, camel, c.Column)
	case opLT:
		return fmt.Sprintf("%s.%sLT(*%s)", pkg, camel, c.Column)
	case opLTE:
		return fmt.Sprintf("%s.%sLTE(*%s)", pkg, camel, c.Column)
	case opLike:
		return fmt.Sprintf("%s.%sContains(*%s)", pkg, camel, c.Column)
	default:
		return fmt.Sprintf("%s.%sEQ(*%s)", pkg, camel, c.Column)
	}
}

// buildEntPredicate generates a single ent predicate call.
func buildEntPredicate(c condition, entPkgName string) string {
	camel := toCamelCase(c.Column)
	pkg := entPkgName

	switch c.Op {
	case opEQ:
		return fmt.Sprintf("%s.%sEQ(%s)", pkg, camel, c.Column)
	case opNEQ:
		return fmt.Sprintf("%s.%sNEQ(%s)", pkg, camel, c.Column)
	case opGT:
		return fmt.Sprintf("%s.%sGT(%s)", pkg, camel, c.Column)
	case opGTE:
		return fmt.Sprintf("%s.%sGTE(%s)", pkg, camel, c.Column)
	case opLT:
		return fmt.Sprintf("%s.%sLT(%s)", pkg, camel, c.Column)
	case opLTE:
		return fmt.Sprintf("%s.%sLTE(%s)", pkg, camel, c.Column)
	case opIN:
		return fmt.Sprintf("%s.%sIn(%sList...)", pkg, camel, c.Column)
	case opLike:
		return fmt.Sprintf("%s.%sContains(%s)", pkg, camel, c.Column)
	case opIsNull:
		return fmt.Sprintf("%s.%sIsNil()", pkg, camel)
	case opIsNotNull:
		return fmt.Sprintf("%s.%sNotNil()", pkg, camel)
	case opBetween:
		return fmt.Sprintf("%s.%sGTE(%sMin), %s.%sLTE(%sMax)", pkg, camel, c.Column, pkg, camel, c.Column)
	default:
		return fmt.Sprintf("%s.%sEQ(%s)", pkg, camel, c.Column)
	}
}

// buildOrderBy generates ent Order() call from parsed ORDER BY clause.
// e.g. "uac.id ASC" → Order(iamuserappcid.ByID())
// e.g. "id DESC" → Order(iamuserappcid.ByID(sql.OrderDesc()))
func buildOrderBy(b *strings.Builder, p *parsedSQL, entPkgName string) {
	if p.OrderBy == "" {
		return
	}

	// Parse ORDER BY parts: "col1 ASC, col2 DESC"
	parts := strings.Split(p.OrderBy, ",")
	var orderCalls []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		tokens := strings.Fields(part)
		if len(tokens) == 0 {
			continue
		}

		col := tokens[0]
		dir := "ASC"
		if len(tokens) >= 2 {
			dir = strings.ToUpper(tokens[1])
		}

		// Strip table alias prefix (e.g. "uac.id" → "id")
		if idx := strings.Index(col, "."); idx >= 0 {
			col = col[idx+1:]
		}

		camel := toCamelCase(col)
		if dir == "DESC" {
			orderCalls = append(orderCalls, fmt.Sprintf("%s.By%s(sql.OrderDesc())", entPkgName, camel))
		} else {
			orderCalls = append(orderCalls, fmt.Sprintf("%s.By%s()", entPkgName, camel))
		}
	}

	if len(orderCalls) > 0 {
		fmt.Fprintf(b, "\t\tOrder(%s).\n", strings.Join(orderCalls, ", "))
	}
}

// ────────────────────── File manipulation ──────────────────────

// appendToInterface inserts a method signature before the closing brace of the interface.
func appendToInterface(filePath, methodName, sig string) {
	if !pathx.FileExists(filePath) {
		fmt.Printf("  ⚠ DAO interface file not found: %s (run gen-rpc-ent-logic first)\n", filePath)
		return
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Printf("  ⚠ Failed to read %s: %v\n", filePath, err)
		return
	}

	old := string(content)

	// Check if method already exists
	if strings.Contains(old, methodName+"(") {
		fmt.Printf("  ⊘ Method %s already exists in %s, skipping\n", methodName, filePath)
		return
	}

	// Find the closing brace of the interface (the last "}" preceded by a line that's part of an interface)
	// Strategy: find "type XxxDao interface {" then find the matching "}"
	ifaceRe := regexp.MustCompile(`(?s)(type\s+\w+Dao\s+interface\s*\{)(.*)`)
	loc := ifaceRe.FindStringIndex(old)
	if loc == nil {
		fmt.Printf("  ⚠ Cannot find interface definition in %s\n", filePath)
		return
	}

	// Find the matching closing brace after the interface opening
	ifaceStart := loc[0]
	braceDepth := 0
	closingIdx := -1
	for i := ifaceStart; i < len(old); i++ {
		if old[i] == '{' {
			braceDepth++
		} else if old[i] == '}' {
			braceDepth--
			if braceDepth == 0 {
				closingIdx = i
				break
			}
		}
	}

	if closingIdx < 0 {
		fmt.Printf("  ⚠ Cannot find closing brace of interface in %s\n", filePath)
		return
	}

	// Insert before the closing brace
	newContent := old[:closingIdx] + sig + old[closingIdx:]
	if err := os.WriteFile(filePath, []byte(newContent), 0644); err != nil {
		fmt.Printf("  ⚠ Failed to write %s: %v\n", filePath, err)
		return
	}
	fmt.Printf("  → Appended %s to %s\n", methodName, filePath)
}

// appendToImpl appends a method implementation to the end of the impl file.
func appendToImpl(filePath, methodName, body string) {
	if !pathx.FileExists(filePath) {
		fmt.Printf("  ⚠ DAO impl file not found: %s (run gen-rpc-ent-logic first)\n", filePath)
		return
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Printf("  ⚠ Failed to read %s: %v\n", filePath, err)
		return
	}

	old := string(content)

	// Check if method already exists
	if strings.Contains(old, methodName+"(") {
		fmt.Printf("  ⊘ Method %s already exists in %s, skipping\n", methodName, filePath)
		return
	}

	// Append to end
	newContent := strings.TrimRight(old, "\n") + "\n\n" + body + "\n"
	if err := os.WriteFile(filePath, []byte(newContent), 0644); err != nil {
		fmt.Printf("  ⚠ Failed to write %s: %v\n", filePath, err)
		return
	}

	// Ensure logx import exists if the generated code uses logx.Field
	if strings.Contains(body, "logx.Field(") {
		ensureImport(filePath, "github.com/zeromicro/go-zero/core/logx")
	}
	// Ensure ent dialect/sql import if using sql.OrderDesc()
	if strings.Contains(body, "sql.OrderDesc()") {
		ensureImport(filePath, "entgo.io/ent/dialect/sql")
	}

	fmt.Printf("  → Appended %s to %s\n", methodName, filePath)
}

// ────────────────────── Helpers ──────────────────────

func toCamelCase(s string) string {
	return generator.GoPascal(s)
}

func toSnakeCase(s string) string {
	return generator.FileSnake(s)
}

func isBaseField(name string) bool {
	switch name {
	case "id", "created_at", "updated_at", "create_time", "update_time":
		return true
	}
	return false
}

// ────────────────────── Mock regeneration ──────────────────────

// regenerateMock re-parses the DAO interface file and regenerates the mock.
// Called after gen-dao-sql appends a new method to the interface.
func regenerateMock(outputDir, modulePath, modelName, modelSnake, daoFilePath string) {
	mockDir := filepath.Join(outputDir, "internal", "dao", "mock")
	if err := pathx.MkdirIfNotExist(mockDir); err != nil {
		fmt.Printf("  ⚠ Failed to create mock dir: %v\n", err)
		return
	}

	methods, err := parseMockMethods(daoFilePath)
	if err != nil {
		fmt.Printf("  ⚠ Failed to parse DAO interface for mock: %v\n", err)
		return
	}

	daoName := modelName + "Dao"
	mockName := "Mock" + daoName

	var methodsCode strings.Builder
	for _, m := range methods {
		fmt.Fprintf(&methodsCode, "\n// %s mocks the %s method.\n", m.name, m.name)
		fmt.Fprintf(&methodsCode, "func (m *%s) %s(%s) %s {\n", mockName, m.name, m.params, m.returns)
		fmt.Fprintf(&methodsCode, "\targs := m.Called(%s)\n", m.callArgs)
		fmt.Fprintf(&methodsCode, "%s", m.returnStmt)
		fmt.Fprintf(&methodsCode, "}\n")
	}

	content := fmt.Sprintf(`package mock

// Code generated by zctl. DO NOT EDIT.
// Re-generated each time the DAO interface changes (gen-rpc-ent-logic / gen-dao-sql).

import (
	"context"

	"%s/ent"
	"%s/internal/dao"
	"github.com/stretchr/testify/mock"
)

// %s is a mock implementation of dao.%s.
type %s struct {
	mock.Mock
}

// Compile-time check that %s implements dao.%s.
var _ dao.%s = (*%s)(nil)
%s`, modulePath, modulePath,
		mockName, daoName,
		mockName,
		mockName, daoName,
		daoName, mockName,
		methodsCode.String())

	mockFile := filepath.Join(mockDir, modelSnake+"_dao_mock.go")
	if err := os.WriteFile(mockFile, []byte(content), 0644); err != nil {
		fmt.Printf("  ⚠ Failed to write mock: %v\n", err)
		return
	}
	fmt.Printf("  → Regenerated mock: %s\n", mockFile)
}

type mockMethodInfo struct {
	name       string
	params     string
	returns    string
	callArgs   string
	returnStmt string
}

// parseMockMethods reads a DAO interface file and extracts method signatures for mock generation.
func parseMockMethods(filePath string) ([]mockMethodInfo, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var methods []mockMethodInfo
	lines := strings.Split(string(content), "\n")
	inInterface := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "interface {") {
			inInterface = true
			continue
		}
		if inInterface && trimmed == "}" {
			break
		}
		if !inInterface || trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}

		m, ok := parseMockMethodLine(trimmed)
		if ok {
			methods = append(methods, m)
		}
	}

	return methods, nil
}

func parseMockMethodLine(line string) (mockMethodInfo, bool) {
	parenOpen := strings.Index(line, "(")
	if parenOpen < 0 {
		return mockMethodInfo{}, false
	}
	methodName := strings.TrimSpace(line[:parenOpen])

	// Find matching closing paren
	depth := 0
	paramEnd := -1
	for i := parenOpen; i < len(line); i++ {
		if line[i] == '(' {
			depth++
		} else if line[i] == ')' {
			depth--
			if depth == 0 {
				paramEnd = i
				break
			}
		}
	}
	if paramEnd < 0 {
		return mockMethodInfo{}, false
	}

	params := line[parenOpen+1 : paramEnd]
	returns := strings.TrimSpace(line[paramEnd+1:])

	// Qualify dao-local types for mock package (e.g. *IamCIDListFilter → *dao.IamCIDListFilter)
	params = entgen.QualifyDaoTypes(params)
	returns = entgen.QualifyDaoTypes(returns)

	callArgs := mockExtractParamNames(params)
	returnStmt := mockBuildReturn(returns)

	return mockMethodInfo{
		name:       methodName,
		params:     params,
		returns:    returns,
		callArgs:   callArgs,
		returnStmt: returnStmt,
	}, true
}

func mockExtractParamNames(params string) string {
	var names []string
	for _, p := range strings.Split(params, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		parts := strings.Fields(p)
		if len(parts) >= 1 {
			names = append(names, parts[0])
		}
	}
	return strings.Join(names, ", ")
}

func mockBuildReturn(returns string) string {
	returns = strings.TrimSpace(returns)
	if strings.HasPrefix(returns, "(") && strings.HasSuffix(returns, ")") {
		returns = returns[1 : len(returns)-1]
	}

	parts := mockSplitTypes(returns)

	if len(parts) == 1 && parts[0] == "error" {
		return "\treturn args.Error(0)\n"
	}

	var items []string
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "error" {
			items = append(items, fmt.Sprintf("args.Error(%d)", i))
		} else if p == "int" || p == "int64" || p == "uint64" || p == "int32" {
			items = append(items, fmt.Sprintf("args.Get(%d).(%s)", i, p))
		} else {
			items = append(items, fmt.Sprintf("args.Get(%d).(%s)", i, p))
		}
	}
	return fmt.Sprintf("\treturn %s\n", strings.Join(items, ", "))
}

func mockSplitTypes(s string) []string {
	var result []string
	depth := 0
	start := 0
	for i, ch := range s {
		switch ch {
		case '[', '(':
			depth++
		case ']', ')':
			depth--
		case ',':
			if depth == 0 {
				result = append(result, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(s) {
		result = append(result, strings.TrimSpace(s[start:]))
	}
	return result
}
