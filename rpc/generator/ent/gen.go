package ent

import (
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

		if err := generateForSchema(&genCtx, projectCtx, outputDir, s); err != nil {
			return err
		}

		fmt.Printf("[zctl] Generated module: %s\n", s.Name)
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

func generateForSchema(g *GenContext, projectCtx *ctx.ProjectContext, outputDir string, schema *load.Schema) error {
	modulePath := projectCtx.Path

	// 1. Generate DAO interface
	if err := genDaoInterface(g, outputDir, modulePath, schema); err != nil {
		return err
	}

	// 2. Generate DAO OceanBase impl
	if err := genDaoOceanBaseImpl(g, outputDir, modulePath, schema); err != nil {
		return err
	}

	// 3. Generate DAO mock
	if err := genDaoMock(g, outputDir, modulePath, schema); err != nil {
		return err
	}

	// 4. Generate DAO hook file
	if err := genDaoHook(g, outputDir, modulePath, schema); err != nil {
		return err
	}

	// 5. Generate errcode module file
	if err := genModuleErrcode(g, outputDir, modulePath, schema); err != nil {
		return err
	}

	// 5. Generate test skeleton (in logic/{group}/{model_lower}/ dir)
	if err := genTestSkeleton(g, outputDir, modulePath, schema); err != nil {
		return err
	}

	// 6. Generate module model file
	if err := genModuleModel(g, outputDir, schema); err != nil {
		return err
	}

	// 7. Generate module constants file
	if err := genModuleConst(g, outputDir, schema); err != nil {
		return err
	}

	// 8. Generate desc/{group}/{model_lower}.proto
	if err := genDescProto(g, outputDir, schema); err != nil {
		return err
	}

	return nil
}

// ==================== DAO Interface ====================

func genDaoInterface(g *GenContext, outputDir, modulePath string, schema *load.Schema) error {
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

	// Collect indexed fields for ListFilter (unique fields + fields with explicit indexes)
	indexedFields := collectIndexedFields(schema)

	// Check if soft delete is supported (has deleted_at field)
	hasSoftDelete := hasDeletedAtField(schema)

	// Build GetByXxx methods
	var getMethods strings.Builder
	for _, uf := range uniqueFields {
		camel := toCamelCase(uf.Name)
		goType := mapEntFieldTypeToGo(uf)
		fmt.Fprintf(&getMethods, "\tGetBy%s(ctx context.Context, %s %s) (*ent.%s, error)\n", camel, uf.Name, goType, modelName)
	}

	// Build UpdateByXxx methods
	var updateMethods strings.Builder
	fmt.Fprintf(&updateMethods, "\tUpdateByID(ctx context.Context, data *ent.%s) (*ent.%s, error)\n", modelName, modelName)
	for _, uf := range uniqueFields {
		camel := toCamelCase(uf.Name)
		goType := mapEntFieldTypeToGo(uf)
		fmt.Fprintf(&updateMethods, "\tUpdateBy%s(ctx context.Context, %s %s, data *ent.%s) (*ent.%s, error)\n", camel, uf.Name, goType, modelName, modelName)
	}

	// Build ListFilter struct
	var filterFields strings.Builder
	for _, f := range indexedFields {
		camel := toCamelCase(f.Name)
		goType := mapEntFieldTypeToGo(f)
		fmt.Fprintf(&filterFields, "\t%s *%s // filter by %s\n", camel, goType, f.Name)
	}

	// Build delete method (only soft delete)
	var deleteMethod string
	if hasSoftDelete {
		deleteMethod = "\tDeleteByID(ctx context.Context, id int) error\n"
	}

	content := fmt.Sprintf(`package dao

import (
	"context"

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
`, modulePath, modulePath,
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
func genDaoMock(g *GenContext, outputDir, modulePath string, schema *load.Schema) error {
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

func genDaoOceanBaseImpl(g *GenContext, outputDir, modulePath string, schema *load.Schema) error {
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
	indexedFields := collectIndexedFields(schema)
	hasSoftDelete := hasDeletedAtField(schema)

	// Build field setter lines for Create and Update
	var createSetters, updateSetters strings.Builder
	for _, f := range schema.Fields {
		if isBaseField(f.Name) {
			continue
		}
		camel := toCamelCase(f.Name)
		if f.Optional || f.Nillable {
			fmt.Fprintf(&createSetters, "\t\tSetNillable%s(&data.%s).\n", camel, camel)
			fmt.Fprintf(&updateSetters, "\t\tSetNillable%s(&data.%s).\n", camel, camel)
		} else {
			fmt.Fprintf(&createSetters, "\t\tSet%s(data.%s).\n", camel, camel)
			fmt.Fprintf(&updateSetters, "\t\tSet%s(data.%s).\n", camel, camel)
		}
	}

	var b strings.Builder

	// Header
	fmt.Fprintf(&b, "package impl\n\nimport (\n\t\"context\"\n\n")
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

	// Create
	fmt.Fprintf(&b, "func (d *%sOceanBaseDao) Create(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
	fmt.Fprintf(&b, "\tresult, err := d.cli.Create().\n%s\t\tSave(ctx)\n", createSetters.String())
	fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.Create failed\", ctxutil.ErrField(err))\n", modelName)
	fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBInsertFailed, \"%s.Create: %%v\", err)\n\t}\n", modelName)
	fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.Create ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName)

	// GetByID
	fmt.Fprintf(&b, "// ──── Get single record ────\n\n")
	fmt.Fprintf(&b, "func (d *%sOceanBaseDao) GetByID(ctx context.Context, id int) (*ent.%s, error) {\n", entPkg, modelName)
	fmt.Fprintf(&b, "\tresult, err := d.cli.Get(ctx, id)\n")
	fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: id=%%d\", id)\n\t\t}\n", modelName, modelName)
	fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetByID failed\", ctxutil.IDField(id), ctxutil.ErrField(err))\n", modelName)
	fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.GetByID id=%%d: %%v\", id, err)\n\t}\n", modelName)
	fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.GetByID ok\", ctxutil.IDField(id))\n\treturn result, nil\n}\n\n", modelName)

	// GetByXxx for each unique field
	for _, uf := range uniqueFields {
		camel := toCamelCase(uf.Name)
		goType := mapEntFieldTypeToGo(uf)
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) GetBy%s(ctx context.Context, %s %s) (*ent.%s, error) {\n", entPkg, camel, uf.Name, goType, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Query().Where(%s.%sEQ(%s)).Only(ctx)\n", entPkg, camel, uf.Name)
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: %s=%%v\", %s)\n\t\t}\n", modelName, modelName, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetBy%s failed\", ctxutil.ErrField(err))\n", modelName, camel)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.GetBy%s %s=%%v: %%v\", %s, err)\n\t}\n", modelName, camel, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.GetBy%s ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName, camel)
	}

	// UpdateByID
	fmt.Fprintf(&b, "// ──── Update single record ────\n\n")
	fmt.Fprintf(&b, "func (d *%sOceanBaseDao) UpdateByID(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
	fmt.Fprintf(&b, "\tresult, err := d.cli.UpdateOneID(data.ID).\n%s\t\tSave(ctx)\n", updateSetters.String())
	fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: id=%%d\", data.ID)\n\t\t}\n", modelName, modelName)
	fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateByID failed\", ctxutil.IDField(data.ID), ctxutil.ErrField(err))\n", modelName)
	fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBUpdateFailed, \"%s.UpdateByID: %%v\", err)\n\t}\n", modelName)
	fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.UpdateByID ok\", ctxutil.IDField(data.ID))\n\treturn result, nil\n}\n\n", modelName)

	// UpdateByXxx for each unique field
	for _, uf := range uniqueFields {
		camel := toCamelCase(uf.Name)
		goType := mapEntFieldTypeToGo(uf)
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) UpdateBy%s(ctx context.Context, %s %s, data *ent.%s) (*ent.%s, error) {\n", entPkg, camel, uf.Name, goType, modelName, modelName)
		fmt.Fprintf(&b, "\texisting, err := d.cli.Query().Where(%s.%sEQ(%s)).Only(ctx)\n", entPkg, camel, uf.Name)
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: %s=%%v\", %s)\n\t\t}\n", modelName, modelName, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateBy%s query failed\", ctxutil.ErrField(err))\n", modelName, camel)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.UpdateBy%s query %s=%%v: %%v\", %s, err)\n\t}\n", modelName, camel, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\tresult, err := d.cli.UpdateOneID(existing.ID).\n%s\t\tSave(ctx)\n", updateSetters.String())
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateBy%s failed\", ctxutil.ErrField(err))\n", modelName, camel)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBUpdateFailed, \"%s.UpdateBy%s: %%v\", err)\n\t}\n", modelName, camel)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.UpdateBy%s ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName, camel)
	}

	// Soft delete (only if has deleted_at)
	if hasSoftDelete {
		fmt.Fprintf(&b, "// ──── Delete (soft) ────\n\n")
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) DeleteByID(ctx context.Context, id int) error {\n", entPkg)
		fmt.Fprintf(&b, "\t_, err := d.cli.UpdateOneID(id).\n\t\tSetDeletedAt(time.Now()).\n\t\tSave(ctx)\n")
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn errcode.Newf(errcode.%sNotFound, \"%s not found: id=%%d\", id)\n\t\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.DeleteByID failed\", ctxutil.IDField(id), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn errcode.Wrapf(errcode.DBDeleteFailed, \"%s.DeleteByID id=%%d: %%v\", id, err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.DeleteByID ok\", ctxutil.IDField(id))\n\treturn nil\n}\n\n", modelName)
	}

	// List with filter + optional pagination
	fmt.Fprintf(&b, "// ──── List ────\n\n")
	fmt.Fprintf(&b, "func (d *%sOceanBaseDao) List(ctx context.Context, filter *dao.%sListFilter, page *model.PageInfo) ([]*ent.%s, int, error) {\n", entPkg, modelName, modelName)
	fmt.Fprintf(&b, "\tquery := d.cli.Query()\n\n")
	fmt.Fprintf(&b, "\t// Apply filter conditions (only indexed fields).\n")
	fmt.Fprintf(&b, "\tif filter != nil {\n")
	for _, f := range indexedFields {
		camel := toCamelCase(f.Name)
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

func genModuleErrcode(g *GenContext, outputDir, modulePath string, schema *load.Schema) error {
	dir := filepath.Join(outputDir, "pkg", "errcode")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	// snake_case filename: user_info.go
	filename := generator.FileSnake(schema.Name)
	filePath := filepath.Join(dir, filename+".go")
	if pathx.FileExists(filePath) && !g.Overwrite {
		return nil
	}

	modelName := schema.Name
	// Auto-calculate code segment based on model name hash
	codeBase := 11000

	content := fmt.Sprintf(`package errcode

// ──── %s module error codes %d~%d ────
// Messages come from i18n (pkg/i18n/locale/{lang}.json → key "errcode.{code}").
const (
	%sNotFound     = %d
	%sCreateFailed = %d
	%sUpdateFailed = %d
	%sDeleteFailed = %d
)
`,
		modelName, codeBase, codeBase+999,
		modelName, codeBase+1,
		modelName, codeBase+2,
		modelName, codeBase+3,
		modelName, codeBase+4,
	)

	return os.WriteFile(filePath, []byte(content), 0644)
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
// For ListFilter: only fields with some kind of index.
func collectIndexedFields(schema *load.Schema) []uniqueFieldInfo {
	seen := make(map[string]bool)
	var result []uniqueFieldInfo

	// Unique fields are indexed
	for _, f := range schema.Fields {
		if f.Unique {
			result = append(result, uniqueFieldInfo{Name: f.Name, TypeName: f.Info.Type.String()})
			seen[f.Name] = true
		}
	}

	// Explicit indexes (from Indexes() method)
	for _, idx := range schema.Indexes {
		for _, col := range idx.Fields {
			if seen[col] {
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
	protocCmd := fmt.Sprintf("protoc -I=%s %s --go_out=%s --go-grpc_out=%s",
		abs, filepath.Base(rootProto), typesDir, typesDir)
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
	case "id", "created_at", "updated_at", "create_time", "update_time":
		return true
	}
	return false
}

func toCamelCase(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// ==================== Desc Proto Generation ====================

// genDescProto generates desc/{group}/{model_snake}.proto with CRUD messages and service methods.
//
// Naming convention:
//   - rpc createUser (CreateUserReq) returns (CreateUserResp)
//   - UserInfo is reused as the detail payload in create/update req and getById resp
//   - PageInfo is reused from base.proto for list pagination
//   - Empty is reused from base.proto for empty responses
//   - No generic IDReq/IDsReq/BaseIDResp; each method has its own {Method}Req/{Method}Resp
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

	content := fmt.Sprintf(`syntax = "proto3";

// ──── %s module ────

// %sInfo 核心详情结构，用于创建/更新/查询详情。
message %sInfo {
%s}

// ──── Request / Response ────

// 创建%s请求
message Create%sReq {
  %sInfo info = 1;
}

// 创建%s响应
message Create%sResp {
  uint64 id = 1;
}

// 更新%s请求
message Update%sReq {
  %sInfo info = 1;
}

// 按ID查询%s请求
message Get%sByIdReq {
  uint64 id = 1;
}

// 删除%s请求（支持批量）
message Delete%sReq {
  repeated uint64 ids = 1;
}

// 获取%s列表请求
message Get%sListReq {
  uint64 page = 1;
  uint64 page_size = 2;
}

// 获取%s列表响应
message Get%sListResp {
  uint64 total = 1;
  repeated %sInfo data = 2;
}

// %s 管理服务
service %s {
  // 创建%s
  rpc create%s (Create%sReq) returns (Create%sResp);
  // 更新%s
  rpc update%s (Update%sReq) returns (Empty);
  // 获取%s列表
  rpc get%sList (Get%sListReq) returns (Get%sListResp);
  // 按ID获取%s详情
  rpc get%sById (Get%sByIdReq) returns (%sInfo);
  // 删除%s
  rpc delete%s (Delete%sReq) returns (Empty);
}
`,
		modelName,
		// message UserInfoInfo
		modelName, modelName, infoFields.String(),
		// Create
		modelName, modelName, modelName,
		// CreateResp
		modelName, modelName,
		// Update
		modelName, modelName, modelName,
		// GetById
		modelName, modelName,
		// Delete
		modelName, modelName,
		// GetList
		modelName, modelName,
		// GetListResp
		modelName, modelName, modelName,
		// service
		modelName, g.ServiceName,
		// rpc create
		modelName, modelName, modelName, modelName,
		// rpc update
		modelName, modelName, modelName,
		// rpc getList
		modelName, modelName, modelName, modelName,
		// rpc getById
		modelName, modelName, modelName, modelName,
		// rpc delete
		modelName, modelName, modelName,
	)

	return os.WriteFile(filePath, []byte(content), 0644)
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
