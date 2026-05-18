package ent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/qqz14/zctl/rpc/generator"
	"github.com/qqz14/zctl/util/ctx"
	"github.com/qqz14/zctl/util/format"
	"github.com/qqz14/zctl/util/pathx"

	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
	"entgo.io/ent/entc/load"
	"entgo.io/ent/schema/field"
)

// GenContext holds all parameters for the rpc ent subcommand.
type GenContext struct {
	Schema      string // path to ent/schema
	Output      string // project root output dir
	ServiceName string // rpc service name
	Style       string // file naming style
	ModelName   string // model name (or "all")
	GroupName   string // logic group name
	Overwrite   bool   // overwrite existing files
}

func (g *GenContext) Validate() error {
	if g.Schema == "" {
		return fmt.Errorf("--schema is required")
	}
	if !strings.HasSuffix(g.Schema, "schema") {
		return fmt.Errorf("--schema should point to ent/schema directory")
	}
	if g.ServiceName == "" {
		return fmt.Errorf("--service_name is required")
	}
	if g.ModelName == "" {
		return fmt.Errorf("--model is required")
	}
	return nil
}

// GenEntLogic generates CRUD logic + DAO + errcode for one or all ent schemas.
func GenEntLogic(g *GenContext) error {
	fmt.Println("[zctl] Generating from ent schema...")

	outputDir, err := filepath.Abs(g.Output)
	if err != nil {
		return err
	}

	// Run "go mod tidy" before loading ent schema to ensure all dependencies are resolved.
	// Without this, entc.LoadGraph may fail with "go: updates to go.mod needed".
	fmt.Println("[zctl] Running go mod tidy...")
	tidyCmd := exec.Command("go", "mod", "tidy")
	tidyCmd.Dir = outputDir
	tidyCmd.Stdout = os.Stdout
	tidyCmd.Stderr = os.Stderr
	if err := tidyCmd.Run(); err != nil {
		return fmt.Errorf("go mod tidy failed: %w", err)
	}

	schemas, err := entc.LoadGraph(g.Schema, &gen.Config{})
	if err != nil {
		return fmt.Errorf("failed to load ent schema: %w", err)
	}

	workDir, err := filepath.Abs("./")
	if err != nil {
		return err
	}

	projectCtx, err := ctx.Prepare(workDir)
	if err != nil {
		return err
	}

	// Build a map from schema name → gen.Type for StructField() access.
	// gen.Type.Fields[i].StructField() returns the correct Go PascalCase name
	// with initialisms (e.g. "api_code" → "APICode", "uid" → "UID").
	nodeMap := make(map[string]*gen.Type)
	for _, node := range schemas.Nodes {
		nodeMap[node.Name] = node
	}

	var processedSchemas []string
	for _, s := range schemas.Schemas {
		if g.ModelName != "all" && g.ModelName != s.Name {
			continue
		}

		genCtx := *g
		if g.ModelName == "all" {
			genCtx.GroupName = generator.DirName(s.Name)
			genCtx.ModelName = s.Name
		}
		if genCtx.GroupName == "" {
			genCtx.GroupName = generator.DirName(s.Name)
		}

		// Build field name → Go struct field name mapping from gen.Type
		fieldMap := buildFieldMap(nodeMap[s.Name])

		if err := generateForSchema(&genCtx, projectCtx, outputDir, s, fieldMap); err != nil {
			return err
		}

		processedSchemas = append(processedSchemas, s.Name)
		fmt.Printf("[zctl] Generated module: %s\n", s.Name)
	}

	// Generate unified DAO errcode file (all modules in one file, with unique code segments + i18n)
	// When model=all, use all schemas; when model=single, still regenerate with all schemas
	// to maintain correct segment numbering.
	var allSchemaNames []string
	for _, s := range schemas.Schemas {
		allSchemaNames = append(allSchemaNames, s.Name)
	}
	if err := genDaoErrcodeAll(outputDir, allSchemaNames); err != nil {
		fmt.Printf("[zctl] Warning: failed to generate dao errcode: %v\n", err)
	} else {
		fmt.Printf("[zctl] Generated pkg/errcode/dao.go (%d modules)\n", len(allSchemaNames))
	}

	// After generating desc protos, auto-run merge + protoc + logic/server generation
	// so that logic files have correct pb signatures immediately.
	fmt.Println("[zctl] Auto-running merge-proto + protoc + logic/server generation...")
	if err := autoGenRpc(outputDir, g.Style); err != nil {
		fmt.Printf("[zctl] Warning: auto gen-rpc failed: %v\n", err)
		fmt.Println("[zctl] You can manually run: make gen-rpc")
	}

	// Install ent infra (entlog/entx + ent init in svc) and register DAOs
	// into ServiceContext. Idempotent — re-running has no extra side effect.
	if err := generator.EnsureEntInfra(outputDir, projectCtx.Path); err != nil {
		fmt.Printf("[zctl] Warning: failed to ensure ent infra: %v\n", err)
	} else {
		fmt.Println("[zctl] ent infra ready (entlog/entx + ServiceContext patched).")
	}

	fmt.Println("[zctl] Done.")
	return nil
}

func generateForSchema(g *GenContext, projectCtx *ctx.ProjectContext, outputDir string, schema *load.Schema, fieldMap map[string]string) error {
	modulePath := projectCtx.Path
	modelSnake := generator.FileSnake(schema.Name)

	// ── 先看后做：在生成 DAO 之前，记住 DAO 文件是否已经存在 ──
	// 如果之前就有 DAO → 说明不是第一次跑 → 后面跳过 proto 生成
	// 如果之前没有 DAO → 说明第一次跑 → 后面正常生成 proto
	daoFilePath := filepath.Join(outputDir, "internal", "dao", modelSnake+"_dao.go")
	daoPreExisted := pathx.FileExists(daoFilePath)

	// 1. Generate DAO interface
	if err := genDaoInterface(g, outputDir, modulePath, schema, fieldMap); err != nil {
		return err
	}

	// 2. Generate DAO OceanBase impl
	if err := genDaoOceanBaseImpl(g, outputDir, modulePath, schema, fieldMap); err != nil {
		return err
	}

	// 3. Generate DAO mock
	if err := genDaoMock(g, outputDir, modulePath, schema, fieldMap); err != nil {
		return err
	}

	// 4. Generate DAO hook file
	if err := genDaoHook(g, outputDir, modulePath, schema); err != nil {
		return err
	}

	// ── 步骤 5-9：一次性脚手架（desc / errcode / test skeleton / model / consts / desc proto）──
	// 与 DAO 是否首次生成保持一致：
	//   - DAO 之前不存在（首次）→ 全套生成
	//   - DAO 已存在（非首次）→ 全部跳过，让 desc 成为后续 logic/consts/model 的唯一权威源
	//     用户可以放心删除 desc / logic / pkg/model / pkg/consts / pkg/errcode 中任意文件，
	//     重跑不会被找回；如需强制重建请用 --overwrite。
	if daoPreExisted && !g.Overwrite {
		fmt.Printf("  ⊘ DAO already existed for %s, skipping scaffold generation (desc/errcode/model/consts/test) — use --overwrite to force\n", schema.Name)
		return nil
	}

	// 5. Generate errcode module file
	if err := genModuleErrcode(g, outputDir, modulePath, schema); err != nil {
		return err
	}

	// 6. Generate test skeleton (in logic/{group}/{model_lower}/ dir)
	if err := genTestSkeleton(g, outputDir, modulePath, schema); err != nil {
		return err
	}

	// 7. Generate module model file
	if err := genModuleModel(g, outputDir, schema); err != nil {
		return err
	}

	// 8. Generate module constants file
	if err := genModuleConst(g, outputDir, schema); err != nil {
		return err
	}

	// 9. Generate desc/{group}/{model_lower}.proto
	if err := genDescProto(g, outputDir, schema); err != nil {
		return err
	}

	return nil
}

// ==================== DAO Interface ====================

func genDaoInterface(g *GenContext, outputDir, modulePath string, schema *load.Schema, fieldMap map[string]string) error {
	dir := filepath.Join(outputDir, "internal", "dao")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	// Use snake_case: user_info_dao.go
	filename := generator.FileSnake(schema.Name) + "_dao"
	filePath := filepath.Join(dir, filename+".go")
	if pathx.FileExists(filePath) && !g.Overwrite {
		return nil
	}

	modelName := schema.Name

	// Collect unique fields (for GetByXxx / UpdateByXxx)
	uniqueFields := collectUniqueFields(schema)

	// Collect composite unique indexes (for GetByXxxYyyZzz)
	compositeIndexes := collectCompositeUniqueIndexes(schema)

	// Collect indexed fields for ListFilter (unique fields + fields with explicit indexes)
	indexedFields := collectIndexedFields(schema)

	// Check if soft delete is supported (has deleted_at field)
	hasSoftDelete := hasDeletedAtField(schema)

	// Build GetByXxx methods (single unique field + composite unique indexes)
	var getMethods strings.Builder
	for _, uf := range uniqueFields {
		camel := entFieldName(fieldMap, uf.Name)
		goType := mapEntFieldTypeToGo(uf)
		fmt.Fprintf(&getMethods, "\tGetBy%s(ctx context.Context, %s %s) (*ent.%s, error)\n", camel, uf.Name, goType, modelName)
	}
	for _, ci := range compositeIndexes {
		methodName := ci.MethodName(fieldMap)
		var params []string
		for _, f := range ci.Fields {
			goType := mapEntFieldTypeToGo(f)
			params = append(params, fmt.Sprintf("%s %s", f.Name, goType))
		}
		fmt.Fprintf(&getMethods, "\tGetBy%s(ctx context.Context, %s) (*ent.%s, error)\n", methodName, strings.Join(params, ", "), modelName)
	}

	// Build UpdateByXxx methods
	var updateMethods strings.Builder
	fmt.Fprintf(&updateMethods, "\tUpdateByID(ctx context.Context, data *ent.%s) (*ent.%s, error)\n", modelName, modelName)
	for _, uf := range uniqueFields {
		camel := entFieldName(fieldMap, uf.Name)
		goType := mapEntFieldTypeToGo(uf)
		fmt.Fprintf(&updateMethods, "\tUpdateBy%s(ctx context.Context, %s %s, data *ent.%s) (*ent.%s, error)\n", camel, uf.Name, goType, modelName, modelName)
	}

	// Build ListFilter struct
	var filterFields strings.Builder
	for _, f := range indexedFields {
		camel := entFieldName(fieldMap, f.Name)
		goType := mapEntFieldTypeToGo(f)
		fmt.Fprintf(&filterFields, "\t%s *%s // filter by %s\n", camel, goType, f.Name)
	}

	// Build delete method (only soft delete)
	var deleteMethod string
	if hasSoftDelete {
		deleteMethod = "\tDeleteByID(ctx context.Context, id int) error\n"
	}

	// Check if any field type needs "time" import (for ListFilter / method params)
	needTimeImport := false
	for _, f := range indexedFields {
		if mapEntFieldTypeToGo(f) == "time.Time" {
			needTimeImport = true
			break
		}
	}
	if !needTimeImport {
		for _, f := range uniqueFields {
			if mapEntFieldTypeToGo(f) == "time.Time" {
				needTimeImport = true
				break
			}
		}
	}

	timeImport := ""
	if needTimeImport {
		timeImport = "\t\"time\"\n\n"
	}

	content := fmt.Sprintf(`package dao

import (
	"context"
%s
	"%s/ent"
	"%s/pkg/model"
)

// %sListFilter holds filter conditions for %s list queries.
// Only indexed fields are supported for filtering.
type %sListFilter struct {
%s}

// %sDao defines data access interface for %s.
//
// Handle semantics:
//   - The instance constructed by NewXxxDao(client) holds *ent.%sClient
//     from client.%s, backed by the connection pool (non-transactional).
//   - WithTx(tx) derives a new instance bound to tx.%s; multiple DAOs
//     calling WithTx with the same tx share one transaction.
type %sDao interface {
	WithTx(tx *ent.Tx) %sDao

	Create(ctx context.Context, data *ent.%s) (*ent.%s, error)

	// ──── Get single record ────

	GetByID(ctx context.Context, id int) (*ent.%s, error)
%s
	// ──── Update single record ────

%s
	// ──── Delete ────
%s
	// ──── List ────

	List(ctx context.Context, filter *%sListFilter, page *model.PageInfo) ([]*ent.%s, int, error)
}
`, timeImport, modulePath, modulePath,
		modelName, modelName,
		modelName,
		filterFields.String(),
		modelName, modelName,
		modelName, modelName, modelName,
		modelName,
		modelName,
		modelName, modelName,
		modelName,
		getMethods.String(),
		updateMethods.String(),
		deleteMethod,
		modelName, modelName)

	return os.WriteFile(filePath, []byte(content), 0644)
}

// ==================== DAO Mock ====================

// genDaoMock generates internal/dao/mock/{model}_dao_mock.go with mock implementation.
// This is always overwritten because it must stay in sync with the interface.
func genDaoMock(g *GenContext, outputDir, modulePath string, schema *load.Schema, fieldMap map[string]string) error {
	dir := filepath.Join(outputDir, "internal", "dao", "mock")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	modelName := schema.Name
	modelSnake := generator.FileSnake(modelName)
	daoName := modelName + "Dao"
	mockName := "Mock" + daoName

	// Read the DAO interface file to parse method signatures
	daoFile := filepath.Join(outputDir, "internal", "dao", modelSnake+"_dao.go")
	methods, err := parseDaoMethods(daoFile)
	if err != nil {
		// If we can't parse, generate from known CRUD methods
		methods = defaultCRUDMethods(modelName)
	}

	var methodsCode strings.Builder

	// Always emit WithTx first with a hand-written body (parser cannot infer
	// the correct nil-fallback for an interface return).
	fmt.Fprintf(&methodsCode, "\n// WithTx mocks the WithTx method.\n")
	fmt.Fprintf(&methodsCode, "func (m *%s) WithTx(tx *ent.Tx) dao.%s {\n", mockName, daoName)
	fmt.Fprintf(&methodsCode, "\targs := m.Called(tx)\n")
	fmt.Fprintf(&methodsCode, "\tif v := args.Get(0); v != nil {\n")
	fmt.Fprintf(&methodsCode, "\t\treturn v.(dao.%s)\n", daoName)
	fmt.Fprintf(&methodsCode, "\t}\n")
	fmt.Fprintf(&methodsCode, "\treturn m\n")
	fmt.Fprintf(&methodsCode, "}\n")

	for _, m := range methods {
		// Skip WithTx — already emitted above.
		if m.Name == "WithTx" {
			continue
		}
		// Generate mock method
		fmt.Fprintf(&methodsCode, "\n// %s mocks the %s method.\n", m.Name, m.Name)
		fmt.Fprintf(&methodsCode, "func (m *%s) %s(%s) %s {\n", mockName, m.Name, m.Params, m.Returns)
		fmt.Fprintf(&methodsCode, "\targs := m.Called(%s)\n", m.CallArgs)

		// Generate return statement based on return types
		if m.ReturnStmt != "" {
			fmt.Fprintf(&methodsCode, "%s", m.ReturnStmt)
		}
		fmt.Fprintf(&methodsCode, "}\n")
	}

	content := fmt.Sprintf(`package mock

// Code generated by zctl. DO NOT EDIT.
// Re-generated each time the DAO interface changes (gen-rpc-ent-logic / gen-dao-sql).

import (
	"context"

	"%s/ent"
	"%s/internal/dao"
	"%s/pkg/model"
	"github.com/stretchr/testify/mock"
)

// %s is a mock implementation of dao.%s.
type %s struct {
	mock.Mock
}

// Compile-time check that %s implements dao.%s.
var _ dao.%s = (*%s)(nil)
%s`, modulePath, modulePath, modulePath,
		mockName, daoName,
		mockName,
		mockName, daoName,
		daoName, mockName,
		methodsCode.String())

	filePath := filepath.Join(dir, modelSnake+"_dao_mock.go")
	return os.WriteFile(filePath, []byte(content), 0644)
}

// daoMethod holds parsed info about a DAO interface method.
type daoMethod struct {
	Name       string
	Params     string // full parameter list
	Returns    string // full return type
	CallArgs   string // args to pass to m.Called()
	ReturnStmt string // return statement body
}

// parseDaoMethods reads a DAO interface file and extracts method signatures.
func parseDaoMethods(filePath string) ([]daoMethod, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var methods []daoMethod
	lines := strings.Split(string(content), "\n")
	inInterface := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.Contains(trimmed, "interface {") {
			inInterface = true
			continue
		}
		if inInterface && trimmed == "}" {
			inInterface = false
			continue
		}
		if !inInterface || trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}

		m, ok := parseMethodLine(trimmed)
		if ok {
			methods = append(methods, m)
		}
	}

	return methods, nil
}

// parseMethodLine parses a single interface method line like:
//
//	Create(ctx context.Context, data *ent.User) (*ent.User, error)
func parseMethodLine(line string) (daoMethod, bool) {
	// Match: MethodName(params) returnType
	parenOpen := strings.Index(line, "(")
	if parenOpen < 0 {
		return daoMethod{}, false
	}
	methodName := strings.TrimSpace(line[:parenOpen])

	// Find matching closing paren for params
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
		return daoMethod{}, false
	}

	params := line[parenOpen+1 : paramEnd]
	returns := strings.TrimSpace(line[paramEnd+1:])

	// Qualify dao-local types (e.g. *IamCIDListFilter → *dao.IamCIDListFilter)
	// so mock package can reference them correctly.
	params = qualifyDaoTypes(params)
	returns = qualifyDaoTypes(returns)

	// Build call args (just the param names, not types)
	callArgs := extractParamNames(params)

	// Build return statement
	returnStmt := buildReturnStmt(returns)

	return daoMethod{
		Name:       methodName,
		Params:     params,
		Returns:    returns,
		CallArgs:   callArgs,
		ReturnStmt: returnStmt,
	}, true
}

// extractParamNames extracts parameter names from a param list.
// "ctx context.Context, data *ent.User" → "ctx, data"
func extractParamNames(params string) string {
	var names []string
	for _, p := range strings.Split(params, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Handle "page, pageSize int" (multiple names sharing a type)
		parts := strings.Fields(p)
		if len(parts) >= 1 {
			// If the first part doesn't contain '.', '[', or '*', it's a name
			name := parts[0]
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

// buildReturnStmt builds the return statement for a mock method.
func buildReturnStmt(returns string) string {
	returns = strings.TrimSpace(returns)
	// Remove outer parens if present
	if strings.HasPrefix(returns, "(") && strings.HasSuffix(returns, ")") {
		returns = returns[1 : len(returns)-1]
	}

	parts := splitReturnTypes(returns)

	if len(parts) == 1 && parts[0] == "error" {
		return "\treturn args.Error(0)\n"
	}

	var lines []string
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "error" {
			lines = append(lines, fmt.Sprintf("args.Error(%d)", i))
		} else if p == "int" || p == "int64" || p == "uint64" || p == "int32" {
			lines = append(lines, fmt.Sprintf("args.Get(%d).(%s)", i, p))
		} else if strings.HasPrefix(p, "*") || strings.HasPrefix(p, "[]*") || strings.HasPrefix(p, "[]") {
			// Pointer or slice type: use type assertion with nil check
			lines = append(lines, fmt.Sprintf("args.Get(%d).(%s)", i, p))
		} else {
			lines = append(lines, fmt.Sprintf("args.Get(%d).(%s)", i, p))
		}
	}

	return fmt.Sprintf("\treturn %s\n", strings.Join(lines, ", "))
}

// splitReturnTypes splits return type list respecting generics/brackets.
func splitReturnTypes(s string) []string {
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

// QualifyDaoTypes rewrites types that are defined in the dao package (not a
// built-in or already-qualified type) to use a "dao." prefix.
// e.g. "*IamCIDListFilter" → "*dao.IamCIDListFilter"
// This is needed because the parsed signature comes from the dao package
// where these types need no prefix, but mock code lives in the mock package.
// Exported for use by cli/dao.go's regenerateMock.
func QualifyDaoTypes(sig string) string {
	return qualifyDaoTypes(sig)
}

func qualifyDaoTypes(sig string) string {
	// We operate on each comma-separated parameter/return segment.
	parts := strings.Split(sig, ",")
	for i, p := range parts {
		parts[i] = qualifySegment(p)
	}
	return strings.Join(parts, ",")
}

// qualifySegment qualifies a single param or return-type segment.
// Input examples: " filter *IamCIDListFilter", " *ent.User", " error"
func qualifySegment(seg string) string {
	trimmed := strings.TrimSpace(seg)
	if trimmed == "" {
		return seg
	}

	// Split "name type" for params, or just "type" for returns
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return seg
	}

	// The type token is the last field (or the only field for returns)
	typeIdx := len(fields) - 1
	typeTok := fields[typeIdx]

	qualified := qualifyTypeToken(typeTok)
	if qualified == typeTok {
		return seg // unchanged
	}

	fields[typeIdx] = qualified
	// Preserve leading whitespace
	leading := ""
	for _, ch := range seg {
		if ch == ' ' || ch == '\t' {
			leading += string(ch)
		} else {
			break
		}
	}
	return leading + strings.Join(fields, " ")
}

// qualifyTypeToken adds "dao." prefix to a type token if it looks like
// a dao-local type (starts with uppercase, no dot = no package qualifier).
// Handles pointer and slice prefixes: "*Foo" → "*dao.Foo", "[]*Foo" → "[]*dao.Foo"
func qualifyTypeToken(tok string) string {
	// Known packages / built-in types that should NOT be qualified
	builtins := map[string]bool{
		"error": true, "string": true, "bool": true,
		"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
		"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
		"float32": true, "float64": true, "byte": true, "rune": true,
		"interface{}": true, "any": true,
	}

	// Strip pointer/slice prefix
	prefix := ""
	rest := tok
	if strings.HasPrefix(rest, "[]*") {
		prefix = "[]*"
		rest = rest[3:]
	} else if strings.HasPrefix(rest, "[]") {
		prefix = "[]"
		rest = rest[2:]
	} else if strings.HasPrefix(rest, "*") {
		prefix = "*"
		rest = rest[1:]
	}

	// If it already has a dot (e.g. "ent.User", "model.PageInfo", "context.Context"), skip
	if strings.Contains(rest, ".") {
		return tok
	}

	// If it's a built-in type, skip
	if builtins[rest] {
		return tok
	}

	// If it starts with uppercase, it's likely a dao-local type
	if len(rest) > 0 && rest[0] >= 'A' && rest[0] <= 'Z' {
		return prefix + "dao." + rest
	}

	return tok
}

// defaultCRUDMethods returns fallback mock methods when parsing fails.
// Now aligned with new interface: no Delete if no deleted_at, List uses filter+page.
func defaultCRUDMethods(modelName string) []daoMethod {
	entType := "*ent." + modelName
	entSlice := "[]*ent." + modelName
	return []daoMethod{
		{
			Name:       "Create",
			Params:     "ctx context.Context, data " + entType,
			Returns:    "(" + entType + ", error)",
			CallArgs:   "ctx, data",
			ReturnStmt: fmt.Sprintf("\treturn args.Get(0).(%s), args.Error(1)\n", entType),
		},
		{
			Name:       "GetByID",
			Params:     "ctx context.Context, id int",
			Returns:    "(" + entType + ", error)",
			CallArgs:   "ctx, id",
			ReturnStmt: fmt.Sprintf("\treturn args.Get(0).(%s), args.Error(1)\n", entType),
		},
		{
			Name:       "UpdateByID",
			Params:     "ctx context.Context, data " + entType,
			Returns:    "(" + entType + ", error)",
			CallArgs:   "ctx, data",
			ReturnStmt: fmt.Sprintf("\treturn args.Get(0).(%s), args.Error(1)\n", entType),
		},
		{
			Name:       "List",
			Params:     "ctx context.Context, filter *dao." + modelName + "ListFilter, page *model.PageInfo",
			Returns:    "(" + entSlice + ", int, error)",
			CallArgs:   "ctx, filter, page",
			ReturnStmt: fmt.Sprintf("\treturn args.Get(0).(%s), args.Get(1).(int), args.Error(2)\n", entSlice),
		},
	}
}

func genDaoOceanBaseImpl(g *GenContext, outputDir, modulePath string, schema *load.Schema, fieldMap map[string]string) error {
	dir := filepath.Join(outputDir, "internal", "dao", "impl")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	// Use snake_case: user_info_oceanbase.go
	filename := generator.FileSnake(schema.Name) + "_oceanbase"
	filePath := filepath.Join(dir, filename+".go")
	if pathx.FileExists(filePath) && !g.Overwrite {
		return nil
	}

	modelName := schema.Name
	entPkg := generator.EntPkg(schema.Name)
	uniqueFields := collectUniqueFields(schema)
	compositeIndexes := collectCompositeUniqueIndexes(schema)
	indexedFields := collectIndexedFields(schema)
	hasSoftDelete := hasDeletedAtField(schema)

	// Build field setter lines for Create and Update (excluding base fields + deleted_at)
	var createSetters, updateSetters strings.Builder
	// For raw SQL: column names, placeholders, UPDATE clauses, and data.Xxx args
	var sqlColumns, sqlPlaceholders []string
	var sqlUpdateClauses []string
	var sqlArgs []string
	for _, f := range schema.Fields {
		if isBaseField(f.Name) {
			continue
		}
		camel := entFieldName(fieldMap, f.Name)
		// JSON fields (e.g. field.JSON("x", []string{})) do NOT have SetNillable in ent.
		// They only have Set/Append/Clear. Use Set even if Optional/Nillable.
		isJSON := f.Info != nil && f.Info.Type == field.TypeJSON
		if (f.Optional || f.Nillable) && !isJSON {
			if f.Nillable {
				fmt.Fprintf(&createSetters, "\t\tSetNillable%s(data.%s).\n", camel, camel)
				fmt.Fprintf(&updateSetters, "\t\tSetNillable%s(data.%s).\n", camel, camel)
			} else {
				fmt.Fprintf(&createSetters, "\t\tSetNillable%s(&data.%s).\n", camel, camel)
				fmt.Fprintf(&updateSetters, "\t\tSetNillable%s(&data.%s).\n", camel, camel)
			}
		} else {
			fmt.Fprintf(&createSetters, "\t\tSet%s(data.%s).\n", camel, camel)
			fmt.Fprintf(&updateSetters, "\t\tSet%s(data.%s).\n", camel, camel)
		}
		// For raw SQL generation
		sqlColumns = append(sqlColumns, f.Name)
		sqlPlaceholders = append(sqlPlaceholders, "?")
		sqlArgs = append(sqlArgs, "data."+camel)
		// ON DUPLICATE KEY UPDATE: restore field only if deleted_at IS NOT NULL
		sqlUpdateClauses = append(sqlUpdateClauses, fmt.Sprintf("  %s = IF(deleted_at IS NOT NULL, VALUES(%s), %s)", f.Name, f.Name, f.Name))
	}

	// Determine the first composite unique index for upsert query (if exists)
	// This is used in Create's upsert logic to find existing records.
	var upsertWhereClause string
	if hasSoftDelete && len(compositeIndexes) > 0 {
		ci := compositeIndexes[0]
		var whereParts []string
		for _, f := range ci.Fields {
			camel := entFieldName(fieldMap, f.Name)
			whereParts = append(whereParts, fmt.Sprintf("\t\t\t%s.%sEQ(data.%s),", entPkg, camel, camel))
		}
		upsertWhereClause = strings.Join(whereParts, "\n")
	} else if hasSoftDelete && len(uniqueFields) > 0 {
		// Fallback to first single unique field
		uf := uniqueFields[0]
		camel := entFieldName(fieldMap, uf.Name)
		upsertWhereClause = fmt.Sprintf("\t\t\t%s.%sEQ(data.%s),", entPkg, camel, camel)
	}

	var b strings.Builder

	// Header
	timeImportImpl := ""
	if hasSoftDelete {
		timeImportImpl = "\t\"time\"\n"
	}
	fmt.Fprintf(&b, "package impl\n\nimport (\n\t\"context\"\n%s\n", timeImportImpl)
	fmt.Fprintf(&b, "\t\"%s/ent\"\n", modulePath)
	fmt.Fprintf(&b, "\t\"%s/ent/%s\"\n", modulePath, entPkg)
	fmt.Fprintf(&b, "\t\"%s/internal/dao\"\n", modulePath)
	fmt.Fprintf(&b, "\t\"%s/pkg/ctxutil\"\n", modulePath)
	fmt.Fprintf(&b, "\t\"%s/pkg/errcode\"\n", modulePath)
	fmt.Fprintf(&b, "\t\"%s/pkg/model\"\n", modulePath)
	fmt.Fprintf(&b, ")\n\n")

	// Struct + constructor + WithTx
	fmt.Fprintf(&b, "type %sOceanBaseDao struct {\n\tcli *ent.%sClient\n}\n\n", entPkg, modelName)
	fmt.Fprintf(&b, "func New%sOceanBaseDao(client *ent.Client) dao.%sDao {\n\treturn &%sOceanBaseDao{cli: client.%s}\n}\n\n", modelName, modelName, entPkg, modelName)
	fmt.Fprintf(&b, "func (d *%sOceanBaseDao) WithTx(tx *ent.Tx) dao.%sDao {\n\treturn &%sOceanBaseDao{cli: tx.%s}\n}\n\n", entPkg, modelName, entPkg, modelName)

	// Create — raw SQL for soft-delete tables (1 IO), ent Create for others
	tableName := generator.FileSnake(schema.Name)
	if hasSoftDelete && upsertWhereClause != "" {
		// Build the raw SQL string
		allCols := append(sqlColumns, "created_at", "updated_at")
		allPlaceholders := append(sqlPlaceholders, "?", "?")
		allUpdateClauses := append(sqlUpdateClauses,
			"  deleted_at = IF(deleted_at IS NOT NULL, NULL, deleted_at)",
			"  updated_at = VALUES(updated_at)",
		)

		fmt.Fprintf(&b, "// Create 创建记录（幂等，1 次 IO）。\n")
		fmt.Fprintf(&b, "//   - 新建：直接 INSERT。\n")
		fmt.Fprintf(&b, "//   - 唯一键冲突且已软删（deleted_at IS NOT NULL）：恢复记录。\n")
		fmt.Fprintf(&b, "//   - 唯一键冲突且活跃：不做任何变更（幂等）。\n")
		fmt.Fprintf(&b, "//\n")
		fmt.Fprintf(&b, "// 使用 id=LAST_INSERT_ID(id) 确保任何情况下都能通过 LastInsertId 拿到正确主键。\n")
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) Create(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
		fmt.Fprintf(&b, "\tnow := time.Now().UTC()\n\n")
		fmt.Fprintf(&b, "\tconst sql = `\nINSERT INTO %s (%s)\nVALUES (%s)\nON DUPLICATE KEY UPDATE\n  id = LAST_INSERT_ID(id),\n%s\n`\n\n",
			tableName,
			strings.Join(allCols, ", "),
			strings.Join(allPlaceholders, ", "),
			strings.Join(allUpdateClauses, ",\n"),
		)
		fmt.Fprintf(&b, "\tresult, err := d.cli.ExecContext(ctx, sql,\n\t\t%s, now, now,\n\t)\n", strings.Join(sqlArgs, ", "))
		fmt.Fprintf(&b, "\tif err != nil {\n")
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.Create failed\", ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBInsertFailed, \"%s.Create: %%v\", err)\n", modelName)
		fmt.Fprintf(&b, "\t}\n\n")
		fmt.Fprintf(&b, "\tid, _ := result.LastInsertId()\n")
		fmt.Fprintf(&b, "\tdata.ID = int(id)\n")
		fmt.Fprintf(&b, "\tdata.CreatedAt = now\n")
		fmt.Fprintf(&b, "\tdata.UpdatedAt = now\n")
		fmt.Fprintf(&b, "\treturn data, nil\n}\n\n")
	} else {
		// Plain create (no soft delete or no unique key)
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) Create(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Create().\n%s\t\tSave(ctx)\n", createSetters.String())
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.Create failed\", ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBInsertFailed, \"%s.Create: %%v\", err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.Create ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName)
	}

	// GetByID — with soft-delete filter
	fmt.Fprintf(&b, "// ──── Get single record ────\n\n")
	if hasSoftDelete {
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) GetByID(ctx context.Context, id int) (*ent.%s, error) {\n", entPkg, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Query().Where(%s.ID(id), %s.DeletedAtIsNil()).Only(ctx)\n", entPkg, entPkg)
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: id=%%d\", id)\n\t\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetByID failed\", ctxutil.IDField(id), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.GetByID id=%%d: %%v\", id, err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.GetByID ok\", ctxutil.IDField(id))\n\treturn result, nil\n}\n\n", modelName)
	} else {
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) GetByID(ctx context.Context, id int) (*ent.%s, error) {\n", entPkg, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Get(ctx, id)\n")
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: id=%%d\", id)\n\t\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetByID failed\", ctxutil.IDField(id), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.GetByID id=%%d: %%v\", id, err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.GetByID ok\", ctxutil.IDField(id))\n\treturn result, nil\n}\n\n", modelName)
	}

	// GetByXxx for each single unique field — with soft-delete filter
	for _, uf := range uniqueFields {
		camel := entFieldName(fieldMap, uf.Name)
		goType := mapEntFieldTypeToGo(uf)
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) GetBy%s(ctx context.Context, %s %s) (*ent.%s, error) {\n", entPkg, camel, uf.Name, goType, modelName)
		if hasSoftDelete {
			fmt.Fprintf(&b, "\tresult, err := d.cli.Query().Where(%s.%sEQ(%s), %s.DeletedAtIsNil()).Only(ctx)\n", entPkg, camel, uf.Name, entPkg)
		} else {
			fmt.Fprintf(&b, "\tresult, err := d.cli.Query().Where(%s.%sEQ(%s)).Only(ctx)\n", entPkg, camel, uf.Name)
		}
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: %s=%%v\", %s)\n\t\t}\n", modelName, modelName, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetBy%s failed\", ctxutil.ErrField(err))\n", modelName, camel)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.GetBy%s %s=%%v: %%v\", %s, err)\n\t}\n", modelName, camel, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.GetBy%s ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName, camel)
	}

	// GetByXxxYyyZzz for each composite unique index — with soft-delete filter
	for _, ci := range compositeIndexes {
		methodName := ci.MethodName(fieldMap)
		var params, whereParts []string
		for _, f := range ci.Fields {
			camel := entFieldName(fieldMap, f.Name)
			goType := mapEntFieldTypeToGo(f)
			params = append(params, fmt.Sprintf("%s %s", f.Name, goType))
			whereParts = append(whereParts, fmt.Sprintf("%s.%sEQ(%s)", entPkg, camel, f.Name))
		}
		if hasSoftDelete {
			whereParts = append(whereParts, fmt.Sprintf("%s.DeletedAtIsNil()", entPkg))
		}
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) GetBy%s(ctx context.Context, %s) (*ent.%s, error) {\n", entPkg, methodName, strings.Join(params, ", "), modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Query().Where(%s).Only(ctx)\n", strings.Join(whereParts, ", "))
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found by composite key\")\n\t\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetBy%s failed\", ctxutil.ErrField(err))\n", modelName, methodName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.GetBy%s: %%v\", err)\n\t}\n", modelName, methodName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.GetBy%s ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName, methodName)
	}

	// UpdateByID — with soft-delete filter
	fmt.Fprintf(&b, "// ──── Update single record ────\n\n")
	if hasSoftDelete {
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) UpdateByID(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
		fmt.Fprintf(&b, "\taffected, err := d.cli.Update().\n\t\tWhere(%s.ID(data.ID), %s.DeletedAtIsNil()).\n%s\t\tSave(ctx)\n", entPkg, entPkg, updateSetters.String())
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateByID failed\", ctxutil.IDField(data.ID), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBUpdateFailed, \"%s.UpdateByID: %%v\", err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tif affected == 0 {\n\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: id=%%d\", data.ID)\n\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Get(ctx, data.ID)\n")
		fmt.Fprintf(&b, "\tif err != nil {\n\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.UpdateByID refetch: %%v\", err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.UpdateByID ok\", ctxutil.IDField(data.ID))\n\treturn result, nil\n}\n\n", modelName)
	} else {
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) UpdateByID(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.UpdateOneID(data.ID).\n%s\t\tSave(ctx)\n", updateSetters.String())
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: id=%%d\", data.ID)\n\t\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateByID failed\", ctxutil.IDField(data.ID), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBUpdateFailed, \"%s.UpdateByID: %%v\", err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.UpdateByID ok\", ctxutil.IDField(data.ID))\n\treturn result, nil\n}\n\n", modelName)
	}

	// UpdateByXxx for each unique field — with soft-delete filter
	for _, uf := range uniqueFields {
		camel := entFieldName(fieldMap, uf.Name)
		goType := mapEntFieldTypeToGo(uf)
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) UpdateBy%s(ctx context.Context, %s %s, data *ent.%s) (*ent.%s, error) {\n", entPkg, camel, uf.Name, goType, modelName, modelName)
		if hasSoftDelete {
			fmt.Fprintf(&b, "\texisting, err := d.cli.Query().Where(%s.%sEQ(%s), %s.DeletedAtIsNil()).Only(ctx)\n", entPkg, camel, uf.Name, entPkg)
		} else {
			fmt.Fprintf(&b, "\texisting, err := d.cli.Query().Where(%s.%sEQ(%s)).Only(ctx)\n", entPkg, camel, uf.Name)
		}
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: %s=%%v\", %s)\n\t\t}\n", modelName, modelName, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateBy%s query failed\", ctxutil.ErrField(err))\n", modelName, camel)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.UpdateBy%s query %s=%%v: %%v\", %s, err)\n\t}\n", modelName, camel, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\tresult, err := d.cli.UpdateOneID(existing.ID).\n%s\t\tSave(ctx)\n", updateSetters.String())
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateBy%s failed\", ctxutil.ErrField(err))\n", modelName, camel)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBUpdateFailed, \"%s.UpdateBy%s: %%v\", err)\n\t}\n", modelName, camel)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.UpdateBy%s ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName, camel)
	}

	// Soft delete — with DeletedAtIsNil filter
	if hasSoftDelete {
		fmt.Fprintf(&b, "// ──── Delete (soft) ────\n\n")
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) DeleteByID(ctx context.Context, id int) error {\n", entPkg)
		fmt.Fprintf(&b, "\taffected, err := d.cli.Update().\n\t\tWhere(%s.ID(id), %s.DeletedAtIsNil()).\n\t\tSetDeletedAt(time.Now()).\n\t\tSave(ctx)\n", entPkg, entPkg)
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.DeleteByID failed\", ctxutil.IDField(id), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn errcode.Wrapf(errcode.DBDeleteFailed, \"%s.DeleteByID id=%%d: %%v\", id, err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tif affected == 0 {\n\t\treturn errcode.Newf(errcode.%sNotFound, \"%s not found: id=%%d\", id)\n\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.DeleteByID ok\", ctxutil.IDField(id))\n\treturn nil\n}\n\n", modelName)
	}

	// List with filter + optional pagination + soft-delete filter
	fmt.Fprintf(&b, "// ──── List ────\n\n")
	fmt.Fprintf(&b, "func (d *%sOceanBaseDao) List(ctx context.Context, filter *dao.%sListFilter, page *model.PageInfo) ([]*ent.%s, int, error) {\n", entPkg, modelName, modelName)
	if hasSoftDelete {
		fmt.Fprintf(&b, "\tquery := d.cli.Query().Where(%s.DeletedAtIsNil())\n\n", entPkg)
	} else {
		fmt.Fprintf(&b, "\tquery := d.cli.Query()\n\n")
	}
	fmt.Fprintf(&b, "\t// Apply filter conditions (only indexed fields).\n")
	fmt.Fprintf(&b, "\tif filter != nil {\n")
	for _, f := range indexedFields {
		camel := entFieldName(fieldMap, f.Name)
		fmt.Fprintf(&b, "\t\tif filter.%s != nil {\n\t\t\tquery = query.Where(%s.%sEQ(*filter.%s))\n\t\t}\n", camel, entPkg, camel, camel)
	}
	fmt.Fprintf(&b, "\t}\n\n")
	fmt.Fprintf(&b, "\ttotal, err := query.Clone().Count(ctx)\n")
	fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.List count failed\", ctxutil.ErrField(err))\n", modelName)
	fmt.Fprintf(&b, "\t\treturn nil, 0, errcode.Wrapf(errcode.DBQueryFailed, \"%s.List count: %%v\", err)\n\t}\n", modelName)
	fmt.Fprintf(&b, "\tif total == 0 {\n\t\treturn nil, 0, nil\n\t}\n\n")
	fmt.Fprintf(&b, "\t// Apply pagination only when page is not nil.\n")
	fmt.Fprintf(&b, "\tif page != nil {\n\t\tquery = query.\n\t\t\tOffset((page.Page - 1) * page.PageSize).\n\t\t\tLimit(page.PageSize)\n\t}\n\n")
	fmt.Fprintf(&b, "\tlist, err := query.\n\t\tOrder(%s.ByID()).\n\t\tAll(ctx)\n", entPkg)
	fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.List query failed\", ctxutil.ErrField(err))\n", modelName)
	fmt.Fprintf(&b, "\t\treturn nil, 0, errcode.Wrapf(errcode.DBQueryFailed, \"%s.List: %%v\", err)\n\t}\n", modelName)
	fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.List ok\", ctxutil.CountField(total))\n\treturn list, total, nil\n}\n", modelName)

	return os.WriteFile(filePath, []byte(b.String()), 0644)
}

// ==================== DAO Hook ====================

// genDaoHook generates internal/dao/hook/{model_snake}_hook.go with a placeholder
// for ent hooks (e.g. audit logging, soft-delete filtering, field defaults).
// The file is NOT overwritten if it already exists — user edits are preserved.
func genDaoHook(g *GenContext, outputDir, modulePath string, schema *load.Schema) error {
	dir := filepath.Join(outputDir, "internal", "dao", "hook")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	modelName := schema.Name
	modelSnake := generator.FileSnake(modelName)
	filePath := filepath.Join(dir, modelSnake+"_hook.go")
	if pathx.FileExists(filePath) {
		return nil // never overwrite — user may have added hooks
	}

	content := fmt.Sprintf(`package hook

import (
	"%s/ent"
)

// Register%sHooks registers ent hooks for %s.
// Called once during service initialization (e.g. in NewServiceContext).
//
// Example hooks:
//   - Audit logging: log who created/updated a record
//   - Soft-delete filter: auto-add WHERE deleted_at IS NULL
//   - Field defaults: set created_by from ctx
//
// Usage in svc:
//
//	hook.Register%sHooks(entClient)
func Register%sHooks(client *ent.Client) {
	// TODO: add hooks here, for example:
	//
	// client.%s.Use(func(next ent.Mutator) ent.Mutator {
	// 	return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
	// 		// before mutation
	// 		v, err := next.Mutate(ctx, m)
	// 		// after mutation
	// 		return v, err
	// 	})
	// })
}
`, modulePath,
		modelName, modelName,
		modelName,
		modelName,
		modelName)

	return os.WriteFile(filePath, []byte(content), 0644)
}

// ==================== Module Errcode ====================

// genModuleErrcode is now a no-op for individual schemas.
// All DAO error codes are generated collectively by genDaoErrcodeAll after the loop.
func genModuleErrcode(g *GenContext, outputDir, modulePath string, schema *load.Schema) error {
	return nil // handled by genDaoErrcodeAll
}

// genDaoErrcodeAll generates a single pkg/errcode/dao.go containing error codes
// for ALL ent schemas, with each module assigned a unique 100-code segment.
// It also auto-appends empty i18n entries to locale JSON files.
//
// Segment layout (base = 11000):
//
//	Module 0: 11100 ~ 11199
//	Module 1: 11200 ~ 11299
//	...
//
// Always overwrites to ensure consistency across all modules.
func genDaoErrcodeAll(outputDir string, schemaNames []string) error {
	dir := filepath.Join(outputDir, "pkg", "errcode")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	const segmentBase = 11000
	const segmentSize = 100

	var b strings.Builder
	b.WriteString("package errcode\n\n")
	b.WriteString("// Code generated by zctl. DO NOT EDIT.\n")
	b.WriteString("// DAO module error codes — each module gets a 100-code segment.\n")
	b.WriteString("// Messages come from i18n (pkg/i18n/locale/{lang}.json → key \"errcode.{code}\").\n\n")

	// Collect all error code → empty string for i18n
	var i18nCodes []int

	for i, name := range schemaNames {
		base := segmentBase + (i+1)*segmentSize // 11100, 11200, 11300, ...
		notFound := base + 1
		createFailed := base + 2
		updateFailed := base + 3
		deleteFailed := base + 4

		b.WriteString(fmt.Sprintf("// ──── %s: %d ~ %d ────\n", name, base, base+segmentSize-1))
		b.WriteString("const (\n")
		b.WriteString(fmt.Sprintf("\t%sNotFound     = %d\n", name, notFound))
		b.WriteString(fmt.Sprintf("\t%sCreateFailed = %d\n", name, createFailed))
		b.WriteString(fmt.Sprintf("\t%sUpdateFailed = %d\n", name, updateFailed))
		b.WriteString(fmt.Sprintf("\t%sDeleteFailed = %d\n", name, deleteFailed))
		b.WriteString(")\n\n")

		i18nCodes = append(i18nCodes, notFound, createFailed, updateFailed, deleteFailed)
	}

	filePath := filepath.Join(dir, "dao.go")
	if err := os.WriteFile(filePath, []byte(b.String()), 0644); err != nil {
		return err
	}

	// Auto-append i18n entries
	if err := appendI18nEntries(outputDir, i18nCodes); err != nil {
		fmt.Printf("  ⚠ Failed to update i18n locale files: %v\n", err)
	}

	return nil
}

// appendI18nEntries adds missing error code keys to all locale JSON files
// with empty string values. Existing keys are NOT overwritten.
func appendI18nEntries(outputDir string, codes []int) error {
	localeDir := filepath.Join(outputDir, "pkg", "i18n", "locale")
	entries, err := os.ReadDir(localeDir)
	if err != nil {
		return nil // i18n not set up yet, skip silently
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		filePath := filepath.Join(localeDir, entry.Name())
		if err := mergeI18nCodes(filePath, codes); err != nil {
			return err
		}
	}
	return nil
}

// mergeI18nCodes reads a locale JSON file, adds missing errcode keys with empty
// values, and writes it back with stable formatting.
func mergeI18nCodes(filePath string, codes []int) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	// Simple JSON parse: we expect {"errcode": {"key": "val", ...}, ...}
	// Use encoding/json for safety
	var root map[string]map[string]string
	if err := json.Unmarshal(data, &root); err != nil {
		return nil // malformed JSON, skip
	}

	errSection, ok := root["errcode"]
	if !ok {
		errSection = make(map[string]string)
		root["errcode"] = errSection
	}

	changed := false
	for _, code := range codes {
		key := fmt.Sprintf("%d", code)
		if _, exists := errSection[key]; !exists {
			errSection[key] = ""
			changed = true
		}
	}

	if !changed {
		return nil
	}

	// Write back with indentation
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, append(out, '\n'), 0644)
}

// ==================== Module Model (placeholder) ====================

func genModuleModel(g *GenContext, outputDir string, schema *load.Schema) error {
	dir := filepath.Join(outputDir, "pkg", "model")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	filename := generator.FileSnake(schema.Name)
	filePath := filepath.Join(dir, filename+".go")
	if pathx.FileExists(filePath) {
		return nil // don't overwrite model files
	}

	content := fmt.Sprintf(`package model

// ──── %s module models ────
// Define VO/DTO structs for %s module here.
`, schema.Name, schema.Name)

	return os.WriteFile(filePath, []byte(content), 0644)
}

// ==================== Module Constants (placeholder) ====================

func genModuleConst(g *GenContext, outputDir string, schema *load.Schema) error {
	dir := filepath.Join(outputDir, "pkg", "consts")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	filename := generator.FileSnake(schema.Name)
	filePath := filepath.Join(dir, filename+".go")
	if pathx.FileExists(filePath) {
		return nil // don't overwrite const files
	}

	content := fmt.Sprintf(`package consts

// ──── %s module constants ────
// Define business constants for %s module here.
`, schema.Name, schema.Name)

	return os.WriteFile(filePath, []byte(content), 0644)
}

// ==================== Test Skeleton ====================

func genTestSkeleton(g *GenContext, outputDir, modulePath string, schema *load.Schema) error {
	modelDir := generator.DirName(schema.Name)
	// Test file goes to logic/{group}/{model}/ (same directory as logic files generated by gen-rpc)
	testDir := filepath.Join(outputDir, "internal", "logic", g.GroupName, modelDir)
	if err := pathx.MkdirIfNotExist(testDir); err != nil {
		return err
	}

	modelName := schema.Name
	modelLower := strings.ToLower(modelName)
	pkgName := generator.PkgName(modelName) // package name matches ent convention: all lowercase

	filename, err := format.FileNamingFormat(g.Style, modelLower+"_test")
	if err != nil {
		return err
	}
	filePath := filepath.Join(testDir, filename+".go")
	if pathx.FileExists(filePath) && !g.Overwrite {
		return nil
	}

	content := fmt.Sprintf(`package %s

import (
	"context"
	"testing"
)

func TestCreate%s(t *testing.T) {
	_ = context.Background()
	t.Skip("TODO: implement - mock %sDao and test Create")
}

func TestGet%sById(t *testing.T) {
	_ = context.Background()
	t.Skip("TODO: implement - mock %sDao and test GetByID")
}

func TestUpdate%s(t *testing.T) {
	_ = context.Background()
	t.Skip("TODO: implement - mock %sDao and test Update")
}

func TestDelete%s(t *testing.T) {
	_ = context.Background()
	t.Skip("TODO: implement - mock %sDao and test Delete")
}

func TestGet%sList(t *testing.T) {
	_ = context.Background()
	t.Skip("TODO: implement - mock %sDao and test List")
}
`, pkgName,
		modelName, modelName,
		modelName, modelName,
		modelName, modelName,
		modelName, modelName,
		modelName, modelName,
	)

	return os.WriteFile(filePath, []byte(content), 0644)
}

// ==================== Schema analysis helpers ====================

// uniqueFieldInfo holds info about a unique field for code generation.
type uniqueFieldInfo struct {
	Name     string // snake_case field name
	TypeName string // ent type string (e.g. "string", "int")
}

// collectUniqueFields returns fields that have .Unique() set on them.
func collectUniqueFields(schema *load.Schema) []uniqueFieldInfo {
	var result []uniqueFieldInfo
	for _, f := range schema.Fields {
		if f.Unique {
			result = append(result, uniqueFieldInfo{Name: f.Name, TypeName: f.Info.Type.String()})
		}
	}
	return result
}

// collectIndexedFields returns fields that have indexes (unique fields + explicit index fields).
// For ListFilter: only fields with some kind of index. Excludes deleted_at (handled by soft-delete logic).
func collectIndexedFields(schema *load.Schema) []uniqueFieldInfo {
	seen := make(map[string]bool)
	var result []uniqueFieldInfo

	// Unique fields are indexed
	for _, f := range schema.Fields {
		if f.Unique && f.Name != "deleted_at" {
			result = append(result, uniqueFieldInfo{Name: f.Name, TypeName: f.Info.Type.String()})
			seen[f.Name] = true
		}
	}

	// Explicit indexes (from Indexes() method)
	for _, idx := range schema.Indexes {
		for _, col := range idx.Fields {
			if seen[col] || col == "deleted_at" {
				continue
			}
			// Find field info
			for _, f := range schema.Fields {
				if f.Name == col {
					result = append(result, uniqueFieldInfo{Name: f.Name, TypeName: f.Info.Type.String()})
					seen[col] = true
					break
				}
			}
		}
	}

	// Also add "status" if it exists (common query field even without explicit index)
	for _, f := range schema.Fields {
		if f.Name == "status" && !seen["status"] {
			result = append(result, uniqueFieldInfo{Name: f.Name, TypeName: f.Info.Type.String()})
			seen["status"] = true
		}
	}

	return result
}

// hasDeletedAtField checks if the schema has a "deleted_at" field (for soft delete).
func hasDeletedAtField(schema *load.Schema) bool {
	for _, f := range schema.Fields {
		if f.Name == "deleted_at" {
			return true
		}
	}
	return false
}

// compositeUniqueIndex represents a composite unique index with multiple fields.
type compositeUniqueIndex struct {
	Fields []uniqueFieldInfo // ordered fields in the composite index
}

// MethodName returns the Go method name suffix like "AppCodeAPIIDPermCode".
func (c compositeUniqueIndex) MethodName(fieldMap map[string]string) string {
	var parts []string
	for _, f := range c.Fields {
		parts = append(parts, entFieldName(fieldMap, f.Name))
	}
	return strings.Join(parts, "")
}

// collectCompositeUniqueIndexes returns composite unique indexes (Unique=true, len(Fields)>1).
// Excludes indexes that contain deleted_at.
func collectCompositeUniqueIndexes(schema *load.Schema) []compositeUniqueIndex {
	var result []compositeUniqueIndex
	for _, idx := range schema.Indexes {
		if !idx.Unique || len(idx.Fields) <= 1 {
			continue
		}
		// Skip indexes containing deleted_at
		hasDeletedAt := false
		for _, col := range idx.Fields {
			if col == "deleted_at" {
				hasDeletedAt = true
				break
			}
		}
		if hasDeletedAt {
			continue
		}
		var fields []uniqueFieldInfo
		for _, col := range idx.Fields {
			for _, f := range schema.Fields {
				if f.Name == col {
					fields = append(fields, uniqueFieldInfo{Name: f.Name, TypeName: f.Info.Type.String()})
					break
				}
			}
		}
		if len(fields) == len(idx.Fields) {
			result = append(result, compositeUniqueIndex{Fields: fields})
		}
	}
	return result
}

// mapEntFieldTypeToGo maps an ent field info to a Go type string.
func mapEntFieldTypeToGo(f uniqueFieldInfo) string {
	switch f.TypeName {
	case "string":
		return "string"
	case "bool":
		return "bool"
	case "int":
		return "int"
	case "int8":
		return "int8"
	case "int16":
		return "int16"
	case "int32":
		return "int32"
	case "int64":
		return "int64"
	case "uint":
		return "uint"
	case "uint8":
		return "uint8"
	case "uint16":
		return "uint16"
	case "uint32":
		return "uint32"
	case "uint64":
		return "uint64"
	case "float32":
		return "float32"
	case "float64":
		return "float64"
	case "time.Time":
		return "time.Time"
	default:
		return "string"
	}
}

// ==================== Auto gen-rpc (merge + protoc + logic) ====================

// autoGenRpc performs merge-proto + protoc + GenLogicFiles after desc protos are generated.
// This ensures logic files are created with correct pb signatures in one step.
func autoGenRpc(abs, style string) error {
	// 1. Find service name
	serviceName := filepath.Base(abs)
	if data, err := os.ReadFile(filepath.Join(abs, "Makefile")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "SERVICE_STYLE=") {
				if v := strings.TrimSpace(strings.TrimPrefix(line, "SERVICE_STYLE=")); v != "" {
					serviceName = v
					break
				}
			}
		}
	}

	// 2. Merge desc/ → root proto
	descDir := filepath.Join(abs, "desc")
	rootProto := filepath.Join(abs, serviceName+".proto")
	if err := generator.MergeDescProtos(descDir, rootProto, serviceName); err != nil {
		return fmt.Errorf("merge proto: %w", err)
	}
	fmt.Printf("[zctl] Merged desc/ → %s.proto\n", serviceName)

	// 3. Run protoc
	typesDir := filepath.Join(abs, "types")
	pathx.MkdirIfNotExist(typesDir)

	// Map validate.proto's go_package so protoc doesn't generate it locally —
	// the project imports it as a Go module dependency instead.
	validateMapping := "Mbuf/validate/validate.proto=buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	protocCmd := fmt.Sprintf(
		"protoc -I=%s -I=%s %s"+
			" --go_out=%s --go_opt=%s"+
			" --go-grpc_out=%s --go-grpc_opt=%s",
		abs, filepath.Join(abs, "proto"), filepath.Base(rootProto),
		typesDir, validateMapping,
		typesDir, validateMapping,
	)
	fmt.Printf("[zctl] Running: %s\n", protocCmd)
	cmd := exec.Command("sh", "-c", protocCmd)
	cmd.Dir = abs
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("protoc: %w", err)
	}

	// 4. Auto-generate enum helpers
	_ = generator.GenEnumsFromProto(abs)

	// 5. Generate logic files with correct pb signatures
	if style == "" {
		style = "go_zero"
	}
	if err := generator.GenLogicFiles(abs, style, false); err != nil {
		return fmt.Errorf("gen logic: %w", err)
	}

	return nil
}

// ==================== Helpers ====================

func isBaseField(name string) bool {
	switch name {
	case "id", "created_at", "updated_at", "create_time", "update_time", "deleted_at":
		return true
	}
	return false
}

func toCamelCase(s string) string {
	return generator.GoPascal(s)
}

// buildFieldMap creates a mapping from snake_case field name to Go PascalCase struct field name
// using ent's gen.Type.Fields[i].StructField(), which correctly handles Go initialisms
// (e.g. "api_code" → "APICode", "uid" → "UID", "url" → "URL").
func buildFieldMap(node *gen.Type) map[string]string {
	m := make(map[string]string)
	if node == nil {
		return m
	}
	for _, f := range node.Fields {
		m[f.Name] = f.StructField()
	}
	return m
}

// entFieldName returns the Go PascalCase struct field name for a given snake_case field name.
// It first looks up the fieldMap (built from ent gen.Type), falling back to GoPascal.
func entFieldName(fieldMap map[string]string, snakeName string) string {
	if v, ok := fieldMap[snakeName]; ok {
		return v
	}
	return generator.GoPascal(snakeName)
}

// ==================== Desc Proto Generation ====================

// genDescProto generates desc/{group}/{model_snake}.proto with messages and service methods
// derived from the DAO interface. If the DAO file already exists (meaning proto was already
// generated once), skip proto generation to avoid overwriting user edits.
//
// Naming convention:
//   - rpc CreateUser (CreateUserReq) returns (CreateUserResp)  ← 大写开头
//   - UserInfo is reused as the detail payload in create/update req and getById resp
//   - PageInfo is reused from base.proto for list pagination
//   - Empty is reused from base.proto for empty responses
//   - Proto methods are generated from DAO interface methods (not hardcoded CRUD)
func genDescProto(g *GenContext, outputDir string, schema *load.Schema) error {
	modelName := schema.Name
	modelSnake := generator.FileSnake(modelName) // for proto file name: user_info.proto
	groupName := g.GroupName

	descDir := filepath.Join(outputDir, "desc", groupName)
	if err := pathx.MkdirIfNotExist(descDir); err != nil {
		return err
	}

	filePath := filepath.Join(descDir, modelSnake+".proto")
	if pathx.FileExists(filePath) && !g.Overwrite {
		return nil
	}

	// Build message fields from ent schema
	var infoFields strings.Builder
	fieldNum := 1
	infoFields.WriteString(fmt.Sprintf("  // 主键ID\n  optional uint64 id = %d;\n", fieldNum))
	fieldNum++
	infoFields.WriteString(fmt.Sprintf("  // 创建时间\n  optional int64 created_at = %d;\n", fieldNum))
	fieldNum++
	infoFields.WriteString(fmt.Sprintf("  // 更新时间\n  optional int64 updated_at = %d;\n", fieldNum))
	fieldNum++

	for _, f := range schema.Fields {
		if isBaseField(f.Name) {
			continue
		}
		protoType := goTypeToProtoType(f.Info.Type.String())
		optional := ""
		if f.Optional || f.Nillable {
			optional = "optional "
		}
		// Add comment from ent schema if available
		if f.Comment != "" {
			infoFields.WriteString(fmt.Sprintf("  // %s\n", f.Comment))
		}
		infoFields.WriteString(fmt.Sprintf("  %s%s %s = %d;\n", optional, protoType, f.Name, fieldNum))
		fieldNum++
	}

	// Collect unique fields and check soft delete for generating appropriate methods
	uniqueFields := collectUniqueFields(schema)
	hasSoftDelete := hasDeletedAtField(schema)

	// Build proto content: messages + service methods from DAO interface methods
	var b strings.Builder
	b.WriteString(fmt.Sprintf("syntax = \"proto3\";\n\n"))
	b.WriteString(fmt.Sprintf("// ──── %s module ────\n\n", modelName))
	b.WriteString(fmt.Sprintf("// %sInfo 核心详情结构，用于创建/更新/查询详情。\n", modelName))
	b.WriteString(fmt.Sprintf("message %sInfo {\n%s}\n\n", modelName, infoFields.String()))

	b.WriteString("// ──── Request / Response ────\n\n")

	// Track generated messages and rpc methods
	type rpcEntry struct {
		comment string
		method  string // rpc method name (PascalCase)
		req     string
		resp    string
	}
	var rpcs []rpcEntry
	generatedMessages := make(map[string]bool)

	// ── Generate messages and rpcs based on DAO methods ──

	// 1. Create → CreateXxxReq / CreateXxxResp
	b.WriteString(fmt.Sprintf("// 创建%s请求\n", modelName))
	b.WriteString(fmt.Sprintf("message Create%sReq {\n  %sInfo info = 1;\n}\n\n", modelName, modelName))
	b.WriteString(fmt.Sprintf("// 创建%s响应\n", modelName))
	b.WriteString(fmt.Sprintf("message Create%sResp {\n  uint64 id = 1;\n}\n\n", modelName))
	generatedMessages["Create"+modelName+"Req"] = true
	generatedMessages["Create"+modelName+"Resp"] = true
	rpcs = append(rpcs, rpcEntry{
		comment: fmt.Sprintf("  // 创建%s\n", modelName),
		method:  "Create" + modelName,
		req:     "Create" + modelName + "Req",
		resp:    "Create" + modelName + "Resp",
	})

	// 2. GetByID → GetXxxByIDReq
	b.WriteString(fmt.Sprintf("// 按ID查询%s请求\n", modelName))
	b.WriteString(fmt.Sprintf("message Get%sByIDReq {\n  uint64 id = 1;\n}\n\n", modelName))
	generatedMessages["Get"+modelName+"ByIDReq"] = true
	rpcs = append(rpcs, rpcEntry{
		comment: fmt.Sprintf("  // 按ID获取%s详情\n", modelName),
		method:  "Get" + modelName + "ByID",
		req:     "Get" + modelName + "ByIDReq",
		resp:    modelName + "Info",
	})

	// 3. GetByXxx for each unique field → GetXxxByYyyReq
	for _, uf := range uniqueFields {
		fieldPascal := generator.GoPascal(uf.Name)
		protoType := goTypeToProtoType(uf.TypeName)
		msgName := fmt.Sprintf("Get%sBy%sReq", modelName, fieldPascal)
		if !generatedMessages[msgName] {
			b.WriteString(fmt.Sprintf("// 按%s查询%s请求\n", fieldPascal, modelName))
			b.WriteString(fmt.Sprintf("message %s {\n  %s %s = 1;\n}\n\n", msgName, protoType, uf.Name))
			generatedMessages[msgName] = true
		}
		rpcs = append(rpcs, rpcEntry{
			comment: fmt.Sprintf("  // 按%s获取%s详情\n", fieldPascal, modelName),
			method:  fmt.Sprintf("Get%sBy%s", modelName, fieldPascal),
			req:     msgName,
			resp:    modelName + "Info",
		})
	}

	// 4. UpdateByID → UpdateXxxReq
	b.WriteString(fmt.Sprintf("// 更新%s请求\n", modelName))
	b.WriteString(fmt.Sprintf("message Update%sReq {\n  %sInfo info = 1;\n}\n\n", modelName, modelName))
	generatedMessages["Update"+modelName+"Req"] = true
	rpcs = append(rpcs, rpcEntry{
		comment: fmt.Sprintf("  // 更新%s\n", modelName),
		method:  "Update" + modelName,
		req:     "Update" + modelName + "Req",
		resp:    "Empty",
	})

	// 5. UpdateByXxx for each unique field → UpdateXxxByYyyReq
	for _, uf := range uniqueFields {
		fieldPascal := generator.GoPascal(uf.Name)
		protoType := goTypeToProtoType(uf.TypeName)
		msgName := fmt.Sprintf("Update%sBy%sReq", modelName, fieldPascal)
		if !generatedMessages[msgName] {
			b.WriteString(fmt.Sprintf("// 按%s更新%s请求\n", fieldPascal, modelName))
			b.WriteString(fmt.Sprintf("message %s {\n  %s %s = 1;\n  %sInfo info = 2;\n}\n\n", msgName, protoType, uf.Name, modelName))
			generatedMessages[msgName] = true
		}
		rpcs = append(rpcs, rpcEntry{
			comment: fmt.Sprintf("  // 按%s更新%s\n", fieldPascal, modelName),
			method:  fmt.Sprintf("Update%sBy%s", modelName, fieldPascal),
			req:     msgName,
			resp:    "Empty",
		})
	}

	// 6. DeleteByID (only if soft delete supported)
	if hasSoftDelete {
		b.WriteString(fmt.Sprintf("// 删除%s请求（支持批量）\n", modelName))
		b.WriteString(fmt.Sprintf("message Delete%sReq {\n  repeated uint64 ids = 1;\n}\n\n", modelName))
		generatedMessages["Delete"+modelName+"Req"] = true
		rpcs = append(rpcs, rpcEntry{
			comment: fmt.Sprintf("  // 删除%s\n", modelName),
			method:  "Delete" + modelName,
			req:     "Delete" + modelName + "Req",
			resp:    "Empty",
		})
	}

	// 7. List → GetXxxListReq / GetXxxListResp
	b.WriteString(fmt.Sprintf("// 获取%s列表请求\n", modelName))
	b.WriteString(fmt.Sprintf("message Get%sListReq {\n  uint64 page = 1;\n  uint64 page_size = 2;\n}\n\n", modelName))
	b.WriteString(fmt.Sprintf("// 获取%s列表响应\n", modelName))
	b.WriteString(fmt.Sprintf("message Get%sListResp {\n  uint64 total = 1;\n  repeated %sInfo data = 2;\n}\n\n", modelName, modelName))
	generatedMessages["Get"+modelName+"ListReq"] = true
	generatedMessages["Get"+modelName+"ListResp"] = true
	rpcs = append(rpcs, rpcEntry{
		comment: fmt.Sprintf("  // 获取%s列表\n", modelName),
		method:  "Get" + modelName + "List",
		req:     "Get" + modelName + "ListReq",
		resp:    "Get" + modelName + "ListResp",
	})

	// ── Service block ──
	b.WriteString(fmt.Sprintf("// %s 管理服务\n", modelName))
	b.WriteString(fmt.Sprintf("service %s {\n", g.ServiceName))
	for _, r := range rpcs {
		b.WriteString(r.comment)
		b.WriteString(fmt.Sprintf("  rpc %s (%s) returns (%s);\n", r.method, r.req, r.resp))
	}
	b.WriteString("}\n")

	return os.WriteFile(filePath, []byte(b.String()), 0644)
}

// goTypeToProtoType converts Go type to proto type
func goTypeToProtoType(goType string) string {
	switch goType {
	case "string":
		return "string"
	case "bool":
		return "bool"
	case "int", "int32", "int8", "int16":
		return "int32"
	case "int64":
		return "int64"
	case "uint", "uint32", "uint8", "uint16":
		return "uint32"
	case "uint64":
		return "uint64"
	case "float32":
		return "float"
	case "float64":
		return "double"
	case "time.Time":
		return "int64"
	default:
		return "string"
	}
}
