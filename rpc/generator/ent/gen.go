package ent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/qqz14/zctl/rpc/generator"
	"github.com/qqz14/zctl/util/ctx"
	"github.com/qqz14/zctl/util/name"
	"github.com/qqz14/zctl/util/format"
	"github.com/qqz14/zctl/util/pathx"

	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
	"entgo.io/ent/entc/load"
	"entgo.io/ent/schema/field"
)

// enableLegacyLogicGen 控制 zctl rpc ent 是否在 PhaseB 之后继续执行
// 「protoc 预校验 + PhaseC（errcode/test/model/consts）+ enum + logic/server」。
//
// 默认 false：zctl rpc ent 只负责 dao + EntInfra + desc proto，后续合并 / protoc / logic
// 一律由用户调用 `make gen-rpc`（即 `zctl rpc merge-proto` + `zctl rpc protoc`）完成，
// 与 make gen-rpc 完全同源，不再自己拼 protoc 命令。
//
// 旧路径代码保留在 GenEntLogic 末尾以备回查，需要复用时把这里改成 true 并重编译 zctl 即可。
const enableLegacyLogicGen = false

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

// GenEntLogic generates DAO + desc proto for one or all ent schemas.
//
// 默认职责（对齐用户工作流）：
//   PhaseA  →  写 dao/impl/mock/hook         （独立落盘，失败直接退出）
//   EntInfra → 写 entlog/entx + patch ServiceContext（紧贴 PhaseA，让 dao 阶段产物自洽可编译）
//   PhaseB  →  写 desc proto                  （已存在或 dao 已存在则跳过；空目录/空文件自动剪枝）
//   ── 到此为止。后续 merge desc → 根 .proto → protoc → logic/server 由用户跑 `make gen-rpc`
//      （即 `zctl rpc merge-proto` + `zctl rpc protoc`）完成，确保与 `make gen-rpc` 完全同源。
//
// 旧路径（包内常量 enableLegacyLogicGen，默认 false）：
//   PhaseB 之后追加 protoc 预校验 + PhaseC（errcode/test/model/consts）+ enum + logic/server。
//   需要时改 enableLegacyLogicGen=true 并重编译 zctl；不暴露 CLI flag，避免命令面增加。
//   失败时仅回滚 PhaseB 新增 desc proto / 根 .proto / types/ pb，**dao 与 EntInfra 永不回滚**。
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
			genCtx.GroupName = name.DirName(s.Name)
			genCtx.ModelName = s.Name
		}
		if genCtx.GroupName == "" {
			genCtx.GroupName = name.DirName(s.Name)
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

	// ── EntInfra（紧贴 PhaseA）：写 entlog/entx + patch ServiceContext ──
	// 与 desc/types/protoc 零依赖；DAO 一旦落盘就把基础设施补齐，保证 dao 阶段产物自洽可编译，
	// 方便用户在 PhaseB 还没写 desc 时也能直接 `go build` 验证 dao。
	if err := generator.EnsureEntInfra(outputDir, projectCtx.Path); err != nil {
		fmt.Printf("[zctl] Warning: failed to ensure ent infra: %v\n", err)
	} else {
		fmt.Println("[zctl] ent infra ready (entlog/entx + ServiceContext patched).")
	}

	// ── PhaseB: 生成 desc proto（两阶段提交：plan → prune → commit） ──
	// tracker 仅追踪 desc/ 与 types/，dao 保留不动。根 .proto 单独通过 rootProtoExistedBefore 处理。
	tracker := newFileTracker(
		filepath.Join(outputDir, "desc"),
		filepath.Join(outputDir, "types"),
	)
	tracker.snapshot()

	rootProtoCandidate := filepath.Join(outputDir, deriveServiceFileBase(outputDir)+".proto")
	rootProtoExistedBefore := pathx.FileExists(rootProtoCandidate)

	// ── PhaseB-Plan: 把"本次该新增什么"全算到内存里 ──
	// 与 zctl-commands.md §4 "DAO 是只生成一次的判断依据" 严格对齐：
	//   1) 目标 desc/<group>/<model>.proto 已存在 OR DAO 已存在 → 整个 schema 视为 PhaseB 已完成，跳过；
	//      不动用户已有 proto 一个字节（无论里面是 CRUD 还是用户自定义 rpc），不在别处补差量。
	//   2) 目标不存在 且 DAO 是本次新建 → 渲染该 schema 的全集 message/rpc，单文件落到
	//      desc/<group>/<model>.proto，与 make gen-rpc 全新生成时的产物完全一致。
	//   3) commit 阶段会把空 file / 空 dir 自动剪枝（多 schema 部分跳过部分新建场景）。
	plan, err := newDescPlan(g, outputDir)
	if err != nil {
		return err
	}
	for i := range processed {
		p := &processed[i]
		plan.addSchema(&p.genCtx, p.schema, p.genCtx.GroupName, p.daoPreExisted)
	}

	// ── PhaseB-Commit: 落盘 plan，空目录/空文件自动剪枝 ──
	if err := plan.commit(tracker); err != nil {
		tracker.rollback()
		return err
	}

	for _, p := range processed {
		fmt.Printf("[zctl] Generated module: %s\n", p.schema.Name)
	}

	// ── 生成统一的 DAO bizcode 文件：pkg/bizcode/dao.go ──
	// 与 PhaseA 强耦合（dao 实现引用 bizcode.<Model>NotFound / CreateFailed / UpdateFailed / DeleteFailed），
	// 因此必须在新工作流里也跑，否则 `go build` 直接挂；与 protoc/PhaseC 零依赖，前置即可。
	{
		var allSchemaNames []string
		for _, s := range schemas.Schemas {
			allSchemaNames = append(allSchemaNames, s.Name)
		}
		if err := genDaoErrcodeAll(outputDir, allSchemaNames); err != nil {
			fmt.Printf("[zctl] Warning: failed to generate dao bizcode: %v\n", err)
		} else {
			fmt.Printf("[zctl] Generated pkg/bizcode/dao.go (%d modules)\n", len(allSchemaNames))
		}
	}

	// 默认到这里就结束：用户自己跑 `make gen-rpc` 完成 merge + protoc + logic/server。
	if !enableLegacyLogicGen {
		fmt.Println("[zctl] Done. Run `make gen-rpc` to generate logic/server from desc/.")
		return nil
	}

	// ──────────────────────────────────────────────────────────────────
	// 以下为旧路径（默认关闭，enableLegacyLogicGen=true 时启用）：
	//   protoc 预校验 + PhaseC + 尾部 enum/logic
	// 保留代码以便回查与必要时复用，正式工作流请直接走 `make gen-rpc`。
	// ──────────────────────────────────────────────────────────────────

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
	}

	// 注意：DAO 统一 bizcode 文件已在 PhaseB 之后、新/旧流程分叉处生成，此处不再重复。

	// ── 尾部：enum + logic 文件（protoc 已成功，几乎不会失败） ──
	if err := runPostProtoc(outputDir, g.Style); err != nil {
		fmt.Printf("[zctl] Warning: post-protoc generation failed: %v\n", err)
		fmt.Println("[zctl] You can manually run: make gen-rpc")
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
	modelSnake := name.FileSnake(schema.Name)

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
	filename := name.FileSnake(schema.Name) + "_dao"
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
	modelSnake := name.FileSnake(modelName)
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
	filename := name.FileSnake(schema.Name) + "_oceanbase"
	filePath := filepath.Join(dir, filename+".go")
	if pathx.FileExists(filePath) && !g.Overwrite {
		return nil
	}

	modelName := schema.Name
	entPkg := name.EntPkg(schema.Name)
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
	fmt.Fprintf(&b, "\t\"%s/pkg/bizcode\"\n", modulePath)
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
		fmt.Fprintf(&b, "//\tINSERT INTO %s(...) VALUES (...)\n", name.FileSnake(schema.Name))
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
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Newf(bizcode.DBInsertFailed, \"%s.Create: %%v\", err)\n", modelName)
		fmt.Fprintf(&b, "\t}\n")
		fmt.Fprintf(&b, "\tdata.ID = id\n")
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.Create ok\", ctxutil.IDField(id))\n", modelName)
		fmt.Fprintf(&b, "\treturn data, nil\n}\n\n")
	} else {
		// Plain create (no soft delete or no unique key)
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) Create(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Create().\n%s\t\tSave(ctx)\n", createSetters.String())
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.Create failed\", ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Newf(bizcode.DBInsertFailed, \"%s.Create: %%v\", err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.Create ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName)
	}

	// GetByID — with soft-delete filter.
	// Primary-key Go type derived from ent schema (single source of truth).
	idGoType := idFieldGoType(schema)
	fmt.Fprintf(&b, "// ──── Get single record ────\n\n")
	if hasSoftDelete {
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) GetByID(ctx context.Context, id %s) (*ent.%s, error) {\n", entPkg, idGoType, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Query().Where(%s.ID(id), %s.DeletedAtIsNil()).Only(ctx)\n", entPkg, entPkg)
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(bizcode.%sNotFound, \"%s not found: id=%%d\", id)\n\t\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetByID failed\", ctxutil.IDField(id), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Newf(bizcode.DBQueryFailed, \"%s.GetByID id=%%d: %%v\", id, err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.GetByID ok\", ctxutil.IDField(id))\n\treturn result, nil\n}\n\n", modelName)
	} else {
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) GetByID(ctx context.Context, id %s) (*ent.%s, error) {\n", entPkg, idGoType, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Get(ctx, id)\n")
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(bizcode.%sNotFound, \"%s not found: id=%%d\", id)\n\t\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetByID failed\", ctxutil.IDField(id), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Newf(bizcode.DBQueryFailed, \"%s.GetByID id=%%d: %%v\", id, err)\n\t}\n", modelName)
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
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(bizcode.%sNotFound, \"%s not found: %s=%%v\", %s)\n\t\t}\n", modelName, modelName, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetBy%s failed\", ctxutil.ErrField(err))\n", modelName, camel)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Newf(bizcode.DBQueryFailed, \"%s.GetBy%s %s=%%v: %%v\", %s, err)\n\t}\n", modelName, camel, uf.Name, uf.Name)
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
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(bizcode.%sNotFound, \"%s not found by composite key\")\n\t\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.GetBy%s failed\", ctxutil.ErrField(err))\n", modelName, methodName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Newf(bizcode.DBQueryFailed, \"%s.GetBy%s: %%v\", err)\n\t}\n", modelName, methodName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.GetBy%s ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName, methodName)
	}

	// UpdateByID — with soft-delete filter
	fmt.Fprintf(&b, "// ──── Update single record ────\n\n")
	if hasSoftDelete {
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) UpdateByID(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
		fmt.Fprintf(&b, "\taffected, err := d.cli.Update().\n\t\tWhere(%s.ID(data.ID), %s.DeletedAtIsNil()).\n%s\t\tSave(ctx)\n", entPkg, entPkg, updateSetters.String())
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateByID failed\", ctxutil.IDField(data.ID), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Newf(bizcode.DBUpdateFailed, \"%s.UpdateByID: %%v\", err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tif affected == 0 {\n\t\treturn nil, errcode.Newf(bizcode.%sNotFound, \"%s not found: id=%%d\", data.ID)\n\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.Get(ctx, data.ID)\n")
		fmt.Fprintf(&b, "\tif err != nil {\n\t\treturn nil, errcode.Newf(bizcode.DBQueryFailed, \"%s.UpdateByID refetch: %%v\", err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.UpdateByID ok\", ctxutil.IDField(data.ID))\n\treturn result, nil\n}\n\n", modelName)
	} else {
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) UpdateByID(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n", entPkg, modelName, modelName)
		fmt.Fprintf(&b, "\tresult, err := d.cli.UpdateOneID(data.ID).\n%s\t\tSave(ctx)\n", updateSetters.String())
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(bizcode.%sNotFound, \"%s not found: id=%%d\", data.ID)\n\t\t}\n", modelName, modelName)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateByID failed\", ctxutil.IDField(data.ID), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Newf(bizcode.DBUpdateFailed, \"%s.UpdateByID: %%v\", err)\n\t}\n", modelName)
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
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tif ent.IsNotFound(err) {\n\t\t\treturn nil, errcode.Newf(bizcode.%sNotFound, \"%s not found: %s=%%v\", %s)\n\t\t}\n", modelName, modelName, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateBy%s query failed\", ctxutil.ErrField(err))\n", modelName, camel)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Newf(bizcode.DBQueryFailed, \"%s.UpdateBy%s query %s=%%v: %%v\", %s, err)\n\t}\n", modelName, camel, uf.Name, uf.Name)
		fmt.Fprintf(&b, "\tresult, err := d.cli.UpdateOneID(existing.ID).\n%s\t\tSave(ctx)\n", updateSetters.String())
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.UpdateBy%s failed\", ctxutil.ErrField(err))\n", modelName, camel)
		fmt.Fprintf(&b, "\t\treturn nil, errcode.Newf(bizcode.DBUpdateFailed, \"%s.UpdateBy%s: %%v\", err)\n\t}\n", modelName, camel)
		fmt.Fprintf(&b, "\tctxutil.L(ctx).Debugw(\"dao.%s.UpdateBy%s ok\", ctxutil.IDField(result.ID))\n\treturn result, nil\n}\n\n", modelName, camel)
	}

	// Soft delete — with DeletedAtIsNil filter.
	// Primary-key Go type already resolved above as idGoType.
	if hasSoftDelete {
		fmt.Fprintf(&b, "// ──── Delete (soft) ────\n\n")
		fmt.Fprintf(&b, "func (d *%sOceanBaseDao) DeleteByID(ctx context.Context, id %s) error {\n", entPkg, idGoType)
		fmt.Fprintf(&b, "\taffected, err := d.cli.Update().\n\t\tWhere(%s.ID(id), %s.DeletedAtIsNil()).\n\t\tSetDeletedAt(time.Now()).\n\t\tSave(ctx)\n", entPkg, entPkg)
		fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.DeleteByID failed\", ctxutil.IDField(id), ctxutil.ErrField(err))\n", modelName)
		fmt.Fprintf(&b, "\t\treturn errcode.Newf(bizcode.DBDeleteFailed, \"%s.DeleteByID id=%%d: %%v\", id, err)\n\t}\n", modelName)
		fmt.Fprintf(&b, "\tif affected == 0 {\n\t\treturn errcode.Newf(bizcode.%sNotFound, \"%s not found: id=%%d\", id)\n\t}\n", modelName, modelName)
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
	fmt.Fprintf(&b, "\t\treturn nil, 0, errcode.Newf(bizcode.DBQueryFailed, \"%s.List count: %%v\", err)\n\t}\n", modelName)
	fmt.Fprintf(&b, "\tif total == 0 {\n\t\treturn nil, 0, nil\n\t}\n\n")
	fmt.Fprintf(&b, "\t// Apply pagination only when page is not nil.\n")
	fmt.Fprintf(&b, "\tif page != nil {\n\t\tquery = query.\n\t\t\tOffset((page.Page - 1) * page.PageSize).\n\t\t\tLimit(page.PageSize)\n\t}\n\n")
	fmt.Fprintf(&b, "\tlist, err := query.\n\t\tOrder(%s.ByID()).\n\t\tAll(ctx)\n", entPkg)
	fmt.Fprintf(&b, "\tif err != nil {\n\t\tctxutil.L(ctx).Errorw(\"dao.%s.List query failed\", ctxutil.ErrField(err))\n", modelName)
	fmt.Fprintf(&b, "\t\treturn nil, 0, errcode.Newf(bizcode.DBQueryFailed, \"%s.List: %%v\", err)\n\t}\n", modelName)
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
	modelSnake := name.FileSnake(modelName)
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

// genDaoErrcodeAll generates a single pkg/bizcode/dao.go containing error codes
// for ALL ent schemas, with each module assigned a unique 100-code segment.
// It also auto-appends empty i18n entries to locale JSON files.
//
// Segment layout (base = 11000):
//
//	Module 0: 11100 ~ 11199
//	Module 1: 11200 ~ 11299
//	...
//
// IMPORTANT: existing module → segment assignments are NEVER changed.
// New modules are appended after the current highest segment.
// This prevents re-ordering of schemaNames from stealing already-assigned codes.
func genDaoErrcodeAll(outputDir string, schemaNames []string) error {
	dir := filepath.Join(outputDir, "pkg", "bizcode")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	const segmentBase = 11000
	const segmentSize = 100

	// ── Step 1: 读取已有 dao.go，解析 模块名 → base 的已分配映射 ──
	// 格式示例：// ──── IamUser: 11900 ~ 11999 ────
	existingBase := make(map[string]int) // moduleName → base
	filePath := filepath.Join(dir, "dao.go")
	if existing, err := os.ReadFile(filePath); err == nil {
		// 逐行扫描注释行，提取已分配号段
		segmentHeaderRe := regexp.MustCompile(`//\s*────\s+(\w+):\s+(\d+)\s*~`)
		for _, line := range strings.Split(string(existing), "\n") {
			if m := segmentHeaderRe.FindStringSubmatch(line); m != nil {
				base, _ := strconv.Atoi(m[2])
				existingBase[m[1]] = base
			}
		}
	}

	// ── Step 2: 计算当前已分配的最大 base，新模块从此往后累加 ──
	maxBase := segmentBase
	for _, base := range existingBase {
		if base > maxBase {
			maxBase = base
		}
	}

	// ── Step 3: 为每个 schema 确定 base（已有保持不变，新增往后累加）──
	// 保持输出顺序：先按已有 base 排序的旧模块，再按首次出现顺序的新模块
	type moduleEntry struct {
		name string
		base int
	}
	var entries []moduleEntry
	for _, name := range schemaNames {
		if base, ok := existingBase[name]; ok {
			entries = append(entries, moduleEntry{name, base})
		} else {
			// 新模块：分配下一个号段
			maxBase += segmentSize
			entries = append(entries, moduleEntry{name, maxBase})
			fmt.Printf("[zctl] Assigned new bizcode segment %d~%d to module %s\n",
				maxBase, maxBase+segmentSize-1, name)
		}
	}

	// 按 base 升序排列，保证文件内容稳定有序
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].base < entries[j].base
	})

	// ── Step 4: 生成文件内容 ──
	var b strings.Builder
	b.WriteString("package bizcode\n\n")
	b.WriteString("// Code generated by zctl. DO NOT EDIT.\n")
	b.WriteString("// DAO module error codes — each module gets a 100-code segment.\n")
	b.WriteString("// Messages come from i18n (pkg/i18n/locale/{lang}.json → key \"bizcode.{code}\").\n\n")

	// Collect all error code → empty string for i18n
	var i18nCodes []int

	for _, e := range entries {
		base := e.base
		notFound := base + 1
		createFailed := base + 2
		updateFailed := base + 3
		deleteFailed := base + 4

		b.WriteString(fmt.Sprintf("// ──── %s: %d ~ %d ────\n", e.name, base, base+segmentSize-1))
		b.WriteString("const (\n")
		b.WriteString(fmt.Sprintf("\t%sNotFound     = %d\n", e.name, notFound))
		b.WriteString(fmt.Sprintf("\t%sCreateFailed = %d\n", e.name, createFailed))
		b.WriteString(fmt.Sprintf("\t%sUpdateFailed = %d\n", e.name, updateFailed))
		b.WriteString(fmt.Sprintf("\t%sDeleteFailed = %d\n", e.name, deleteFailed))
		b.WriteString(")\n\n")

		i18nCodes = append(i18nCodes, notFound, createFailed, updateFailed, deleteFailed)
	}

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

// mergeI18nCodes reads a locale JSON file, adds missing bizcode keys with empty
// values, and writes it back with stable formatting.
func mergeI18nCodes(filePath string, codes []int) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	// Simple JSON parse: we expect {"bizcode": {"key": "val", ...}, ...}
	// Use encoding/json for safety
	var root map[string]map[string]string
	if err := json.Unmarshal(data, &root); err != nil {
		return nil // malformed JSON, skip
	}

	errSection, ok := root["bizcode"]
	if !ok {
		errSection = make(map[string]string)
		root["bizcode"] = errSection
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

	filename := name.FileSnake(schema.Name)
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

	filename := name.FileSnake(schema.Name)
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
	modelDir := name.DirName(schema.Name)
	// Test file goes to logic/{group}/{model}/ (same directory as logic files generated by gen-rpc)
	testDir := filepath.Join(outputDir, "internal", "logic", g.GroupName, modelDir)
	if err := pathx.MkdirIfNotExist(testDir); err != nil {
		return err
	}

	modelName := schema.Name
	modelLower := strings.ToLower(modelName)
	pkgName := name.PkgName(modelName) // package name matches ent convention: all lowercase

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
	return name.GoPascal(s)
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
	return name.GoPascal(snakeName)
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
// genDescProto 是兼容入口（generateForSchema 单 schema 路径仍在用）。
// 内部走与主流程一致的 plan 模式：算差集 → 空剪枝 → 落盘。
// 这样维护单一渲染逻辑（renderProtoFile），不再保留独立的字符串拼接代码。
//
// 行为契约（保持向后兼容）：
//   - 已存在的目标文件 + 未指定 --overwrite → 不动用户文件，返回 nil。
//   - 当差集为空时不创建空目录（与旧实现"已存在则跳过"语义一致）。
//   - 这里不接 fileTracker：兼容入口本就没有 protoc 预校验，由调用方自己负责。
func genDescProto(g *GenContext, outputDir string, schema *load.Schema) error {
	plan, err := newDescPlan(g, outputDir)
	if err != nil {
		return err
	}
	// 兼容入口默认 daoPreExisted=false：addSchema 内部仍会用"目标文件已存在"做幂等保护。
	plan.addSchema(g, schema, g.GroupName, false)
	return plan.commit(nil)
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

// ==================== Desc Plan (two-phase commit + prune) ====================
//
// ==================== Desc Plan (single-file write OR full skip) ====================
//
// 设计要点（v2，与 zctl-commands.md §4 PhaseB 覆盖契约严格对齐）：
//
//  1) 三阶段串行 + 幂等：
//     PhaseA(DAO) → PhaseB(desc proto) → PhaseC(errcode/test/model/consts/protoc/logic)。
//     任一阶段都遵循"已有则跳过、缺失才创建"的硬规则。
//
//  2) PhaseB 跳过判定（只看一个事实）：
//        目标文件 desc/<group>/<model>.proto 已存在 OR DAO 文件已存在 → 整个 schema 视为完成。
//     不再做任何"差集补写"——避免在 desc/ 下凭空多出 _gen.proto / 其它 group 镜像文件。
//
//  3) PhaseB 创建路径：仅当上述判定不命中时，才把该 schema 的全集 message/rpc 写到
//     单文件 desc/<group>/<model>.proto，与 make gen-rpc 全新生成时的预期完全一致。
//
//  4) commit 阶段做空剪枝（多 schema 跑、其中部分跳过部分新建时不会留空目录）。
//
//  5) 仅本次实际写出的文件才进 fileTracker，protoc 校验失败时回滚干净，不动用户已有 proto。

type descPlan struct {
	descRoot string
	g        *GenContext // 公用 ServiceName / Style 等只读字段
	dirs     map[string]*dirPlan
	// schemaTargetGroup 记录"本次为这个 schema 选定的写盘 group"，便于 PhaseC 把
	// logic / errcode / model / consts 落到一致的位置。
	schemaTargetGroup map[string]string

	// ── 扫描态：newDescPlan 时全量扫 desc/，给 addSchema 用作 rpc 名过滤与 message 一致性判定。
	// existingRPCsByService: 服务名 → 该服务下已存在的 rpc method 名集合。
	existingRPCsByService map[string]map[string]bool
	// existingMessages: message 名 → 该 message 在 desc/ 里的定义信息（路径 + 字段签名）。
	// 跨多个 .proto 同名 message 只保留首遇定义（重复定义本就是 protoc 非法状态）。
	existingMessages map[string]existingMessageInfo
}

// existingMessageInfo 描述磁盘上 desc/ 里已定义的某个 message。
// relPath 为相对 desc/ 的路径（如 "user/cs_user_profile.proto"），fields 为按 tag 升序排列的字段签名。
type existingMessageInfo struct {
	relPath string
	fields  []protoFieldSig
}

// protoFieldSig 是字段签名比对单元。比对维度严格为：
//   - repeated / optional 修饰符（map 化为 modifier 字段）
//   - 字段名 name
//   - 字段类型 typeName（含完整自定义类型名，如 "CsUserProfileInfo"）
//   - 字段编号 tag
//
// 不比对：注释 / 字段顺序（外层会按 tag 升序）/ proto option（如 [json_name=...]）。
type protoFieldSig struct {
	modifier string // "" | "optional" | "repeated"
	typeName string
	name     string
	tag      int
}

type dirPlan struct {
	absDir string                     // desc/<group>
	files  map[string]*protoFilePlan // 文件名 → 文件计划
}

// protoFilePlan 描述 desc/<group>/<file>.proto 一份待写 proto 的完整内容。
// 仅当该 schema 在 PhaseB 中被判定为"需要新建"时才会被挂上来。
type protoFilePlan struct {
	absPath   string
	modelName string // schema.Name，用于在校准 service 块名 / 注释时复用
	messages  []protoMessageBlock
	rpcs      []protoRPCEntry
	// imports 记录本文件需要 import 的其它 .proto 路径（相对 desc/，如 "user/cs_user_profile.proto"）。
	// 由 addSchema 在 message 一致性体检时填充，commit 阶段渲染到文件头部。
	imports []string
}

type protoMessageBlock struct {
	name    string
	comment string // 含 "// xxx\n"
	body    string // "{ ... }" 内部，含字段定义；不带 message 头 / 尾大括号
	// fields 是 body 的结构化版本，仅用于跨文件 message 一致性比对，不参与渲染。
	fields []protoFieldSig
}

type protoRPCEntry struct {
	method  string
	req     string
	resp    string
	comment string // "  // xxx\n"
	// deps 列出该 rpc 依赖的所有 message 名（含 Req / Resp / 嵌套 Info），
	// 但不包含 base.proto 提供的内置 message（如 Empty）。供 addSchema 做一致性判定。
	deps []string
}

func newDescPlan(g *GenContext, outputDir string) (*descPlan, error) {
	p := &descPlan{
		descRoot:              filepath.Join(outputDir, "desc"),
		g:                     g,
		dirs:                  make(map[string]*dirPlan),
		schemaTargetGroup:     make(map[string]string),
		existingRPCsByService: make(map[string]map[string]bool),
		existingMessages:      make(map[string]existingMessageInfo),
	}
	// 全量扫描 desc/ 下所有 .proto，构造 rpc 名集合 + message 字段签名表。
	// desc/ 不存在（首次跑）也不算错误，留两个空 map 即可。
	if err := scanDescDir(p.descRoot, p.existingRPCsByService, p.existingMessages); err != nil {
		return nil, err
	}
	return p, nil
}

// addSchema 决定 PhaseB 是否为该 schema 落盘 desc proto。
//
// 唯一判定（对齐 zctl-commands.md §4 "DAO 是只生成一次的判断依据"）：
//   - daoPreExisted=true（PhaseA 判定 DAO 是上一次跑剩下的）→ 整个 PhaseB 跳过；
//   - 目标 desc/<group>/<model>.proto 已存在 → 整个 PhaseB 跳过；
//     此时无论目标文件里是 CRUD 全集、用户自定义 rpc，还是别的什么，都一字不动。
//   - 否则 → 把该 schema 的全集 message + rpc 渲染成单文件，挂到 plan，由 commit 落盘。
//
// 跨 group 重名风险：如果用户已经在 desc/<其它 group>/<model>.proto 持有了 CRUD，
// 那么 PhaseA 几乎一定会发现 DAO 已存在（它们是同一时刻产物），daoPreExisted=true 自然跳过。
// 即便用户只手维护了 proto 而没生成 DAO（极小概率），后续 protoc 校验也会因
// "message/rpc 重名" 而报错并回滚 PhaseB —— 用户显式删除冲突 proto 后再跑即可。
func (p *descPlan) addSchema(genCtx *GenContext, schema *load.Schema, targetGroup string, daoPreExisted bool) {
	modelName := schema.Name
	modelSnake := name.FileSnake(modelName)
	fileName := modelSnake + ".proto"

	dirAbs := filepath.Join(p.descRoot, targetGroup)
	absPath := filepath.Join(dirAbs, fileName)

	// 始终登记 targetGroup，让 PhaseC 能拿到正确的 group 写 logic / errcode 等。
	p.schemaTargetGroup[modelName] = targetGroup

	// 门 1：daoPreExisted=true → DAO 是上一次跑剩下的，本次 PhaseB 不再补任何 desc。
	if daoPreExisted {
		return
	}

	// 门 2：目标 desc/<group>/<model>.proto 已存在 → 文件级覆盖原则：一字不动。
	if pathx.FileExists(absPath) {
		return
	}

	// ── 进入"算预期 → 过滤 → 体检 → 剪枝"流程 ──
	full := buildFullProtoEntries(genCtx, schema)
	if len(full.rpcs) == 0 {
		return
	}

	serviceName := name.GoPascal(genCtx.ServiceName)
	existingRPCs := p.existingRPCsByService[serviceName]

	// 索引：本 schema 全集 message → 字段签名（用于一致性比对 / 落盘剪枝）。
	expectedMsg := make(map[string]protoMessageBlock, len(full.messages))
	for _, m := range full.messages {
		expectedMsg[m.name] = m
	}

	// 步骤 A：rpc 名过滤（同 service 内已存在的 rpc 直接剔除）。
	// 步骤 B：message 一致性体检（依赖 message 中只要有一个"已存在且不一致"，本 rpc 整体跳过 + 告警）。
	// 步骤 C：剩余 rpc 的 message 划分为 "本文件新建" / "import 别处" 两路。
	keptRPCs := make([]protoRPCEntry, 0, len(full.rpcs))
	neededLocalMsg := make(map[string]bool) // 本文件需要原地定义的 message
	importPaths := make(map[string]bool)    // 需 import 的 .proto 相对路径
	for _, r := range full.rpcs {
		if existingRPCs[r.method] {
			fmt.Printf("[zctl] desc rpc already exists in service %s, skip: %s\n", serviceName, r.method)
			continue
		}

		// 该 rpc 依赖的所有 message 中，是否存在 "已存在但不一致" 的——只要有 1 个，整个 rpc 就跳过。
		mismatch := ""
		for _, dep := range r.deps {
			if isBaseProtoMessage(dep) {
				continue // base.proto 提供，跳过比对
			}
			expected, ok := expectedMsg[dep]
			if !ok {
				// 防御：buildFullProtoEntries 应已把所有依赖都登记到 messages，进不到这里。
				continue
			}
			if existing, exists := p.existingMessages[dep]; exists {
				if !messageFieldsEqual(expected.fields, existing.fields) {
					mismatch = fmt.Sprintf("%s (defined in desc/%s, fields differ from ent schema)", dep, existing.relPath)
					break
				}
			}
		}
		if mismatch != "" {
			fmt.Printf("[zctl] desc message conflict, skip rpc %s.%s: %s\n", serviceName, r.method, mismatch)
			continue
		}

		// rpc 通过体检：把它依赖的每个 message 归类。
		for _, dep := range r.deps {
			if isBaseProtoMessage(dep) {
				continue
			}
			if existing, exists := p.existingMessages[dep]; exists {
				// 已存在且一致 → import
				importPaths[existing.relPath] = true
			} else {
				// 不存在 → 本文件新建
				neededLocalMsg[dep] = true
			}
		}
		keptRPCs = append(keptRPCs, r)
	}

	if len(keptRPCs) == 0 {
		// 所有 rpc 都被过滤/跳过 → 不生成文件，目录由 commit 剪枝。
		return
	}

	// 收敛 messages：仅保留本文件需要新建的那部分，按 buildFullProtoEntries 的原顺序。
	keptMsgs := make([]protoMessageBlock, 0, len(full.messages))
	for _, m := range full.messages {
		if neededLocalMsg[m.name] {
			keptMsgs = append(keptMsgs, m)
		}
	}

	// imports 去重 + 字典序排序；自身路径若被算进来，剔除（理论不会，门 2 已挡）。
	selfRel, _ := filepath.Rel(p.descRoot, absPath)
	selfRel = filepath.ToSlash(selfRel)
	imports := make([]string, 0, len(importPaths))
	for ip := range importPaths {
		ip = filepath.ToSlash(ip)
		if ip == selfRel {
			continue
		}
		imports = append(imports, ip)
	}
	sort.Strings(imports)

	dp, ok := p.dirs[dirAbs]
	if !ok {
		dp = &dirPlan{absDir: dirAbs, files: make(map[string]*protoFilePlan)}
		p.dirs[dirAbs] = dp
	}
	fp := &protoFilePlan{
		absPath:   absPath,
		modelName: modelName,
		messages:  keptMsgs,
		rpcs:      keptRPCs,
		imports:   imports,
	}
	dp.files[fileName] = fp
}

// commit 落盘所有 plan。空 file / 空 dir 自动剪枝；写出的文件挂到 tracker 以便回滚。
//
// 双保险：addSchema 已经在 plan 阶段对"目标文件已存在"做过跳过判断，但 plan 计算
// 与 commit 写盘之间不是原子的（用户在中途手动修改 desc/ 也属于合法操作）。这里再
// 兜一次"已存在则不覆盖"，与 zctl-commands.md §4 PhaseB 覆盖契约硬对齐：
// 已有目标文件 → 一字不动 → 视为该 schema 本阶段已完成。
func (p *descPlan) commit(tracker *fileTracker) error {
	for _, dp := range p.dirs {
		// 文件全空 → 跳过整个目录
		nonEmpty := false
		for _, fp := range dp.files {
			if len(fp.rpcs) > 0 {
				nonEmpty = true
				break
			}
		}
		if !nonEmpty {
			continue
		}
		if err := pathx.MkdirIfNotExist(dp.absDir); err != nil {
			return err
		}
		for _, fp := range dp.files {
			if len(fp.rpcs) == 0 {
				continue
			}
			if pathx.FileExists(fp.absPath) {
				// 已存在 → 一字不动，硬对齐 PhaseB"新建/跳过已有"覆盖契约。
				fmt.Printf("[zctl] desc proto already exists, skip: %s\n", fp.absPath)
				continue
			}
			content := renderProtoFile(p.g, fp)
			if err := os.WriteFile(fp.absPath, []byte(content), 0o644); err != nil {
				return err
			}
			rel, _ := filepath.Rel(p.descRoot, fp.absPath)
			fmt.Printf("[zctl] Wrote desc proto: desc/%s\n", rel)
			_ = tracker // tracker 自动通过 snapshot 差集捕获新文件，无需手动登记
		}
	}
	return nil
}

// fullProtoSet 仅是 buildFullProtoEntries 的返回容器。
type fullProtoSet struct {
	messages []protoMessageBlock
	rpcs     []protoRPCEntry
}

// buildFullProtoEntries 基于 ent schema 计算"全集"的 message + rpc 列表。
// 与旧 genDescProto 的渲染规则保持完全一致（CRUD + ByUnique + List + 软删 Delete）。
//
// 注意：这里只产出"块定义"，不直接拼最终文件文本。最终文本由 renderProtoFile 完成，
// 因为是否输出 service header / Info message 取决于差集结果。
func buildFullProtoEntries(g *GenContext, schema *load.Schema) fullProtoSet {
	modelName := schema.Name

	// ── Info message body ──
	var infoFields strings.Builder
	infoSig := []protoFieldSig{}
	fieldNum := 1
	infoFields.WriteString(fmt.Sprintf("  // 主键ID\n  optional uint64 id = %d;\n", fieldNum))
	infoSig = append(infoSig, protoFieldSig{modifier: "optional", typeName: "uint64", name: "id", tag: fieldNum})
	fieldNum++
	if hasField(schema, "created_at") {
		infoFields.WriteString(fmt.Sprintf("  // 创建时间\n  optional int64 created_at = %d;\n", fieldNum))
		infoSig = append(infoSig, protoFieldSig{modifier: "optional", typeName: "int64", name: "created_at", tag: fieldNum})
		fieldNum++
	}
	if hasField(schema, "updated_at") {
		infoFields.WriteString(fmt.Sprintf("  // 更新时间\n  optional int64 updated_at = %d;\n", fieldNum))
		infoSig = append(infoSig, protoFieldSig{modifier: "optional", typeName: "int64", name: "updated_at", tag: fieldNum})
		fieldNum++
	}
	for _, f := range schema.Fields {
		if isBaseField(f.Name) {
			continue
		}
		protoType := goTypeToProtoType(f.Info.Type.String())
		optional := ""
		modifier := ""
		if f.Optional || f.Nillable {
			optional = "optional "
			modifier = "optional"
		}
		if f.Comment != "" {
			infoFields.WriteString(fmt.Sprintf("  // %s\n", f.Comment))
		}
		infoFields.WriteString(fmt.Sprintf("  %s%s %s = %d;\n", optional, protoType, f.Name, fieldNum))
		infoSig = append(infoSig, protoFieldSig{modifier: modifier, typeName: protoType, name: f.Name, tag: fieldNum})
		fieldNum++
	}

	uniqueFields := collectUniqueFields(schema)
	hasSoftDelete := hasDeletedAtField(schema)

	out := fullProtoSet{}
	addMsg := func(name, comment, body string, fields []protoFieldSig) {
		out.messages = append(out.messages, protoMessageBlock{
			name:    name,
			comment: comment,
			body:    body,
			fields:  fields,
		})
	}
	addRPC := func(method, req, resp, comment string, deps []string) {
		out.rpcs = append(out.rpcs, protoRPCEntry{
			method:  method,
			req:     req,
			resp:    resp,
			comment: comment,
			deps:    deps,
		})
	}

	// Info
	addMsg(modelName+"Info",
		fmt.Sprintf("// %sInfo 核心详情结构，用于创建/更新/查询详情。\n", modelName),
		infoFields.String(),
		infoSig)

	// 1. Create
	addMsg("Create"+modelName+"Req",
		fmt.Sprintf("// 创建%s请求\n", modelName),
		fmt.Sprintf("  %sInfo info = 1;\n", modelName),
		[]protoFieldSig{{typeName: modelName + "Info", name: "info", tag: 1}})
	addMsg("Create"+modelName+"Resp",
		fmt.Sprintf("// 创建%s响应\n", modelName),
		"  uint64 id = 1;\n",
		[]protoFieldSig{{typeName: "uint64", name: "id", tag: 1}})
	addRPC("Create"+modelName, "Create"+modelName+"Req", "Create"+modelName+"Resp",
		fmt.Sprintf("  // 创建%s\n", modelName),
		[]string{"Create" + modelName + "Req", "Create" + modelName + "Resp", modelName + "Info"})

	// 2. GetByID
	addMsg("Get"+modelName+"ByIDReq",
		fmt.Sprintf("// 按ID查询%s请求\n", modelName),
		"  uint64 id = 1;\n",
		[]protoFieldSig{{typeName: "uint64", name: "id", tag: 1}})
	addRPC("Get"+modelName+"ByID", "Get"+modelName+"ByIDReq", modelName+"Info",
		fmt.Sprintf("  // 按ID获取%s详情\n", modelName),
		[]string{"Get" + modelName + "ByIDReq", modelName + "Info"})

	// 3. GetByUnique
	for _, uf := range uniqueFields {
		fieldPascal := name.GoPascal(uf.Name)
		protoType := goTypeToProtoType(uf.TypeName)
		msgName := fmt.Sprintf("Get%sBy%sReq", modelName, fieldPascal)
		addMsg(msgName,
			fmt.Sprintf("// 按%s查询%s请求\n", fieldPascal, modelName),
			fmt.Sprintf("  %s %s = 1;\n", protoType, uf.Name),
			[]protoFieldSig{{typeName: protoType, name: uf.Name, tag: 1}})
		addRPC(fmt.Sprintf("Get%sBy%s", modelName, fieldPascal), msgName, modelName+"Info",
			fmt.Sprintf("  // 按%s获取%s详情\n", fieldPascal, modelName),
			[]string{msgName, modelName + "Info"})
	}

	// 4. UpdateByID
	addMsg("Update"+modelName+"Req",
		fmt.Sprintf("// 更新%s请求\n", modelName),
		fmt.Sprintf("  %sInfo info = 1;\n", modelName),
		[]protoFieldSig{{typeName: modelName + "Info", name: "info", tag: 1}})
	addRPC("Update"+modelName, "Update"+modelName+"Req", "Empty",
		fmt.Sprintf("  // 更新%s\n", modelName),
		[]string{"Update" + modelName + "Req", modelName + "Info"})

	// 5. UpdateByUnique
	for _, uf := range uniqueFields {
		fieldPascal := name.GoPascal(uf.Name)
		protoType := goTypeToProtoType(uf.TypeName)
		msgName := fmt.Sprintf("Update%sBy%sReq", modelName, fieldPascal)
		addMsg(msgName,
			fmt.Sprintf("// 按%s更新%s请求\n", fieldPascal, modelName),
			fmt.Sprintf("  %s %s = 1;\n  %sInfo info = 2;\n", protoType, uf.Name, modelName),
			[]protoFieldSig{
				{typeName: protoType, name: uf.Name, tag: 1},
				{typeName: modelName + "Info", name: "info", tag: 2},
			})
		addRPC(fmt.Sprintf("Update%sBy%s", modelName, fieldPascal), msgName, "Empty",
			fmt.Sprintf("  // 按%s更新%s\n", fieldPascal, modelName),
			[]string{msgName, modelName + "Info"})
	}

	// 6. Delete (only soft delete)
	if hasSoftDelete {
		addMsg("Delete"+modelName+"Req",
			fmt.Sprintf("// 删除%s请求（支持批量）\n", modelName),
			"  repeated uint64 ids = 1;\n",
			[]protoFieldSig{{modifier: "repeated", typeName: "uint64", name: "ids", tag: 1}})
		addRPC("Delete"+modelName, "Delete"+modelName+"Req", "Empty",
			fmt.Sprintf("  // 删除%s\n", modelName),
			[]string{"Delete" + modelName + "Req"})
	}

	// 7. List
	addMsg("Get"+modelName+"ListReq",
		fmt.Sprintf("// 获取%s列表请求\n", modelName),
		"  uint64 page = 1;\n  uint64 page_size = 2;\n",
		[]protoFieldSig{
			{typeName: "uint64", name: "page", tag: 1},
			{typeName: "uint64", name: "page_size", tag: 2},
		})
	addMsg("Get"+modelName+"ListResp",
		fmt.Sprintf("// 获取%s列表响应\n", modelName),
		fmt.Sprintf("  uint64 total = 1;\n  repeated %sInfo data = 2;\n", modelName),
		[]protoFieldSig{
			{typeName: "uint64", name: "total", tag: 1},
			{modifier: "repeated", typeName: modelName + "Info", name: "data", tag: 2},
		})
	addRPC("Get"+modelName+"List", "Get"+modelName+"ListReq", "Get"+modelName+"ListResp",
		fmt.Sprintf("  // 获取%s列表\n", modelName),
		[]string{"Get" + modelName + "ListReq", "Get" + modelName + "ListResp", modelName + "Info"})

	return out
}

// renderProtoFile 把一个 protoFilePlan 渲染成最终的 .proto 文件文本。
// 调用方保证 fp.rpcs 非空（空文件已在 commit 阶段剪枝）。
func renderProtoFile(g *GenContext, fp *protoFilePlan) string {
	var b strings.Builder
	b.WriteString("syntax = \"proto3\";\n\n")
	// 不输出 import 语句：本仓库后续会通过 merge-proto 把 desc/ 下所有 .proto 合并成
	// 根 cs-xxx-rpc.proto，跨文件 message 引用在合并后属同一文件，不需要 import。
	// fp.imports 仅用于 PhaseB 一致性体检（已存在且一致的 message → 不在本文件重复定义）。
	b.WriteString(fmt.Sprintf("// ──── %s module ────\n\n", fp.modelName))

	// messages —— 按"是否是 Info / 是否 Req-Resp 配对"原样输出，与旧实现观感一致。
	wroteRRHeader := false
	for _, m := range fp.messages {
		// 在第一个非 Info / 非 Resp 的 Req 出现前打一次 Request/Response 分隔注释，
		// 与旧 genDescProto 的输出风格保持一致。判定规则：name 不以 "Info" 结尾时认为进入 R/R 段。
		if !wroteRRHeader && !strings.HasSuffix(m.name, "Info") {
			b.WriteString("// ──── Request / Response ────\n\n")
			wroteRRHeader = true
		}
		b.WriteString(m.comment)
		b.WriteString(fmt.Sprintf("message %s {\n%s}\n\n", m.name, m.body))
	}

	// service block
	b.WriteString(fmt.Sprintf("// %s 管理服务\n", fp.modelName))
	b.WriteString(fmt.Sprintf("service %s {\n", name.GoPascal(g.ServiceName)))
	for _, r := range fp.rpcs {
		b.WriteString(r.comment)
		b.WriteString(fmt.Sprintf("  rpc %s (%s) returns (%s);\n", r.method, r.req, r.resp))
	}
	b.WriteString("}\n")
	return b.String()
}

// ==================== Desc Scanner（PhaseB 一致性体检依赖） ====================
//
// scanDescDir 全量扫描 desc/ 下所有 .proto 文件，把：
//   - 所有 service 的 rpc 名（按 service 维度分桶）→ rpcs 入参
//   - 所有 top-level message 的字段签名（首遇优先）→ messages 入参
//
// 解析采用纯字符串扫描，足以覆盖本工程内 desc proto 的标准写法：
//   service Foo { rpc Bar (BarReq) returns (BarResp); ... }
//   message Foo { [optional|repeated] <type> <name> = <tag>; ... }
//
// 为简单可靠：忽略嵌套 message（本工程不会写嵌套）；忽略 enum；遇到不可解析行直接跳过。
// desc/ 不存在时静默返回 nil，让首次跑也能过。
func scanDescDir(descRoot string, rpcs map[string]map[string]bool, messages map[string]existingMessageInfo) error {
	info, err := os.Stat(descRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.Walk(descRoot, func(path string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fi.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".proto") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil // 单文件读失败不阻断整体扫描
		}
		rel, _ := filepath.Rel(descRoot, path)
		rel = filepath.ToSlash(rel)
		parseProtoFile(string(raw), rel, rpcs, messages)
		return nil
	})
}

// parseProtoFile 把单个 .proto 文件文本解析进 rpcs / messages 两份索引。
// 详见 scanDescDir 注释里的语法子集。
func parseProtoFile(text, relPath string, rpcs map[string]map[string]bool, messages map[string]existingMessageInfo) {
	lines := strings.Split(stripBlockComments(text), "\n")
	type frame struct {
		kind string // "service" | "message"
		name string
		// for message: 累积字段
		fields []protoFieldSig
	}
	var stack []*frame
	currentService := ""

	push := func(f *frame) { stack = append(stack, f) }
	top := func() *frame {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}
	pop := func() {
		if len(stack) == 0 {
			return
		}
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch f.kind {
		case "service":
			currentService = ""
		case "message":
			// 仅顶层 message（pop 后栈空）入索引；嵌套 message 忽略。
			if len(stack) == 0 {
				if _, exists := messages[f.name]; !exists {
					sortFieldSigsByTag(f.fields)
					messages[f.name] = existingMessageInfo{relPath: relPath, fields: f.fields}
				}
			}
		}
	}

	for _, raw := range lines {
		line := strings.TrimSpace(stripLineComment(raw))
		if line == "" {
			continue
		}
		// 处理形如 "} ... {" 的极少见同行混排，按 { } 拆分逐块处理。
		// 简化：按字符级别推进。
		i := 0
		for i < len(line) {
			c := line[i]
			if c == '}' {
				pop()
				i++
				continue
			}
			// 找 service / message / rpc 关键字（仅在合适层级匹配）
			if strings.HasPrefix(line[i:], "service ") && top() == nil {
				rest := strings.TrimSpace(line[i+len("service "):])
				name, _ := splitIdent(rest)
				if name != "" {
					push(&frame{kind: "service", name: name})
					currentService = name
					if _, ok := rpcs[name]; !ok {
						rpcs[name] = make(map[string]bool)
					}
				}
				if idx := strings.Index(line[i:], "{"); idx >= 0 {
					i += idx + 1
				} else {
					break
				}
				continue
			}
			if strings.HasPrefix(line[i:], "message ") {
				rest := strings.TrimSpace(line[i+len("message "):])
				name, _ := splitIdent(rest)
				if name != "" {
					push(&frame{kind: "message", name: name})
				}
				if idx := strings.Index(line[i:], "{"); idx >= 0 {
					i += idx + 1
				} else {
					break
				}
				continue
			}
			// rpc 行：仅在 service frame 内
			if currentService != "" && strings.HasPrefix(line[i:], "rpc ") {
				rest := strings.TrimSpace(line[i+len("rpc "):])
				name, _ := splitIdent(rest)
				if name != "" {
					rpcs[currentService][name] = true
				}
				// 跳过本行剩余
				break
			}
			// message frame 内的字段行
			if t := top(); t != nil && t.kind == "message" {
				// 一行可能完整一条字段：[optional|repeated] type name = tag;
				if sig, ok := parseProtoFieldLine(line[i:]); ok {
					t.fields = append(t.fields, sig)
				}
				// 一行只支持解析一条字段（本工程 proto 风格统一），余下 i 直接跳到末尾
				break
			}
			i++
		}
	}
	// 防御：未闭合的 frame 全部丢弃（不写入 messages），避免脏数据。
	_ = currentService
}

// stripBlockComments 去掉 /* ... */ 块注释（不递归 / 不处理字符串字面量内的伪注释，足够本工程使用）。
func stripBlockComments(s string) string {
	for {
		start := strings.Index(s, "/*")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], "*/")
		if end < 0 {
			return s[:start]
		}
		s = s[:start] + s[start+end+2:]
	}
}

// stripLineComment 去掉 "//" 之后的行尾注释。
func stripLineComment(s string) string {
	if idx := strings.Index(s, "//"); idx >= 0 {
		return s[:idx]
	}
	return s
}

// splitIdent 从 s 头部抽出一个标识符（字母/数字/下划线），返回 ident 与剩余串。
func splitIdent(s string) (string, string) {
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			i++
			continue
		}
		break
	}
	return s[:i], s[i:]
}

// parseProtoFieldLine 解析一行 message 字段。期望形如：
//
//	[optional|repeated] <type> <name> = <tag>[ ... ];
//
// 解析失败返回 ok=false。
func parseProtoFieldLine(line string) (protoFieldSig, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return protoFieldSig{}, false
	}
	// 必须含 "=" 与 ";"，否则不是字段
	if !strings.Contains(line, "=") || !strings.Contains(line, ";") {
		return protoFieldSig{}, false
	}
	// 识别 modifier
	modifier := ""
	switch {
	case strings.HasPrefix(line, "optional "):
		modifier = "optional"
		line = strings.TrimPrefix(line, "optional ")
	case strings.HasPrefix(line, "repeated "):
		modifier = "repeated"
		line = strings.TrimPrefix(line, "repeated ")
	case strings.HasPrefix(line, "required "):
		modifier = "required"
		line = strings.TrimPrefix(line, "required ")
	}
	line = strings.TrimSpace(line)

	// type
	typeName, rest := splitIdent(line)
	if typeName == "" {
		return protoFieldSig{}, false
	}
	rest = strings.TrimSpace(rest)

	// name
	fieldName, rest := splitIdent(rest)
	if fieldName == "" {
		return protoFieldSig{}, false
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "=") {
		return protoFieldSig{}, false
	}
	rest = strings.TrimSpace(rest[1:])

	// tag（数字串到第一个非数字为止）
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return protoFieldSig{}, false
	}
	tag := 0
	for k := 0; k < j; k++ {
		tag = tag*10 + int(rest[k]-'0')
	}

	return protoFieldSig{
		modifier: modifier,
		typeName: typeName,
		name:     fieldName,
		tag:      tag,
	}, true
}

// sortFieldSigsByTag 按 tag 升序排序字段签名，便于一致性比对忽略源文件中的字段顺序。
func sortFieldSigsByTag(fs []protoFieldSig) {
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].tag < fs[j].tag })
}

// messageFieldsEqual 在"忽略字段顺序、忽略注释、忽略 option"的前提下严格比对两组字段：
//   - 字段数量必须一致
//   - 按 tag 升序逐个比对：modifier / typeName / name / tag 必须 100% 相同
func messageFieldsEqual(a, b []protoFieldSig) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]protoFieldSig(nil), a...)
	bb := append([]protoFieldSig(nil), b...)
	sortFieldSigsByTag(aa)
	sortFieldSigsByTag(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

// isBaseProtoMessage 判定 message 名是否属于 base.proto 内置类型，这类 message 不参与跨文件一致性比对。
func isBaseProtoMessage(name string) bool {
	switch name {
	case "Empty", "PageInfo", "BaseIDReq", "BaseUUIDReq":
		return true
	}
	return false
}
