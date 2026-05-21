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
//
// 三阶段原子化流程（**dao 永不回滚**）：
//   PhaseA  →  写 dao/impl/mock/hook         （独立落盘，失败直接退出，不进 tracker）
//   PhaseB  →  写 desc proto                  （进 tracker，与 protoc 一起作为原子单元）
//   protoc 预校验  →  merge desc/ → 根 .proto → protoc 编译
//                     失败：tracker 回滚 PhaseB 新增 desc proto + 根 .proto + types/ 新增 pb，
//                           **dao 保留**，PhaseC + 尾部全部跳过；用户照着 dao 改 desc 后重跑即可。
//                     成功：继续
//   PhaseC  →  写 errcode 模块 / test 骨架 / pkg/model / pkg/consts
//   尾部    →  genDaoErrcodeAll / GenEnumsFromProto / GenLogicFiles / EnsureEntInfra
//
// 这样保证：
//  1) dao 永远会被生成（前置物，proto 出错时也是用户排查依据）；
//  2) protoc 失败时 desc proto 与 logic/test 都回退干净，不会出现 logic/<group>/<model>/_test.go 孤儿；
//  3) 用户对 desc proto 的修改重跑只走 dao 跳过 + 重新校验路径，不会被覆盖。
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
	nodeMap := make(map[string]*gen.Type)
	for _, node := range schemas.Nodes {
		nodeMap[node.Name] = node
	}

	// 收集所有要处理的 schema 上下文，供后续多阶段循环复用。
	type schemaWithCtx struct {
		schema        *load.Schema
		genCtx        GenContext
		fieldMap      map[string]string
		daoPreExisted bool
	}
	var processed []schemaWithCtx

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

		fieldMap := buildFieldMap(nodeMap[s.Name])

		// ── PhaseA: 生成 DAO（独立落盘，不进 tracker） ──
		// dao 是后续所有产物的前置物：proto 出错用户也要照着 dao 改 desc，所以 dao 必须保住。
		preExisted, err := generateSchemaPhaseA(&genCtx, projectCtx, outputDir, s, fieldMap)
		if err != nil {
			return err
		}

		processed = append(processed, schemaWithCtx{
			schema:        s,
			genCtx:        genCtx,
			fieldMap:      fieldMap,
			daoPreExisted: preExisted,
		})
	}

	// ── PhaseB: 生成 desc proto（进 tracker，与 protoc 一起作为原子单元） ──
	// 仅追踪 desc/ 与 types/，dao 保留不动。根 .proto 单独通过 trackedRootProto 处理。
	tracker := newFileTracker(
		filepath.Join(outputDir, "desc"),
		filepath.Join(outputDir, "types"),
	)
	tracker.snapshot()

	rootProtoCandidate := filepath.Join(outputDir, deriveServiceFileBase(outputDir)+".proto")
	rootProtoExistedBefore := pathx.FileExists(rootProtoCandidate)

	// ── PhaseB 预校验 + 写 desc proto ──
	// 规则（与 make gen-rpc 的幂等语义对齐）：
	//   1) dao 已存在 + 未 --overwrite        → 跳过 desc 写入（"以现有 desc 为权威源"）
	//   2) desc/<targetGroup>/<x>.proto 已存在 → 跳过（不动用户已有 proto）
	//   3) desc/<其它 group>/<x>.proto 已存在  → 跳过 + 把本次 GroupName 校准到已有 group，
	//                                            后续 logic / errcode / model / consts 都落到正确位置
	//   4) 任何 group 下都没有同名 proto      → 按默认 GroupName 在 desc/<group>/ 下新建
	// 典型场景：用户历史在 desc/user/cs_user_profile.proto，这次没传 --group，
	//   默认 group=DirName(schemaName)=csuserprofile → 命中规则 3：跳过写盘 + 把 GroupName
	//   改回 user，避免同名 proto 在两个 group 下共存被 protoc 报"already defined"，也避免
	//   logic/csuserprofile/ 与 desc/user/ 错位。
	descRoot := filepath.Join(outputDir, "desc")
	for i := range processed {
		p := &processed[i]
		modelSnake := generator.FileSnake(p.schema.Name)
		protoFileName := modelSnake + ".proto"

		// dao 已存在 + 未 overwrite：维持原语义，不动 desc
		if p.daoPreExisted && !p.genCtx.Overwrite {
			continue
		}

		targetGroup := p.genCtx.GroupName
		targetPath := filepath.Join(descRoot, targetGroup, protoFileName)

		// 规则 2：目标位置已有同名 proto → 跳过（不覆盖用户既有 proto）
		if pathx.FileExists(targetPath) && !p.genCtx.Overwrite {
			fmt.Printf("[zctl] desc proto already exists, skip: desc/%s/%s\n", targetGroup, protoFileName)
			continue
		}

		// 规则 3：其它 group 下有同名 proto → 跳过写入并校准 GroupName，后续 logic 落到正确位置
		if existingPaths := findProtoConflicts(descRoot, protoFileName, targetGroup); len(existingPaths) > 0 {
			rel, _ := filepath.Rel(descRoot, existingPaths[0])
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) >= 2 {
				existingGroup := parts[0]
				fmt.Printf("[zctl] desc proto already exists under another group, reuse it: desc/%s/%s (was going to write desc/%s/%s)\n",
					existingGroup, protoFileName, targetGroup, protoFileName)
				p.genCtx.GroupName = existingGroup
			}
			continue
		}

		// 规则 4：全新 schema → 按默认 GroupName 写盘
		if err := genDescProto(&p.genCtx, outputDir, p.schema); err != nil {
			tracker.rollback()
			return err
		}
	}

	// ── protoc 预校验：merge + protoc，失败仅回滚 PhaseB ──
	fmt.Println("[zctl] Validating proto: merge desc/ + protoc compile ...")
	rootProto, mergeErr := runMergeAndProtoc(outputDir, g.Style)
	if mergeErr != nil {
		fmt.Printf("[zctl] ✗ proto validation failed: %v\n", mergeErr)
		fmt.Println("[zctl] Rolling back desc proto / merged root proto / pb files generated in this run ...")
		fmt.Println("[zctl] (DAO files are preserved — fix desc proto and re-run)")
		// 仅当根 .proto 是本次新生成的才删除（保护用户 PhaseB 之前就有的根 .proto）
		if rootProto != "" && !rootProtoExistedBefore {
			if _, err := os.Stat(rootProto); err == nil {
				_ = os.Remove(rootProto)
				fmt.Printf("[zctl] Removed merged root proto: %s\n", rootProto)
			}
		}
		tracker.rollback()
		return fmt.Errorf("proto validation failed, rolled back desc/types (DAO preserved): %w", mergeErr)
	}
	fmt.Println("[zctl] ✓ proto validation passed.")

	// ── PhaseC: 生成 errcode/test/model/consts，仅在 protoc 通过后执行 ──
	for _, p := range processed {
		if err := generateSchemaPhaseC(&p.genCtx, outputDir, p.schema, p.daoPreExisted); err != nil {
			return err
		}
		fmt.Printf("[zctl] Generated module: %s\n", p.schema.Name)
	}

	// Generate unified DAO errcode file (all modules in one file, with unique code segments + i18n)
	var allSchemaNames []string
	for _, s := range schemas.Schemas {
		allSchemaNames = append(allSchemaNames, s.Name)
	}
	if err := genDaoErrcodeAll(outputDir, allSchemaNames); err != nil {
		fmt.Printf("[zctl] Warning: failed to generate dao errcode: %v\n", err)
	} else {
		fmt.Printf("[zctl] Generated pkg/errcode/dao.go (%d modules)\n", len(allSchemaNames))
	}

	// ── 尾部：enum + logic 文件（protoc 已成功，几乎不会失败） ──
	if err := runPostProtoc(outputDir, g.Style); err != nil {
		fmt.Printf("[zctl] Warning: post-protoc generation failed: %v\n", err)
		fmt.Println("[zctl] You can manually run: make gen-rpc")
	}

	// Install ent infra (entlog/entx + ent init in svc) and register DAOs into ServiceContext.
	if err := generator.EnsureEntInfra(outputDir, projectCtx.Path); err != nil {
		fmt.Printf("[zctl] Warning: failed to ensure ent infra: %v\n", err)
	} else {
		fmt.Println("[zctl] ent infra ready (entlog/entx + ServiceContext patched).")
	}

	fmt.Println("[zctl] Done.")
	return nil
}

// deriveServiceFileBase 从 Makefile 的 SERVICE_STYLE 推导根 proto 文件名（不含后缀）。
// 与 runMergeAndProtoc 内部的同名解析逻辑保持一致，避免重复实现。
func deriveServiceFileBase(abs string) string {
	serviceName := filepath.Base(abs)
	if data, err := os.ReadFile(filepath.Join(abs, "Makefile")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "SERVICE_STYLE=") {
				if v := strings.TrimSpace(strings.TrimPrefix(line, "SERVICE_STYLE=")); v != "" {
					return v
				}
			}
		}
	}
	return serviceName
}

// findProtoConflicts 在 descRoot 下递归查找所有 base==targetFileName 的 proto 文件，
// 返回那些位于非 targetGroup 目录下的路径。targetGroup 为空时不做过滤（任何重名都算冲突）。
//
// 用于 PhaseB 写 desc proto 前的预校验，避免出现 desc/<groupA>/x.proto 与 desc/<groupB>/x.proto
// 共存导致 merge 后 protoc 报"already defined"的尴尬场景。
func findProtoConflicts(descRoot, targetFileName, targetGroup string) []string {
	var conflicts []string
	_ = filepath.Walk(descRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) != targetFileName {
			return nil
		}
		// 计算该 proto 所在的一级 group 目录（desc/<group>/...）
		rel, relErr := filepath.Rel(descRoot, path)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 2 {
			// 直接放在 desc/ 根下（如 base.proto）— 不算 group 内的
			return nil
		}
		fileGroup := parts[0]
		if fileGroup != targetGroup {
			conflicts = append(conflicts, path)
		}
		return nil
	})
	return conflicts
}

func generateForSchema(g *GenContext, projectCtx *ctx.ProjectContext, outputDir string, schema *load.Schema, fieldMap map[string]string) error {
	// 兼容入口（历史调用方使用）：依次跑 PhaseA → desc proto → PhaseC，无 protoc 校验。
	// 新主流程 GenEntLogic 已不再走这里，而是显式拆 A / B / C 三阶段并加 protoc 预校验。
	preExisted, err := generateSchemaPhaseA(g, projectCtx, outputDir, schema, fieldMap)
	if err != nil {
		return err
	}
	if !preExisted || g.Overwrite {
		if err := genDescProto(g, outputDir, schema); err != nil {
			return err
		}
	}
	return generateSchemaPhaseC(g, outputDir, schema, preExisted)
}

// generateSchemaPhaseA 仅生成 DAO 层产物（DAO 接口 / OB 实现 / Mock / Hook）。
//
// **dao 永远独立落盘**，不参与 protoc 校验失败回滚。即便 desc proto 校验失败，
// dao 也是用户照着改 desc 的关键参考物，必须保留。
//
// 返回 daoPreExisted —— 表示"在本函数执行之前 DAO 文件就已存在"。这是后续
// PhaseB（desc proto）/ PhaseC（errcode/test/model/consts）是否跳过的判定依据，
// 与原 generateForSchema 中的语义保持一致。
func generateSchemaPhaseA(g *GenContext, projectCtx *ctx.ProjectContext, outputDir string, schema *load.Schema, fieldMap map[string]string) (daoPreExisted bool, err error) {
	modulePath := projectCtx.Path
	modelSnake := generator.FileSnake(schema.Name)

	// ── 先看后做：在生成 DAO 之前，记住 DAO 文件是否已经存在 ──
	daoFilePath := filepath.Join(outputDir, "internal", "dao", modelSnake+"_dao.go")
	daoPreExisted = pathx.FileExists(daoFilePath)

	// 1. Generate DAO interface
	if err = genDaoInterface(g, outputDir, modulePath, schema, fieldMap); err != nil {
		return
	}

	// 2. Generate DAO OceanBase impl
	if err = genDaoOceanBaseImpl(g, outputDir, modulePath, schema, fieldMap); err != nil {
		return
	}

	// 3. Generate DAO mock
	if err = genDaoMock(g, outputDir, modulePath, schema, fieldMap); err != nil {
		return
	}

	// 4. Generate DAO hook file
	if err = genDaoHook(g, outputDir, modulePath, schema); err != nil {
		return
	}
	return
}

// generateSchemaPhaseC 生成"protoc 预校验通过后"的产物：
//   errcode 模块文件 / test 骨架 / pkg/model 占位 / pkg/consts 占位。
//
// 这些产物全部依赖 desc proto 已经合法（否则 logic/test 会引用不存在的 pb 类型）。
// 当 daoPreExisted=true 且未指定 --overwrite 时，与原逻辑一致整体跳过。
func generateSchemaPhaseC(g *GenContext, outputDir string, schema *load.Schema, daoPreExisted bool) error {
	if daoPreExisted && !g.Overwrite {
		fmt.Printf("  ⊘ DAO already existed for %s, skipping scaffold generation (errcode/model/consts/test) — use --overwrite to force\n", schema.Name)
		return nil
	}

	// 5. Generate errcode module file
	if err := genModuleErrcode(g, outputDir, "", schema); err != nil {
		return err
	}

	// 6. Generate test skeleton (in logic/{group}/{model_lower}/ dir)
	if err := genTestSkeleton(g, outputDir, "", schema); err != nil {
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

	// Build delete method (only soft delete).
	// Primary-key Go type is derived from the ent schema (single source of truth).
	idGoType := idFieldGoType(schema)
	var deleteMethod string
	if hasSoftDelete {
		deleteMethod = fmt.Sprintf("\tDeleteByID(ctx context.Context, id %s) error\n", idGoType)
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

	GetByID(ctx context.Context, id %s) (*ent.%s, error)
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
		// GetByID(ctx context.Context, id %s) (*ent.%s, error)
		// 第一个 verb 是主键 Go 类型（来自 ent schema：uint64/int64/string 等），第二个是 ent 模型名。
		idGoType, modelName,
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
		// If we can't parse, generate from known CRUD methods.
		// Primary-key type still derived from ent schema (single source of truth).
		methods = defaultCRUDMethods(modelName, idFieldGoType(schema))
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
// idGoType is the primary-key Go type derived from the ent schema (single source of truth).
func defaultCRUDMethods(modelName, idGoType string) []daoMethod {
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
			Params:     fmt.Sprintf("ctx context.Context, id %s", idGoType),
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
	// hasUpdatedAt: schema is the source of truth. ent's UpdateDefault(time.Now) hook only
	// runs on OpUpdate path; OnConflictColumns().Update(...) is OpCreate context, so
	// updated_at is NOT auto-refreshed in the conflict branch. We must (and only must)
	// emit `SetUpdatedAt(now)` when the schema actually has this field.
	hasUpdatedAt := hasUpdatedAtField(schema)

	// Build field setter lines for Create and Update (excluding base fields + deleted_at).
	// Immutable() fields go into createSetters only; updateSetters skips them so that
	//身份字段（业务唯一键 / 创建人 / 创建来源 等被显式 Immutable 标记的字段）不会被 Update 覆盖。
	var createSetters, updateSetters strings.Builder
	for _, f := range schema.Fields {
		if isBaseField(f.Name) {
			continue
		}
		camel := entFieldName(fieldMap, f.Name)
		// JSON fields (e.g. field.JSON("x", []string{})) do NOT have SetNillable in ent.
		// They only have Set/Append/Clear. Use Set even if Optional/Nillable.
		isJSON := f.Info != nil && f.Info.Type == field.TypeJSON
		var setterLine string
		if (f.Optional || f.Nillable) && !isJSON {
			if f.Nillable {
				setterLine = fmt.Sprintf("\t\tSetNillable%s(data.%s).\n", camel, camel)
			} else {
				setterLine = fmt.Sprintf("\t\tSetNillable%s(&data.%s).\n", camel, camel)
			}
		} else {
			setterLine = fmt.Sprintf("\t\tSet%s(data.%s).\n", camel, camel)
		}
		createSetters.WriteString(setterLine)
		if !f.Immutable {
			updateSetters.WriteString(setterLine)
		}
	}

	// Determine the conflict columns (composite unique index first, fallback to first single unique field)
	// for ent OnConflictColumns. Only meaningful when soft-delete is on.
	var conflictColCamels []string
	if hasSoftDelete && len(compositeIndexes) > 0 {
		for _, f := range compositeIndexes[0].Fields {
			conflictColCamels = append(conflictColCamels, entFieldName(fieldMap, f.Name))
		}
	} else if hasSoftDelete && len(uniqueFields) > 0 {
		conflictColCamels = append(conflictColCamels, entFieldName(fieldMap, uniqueFields[0].Name))
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

	// Create — ent OnConflictColumns for soft-delete tables (1 IO, idempotent),
	// plain ent Create for tables without soft-delete or without unique key.
	if hasSoftDelete && len(conflictColCamels) > 0 {
		// Build the OnConflictColumns argument list, e.g.:
		//     iamapi.FieldAppCode,
		//     iamapi.FieldProtocol,
		var conflictColLines strings.Builder
		for _, c := range conflictColCamels {
			fmt.Fprintf(&conflictColLines, "\t\t\t%s.Field%s,\n", entPkg, c)
		}

		fmt.Fprintf(&b, "// Create 创建/恢复单条记录（幂等，1 次 IO）。\n")
		fmt.Fprintf(&b, "//\n")
		fmt.Fprintf(&b, "// IO 路径（恒 1 次，由 dialect 翻译为 UPSERT）：\n")
		fmt.Fprintf(&b, "//\n")
		fmt.Fprintf(&b, "//\tINSERT INTO %s(...) VALUES (...)\n", generator.FileSnake(schema.Name))
		if hasUpdatedAt {
			fmt.Fprintf(&b, "//\tON DUPLICATE KEY UPDATE deleted_at=NULL, updated_at=NOW()\n")
		} else {
			fmt.Fprintf(&b, "//\tON DUPLICATE KEY UPDATE deleted_at=NULL\n")
		}
		fmt.Fprintf(&b, "//\n")
		fmt.Fprintf(&b, "// 语义：\n")
		fmt.Fprintf(&b, "//   - 新建：直接 INSERT。\n")
		fmt.Fprintf(&b, "//   - 唯一键冲突且已软删（deleted_at IS NOT NULL）：恢复记录为活跃。\n")
		if hasUpdatedAt {
			fmt.Fprintf(&b, "//   - 唯一键冲突且活跃：deleted_at 本就 NULL，ClearDeletedAt 为 no-op；仅刷新 updated_at（幂等）。\n")
		} else {
			fmt.Fprintf(&b, "//   - 唯一键冲突且活跃：deleted_at 本就 NULL，ClearDeletedAt 为 no-op，整次操作幂等。\n")
		}
		fmt.Fprintf(&b, "//\n")
		fmt.Fprintf(&b, "// 走 ent OpCreate hook（cachex 据 dirty fields 失效缓存）；ID 由 ent 内部\n")
		fmt.Fprintf(&b, "// 通过 LAST_INSERT_ID(id) 技巧回填，无需二次查询。\n")
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) Create(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
		// `now` only when we will explicitly call SetUpdatedAt(now) below.
		if hasUpdatedAt {
			fmt.Fprintf(&b, "\tnow := time.Now()\n")
		}
		fmt.Fprintf(&b, "\tid, err := d.cli.Create().\n%s", createSetters.String())
		fmt.Fprintf(&b, "\t\tOnConflictColumns(\n%s\t\t).\n", conflictColLines.String())
		fmt.Fprintf(&b, "\t\tUpdate(func(u *ent.%sUpsert) {\n", modelName)
		if hasUpdatedAt {
			fmt.Fprintf(&b, "\t\t\tu.ClearDeletedAt().SetUpdatedAt(now)\n")
		} else {
			fmt.Fprintf(&b, "\t\t\tu.ClearDeletedAt()\n")
		}
		fmt.Fprintf(&b, "\t\t}).\n")
		fmt.Fprintf(&b, "\t\tID(ctx)\n")
		fmt.Fprintf(&b, "\tif err != nil {\n")
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.Create failed\", ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBInsertFailed, \"%s.Create: %%v\", err)\n", modelName)
		fmt.Fprintf(&b, "\t}\n")
		fmt.Fprintf(&b, "\tdata.ID = id\n")
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.Create ok\", ctxutil.IDField(id))\n", modelName)
		fmt.Fprintf(&b, "\treturn data, nil\n}\n\n")
	} else {
		// Plain create (no soft delete or no unique key)
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) Create(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Create().\n%s\t\tSave(ctx)\n", createSetters.String())
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.Create failed\", ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBInsertFailed, \"%s.Create: %%v\", err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.Create ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName)
	}

	// GetByID — with soft-delete filter.
	// Primary-key Go type derived from ent schema (single source of truth).
	idGoType := idFieldGoType(schema)
	fmt.Fprintf(&b, "// ──── Get single record ────\n\n")
	if hasSoftDelete {
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) GetByID(ctx context.Context, id %s) (*ent.%s, error) {\n", entPkg, idGoType, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Query().Where(%s.ID(id), %s.DeletedAtIsNil()).Only(ctx)\n", entPkg, entPkg)
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(errcode.%sNotFound, \"%s not found: id=%%d\", id)\n\t\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetByID failed\", ctxutil.IDField(id), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Wrapf(errcode.DBQueryFailed, \"%s.GetByID id=%%d: %%v\", id, err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.GetByID ok\", ctxutil.IDField(id))\n\treturn result, nil\n}\n\n", modelName)
	} else {
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) GetByID(ctx context.Context, id %s) (*ent.%s, error) {\n", entPkg, idGoType, modelName)
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

	// Soft delete — with DeletedAtIsNil filter.
	// Primary-key Go type already resolved above as idGoType.
	if hasSoftDelete {
		fmt.Fprintf(&b, "// ──── Delete (soft) ────\n\n")
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) DeleteByID(ctx context.Context, id %s) error {\n", entPkg, idGoType)
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

// hasField checks whether the schema declares a field with the given snake_case name.
// Used to gate generation of code paths that depend on a specific base/special field
// (e.g. deleted_at for soft-delete, updated_at for explicit refresh in upsert closure,
// created_at for the timestamp column in proto Info messages).
func hasField(schema *load.Schema, name string) bool {
	for _, f := range schema.Fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// hasDeletedAtField checks if the schema has a "deleted_at" field (for soft delete).
func hasDeletedAtField(schema *load.Schema) bool {
	return hasField(schema, "deleted_at")
}

// hasUpdatedAtField checks if the schema has an "updated_at" field. ent's UpdateDefault
// hook only fires on the OpUpdate path; OnConflictColumns().Update() runs OpCreate hooks
// and therefore does NOT auto-refresh updated_at, so the upsert closure must set it explicitly
// when (and only when) the schema actually has this field.
func hasUpdatedAtField(schema *load.Schema) bool {
	return hasField(schema, "updated_at")
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

// idFieldGoType returns the Go type of the schema's primary-key ("id") field,
// **directly derived from the ent schema** as the single source of truth.
//
// Resolution rules:
//  1. If the schema explicitly declares an `id` field (e.g. `field.Uint64("id")`),
//     return its real Go type via mapEntFieldTypeToGo (uint64 / int64 / uint32 / ...).
//  2. Otherwise fall back to "int", which matches ent's implicit default primary key.
//
// Used by every dao template that references the primary-key parameter type
// (GetByID / DeleteByID, etc.) so codegen never drifts from the ent schema.
func idFieldGoType(schema *load.Schema) string {
	for _, f := range schema.Fields {
		if f.Name == "id" {
			return mapEntFieldTypeToGo(uniqueFieldInfo{Name: f.Name, TypeName: f.Info.Type.String()})
		}
	}
	return "int"
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

// runMergeAndProtoc 仅执行 merge desc → 根 proto + protoc 编译两步。
// 失败时返回 error，调用方负责 rollback。这是"protoc 预校验"的核心：
// 任何 desc proto 之间的 message/service 名冲突会在这里立即暴露。
//
// 返回额外的 rootProto 路径，便于调用方登记到 tracker（失败时删掉）。
func runMergeAndProtoc(abs, style string) (rootProto string, err error) {
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
	rootProto = filepath.Join(abs, serviceName+".proto")
	if mErr := generator.MergeDescProtos(descDir, rootProto, serviceName); mErr != nil {
		return rootProto, fmt.Errorf("merge proto: %w", mErr)
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
	if rErr := cmd.Run(); rErr != nil {
		return rootProto, fmt.Errorf("protoc: %w", rErr)
	}

	return rootProto, nil
}

// runPostProtoc 在 protoc 成功后执行 enum 提取与 logic 文件生成。
// 这一阶段只读 protoc 产物 + 写 logic 文件，不会再触发 protoc 错误。
func runPostProtoc(abs, style string) error {
	_ = generator.GenEnumsFromProto(abs)

	if style == "" {
		style = "go_zero"
	}
	if err := generator.GenLogicFiles(abs, style, false); err != nil {
		return fmt.Errorf("gen logic: %w", err)
	}
	return nil
}

// autoGenRpc 兼容旧调用入口：merge + protoc + post。
// 推荐新代码使用 runMergeAndProtoc + runPostProtoc 两段式调用，便于做 protoc 预校验。
func autoGenRpc(abs, style string) error {
	if _, err := runMergeAndProtoc(abs, style); err != nil {
		return err
	}
	return runPostProtoc(abs, style)
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

	// Build message fields from ent schema.
	// Base/audit columns (id / created_at / updated_at) are emitted as the first 1~3 fields
	// ONLY when the schema actually declares them. ent provides the `id` primary key implicitly,
	// so id is always emitted; created_at / updated_at must be schema-driven to avoid
	// fabricating columns the underlying table does not have (e.g. an iam_user_role schema
	// that intentionally has no updated_at must NOT produce updated_at in proto Info).
	var infoFields strings.Builder
	fieldNum := 1
	infoFields.WriteString(fmt.Sprintf("  // 主键ID\n  optional uint64 id = %d;\n", fieldNum))
	fieldNum++
	if hasField(schema, "created_at") {
		infoFields.WriteString(fmt.Sprintf("  // 创建时间\n  optional int64 created_at = %d;\n", fieldNum))
		fieldNum++
	}
	if hasField(schema, "updated_at") {
		infoFields.WriteString(fmt.Sprintf("  // 更新时间\n  optional int64 updated_at = %d;\n", fieldNum))
		fieldNum++
	}

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
	// generator.GoPascal: dashed/snake names → PascalCase ident, e.g. "cs-agent-rpc" → "CsAgentRpc"
	b.WriteString(fmt.Sprintf("service %s {\n", generator.GoPascal(g.ServiceName)))
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

// ==================== File Tracker (atomic generation rollback) ====================
//
// fileTracker 用于"原子化"生成：先对若干关键目录拍快照，等 PhaseA 写盘 + protoc 校验
// 全部通过后再继续 PhaseB；若 protoc 校验失败，按 diff 把 PhaseA 新建的文件全部删除，
// 让磁盘回到 PhaseA 之前的状态，避免出现"desc/dao 新建了一半，logic/test 又孤儿留下"
// 的不一致状态。
//
// 仅追踪"新增文件"，不追踪"被覆盖的文件"——因为现有 gen 函数对已存在文件一律 skip，
// 所以新增 = diff 即可表达全部副作用。
type fileTracker struct {
	// roots 是要追踪的目录绝对路径（递归）。
	roots []string
	// before 记录每个目录在 snapshot 时刻已存在的文件集合（绝对路径）。
	before map[string]struct{}
	// extraFiles 是手动登记的额外文件（如根 .proto），不属于任何 root 但要参与回滚。
	extraFiles []string
}

func newFileTracker(roots ...string) *fileTracker {
	return &fileTracker{
		roots:  roots,
		before: make(map[string]struct{}),
	}
}

// snapshot 记录当前所有 root 目录下的文件列表，作为回滚基线。
func (t *fileTracker) snapshot() {
	for _, root := range t.roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			t.before[path] = struct{}{}
			return nil
		})
	}
}

// trackExtra 登记一个不在 root 目录范围内、但需要参与回滚的文件（典型如根 .proto）。
// 仅当 snapshot 时该文件不存在，才会在 rollback 时尝试删除。
func (t *fileTracker) trackExtra(path string) {
	if _, err := os.Stat(path); err == nil {
		// 已存在 → snapshot 之前就有 → 不属于"新增"，不删
		return
	}
	t.extraFiles = append(t.extraFiles, path)
}

// rollback 删除 snapshot 之后新增的所有文件，并清理产生的空目录。
func (t *fileTracker) rollback() {
	var deleted []string

	// 1. 删除 root 目录下的新增文件
	for _, root := range t.roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if _, existed := t.before[path]; !existed {
				if rmErr := os.Remove(path); rmErr == nil {
					deleted = append(deleted, path)
				}
			}
			return nil
		})
	}

	// 2. 删除手动登记的 extra 文件
	for _, p := range t.extraFiles {
		if _, err := os.Stat(p); err == nil {
			if rmErr := os.Remove(p); rmErr == nil {
				deleted = append(deleted, p)
			}
		}
	}

	// 3. 清理由此产生的空目录（自底向上，最多三层，避免误删）
	dirSet := make(map[string]struct{})
	for _, p := range deleted {
		d := filepath.Dir(p)
		dirSet[d] = struct{}{}
		dirSet[filepath.Dir(d)] = struct{}{}
	}
	// 对路径长度逆序排序，先删深层目录
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	// 简单冒泡：长度长的排前面
	for i := 0; i < len(dirs); i++ {
		for j := i + 1; j < len(dirs); j++ {
			if len(dirs[j]) > len(dirs[i]) {
				dirs[i], dirs[j] = dirs[j], dirs[i]
			}
		}
	}
	for _, d := range dirs {
		// 仅当目录为空时才删
		entries, err := os.ReadDir(d)
		if err == nil && len(entries) == 0 {
			_ = os.Remove(d)
		}
	}

	if len(deleted) > 0 {
		fmt.Printf("[zctl] Rolled back %d file(s) generated in this run.\n", len(deleted))
	}
}
