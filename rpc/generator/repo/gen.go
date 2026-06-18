// Package repo (zctl generator) emits the cache-aware repo three-piece set
// (interface / impl / mock) alongside the dao layer. It is invoked once per
// schema by ent.GenEntLogic so that — by construction — every dao impl file
// has a paired repo file, even when no cacheable method qualifies.
//
// Identification strategy:
//
//  1. The plan is derived ENTIRELY from the already-generated dao files
//     (interface AST + impl file text), NOT from the ent schema. This keeps
//     the generator on a single source of truth — whatever dao actually
//     exposes is what repo wraps.
//
//  2. Write candidates: determined by inspecting the dao impl BODY for ent
//     mutation method calls (.Save(, .Exec(, .ExecX(, UpdateOneID(, CreateBulk(,
//     DeleteOneID(, DeleteOne(). If the function body calls any of these ent
//     mutators, it qualifies as a write — regardless of the method name. This
//     covers Create, Update*, Delete*, CreateBatch, and any custom write methods.
//
//  3. Read candidates: ONLY the GetByID method (primary-key point lookup) is
//     generated for the repo layer. The cache key is generated using the
//     primary key. A comment above the key reminds users to replace it with
//     their actual business key if needed.
//
//  4. Cache key: every read path uses the same single primary-key form
//
//        <service>:repo:<table>:id:%d
//
//     The key is a DEFAULT scaffold — a comment above reminds users to
//     replace the key with actual business semantics.
//
//  5. All write methods from step 2 MUST be generated in the repo interface
//     and impl. If a method's signature cannot be fully understood for
//     rendering its internal logic, the impl body is a TODO placeholder —
//     but the method signature is still registered so the file compiles.
//
//  6. Empty plan: when no candidate qualifies, the repo file is still emitted
//     to keep the 1:1 dao↔repo file invariant, but contains only WithTx.
//
//  7. Existing files are NEVER overwritten — once a repo file lands users
//     are free to hand-edit it; regen leaves it alone. (Mock is always
//     overwritten, mirroring dao mock policy.)
package repo

import (
	"fmt"
	"go/ast"
	goformat "go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"entgo.io/ent/entc/load"

	"github.com/qqz14/zctl/util/name"
	"github.com/qqz14/zctl/util/pathx"
)

// GenContext mirrors the relevant subset of ent.GenContext so this package
// can stay zero-dep on the ent generator package (avoids import cycle).
type GenContext struct {
	Output    string // project root (absolute path)
	Module    string // go.mod module path, e.g. "cs-agent-rpc"
	Overwrite bool
}

// GenRepoForSchema is the single entry point; called once per schema right
// after the dao three-piece set has been written. Output paths:
//
//	internal/repo/{model_snake}_repo.go        (interface)
//	internal/repo/impl/{model_snake}_repo.go   (impl)
//	internal/repo/mock/{model_snake}_repo_mock.go  (mock)
//
// base.go is written separately (idempotent; not per-schema).
//
// fieldMap is currently unused but retained on the signature to avoid an
// API-shape churn for callers and to keep room for future per-column
// rendering needs.
func GenRepoForSchema(g *GenContext, schema *load.Schema, fieldMap map[string]string) error {
	_ = fieldMap

	// 1. ensure base.go (idempotent)
	if err := writeRepoBase(g.Output, g.Module); err != nil {
		return fmt.Errorf("write repo base: %w", err)
	}

	// 2. compute plan from the just-emitted dao files (interface + impl)
	plan, err := buildPlanFromDao(g.Output, schema.Name)
	if err != nil {
		return fmt.Errorf("build repo plan from dao for %s: %w", schema.Name, err)
	}

	// 3. interface
	if err := writeRepoInterface(g, schema, plan); err != nil {
		return err
	}

	// 4. impl
	if err := writeRepoImpl(g, schema, plan); err != nil {
		return err
	}

	// 5. mock
	if err := writeRepoMock(g, schema, plan); err != nil {
		return err
	}

	return nil
}

// ─── Plan model ──────────────────────────────────────────────────────────────

// readMethod is a dao Get-candidate that survives qualification.
type readMethod struct {
	Name      string // e.g. "GetByID", "GetByCidUID"
	ParamList string // formatted "ctx context.Context, ..." stripped to ", a A, b B"
	CallArgs  string // "id" or "cid, uid" — for delegation to dao
}

// writeMethod is a dao write-candidate whose impl body calls ent mutation methods.
type writeMethod struct {
	Name string // "Create" / "UpdateByID" / "DeleteByID" / "CreateBatch" / etc.

	// ParamList / CallArgs are the verbatim dao signature minus ctx — used
	// for repo-side method signature rendering and the dao-call argument
	// list. Mirroring the dao verbatim is the only correct strategy.
	ParamList string // e.g. "app_code string, data *ent.IamApp"
	CallArgs  string // e.g. "app_code, data"

	// EntPtrParam is the name of the *ent.<Model> parameter when present,
	// or "" when the method takes a bare "id <T>" instead. Used solely
	// for cache-key derivation (e.g. "updated.ID" / "data.ID").
	EntPtrParam string

	// IDParam is the name of a bare "id <T>" parameter (e.g. DeleteByID),
	// or "" when the method takes *ent.<Model> instead. Used solely for
	// cache-key derivation when no ent pointer is available.
	IDParam string

	// Returns indicates the dao method's return shape.
	//   - "(*ent.<Model>, error)" → ReturnsEntity = true
	//   - "error"                  → ReturnsEntity = false (e.g. DeleteByID)
	ReturnsEntity bool

	// IsTodo is true when the method's signature doesn't match a known
	// renderable shape. The impl body will be a TODO placeholder, but the
	// method is still registered so the code compiles.
	IsTodo bool

	// ReturnTypes stores the full return type list for TODO methods, used
	// to render zero-value returns.
	ReturnTypes []string
}

// repoPlan is the rendered-once-per-schema decision package.
type repoPlan struct {
	Reads     []readMethod
	Writes    []writeMethod
	IDGoType  string // primary-key go type (from dao.GetByID's `id` param)
	HasReads  bool   // shorthand: len(Reads) > 0
	HasWrites bool   // shorthand: len(Writes) > 0
}

// ─── Plan derivation: dao files → plan ─────────────────────────────────────

// buildPlanFromDao parses the just-emitted dao interface (for signatures)
// and dao OceanBase impl (for body-token detection) to derive the cache
// plan. Returns an empty plan (no error) when either file is absent — the
// repo files in that case will only contain WithTx.
//
// Strategy:
//   - Write detection: inspect the dao impl body for ent mutation calls
//     (.Save(, .Exec(, .ExecX(, UpdateOneID(, CreateBulk(, DeleteOneID(, etc.)
//     ANY method whose body contains a mutation call is a write candidate.
//   - Read detection: ONLY GetByID qualifies (primary-key point lookup).
//   - All qualifying write methods MUST be present in the repo. If the
//     signature doesn't match a known renderable shape, the impl body is
//     a TODO — but the method signature is still registered.
func buildPlanFromDao(outputDir, modelName string) (repoPlan, error) {
	plan := repoPlan{}

	modelSnake := name.FileSnake(modelName)
	ifacePath := filepath.Join(outputDir, "internal", "dao", modelSnake+"_dao.go")
	implPath := filepath.Join(outputDir, "internal", "dao", "impl", modelSnake+"_oceanbase.go")

	if !pathx.FileExists(ifacePath) {
		return plan, nil
	}

	sigs, err := parseDaoIfaceMethods(ifacePath, modelName)
	if err != nil {
		return plan, err
	}

	// Pull the primary-key Go type from GetByID's `id` parameter.
	for _, s := range sigs {
		if s.Name == "GetByID" {
			if t, ok := s.paramType("id"); ok {
				plan.IDGoType = t
			}
			break
		}
	}

	// Read impl text once for body-token detection.
	var implText string
	if pathx.FileExists(implPath) {
		raw, _ := os.ReadFile(implPath)
		implText = string(raw)
	}

	for _, s := range sigs {
		// Skip WithTx — it's not a business method.
		if s.Name == "WithTx" {
			continue
		}

		// Read: ONLY GetByID (primary-key point lookup) qualifies.
		if s.Name == "GetByID" {
			if plan.IDGoType != "" && daoBodyReadsSingleRow(implText, s.Name) {
				plan.Reads = append(plan.Reads, readMethod{
					Name:      s.Name,
					ParamList: s.formatParamsAfterCtx(),
					CallArgs:  s.formatCallArgsAfterCtx(),
				})
			}
			continue
		}

		// Write: check if the impl body calls ent mutation methods.
		if !daoBodyIsWriteOp(implText, s.Name) {
			continue
		}

		// Try strict shape match for full impl rendering.
		entParam, idName, ok := s.matchKnownWriteShape(plan.IDGoType, modelName)
		if ok {
			plan.Writes = append(plan.Writes, writeMethod{
				Name:          s.Name,
				ParamList:     s.formatParamsAfterCtx(),
				CallArgs:      s.formatCallArgsAfterCtx(),
				EntPtrParam:   entParam,
				IDParam:       idName,
				ReturnsEntity: s.ReturnsEntityPtr(modelName),
				IsTodo:        false,
			})
		} else {
			// Shape not fully understood — register as TODO so it compiles
			// but the body is a placeholder.
			plan.Writes = append(plan.Writes, writeMethod{
				Name:          s.Name,
				ParamList:     s.formatParamsAfterCtx(),
				CallArgs:      s.formatCallArgsAfterCtx(),
				EntPtrParam:   "",
				IDParam:       "",
				ReturnsEntity: s.ReturnsEntityPtr(modelName),
				IsTodo:        true,
				ReturnTypes:   s.Results,
			})
		}
	}

	plan.HasReads = len(plan.Reads) > 0
	plan.HasWrites = len(plan.Writes) > 0

	return plan, nil
}

// daoSig is one method signature parsed from the dao interface.
type daoSig struct {
	Name       string
	ParamNames []string // including "ctx"
	ParamTypes []string // parallel to ParamNames
	Results    []string // ordered list of return-type strings
}

func (d daoSig) paramType(name string) (string, bool) {
	for i, n := range d.ParamNames {
		if n == name {
			return d.ParamTypes[i], true
		}
	}
	return "", false
}

// containsPrimaryKey reports whether the method's parameters reach the
// primary key — either through a bare `id <idGoType>` parameter or via a
// `*ent.<Model>` (which always carries the PK field by ent's invariants).
//
// idGoType may be empty when dao does not expose GetByID; in that case we
// only accept the *ent.<Model> route.
func (d daoSig) containsPrimaryKey(idGoType, modelName string) bool {
	ent, id := d.locatePrimaryKey(idGoType, modelName)
	return ent != "" || id != ""
}

// locatePrimaryKey returns either the ent-pointer param name OR the bare-id
// param name (whichever is present); both empty when neither is found.
func (d daoSig) locatePrimaryKey(idGoType, modelName string) (entPtrParam, idParam string) {
	target := "*ent." + modelName
	for i, t := range d.ParamTypes {
		if t == target {
			entPtrParam = d.ParamNames[i]
			break
		}
	}
	if entPtrParam == "" && idGoType != "" {
		for i, n := range d.ParamNames {
			if n == "id" && d.ParamTypes[i] == idGoType {
				idParam = n
				break
			}
		}
	}
	return
}

// ReturnsEntityPtr reports whether the first return is *ent.<Model>.
func (d daoSig) ReturnsEntityPtr(modelName string) bool {
	if len(d.Results) == 0 {
		return false
	}
	return d.Results[0] == "*ent."+modelName
}

// matchKnownWriteShape reports whether the method's full signature matches
// one of the known write-method shapes the repo generator can fully render.
// Returns the ent-pointer / id param names useful for cache-key derivation;
// ok=false means the method will be rendered as a TODO placeholder.
//
// Accepted shapes (ctx context.Context is implicit param 0):
//
//	Create(ctx, *ent.M)                       (*ent.M, error)
//	CreateBatch(ctx, []*ent.M)                error
//	UpdateByID(ctx, *ent.M)                   (*ent.M, error)
//	UpdateBy<X>(ctx, x T, *ent.M)             (*ent.M, error)
//	DeleteByID(ctx, id <idGoType>)            error
//	DeleteByID(ctx, *ent.M)                   error
//	DeleteByIDs(ctx, ids []<idGoType>)         error
//
// Anything else → ok=false (method gets a TODO impl).
func (d daoSig) matchKnownWriteShape(idGoType, modelName string) (entPtrParam, idParam string, ok bool) {
	// Common precondition: leading ctx and at least 1 more param.
	if len(d.ParamNames) < 2 || d.ParamNames[0] != "ctx" || d.ParamTypes[0] != "context.Context" {
		return "", "", false
	}
	entPtrType := "*ent." + modelName
	entSliceType := "[]*ent." + modelName

	// ── Create / Update shapes ──
	if d.Name == "Create" || strings.HasPrefix(d.Name, "Update") {
		// Must return (*ent.M, error)
		if len(d.Results) != 2 || d.Results[0] != entPtrType || d.Results[1] != "error" {
			return "", "", false
		}
		switch len(d.ParamTypes) {
		case 2:
			// (ctx, *ent.M)
			if d.ParamTypes[1] != entPtrType {
				return "", "", false
			}
			return d.ParamNames[1], "", true
		case 3:
			// (ctx, key T, *ent.M) — secondary-key + ent pointer (UpdateBy<X>)
			if !strings.HasPrefix(d.Name, "Update") {
				return "", "", false
			}
			if d.ParamTypes[2] != entPtrType {
				return "", "", false
			}
			return d.ParamNames[2], "", true
		default:
			return "", "", false
		}
	}

	// ── CreateBatch shape ──
	if d.Name == "CreateBatch" {
		// (ctx, []*ent.M) → error
		if len(d.Results) != 1 || d.Results[0] != "error" {
			return "", "", false
		}
		if len(d.ParamTypes) != 2 || d.ParamTypes[1] != entSliceType {
			return "", "", false
		}
		return d.ParamNames[1], "", true
	}

	// ── Delete shapes ──
	if strings.HasPrefix(d.Name, "Delete") {
		// Must return only `error`
		if len(d.Results) != 1 || d.Results[0] != "error" {
			return "", "", false
		}
		if len(d.ParamTypes) == 2 {
			// (ctx, *ent.M)
			if d.ParamTypes[1] == entPtrType {
				return d.ParamNames[1], "", true
			}
			// (ctx, id <idGoType>)
			if idGoType != "" && d.ParamNames[1] == "id" && d.ParamTypes[1] == idGoType {
				return "", "id", true
			}
			// (ctx, ids []<idGoType>) — batch delete by IDs
			if d.ParamTypes[1] == "[]"+idGoType {
				return "", d.ParamNames[1], true
			}
		}
		return "", "", false
	}

	return "", "", false
}

// formatParamsAfterCtx renders parameters EXCLUDING the leading ctx, in the
// canonical "name Type, name Type" form. Adjacent same-typed neighbors are
// kept verbose (no merging) — the dao iface already serializes them this
// way and verbatim copy keeps regen diffs minimal.
func (d daoSig) formatParamsAfterCtx() string {
	if len(d.ParamNames) <= 1 {
		return ""
	}
	var parts []string
	for i := 1; i < len(d.ParamNames); i++ {
		parts = append(parts, d.ParamNames[i]+" "+d.ParamTypes[i])
	}
	return strings.Join(parts, ", ")
}

// formatCallArgsAfterCtx joins parameter names after ctx, e.g. "cid, uid".
func (d daoSig) formatCallArgsAfterCtx() string {
	if len(d.ParamNames) <= 1 {
		return ""
	}
	return strings.Join(d.ParamNames[1:], ", ")
}

// parseDaoIfaceMethods walks the dao interface AST and returns all method
// signatures of the <Model>Dao interface type. Embedded interfaces (none
// expected today) are ignored.
func parseDaoIfaceMethods(filePath, modelName string) ([]daoSig, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, nil, parser.AllErrors)
	if err != nil {
		return nil, err
	}

	wantTypeName := modelName + "Dao"
	var sigs []daoSig

	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != wantTypeName {
				continue
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}
			for _, method := range it.Methods.List {
				if len(method.Names) == 0 {
					continue // embedded interface
				}
				ft, ok := method.Type.(*ast.FuncType)
				if !ok {
					continue
				}
				sig := daoSig{Name: method.Names[0].Name}
				if ft.Params != nil {
					for _, p := range ft.Params.List {
						typeStr := exprToString(p.Type)
						if len(p.Names) == 0 {
							sig.ParamNames = append(sig.ParamNames, "")
							sig.ParamTypes = append(sig.ParamTypes, typeStr)
							continue
						}
						for _, n := range p.Names {
							sig.ParamNames = append(sig.ParamNames, n.Name)
							sig.ParamTypes = append(sig.ParamTypes, typeStr)
						}
					}
				}
				if ft.Results != nil {
					for _, r := range ft.Results.List {
						typeStr := exprToString(r.Type)
						count := len(r.Names)
						if count == 0 {
							count = 1
						}
						for i := 0; i < count; i++ {
							sig.Results = append(sig.Results, typeStr)
						}
					}
				}
				sigs = append(sigs, sig)
			}
		}
	}
	return sigs, nil
}

// exprToString renders an ast type expression back to its source text.
// Handles the small set of forms ent dao actually uses: identifiers,
// selectors (pkg.Type), pointers, and simple slices.
func exprToString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprToString(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprToString(v.X)
	case *ast.ArrayType:
		return "[]" + exprToString(v.Elt)
	default:
		return fmt.Sprintf("%T", e)
	}
}

// daoBodyReadsSingleRow reports whether the named method's body in the dao
// impl text contains a single-row terminator. We accept three ent forms:
//
//	.Only(    .First(    cli.Get(
//
// and explicitly REJECT .OnlyID( / .FirstID( (those return only an id, not
// a full entity, so caching them would give wrong shape on repo's read
// path). The check is a coarse text scan inside the function body; ent's
// generated dao bodies are simple enough that this is reliable in practice.
func daoBodyReadsSingleRow(implText, methodName string) bool {
	if implText == "" {
		return false
	}
	body := extractFuncBody(implText, methodName)
	if body == "" {
		return false
	}
	// Reject ID-only sentinels first so they don't get confused with ".Only(".
	stripped := body
	stripped = strings.ReplaceAll(stripped, ".OnlyID(", " ")
	stripped = strings.ReplaceAll(stripped, ".FirstID(", " ")
	if strings.Contains(stripped, ".Only(") ||
		strings.Contains(stripped, ".First(") ||
		strings.Contains(stripped, ".Get(") {
		return true
	}
	return false
}

// daoBodyIsWriteOp reports whether the named method's body in the dao impl
// calls ent mutation methods, indicating it performs a write (insert/update/delete).
//
// Detected ent write patterns:
//   - .Save(ctx)              — used after Create/UpdateOneID builders
//   - .Exec(ctx)             — used after CreateBulk/Update/Delete builders
//   - .ExecX(ctx)            — same but panics on error
//   - .ID(ctx)               — used after Create().OnConflict...().Update().ID(ctx)
//   - UpdateOneID(           — explicit update-by-ID builder start
//   - CreateBulk(            — batch insert builder start
//   - DeleteOneID(           — explicit delete-by-ID builder start
//   - DeleteOne(             — explicit delete single-entity builder start
//   - .Delete().             — delete builder chain (cli.Delete().Where(...).Exec())
//
// This is strictly more accurate than name-based matching because it catches:
//   - CreateBatch (calls CreateBulk internally)
//   - Custom-named write methods (e.g. ResetPasswordAndStatus)
//   - Soft-delete methods that use Update().Where(...).SetDeletedAt(...).Save()
//
// It deliberately does NOT match read-only .Only(/.All(/.Count(/.First(/.Get(
// which are the ent query terminators.
func daoBodyIsWriteOp(implText, methodName string) bool {
	if implText == "" {
		return false
	}
	body := extractFuncBody(implText, methodName)
	if body == "" {
		return false
	}

	// Ent write-terminator patterns.
	writePatterns := []string{
		".Save(ctx)",
		".Save(ctx )",
		".Exec(ctx)",
		".Exec(ctx )",
		".ExecX(ctx)",
		".ExecX(ctx )",
		".ID(ctx)",
		".ID(ctx )",
		"UpdateOneID(",
		"CreateBulk(",
		"DeleteOneID(",
		"DeleteOne(",
		".Delete().",
		// Raw SQL patterns (ExecContext is also a write indicator)
		".ExecContext(",
	}

	for _, pat := range writePatterns {
		if strings.Contains(body, pat) {
			return true
		}
	}

	// Also match Save/Exec with semicolons that appear in multi-line contexts:
	//   .Save(ctx); err != nil {
	//   .Exec(ctx); err != nil {
	if strings.Contains(body, ".Save(") && !strings.Contains(body, ".SaveX(") {
		// Distinguish from Query paths: Query().Only() vs Create().Save()
		// If Save is present AND no Query() chain precedes, it's a write.
		// But to be safe, just check Save is present — ent Query never uses .Save().
		return true
	}
	if strings.Contains(body, ".Exec(") {
		// ent Query never uses .Exec() — only mutation builders and CreateBulk use it.
		return true
	}

	return false
}

// extractFuncBody returns the substring between the matching {} of the
// (first) function whose definition contains "func (... ) <methodName>(".
// Returns "" if no balanced body is found.
func extractFuncBody(src, methodName string) string {
	// Look for "func (" first to anchor receiver-bound methods, then for
	// the method name with "(" right after — avoids matching local var
	// names that happen to share the function's identifier.
	cursor := 0
	for {
		fi := strings.Index(src[cursor:], "func (")
		if fi < 0 {
			return ""
		}
		fi += cursor
		// Find ") <methodName>(" after the receiver.
		recvEnd := strings.Index(src[fi:], ")")
		if recvEnd < 0 {
			return ""
		}
		nameStart := fi + recvEnd + 1
		// skip spaces
		for nameStart < len(src) && src[nameStart] == ' ' {
			nameStart++
		}
		if strings.HasPrefix(src[nameStart:], methodName+"(") {
			// Find the opening { after the signature.
			open := strings.Index(src[nameStart:], "{")
			if open < 0 {
				return ""
			}
			open += nameStart
			depth := 0
			for i := open; i < len(src); i++ {
				switch src[i] {
				case '{':
					depth++
				case '}':
					depth--
					if depth == 0 {
						return src[open+1 : i]
					}
				}
			}
			return ""
		}
		cursor = nameStart
	}
}

// ─── Interface file ─────────────────────────────────────────────────────────

func writeRepoInterface(g *GenContext, schema *load.Schema, plan repoPlan) error {
	dir := filepath.Join(g.Output, "internal", "repo")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	target := filepath.Join(dir, name.FileSnake(schema.Name)+"_repo.go")
	if pathx.FileExists(target) && !g.Overwrite {
		return nil
	}

	model := schema.Name

	// methodSurface for the docblock — graceful fallback when there are
	// no cache-bound reads.
	var methodSurface string
	if plan.HasReads {
		var names []string
		for _, m := range plan.Reads {
			names = append(names, m.Name)
		}
		methodSurface = strings.Join(names, " / ")
	} else {
		methodSurface = "(no cache-bound point lookups; only WithTx)"
	}

	withTxExample := "Xxx(...)"
	if plan.HasReads {
		withTxExample = plan.Reads[0].Name + "(...)"
	}

	var body strings.Builder
	if plan.HasReads {
		body.WriteString("\n\t// ──── Read (cache-through) ────\n")
		for _, m := range plan.Reads {
			fmt.Fprintf(&body,
				"\n\t// %s reads via primary-key cache. Cache miss falls back to dao\n"+
					"\t// and the result is written back with the configured TTL.\n"+
					"\t%s(ctx context.Context, %s) (*ent.%s, error)\n",
				m.Name, m.Name, m.ParamList, model)
		}
	}
	if plan.HasWrites {
		body.WriteString("\n\t// ──── Write (DB then cache invalidation) ────\n")
		for _, w := range plan.Writes {
			body.WriteString("\n")
			body.WriteString(renderWriteSignatureDoc(w, model))
		}
	}

	// Imports trimmed when the file collapses to only WithTx — ent stays,
	// context is unused, so it must NOT be listed.
	imports := fmt.Sprintf("\t\"%s/ent\"", g.Module)
	if plan.HasReads || plan.HasWrites {
		imports = fmt.Sprintf("\t\"context\"\n\n\t\"%s/ent\"", g.Module)
	}

	content := fmt.Sprintf(`package repo

// Code generated by zctl. DO NOT EDIT.
// Re-generated each time the dao interface or schema changes (gen-rpc-ent-logic).

import (
%[1]s
)

// %[2]sRepo is the cache-aware aggregate accessor for %[3]s.
//
// Rules of thumb for callers:
//   - Default to this Repo for business reads/writes — it's transparent.
//   - Drop down to dao.%[2]sDao only when you explicitly need a
//     fresh-from-DB read (audit, reconciliation, strong-consistency check).
//
// Method surface mirrors a deliberate SUBSET of dao.%[2]sDao: only
// the operations that benefit from caching (point lookups returning a
// single row whose parameters reach the primary key) and the writes that
// must trigger invalidation. Bulk / paginated / report-style queries are
// NOT exposed here — callers should hit dao directly for those.
//
// Cache-bound surface: %[5]s
type %[2]sRepo interface {
	// WithTx returns a Repo bound to tx. The Base (redis client + ttl) is
	// reused; only the underlying dao is rebound. Mirrors the dao pattern so
	// logic code can write `+"`repo.WithTx(tx).%[6]s`"+` symmetrically.
	WithTx(tx *ent.Tx) %[2]sRepo
%[4]s}
`, imports, model, name.FileSnake(model), body.String(), methodSurface, withTxExample)

	return os.WriteFile(target, []byte(formatGoSource(content, target)), 0644)
}

// renderWriteSignatureDoc renders one write-method signature with its
// docblock for the interface file. The signature is rendered verbatim from
// the dao interface — the repo mirrors it exactly.
func renderWriteSignatureDoc(w writeMethod, model string) string {
	var b strings.Builder
	retStr := renderReturnType(w, model)
	fmt.Fprintf(&b,
		"\t// %s delegates to dao and invalidates the primary-key cache key.\n"+
			"\t%s(ctx context.Context, %s) %s\n",
		w.Name, w.Name, w.ParamList, retStr)
	return b.String()
}

// renderReturnType renders the return type string for a write method.
func renderReturnType(w writeMethod, model string) string {
	if w.IsTodo && len(w.ReturnTypes) > 0 {
		if len(w.ReturnTypes) == 1 {
			return w.ReturnTypes[0]
		}
		return "(" + strings.Join(w.ReturnTypes, ", ") + ")"
	}
	if w.ReturnsEntity {
		return "(*ent." + model + ", error)"
	}
	return "error"
}

// ─── Impl file ──────────────────────────────────────────────────────────────

func writeRepoImpl(g *GenContext, schema *load.Schema, plan repoPlan) error {
	dir := filepath.Join(g.Output, "internal", "repo", "impl")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	target := filepath.Join(dir, name.FileSnake(schema.Name)+"_repo.go")
	if pathx.FileExists(target) && !g.Overwrite {
		return nil
	}

	model := schema.Name
	tableSnake := name.FileSnake(model)

	// One key + one helper covers every read in this file.
	idVerb := verbForGoType(plan.IDGoType)
	keyConstName := "cacheKey" + model + "ByID"
	keyHelperName := "key" + model + "ByID"

	var keyBlock, getMethods, writeMethods strings.Builder

	if plan.HasReads {
		fmt.Fprintf(&keyBlock,
			"// 默认生成的缓存 key，可根据实际场景替换 key 值\n"+
				"const %s = \"%s:repo:%s:id:%s\"\n",
			keyConstName, g.Module, tableSnake, idVerb)
		fmt.Fprintf(&keyBlock,
			"\n// %s formats the primary-key cache key for one %s row.\n"+
				"// Unexported on purpose: only this repo reads / writes / invalidates the key.\n"+
				"func %s(id %s) string {\n"+
				"\treturn fmt.Sprintf(%s, id)\n"+
				"}\n",
			keyHelperName, tableSnake,
			keyHelperName, plan.IDGoType,
			keyConstName)
	}

	// ──── Read methods ────
	if plan.HasReads {
		getMethods.WriteString("\n// ──── Read ────\n")
	}
	for _, m := range plan.Reads {
		// Only GetByID qualifies — always cache by primary key.
		fmt.Fprintf(&getMethods,
			"\n// %s delegates to repo.GetOrLoad with the primary-key cache key.\n"+
				"//\n"+
				"// Inside a transaction GetOrLoad bypasses both cache and singleflight so the\n"+
				"// caller sees its own writes; cache is consulted only on the non-tx path.\n"+
				"func (r *%sRepo) %s(ctx context.Context, %s) (*ent.%s, error) {\n"+
				"\treturn repo.GetOrLoad(ctx, r.base, %s(id),\n"+
				"\t\tfunc(ctx context.Context) (*ent.%s, error) {\n"+
				"\t\t\treturn r.dao.%s(ctx, %s)\n"+
				"\t\t},\n"+
				"\t)\n}\n",
			m.Name,
			lcFirst(model), m.Name, m.ParamList, model,
			keyHelperName,
			model,
			m.Name, m.CallArgs)
	}

	// ──── Write methods ────
	if plan.HasWrites {
		writeMethods.WriteString("\n// ──── Write ────\n")
		for _, w := range plan.Writes {
			if w.IsTodo {
				writeMethods.WriteString(renderWriteImplTodo(w, model, keyHelperName, plan.HasReads))
			} else {
				writeMethods.WriteString(renderWriteImpl(w, model, keyHelperName))
			}
		}
	}

	// Imports adapt to plan emptiness.
	var imports string
	switch {
	case plan.HasReads || plan.HasWrites:
		imports = fmt.Sprintf(`	"context"
	"fmt"

	"%[1]s/ent"
	"%[1]s/internal/dao"
	"%[1]s/internal/repo"`, g.Module)
	default:
		imports = fmt.Sprintf(`	"%[1]s/ent"
	"%[1]s/internal/dao"
	"%[1]s/internal/repo"`, g.Module)
	}

	content := fmt.Sprintf(`// Package repoimpl hosts concrete *Repo implementations.
package repoimpl

// Initial scaffolding generated by zctl (gen-rpc-ent-logic).
//
// This file is generated ONCE per table and then owned by you — regen
// will skip it as long as the file exists. Feel free to edit: tune cache
// keys, layer in secondary-key caches, add hash-tags for multi-tenant
// routing, fold in domain-specific invalidation, or rewrite a method
// from scratch.
//
// To re-pull a fresh scaffold (e.g. after the dao iface changed
// shape), delete this file and re-run gen-rpc-ent-logic.

import (
%[1]s
)

// ──── Cache key contract (table-local) ────
//
// Format (single primary-key form, parameterized via fmt verb):
//
//	<service>:repo:<table>:id:<id>
//
%[2]s
// %[3]sRepo wraps a pure-DB dao with the shared repo.Base backbone.
type %[3]sRepo struct {
	dao  dao.%[4]sDao
	base *repo.Base
}

// New%[4]sRepo wires the dao + the shared cache base.
func New%[4]sRepo(d dao.%[4]sDao, base *repo.Base) repo.%[4]sRepo {
	return &%[3]sRepo{dao: d, base: base}
}

// WithTx rebinds only the dao (cache base is process-wide).
func (r *%[3]sRepo) WithTx(tx *ent.Tx) repo.%[4]sRepo {
	return &%[3]sRepo{dao: r.dao.WithTx(tx), base: r.base}
}
%[5]s%[6]s`,
		imports,
		keyBlock.String(),
		lcFirst(model),
		model,
		getMethods.String(),
		writeMethods.String(),
	)

	return os.WriteFile(target, []byte(formatGoSource(content, target)), 0644)
}

// renderWriteImpl renders one write method implementation with cache invalidation.
func renderWriteImpl(w writeMethod, model, keyHelper string) string {
	var b strings.Builder
	switch {
	case w.Name == "Create":
		fmt.Fprintf(&b,
			"\n// Create delegates to dao.Create and invalidates the primary-key cache key.\n"+
				"func (r *%sRepo) Create(ctx context.Context, data *ent.%s) (*ent.%s, error) {\n"+
				"\tcreated, err := r.dao.Create(ctx, data)\n"+
				"\tif err != nil {\n\t\treturn nil, err\n\t}\n"+
				"\tr.base.InvalidateAfterWrite(ctx, %s(created.ID))\n"+
				"\treturn created, nil\n}\n",
			lcFirst(model), model, model, keyHelper)

	case w.Name == "CreateBatch":
		// CreateBatch takes []*ent.Model and returns error.
		fmt.Fprintf(&b,
			"\n// CreateBatch delegates to dao.CreateBatch and invalidates cache keys\n"+
				"// for all affected rows.\n"+
				"func (r *%sRepo) CreateBatch(ctx context.Context, %s) error {\n"+
				"\tif err := r.dao.CreateBatch(ctx, %s); err != nil {\n\t\treturn err\n\t}\n"+
				"\tkeys := make([]string, 0, len(%s))\n"+
				"\tfor _, item := range %s {\n"+
				"\t\tkeys = append(keys, %s(item.ID))\n"+
				"\t}\n"+
				"\tr.base.InvalidateAfterWrite(ctx, keys...)\n"+
				"\treturn nil\n}\n",
			lcFirst(model), w.ParamList,
			w.CallArgs,
			w.EntPtrParam, w.EntPtrParam,
			keyHelper)

	case strings.HasPrefix(w.Name, "Update"):
		fmt.Fprintf(&b,
			"\n// %s persists field changes and invalidates the primary-key cache key.\n"+
				"func (r *%sRepo) %s(ctx context.Context, %s) (*ent.%s, error) {\n"+
				"\tupdated, err := r.dao.%s(ctx, %s)\n"+
				"\tif err != nil {\n\t\treturn nil, err\n\t}\n"+
				"\tr.base.InvalidateAfterWrite(ctx, %s(%s))\n"+
				"\treturn updated, nil\n}\n",
			w.Name,
			lcFirst(model), w.Name, w.ParamList, model,
			w.Name, w.CallArgs,
			keyHelper, writeIDExpr(w))

	case strings.HasPrefix(w.Name, "Delete"):
		// Check if it's a batch delete by IDs (param is a slice type)
		if w.IDParam != "" && w.IDParam != "id" {
			// DeleteByIDs with a slice param — iterate and invalidate each
			fmt.Fprintf(&b,
				"\n// %s removes rows and invalidates the primary-key cache keys.\n"+
					"func (r *%sRepo) %s(ctx context.Context, %s) error {\n"+
					"\tif err := r.dao.%s(ctx, %s); err != nil {\n\t\treturn err\n\t}\n"+
					"\tkeys := make([]string, 0, len(%s))\n"+
					"\tfor _, id := range %s {\n"+
					"\t\tkeys = append(keys, %s(id))\n"+
					"\t}\n"+
					"\tr.base.InvalidateAfterWrite(ctx, keys...)\n"+
					"\treturn nil\n}\n",
				w.Name,
				lcFirst(model), w.Name, w.ParamList,
				w.Name, w.CallArgs,
				w.IDParam, w.IDParam,
				keyHelper)
		} else {
			// Single delete
			fmt.Fprintf(&b,
				"\n// %s removes the row and invalidates the primary-key cache key.\n"+
					"func (r *%sRepo) %s(ctx context.Context, %s) error {\n"+
					"\tif err := r.dao.%s(ctx, %s); err != nil {\n\t\treturn err\n\t}\n"+
					"\tr.base.InvalidateAfterWrite(ctx, %s(%s))\n"+
					"\treturn nil\n}\n",
				w.Name,
				lcFirst(model), w.Name, w.ParamList,
				w.Name, w.CallArgs,
				keyHelper, writeIDExpr(w))
		}
	default:
		// Unknown write method name that still matched a known shape — render generic.
		if w.ReturnsEntity {
			fmt.Fprintf(&b,
				"\n// %s delegates to dao and invalidates the primary-key cache key.\n"+
					"func (r *%sRepo) %s(ctx context.Context, %s) (*ent.%s, error) {\n"+
					"\tresult, err := r.dao.%s(ctx, %s)\n"+
					"\tif err != nil {\n\t\treturn nil, err\n\t}\n"+
					"\tr.base.InvalidateAfterWrite(ctx, %s(result.ID))\n"+
					"\treturn result, nil\n}\n",
				w.Name,
				lcFirst(model), w.Name, w.ParamList, model,
				w.Name, w.CallArgs,
				keyHelper)
		} else {
			fmt.Fprintf(&b,
				"\n// %s delegates to dao and invalidates the primary-key cache key.\n"+
					"func (r *%sRepo) %s(ctx context.Context, %s) error {\n"+
					"\tif err := r.dao.%s(ctx, %s); err != nil {\n\t\treturn err\n\t}\n"+
					"\t// TODO: derive cache key from method params\n"+
					"\treturn nil\n}\n",
				w.Name,
				lcFirst(model), w.Name, w.ParamList,
				w.Name, w.CallArgs)
		}
	}
	return b.String()
}

// renderWriteImplTodo renders a TODO placeholder for write methods whose
// signature can't be fully understood. The method is still registered so
// the code compiles.
func renderWriteImplTodo(w writeMethod, model, _ string, _ bool) string {
	var b strings.Builder
	retStr := renderReturnTypeImpl(w, model)

	// panic() is a statement that terminates control flow — Go does not
	// require a return after it. But for functions returning values, some
	// linters prefer an explicit unreachable return. We omit it since
	// gofmt/go vet accept panic as a terminal.
	fmt.Fprintf(&b,
		"\n// %s — TODO: implement cache invalidation logic.\n"+
			"// The dao body calls ent mutation methods, but the signature shape is not\n"+
			"// fully understood by the generator. Implement the delegation + cache\n"+
			"// invalidation manually.\n"+
			"func (r *%sRepo) %s(ctx context.Context, %s) %s {\n"+
			"\t// TODO: implement — delegate to r.dao.%s(ctx, %s) and invalidate cache\n"+
			"\tpanic(\"not implemented\")\n"+
			"}\n",
		w.Name,
		lcFirst(model), w.Name, w.ParamList, retStr,
		w.Name, w.CallArgs)
	return b.String()
}

// renderReturnTypeImpl renders the return type for an impl method.
func renderReturnTypeImpl(w writeMethod, model string) string {
	if w.IsTodo && len(w.ReturnTypes) > 0 {
		if len(w.ReturnTypes) == 1 {
			return w.ReturnTypes[0]
		}
		return "(" + strings.Join(w.ReturnTypes, ", ") + ")"
	}
	if w.ReturnsEntity {
		return "(*ent." + model + ", error)"
	}
	return "error"
}



// writeIDExpr returns the expression used inside InvalidateAfterWrite to
// derive the primary-key cache key for one write method.
//
// Strategy:
//   - Update*  → "updated.ID"  (post-call refreshed row, most accurate)
//   - Create   → "created.ID"  (caller side already uses this name)
//   - Delete*  → "<entPtr>.ID" if input has *ent.<Model>, else the bare id
//     param name when the dao takes (ctx, id <T>).
//
// Repo signatures are NOT reconstructed here; they are taken verbatim from
// w.ParamList / w.CallArgs (mirrors of the dao iface). That keeps repo
// methods compilable against the real dao even when the dao signature has
// secondary-key parameters in addition to the ent pointer (e.g.
// UpdateByAppCode(ctx, app_code string, data *ent.IamApp)).
func writeIDExpr(w writeMethod) string {
	if w.EntPtrParam != "" {
		switch {
		case strings.HasPrefix(w.Name, "Delete"):
			return w.EntPtrParam + ".ID"
		case strings.HasPrefix(w.Name, "Update"):
			return "updated.ID"
		default:
			return "created.ID"
		}
	}
	return w.IDParam
}

// ─── Mock file ──────────────────────────────────────────────────────────────

func writeRepoMock(g *GenContext, schema *load.Schema, plan repoPlan) error {
	dir := filepath.Join(g.Output, "internal", "repo", "mock")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	target := filepath.Join(dir, name.FileSnake(schema.Name)+"_repo_mock.go")

	model := schema.Name
	mockName := "Mock" + model + "Repo"

	withTxExample := "Xxx(...)"
	if plan.HasReads {
		withTxExample = plan.Reads[0].Name + "(...)"
	}

	var methods strings.Builder
	// WithTx falls back to the receiver so test code rarely has to stub it.
	fmt.Fprintf(&methods, "\n// WithTx mocks the WithTx method.\n")
	fmt.Fprintf(&methods, "//\n")
	fmt.Fprintf(&methods, "// When the test does not stub WithTx, we return the same mock so that calls\n")
	fmt.Fprintf(&methods, "// like `repo.WithTx(tx).%s` keep using the registered\n", withTxExample)
	fmt.Fprintf(&methods, "// expectations on the parent mock — this matches the dao-mock convention and\n")
	fmt.Fprintf(&methods, "// avoids forcing every test to stub a tx-bound clone.\n")
	fmt.Fprintf(&methods, "func (m *%s) WithTx(tx *ent.Tx) repo.%sRepo {\n", mockName, model)
	fmt.Fprintf(&methods, "\targs := m.Called(tx)\n")
	fmt.Fprintf(&methods, "\tif v := args.Get(0); v != nil {\n")
	fmt.Fprintf(&methods, "\t\tif d, ok := v.(repo.%sRepo); ok {\n", model)
	fmt.Fprintf(&methods, "\t\t\treturn d\n")
	fmt.Fprintf(&methods, "\t\t}\n")
	fmt.Fprintf(&methods, "\t}\n\treturn m\n}\n")

	for _, r := range plan.Reads {
		fmt.Fprintf(&methods,
			"\n// %s mocks the %s method.\n"+
				"func (m *%s) %s(ctx context.Context, %s) (*ent.%s, error) {\n"+
				"\targs := m.Called(ctx, %s)\n"+
				"\tif v := args.Get(0); v != nil {\n"+
				"\t\treturn v.(*ent.%s), args.Error(1)\n"+
				"\t}\n"+
				"\treturn nil, args.Error(1)\n}\n",
			r.Name, r.Name,
			mockName, r.Name, r.ParamList, model,
			r.CallArgs,
			model)
	}

	for _, w := range plan.Writes {
		paramSig := w.ParamList
		callArg := w.CallArgs
		retStr := renderReturnType(w, model)

		if w.IsTodo {
			// For TODO methods, render mock based on return types.
			methods.WriteString(renderMockForReturnTypes(w, mockName, model, paramSig, callArg))
		} else if !w.ReturnsEntity {
			// error-only return (e.g. DeleteByID)
			fmt.Fprintf(&methods,
				"\n// %s mocks the %s method.\n"+
					"func (m *%s) %s(ctx context.Context, %s) %s {\n"+
					"\targs := m.Called(ctx, %s)\n"+
					"\treturn args.Error(0)\n}\n",
				w.Name, w.Name,
				mockName, w.Name, paramSig, retStr,
				callArg)
		} else {
			// (*ent.Model, error) return
			fmt.Fprintf(&methods,
				"\n// %s mocks the %s method.\n"+
					"func (m *%s) %s(ctx context.Context, %s) %s {\n"+
					"\targs := m.Called(ctx, %s)\n"+
					"\tif v := args.Get(0); v != nil {\n"+
					"\t\treturn v.(*ent.%s), args.Error(1)\n"+
					"\t}\n"+
					"\treturn nil, args.Error(1)\n}\n",
				w.Name, w.Name,
				mockName, w.Name, paramSig, retStr,
				callArg,
				model)
		}
	}

	content := fmt.Sprintf(`package mock

// Code generated by zctl. DO NOT EDIT.
// Re-generated each time the Repo interface changes (gen-rpc-ent-logic).

import (
	"context"

	"%[1]s/ent"
	"%[1]s/internal/repo"

	"github.com/stretchr/testify/mock"
)

// %[2]s is a mock implementation of repo.%[3]sRepo.
//
// The Repo layer is the cache-aware aggregate accessor; tests for logic code
// typically pin this mock at svcCtx.%[3]sRepo and assert the cache-bound
// point-lookup / write-then-invalidate path without standing up redis or dao.
type %[2]s struct {
	mock.Mock
}

// Compile-time check that %[2]s implements repo.%[3]sRepo.
var _ repo.%[3]sRepo = (*%[2]s)(nil)
%[4]s`, g.Module, mockName, model, methods.String())

	return os.WriteFile(target, []byte(formatGoSource(content, target)), 0644)
}

// renderMockForReturnTypes renders a mock method for a TODO write method
// by inspecting its ReturnTypes slice.
func renderMockForReturnTypes(w writeMethod, mockName, model, paramSig, callArg string) string {
	var b strings.Builder
	retStr := renderReturnType(w, model)

	if len(w.ReturnTypes) == 1 && w.ReturnTypes[0] == "error" {
		fmt.Fprintf(&b,
			"\n// %s mocks the %s method.\n"+
				"func (m *%s) %s(ctx context.Context, %s) %s {\n"+
				"\targs := m.Called(ctx, %s)\n"+
				"\treturn args.Error(0)\n}\n",
			w.Name, w.Name,
			mockName, w.Name, paramSig, retStr,
			callArg)
	} else if len(w.ReturnTypes) == 2 && w.ReturnTypes[1] == "error" && strings.HasPrefix(w.ReturnTypes[0], "*") {
		// (*Something, error) pattern
		fmt.Fprintf(&b,
			"\n// %s mocks the %s method.\n"+
				"func (m *%s) %s(ctx context.Context, %s) %s {\n"+
				"\targs := m.Called(ctx, %s)\n"+
				"\tif v := args.Get(0); v != nil {\n"+
				"\t\treturn v.(%s), args.Error(1)\n"+
				"\t}\n"+
				"\treturn nil, args.Error(1)\n}\n",
			w.Name, w.Name,
			mockName, w.Name, paramSig, retStr,
			callArg,
			w.ReturnTypes[0])
	} else {
		// Generic fallback: just call m.Called and return zero values.
		// Build the return statement from mock args.
		var retParts []string
		for i, t := range w.ReturnTypes {
			if t == "error" {
				retParts = append(retParts, fmt.Sprintf("args.Error(%d)", i))
			} else if strings.HasPrefix(t, "*") || strings.HasPrefix(t, "[]") || strings.HasPrefix(t, "map[") {
				retParts = append(retParts, fmt.Sprintf("args.Get(%d).(%s)", i, t))
			} else {
				retParts = append(retParts, fmt.Sprintf("args.Get(%d).(%s)", i, t))
			}
		}
		fmt.Fprintf(&b,
			"\n// %s mocks the %s method.\n"+
				"func (m *%s) %s(ctx context.Context, %s) %s {\n"+
				"\targs := m.Called(ctx, %s)\n"+
				"\treturn %s\n}\n",
			w.Name, w.Name,
			mockName, w.Name, paramSig, retStr,
			callArg,
			strings.Join(retParts, ", "))
	}
	return b.String()
}

// ─── base.go ────────────────────────────────────────────────────────────────

// writeRepoBase materializes internal/repo/base.go. Idempotent: skips on
// existence so user customizations (e.g. swapping singleflight for an
// alternate stampede-control strategy, or tuning TTL constants) survive
// regeneration. Upgrades to the base template are user-driven (delete +
// regenerate) and intentionally not auto-overwritten — base.go is the one
// hand-tunable knob in this whole layer.
func writeRepoBase(abs, modulePath string) error {
	dir := filepath.Join(abs, "internal", "repo")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	target := filepath.Join(dir, "base.go")
	if pathx.FileExists(target) {
		return nil
	}
	content := fmt.Sprintf(repoBaseTemplate, modulePath)
	return os.WriteFile(target, []byte(formatGoSource(content, target)), 0644)
}

// repoBaseTemplate is the verbatim base.go body, with the module path as the
// only %s placeholder. Kept as a single literal so the template is readable
// at the source — every line here is also a line in the user's repo/base.go.
const repoBaseTemplate = `// Package repo provides a cache-aware aggregate access layer that wraps the
// pure-DB dao. Its design contract:
//
//  1. Read path: cache-aside via GetOrLoad. On miss the loader (dao) is called
//     and the JSON-serialized result is written back with a TTL.
//  2. Write path: write-then-double-delete. The first DEL runs synchronously
//     after the DB write; a second DEL is scheduled after a short delay to
//     evict any concurrently-rebuilt cache entry (the classic "delayed
//     double-delete" pattern).
//  3. Transaction-aware: when called inside entx.WithTx, BOTH deletes are
//     registered as after-commit callbacks. If the tx rolls back, no DEL
//     fires so the cache stays consistent with the (unchanged) DB.
//  4. Context-safe second delete: the deferred timer derives its own ctx with
//     a hard timeout, because the caller's ctx is usually canceled the moment
//     the RPC returns.
//
// Redis client choice: go-zero's *redis.Redis is used as the only handle so
// that single-node and cluster deployments share one type — switching is a
// config flip (RedisConf.Type), not a code change. zctl perf hooks are also
// already wired through the same client constructor.
//
// zctl-generated *Repo implementations only need: pick keys → call this Base.
package repo

import (
	"context"
	"encoding/json"
	"time"

	"%[1]s/internal/dao/entx"
	"%[1]s/pkg/ctxutil"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"golang.org/x/sync/singleflight"
)

// DefaultTTL / DefaultSecondDelay / DefaultSecondDelTimeout are the fallbacks
// when Base is constructed with non-positive values.
const (
	DefaultTTL              = 24 * time.Hour
	DefaultSecondDelay      = 500 * time.Millisecond
	DefaultSecondDelTimeout = 3 * time.Second
)

// Base is the shared backbone embedded by every concrete *Repo implementation.
// It is intentionally tiny and stateless beyond config so it is safe to share
// across all repos in a single process.
//
// The embedded singleflight.Group dedupes concurrent cache-miss loads on the
// SAME key — a textbook defense against cache stampede when a hot entry
// expires under load. Group is safe for concurrent use.
//
// Base MUST be passed around by *Base only; copying the struct copies the
// singleflight.Group, defeating its purpose.
type Base struct {
	rdb              *redis.Redis
	ttl              time.Duration
	secondDelay      time.Duration
	secondDelTimeout time.Duration
	sf               singleflight.Group
}

// NewBase builds a Base. Pass zero for any duration to use the default.
//
// rdb is intentionally typed as go-zero's *redis.Redis (not the raw
// go-redis client) so a single ServiceContext field handles both
// single-node and cluster deployments — go-zero's MustNewRedis dispatches
// internally on RedisConf.Type. zctl perf Redis-capture hooks are also
// applied at the constructor level, so once the *redis.Redis lands here
// it is already instrumented.
func NewBase(rdb *redis.Redis, ttl, secondDelay, secondDelTimeout time.Duration) *Base {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if secondDelay <= 0 {
		secondDelay = DefaultSecondDelay
	}
	if secondDelTimeout <= 0 {
		secondDelTimeout = DefaultSecondDelTimeout
	}
	return &Base{rdb: rdb, ttl: ttl, secondDelay: secondDelay, secondDelTimeout: secondDelTimeout}
}

// Loader is the DB fallback used by GetOrLoad on cache miss.
type Loader[T any] func(ctx context.Context) (T, error)

// GetOrLoad implements read-through caching with stampede protection.
//
// Transaction-aware short-circuit: when called inside entx.WithTx the cache
// path is skipped entirely (loader is invoked directly). Reasoning:
//   - In-tx reads must observe the caller's own uncommitted writes, which the
//     shared cache does not see.
//   - In-tx writes have NOT yet invalidated the cache (invalidation is
//     deferred to after-commit), so a cache hit here would return stale data.
//   - Tx scope is short and serialized; stampede protection is unnecessary.
//
// Outside a transaction the flow is:
//  1. GET key from redis; on hit unmarshal and return.
//  2. On miss: route through singleflight so only one goroutine per key
//     hits the DB; concurrent callers wait on the in-flight result.
//  3. The in-flight goroutine calls the loader, on success writes back to
//     redis with TTL, and returns the value to all waiters.
//  4. Loader errors propagate verbatim (no cache write).
//
// Generic T must be JSON-marshalable. Pointer types are recommended so a nil
// zero-value is distinguishable from a real "all fields zero" record.
//
// Note on the "key missing" signal: go-zero's GetCtx returns ("", nil) when
// the key does not exist (it intentionally swallows redis.Nil). We treat an
// empty string as miss; this matches go-zero idioms across the project.
func GetOrLoad[T any](ctx context.Context, b *Base, key string, loader Loader[T]) (T, error) {
	var zero T

	// In-tx: bypass cache + singleflight, read your own writes.
	if entx.InTx(ctx) {
		return loader(ctx)
	}
	// Defensive: caller passed a nil Base. Degrade to direct loader.
	if b == nil || b.rdb == nil {
		return loader(ctx)
	}

	// 1) try cache
	raw, gerr := b.rdb.GetCtx(ctx, key)
	switch {
	case gerr != nil:
		// transient redis failure: degrade gracefully to DB.
		ctxutil.L(ctx).Errorw("repo.GetOrLoad redis get failed",
			ctxutil.KV("key", key), ctxutil.ErrField(gerr))
	case raw == "":
		// miss: fall through to loader.
	default:
		var v T
		if uerr := json.Unmarshal([]byte(raw), &v); uerr == nil {
			return v, nil
		} else {
			// corrupted entry: log and fall through; the singleflight load
			// path will overwrite it.
			ctxutil.L(ctx).Errorw("repo.GetOrLoad unmarshal failed",
				ctxutil.KV("key", key), ctxutil.ErrField(uerr))
		}
	}

	// 2) collapsed load via singleflight; only one goroutine per key reaches DB.
	v, err, _ := b.sf.Do(key, func() (any, error) {
		val, lerr := loader(ctx)
		if lerr != nil {
			return val, lerr
		}
		// 3) write-back; failure must not break the read path.
		buf, merr := json.Marshal(val)
		if merr != nil {
			ctxutil.L(ctx).Errorw("repo.GetOrLoad marshal failed",
				ctxutil.KV("key", key), ctxutil.ErrField(merr))
			return val, nil
		}
		ttlSec := int(b.ttl / time.Second)
		if serr := b.rdb.SetexCtx(ctx, key, string(buf), ttlSec); serr != nil {
			ctxutil.L(ctx).Errorw("repo.GetOrLoad redis set failed",
				ctxutil.KV("key", key), ctxutil.ErrField(serr))
		}
		return val, nil
	})
	if err != nil {
		return zero, err
	}
	typed, ok := v.(T)
	if !ok {
		ctxutil.L(ctx).Errorw("repo.GetOrLoad singleflight type mismatch",
			ctxutil.KV("key", key))
		return loader(ctx)
	}
	return typed, nil
}

// GetOrLoadList implements read-through caching for list results with
// stampede protection. The only difference from GetOrLoad is:
//
//	Empty results (len == 0) are NEVER written to cache, so subsequent
//	calls for a key that returned an empty list will always go to DB.
//	Non-empty results are cached normally with TTL.
//
// This prevents the "negative cache pollution" problem where a key that
// happened to have no rows permanently blocks DB-fresh reads until TTL.
func GetOrLoadList[T any](ctx context.Context, b *Base, key string, loader Loader[[]T]) ([]T, error) {
	// In-tx: bypass cache + singleflight, read your own writes.
	if entx.InTx(ctx) {
		return loader(ctx)
	}
	// Defensive: caller passed a nil Base. Degrade to direct loader.
	if b == nil || b.rdb == nil {
		return loader(ctx)
	}

	// 1) try cache
	raw, gerr := b.rdb.GetCtx(ctx, key)
	switch {
	case gerr != nil:
		ctxutil.L(ctx).Errorw("repo.GetOrLoadList redis get failed",
			ctxutil.KV("key", key), ctxutil.ErrField(gerr))
	case raw == "":
		// miss: fall through to loader.
	default:
		var v []T
		uerr := json.Unmarshal([]byte(raw), &v)
		if uerr == nil {
			return v, nil
		}
		ctxutil.L(ctx).Errorw("repo.GetOrLoadList unmarshal failed",
			ctxutil.KV("key", key), ctxutil.ErrField(uerr))
	}

	// 2) collapsed load via singleflight.
	v, err, _ := b.sf.Do(key, func() (any, error) {
		val, lerr := loader(ctx)
		if lerr != nil {
			return val, lerr
		}
		// Only cache non-empty results.
		if len(val) == 0 {
			return val, nil
		}
		buf, merr := json.Marshal(val)
		if merr != nil {
			ctxutil.L(ctx).Errorw("repo.GetOrLoadList marshal failed",
				ctxutil.KV("key", key), ctxutil.ErrField(merr))
			return val, nil
		}
		ttlSec := int(b.ttl / time.Second)
		if serr := b.rdb.SetexCtx(ctx, key, string(buf), ttlSec); serr != nil {
			ctxutil.L(ctx).Errorw("repo.GetOrLoadList redis set failed",
				ctxutil.KV("key", key), ctxutil.ErrField(serr))
		}
		return val, nil
	})
	if err != nil {
		return nil, err
	}
	typed, ok := v.([]T)
	if !ok {
		ctxutil.L(ctx).Errorw("repo.GetOrLoadList singleflight type mismatch",
			ctxutil.KV("key", key))
		return loader(ctx)
	}
	return typed, nil
}

// WriteThrough writes a value to the cache without going through the
// read-loader. Used by Get* methods whose lookup parameter is not the
// primary key — they still benefit from caching subsequent id-keyed reads.
//
// Failure to cache is logged but not returned: the caller already has the
// value in hand and the read path must remain unaffected by cache health.
func WriteThrough[T any](ctx context.Context, b *Base, key string, val T) {
	if b == nil || b.rdb == nil {
		return
	}
	if entx.InTx(ctx) {
		// Tx scope: writing to cache here would publish uncommitted state.
		return
	}
	buf, merr := json.Marshal(val)
	if merr != nil {
		ctxutil.L(ctx).Errorw("repo.WriteThrough marshal failed",
			ctxutil.KV("key", key), ctxutil.ErrField(merr))
		return
	}
	ttlSec := int(b.ttl / time.Second)
	if serr := b.rdb.SetexCtx(ctx, key, string(buf), ttlSec); serr != nil {
		ctxutil.L(ctx).Errorw("repo.WriteThrough redis set failed",
			ctxutil.KV("key", key), ctxutil.ErrField(serr))
	}
}

// WriteThrough on Base lets call sites avoid passing the Base type
// parameter at every call.
func (b *Base) WriteThrough(ctx context.Context, key string, val any) {
	WriteThrough(ctx, b, key, val)
}

// InvalidateAfterWrite is the unified write-path entrypoint. It assumes the
// caller has ALREADY persisted the change to the DB (via dao.Update / Create
// / Delete). It then performs:
//
//	in-tx (entx.InTx(ctx) == true):
//	    register the double-delete onto entx after-commit queue, so it does
//	    not fire unless the tx commits.
//
//	no-tx:
//	    run the double-delete immediately on the caller goroutine.
//
// Note: any error from redis is logged but never returned, because the DB
// write has already succeeded and we must not fail the business call due to
// cache eviction issues. Stale entries are bounded by TTL anyway.
func (b *Base) InvalidateAfterWrite(ctx context.Context, keys ...string) {
	if b == nil || b.rdb == nil || len(keys) == 0 {
		return
	}
	if entx.InTx(ctx) {
		entx.RegisterAfterCommit(ctx, func() { b.doubleDelete(keys) })
		return
	}
	b.doubleDelete(keys)
}

// doubleDelete is the single source of truth for the delayed-double-delete
// pattern. Called both inline (no-tx) and via after-commit hook (in-tx); the
// only difference between the two paths is the entry, never the mechanism.
//
// Step 1 (synchronous): single multi-key DelCtx against a fresh bg ctx so a
// canceled caller ctx (already returned to the client) cannot abort it.
// go-zero's DelCtx natively accepts variadic keys and issues one DEL command
// against the server (DEL is atomic and tolerates absent keys).
//
// Step 2 (asynchronous): scheduled via time.AfterFunc. AfterFunc spawns its
// own goroutine on fire so we don't hand-roll one; ctx is detached from any
// RPC scope because the RPC has already returned.
func (b *Base) doubleDelete(keys []string) {
	{
		ctx, cancel := context.WithTimeout(context.Background(), b.secondDelTimeout)
		b.evict(ctx, keys)
		cancel()
	}
	time.AfterFunc(b.secondDelay, func() {
		ctx, cancel := context.WithTimeout(context.Background(), b.secondDelTimeout)
		defer cancel()
		b.evict(ctx, keys)
	})
}

// evict issues a multi-key DEL via go-zero. Missing keys are tolerated
// (DEL on absent keys returns 0 without an error).
func (b *Base) evict(ctx context.Context, keys []string) {
	if len(keys) == 0 {
		return
	}
	if _, err := b.rdb.DelCtx(ctx, keys...); err != nil {
		ctxutil.L(ctx).Errorw("repo.evict failed",
			ctxutil.KV("keys", keys), ctxutil.ErrField(err))
	}
}
`

// ─── helpers ────────────────────────────────────────────────────────────────

// verbForGoType picks the fmt verb for a scalar Go type.
func verbForGoType(goType string) string {
	switch goType {
	case "string":
		return "%s"
	case "bool":
		return "%t"
	case "float32", "float64":
		return "%g"
	case "":
		// Defensive: fall back to %d so the file at least compiles when the
		// id type couldn't be inferred (only happens on schemas without
		// GetByID, which also have no Reads — verb is unused in that case).
		return "%d"
	default:
		return "%d" // all int*/uint*
	}
}

// lcFirst lowercases the first rune; used for unexported impl-struct names.
// "CsAgentMember" → "csAgentMember".
func lcFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'A' && r[0] <= 'Z' {
		r[0] = r[0] + ('a' - 'A')
	}
	return string(r)
}

// formatGoSource runs gofmt on a Go source string. On parse failure it
// returns the raw text plus a stderr warning so the broken output is still
// inspectable.
//
// Why a private copy here rather than calling generator.formatGoSource:
// importing the parent generator package from a sub-package would create
// an import cycle (generator → repo → generator). The function is a
// 6-line wrapper so duplication is cheap.
//
// The post-format pass guarantees:
//
//  1. Output is gofmt-stable. Re-running zctl on already-generated repo
//     files produces byte-identical output, so `git diff` after a regen
//     never shows phantom whitespace churn even if a new dao file changes
//     the longest identifier in a column.
//
//  2. Field-alignment in struct literals (none today, but guarded for
//     future scaffold growth) is computed by gofmt itself rather than by
//     ad-hoc string concatenation — gofmt's tab-vs-single-space heuristic
//     flips when one column outgrows the others, so manual alignment was
//     guaranteed to drift the moment a longer model name was added.
func formatGoSource(code, filePath string) string {
	out, err := goformat.Source([]byte(code))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[zctl] gofmt failed for %s: %v (writing raw)\n", filePath, err)
		return code
	}
	return string(out)
}
