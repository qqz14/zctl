package generator

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	conf "github.com/qqz14/zctl/config"
	"github.com/qqz14/zctl/rpc/parser"
	"github.com/qqz14/zctl/util"
	"github.com/qqz14/zctl/util/name"
	"github.com/qqz14/zctl/util/pathx"
	"github.com/qqz14/zctl/util/stringx"
	"github.com/zeromicro/go-zero/core/collection"
)

const functionTemplate = `
{{if .hasComment}}{{.comment}}{{end}}
func (s *{{.server}}Server) {{.method}} ({{if .notStream}}ctx context.Context,{{if .hasReq}} in {{.request}}{{end}}{{else}}{{if .hasReq}} in {{.request}},{{end}}stream {{.streamBody}}{{end}}) ({{if .notStream}}{{.response}},{{end}}error) {
	l := {{.logicPkg}}.New{{.logicName}}({{if .notStream}}ctx,{{else}}stream.Context(),{{end}}s.svcCtx)
	return l.{{.method}}({{if .hasReq}}in{{if .stream}} ,stream{{end}}{{else}}{{if .stream}}stream{{end}}{{end}})
}
`

//go:embed server.tpl
var serverTemplate string

// GenServer generates rpc server file, which is an implementation of rpc server
func (g *Generator) GenServer(ctx DirContext, proto parser.Proto, cfg *conf.Config,
	c *ZRpcContext) error {
	if !c.Multiple {
		return g.genServerInCompatibility(ctx, proto, cfg, c)
	}

	return g.genServerGroup(ctx, proto, cfg)
}

func (g *Generator) genServerGroup(ctx DirContext, proto parser.Proto, cfg *conf.Config) error {
	dir := ctx.GetServer()
	pkgMap := parser.BuildProtoPackageMap(proto.ImportedProtos)
	for _, service := range proto.Service {
		var (
			serverFile  string
			logicImport string
		)

		serverFilename := name.RpcServerFileName(service.Name)

		serverChildPkg, err := dir.GetChildPackage(service.Name)
		if err != nil {
			return err
		}

		logicChildPkg, err := ctx.GetLogic().GetChildPackage(service.Name)
		if err != nil {
			return err
		}

		serverDir := filepath.Base(serverChildPkg)
		logicImport = fmt.Sprintf(`"%v"`, logicChildPkg)
		serverFile = filepath.Join(dir.Filename, serverDir, serverFilename+".go")

		svcImport := fmt.Sprintf(`"%v"`, ctx.GetSvc().Package)
		pbImport := fmt.Sprintf(`"%v"`, ctx.GetPb().Package)

		imports := collection.NewSet[string]()
		imports.Add(logicImport, svcImport, pbImport)

		head := util.GetHead(proto.Name)

		funcList, extraImportPaths, err := g.genFunctions(proto.PbPackage, proto.GoPackage, service, true, pkgMap)
		if err != nil {
			return err
		}
		for _, imp := range extraImportPaths {
			imports.Add(fmt.Sprintf(`"%s"`, imp))
		}

		text, err := pathx.LoadTemplate(category, serverTemplateFile, serverTemplate)
		if err != nil {
			return err
		}

		notStream := false
		for _, rpc := range service.RPC {
			if !rpc.StreamsRequest && !rpc.StreamsReturns {
				notStream = true
				break
			}
		}

		// Normalize the service ident through ServiceGoIdent so that the symbols
		// we reference (`type {X}Server`, `New{X}Server`, `Unimplemented{X}Server`)
		// always match what the current protoc-gen-go-grpc emits — i.e. "Rpc" is
		// **not** treated as an acronym, even when proto.Service[0].Name is a stale
		// "CsAgentRPC" produced by an older zctl.
		svcIdent := name.ServiceGoIdent(service.Name)

		if err = util.With("server").GoFmt(true).Parse(text).SaveTo(map[string]any{
			"head": head,
			"unimplementedServer": fmt.Sprintf("%s.Unimplemented%sServer", proto.PbPackage,
				svcIdent),
			"server":    svcIdent,
			"imports":   strings.Join(imports.Keys(), pathx.NL),
			"funcs":     strings.Join(funcList, pathx.NL),
			"notStream": notStream,
		}, serverFile, true); err != nil {
			return err
		}
	}
	return nil
}

func (g *Generator) genServerInCompatibility(ctx DirContext, proto parser.Proto,
	cfg *conf.Config, c *ZRpcContext) error {
	dir := ctx.GetServer()
	svcImport := fmt.Sprintf(`"%v"`, ctx.GetSvc().Package)
	pbImport := fmt.Sprintf(`"%v"`, ctx.GetPb().Package)

	imports := collection.NewSet[string]()
	imports.Add(svcImport, pbImport)

	// Build rpcName → group/model mapping from desc/ directory (shared function)
	descDir := filepath.Join(ctx.GetMain().Filename, "desc")
	rpcGroupMap := BuildRpcGroupMap(descDir)

	// Collect logic imports: one per group/model combination.
	// When different groups share the same model name (e.g. cid/register and user/register),
	// disambiguate the import alias by prefixing with the group name: cidRegister, userRegister.
	logicBasePkg := ctx.GetLogic().Package

	// Step 1: count how many distinct groups use each model name
	modelGroupCount := make(map[string]int) // model → number of distinct group/model combos
	modelsUsed := collection.NewSet[string]()
	for _, rpc := range proto.Service[0].RPC {
		gm := rpcGroupMap[rpc.Name]
		if gm.Group == "" {
			continue
		}
		key := gm.Group + "/" + gm.Model
		if !modelsUsed.Contains(key) {
			modelsUsed.Add(key)
			modelGroupCount[gm.Model]++
		}
	}

	// Step 2: build aliasMap (group/model → alias) and imports
	// aliasMap is also used by genFunctionsWithGroup to reference the correct package alias.
	aliasMap := make(map[string]string) // "group/model" → alias
	modelsUsed2 := collection.NewSet[string]()
	for _, rpc := range proto.Service[0].RPC {
		gm := rpcGroupMap[rpc.Name]
		if gm.Group == "" {
			imports.Add(fmt.Sprintf(`"%v"`, logicBasePkg))
			continue
		}
		key := gm.Group + "/" + gm.Model
		if !modelsUsed2.Contains(key) {
			modelsUsed2.Add(key)
			alias := gm.Model
			if modelGroupCount[gm.Model] > 1 {
				// Disambiguate: cidRegister, userRegister
				alias = gm.Group + upperFirst(gm.Model)
			}
			aliasMap[key] = alias
			imports.Add(fmt.Sprintf(`%s "%v/%v/%v"`, alias, logicBasePkg, gm.Group, gm.Model))
		}
	}

	pkgMap := parser.BuildProtoPackageMap(proto.ImportedProtos)
	head := util.GetHead(proto.Name)
	service := proto.Service[0]
	// File-name policy (single-service compat mode only):
	//   internal/server/{input}_server.go — mirror the user's raw input verbatim,
	//   matching the root {input}.go / {input}.proto convention.
	//   See naming-spec.md.
	serverFilename := filepath.Base(ctx.GetMain().Filename) + "_server"
	_ = cfg // cfg.NamingFormat intentionally ignored here

	serverFile := filepath.Join(dir.Filename, serverFilename+".go")
	funcList, extraImportPaths, err := g.genFunctionsWithGroup(proto.PbPackage, proto.GoPackage, service, pkgMap, rpcGroupMap, aliasMap)
	if err != nil {
		return err
	}
	for _, imp := range extraImportPaths {
		imports.Add(fmt.Sprintf(`"%s"`, imp))
	}

	text, err := pathx.LoadTemplate(category, serverTemplateFile, serverTemplate)
	if err != nil {
		return err
	}

	notStream := false
	for _, rpc := range service.RPC {
		if !rpc.StreamsRequest && !rpc.StreamsReturns {
			notStream = true
			break
		}
	}

	// Normalize the service ident — see comment in genServerGroup above.
	svcIdent := name.ServiceGoIdent(service.Name)

	return util.With("server").GoFmt(true).Parse(text).SaveTo(map[string]any{
		"head": head,
		"unimplementedServer": fmt.Sprintf("%s.Unimplemented%sServer", proto.PbPackage,
			svcIdent),
		"server":    svcIdent,
		"imports":   strings.Join(imports.Keys(), pathx.NL),
		"funcs":     strings.Join(funcList, pathx.NL),
		"notStream": notStream,
	}, serverFile, true)
}

func (g *Generator) genFunctions(goPackage, mainGoPackage string, service parser.Service,
	multiple bool, pkgMap map[string]parser.ImportedProto) ([]string, []string, error) {
	var (
		functionList []string
		logicPkg     string
		extraImports []string
	)
	for _, rpc := range service.RPC {
		text, err := pathx.LoadTemplate(category, serverFuncTemplateFile, functionTemplate)
		if err != nil {
			return nil, nil, err
		}

		var logicName string
		if !multiple {
			logicPkg = "logic"
			logicName = fmt.Sprintf("%sLogic", stringx.From(rpc.Name).ToCamel())
		} else {
			nameJoin := fmt.Sprintf("%s_logic", service.Name)
			logicPkg = strings.ToLower(stringx.From(nameJoin).ToCamel())
			logicName = fmt.Sprintf("%sLogic", stringx.From(rpc.Name).ToCamel())
		}

		comment := parser.GetComment(rpc.Doc())
		// `streamServer` references the protoc-gen-go-grpc emitted symbol
		// `{Service}_{Method}Server`; normalize the service ident through
		// ServiceGoIdent so it matches what protoc actually emits even when
		// service.Name is a stale "CsAgentRPC".
		svcIdent := name.ServiceGoIdent(service.Name)
		streamServer := fmt.Sprintf("%s.%s_%s%s", goPackage, svcIdent,
			parser.CamelCase(rpc.Name), "Server")

		reqRef := resolveRPCTypeRef(rpc.RequestType, goPackage, mainGoPackage, pkgMap)
		respRef := resolveRPCTypeRef(rpc.ReturnsType, goPackage, mainGoPackage, pkgMap)
		if reqRef.ImportPath != "" {
			extraImports = append(extraImports, reqRef.ImportPath)
		}
		if respRef.ImportPath != "" {
			extraImports = append(extraImports, respRef.ImportPath)
		}

		buffer, err := util.With("func").Parse(text).Execute(map[string]any{
			"server":     svcIdent,
			"logicName":  logicName,
			"method":     parser.CamelCase(rpc.Name),
			"request":    "*" + reqRef.GoRef,
			"response":   "*" + respRef.GoRef,
			"hasComment": len(comment) > 0,
			"comment":    comment,
			"hasReq":     !rpc.StreamsRequest,
			"stream":     rpc.StreamsRequest || rpc.StreamsReturns,
			"notStream":  !rpc.StreamsRequest && !rpc.StreamsReturns,
			"streamBody": streamServer,
			"logicPkg":   logicPkg,
		})
		if err != nil {
			return nil, nil, err
		}

		functionList = append(functionList, buffer.String())
	}
	return functionList, extraImports, nil
}

// genFunctionsWithGroup generates server functions using group/model-based logic package names.
// aliasMap maps "group/model" keys to their import aliases (may be disambiguated).
func (g *Generator) genFunctionsWithGroup(goPackage, mainGoPackage string, service parser.Service,
	pkgMap map[string]parser.ImportedProto, rpcGroupMap map[string]GroupModel, aliasMap map[string]string) ([]string, []string, error) {
	var (
		functionList []string
		extraImports []string
	)
	for _, rpc := range service.RPC {
		text, err := pathx.LoadTemplate(category, serverFuncTemplateFile, functionTemplate)
		if err != nil {
			return nil, nil, err
		}

		logicName := fmt.Sprintf("%sLogic", stringx.From(rpc.Name).ToCamel())
		// Determine logicPkg from aliasMap (which handles disambiguation)
		gm := rpcGroupMap[rpc.Name]
		logicPkg := "logic"
		if gm.Group != "" && gm.Model != "" {
			key := gm.Group + "/" + gm.Model
			if alias, ok := aliasMap[key]; ok {
				logicPkg = alias
			}
		}

		comment := parser.GetComment(rpc.Doc())
		// `streamServer` references the protoc-gen-go-grpc emitted symbol
		// `{Service}_{Method}Server`; normalize the service ident through
		// ServiceGoIdent so it matches what protoc actually emits even when
		// service.Name is a stale "CsAgentRPC".
		svcIdent := name.ServiceGoIdent(service.Name)
		streamServer := fmt.Sprintf("%s.%s_%s%s", goPackage, svcIdent,
			parser.CamelCase(rpc.Name), "Server")

		reqRef := resolveRPCTypeRef(rpc.RequestType, goPackage, mainGoPackage, pkgMap)
		respRef := resolveRPCTypeRef(rpc.ReturnsType, goPackage, mainGoPackage, pkgMap)
		if reqRef.ImportPath != "" {
			extraImports = append(extraImports, reqRef.ImportPath)
		}
		if respRef.ImportPath != "" {
			extraImports = append(extraImports, respRef.ImportPath)
		}

		buffer, err := util.With("func").Parse(text).Execute(map[string]any{
			"server":     svcIdent,
			"logicName":  logicName,
			"method":     parser.CamelCase(rpc.Name),
			"request":    "*" + reqRef.GoRef,
			"response":   "*" + respRef.GoRef,
			"hasComment": len(comment) > 0,
			"comment":    comment,
			"hasReq":     !rpc.StreamsRequest,
			"stream":     rpc.StreamsRequest || rpc.StreamsReturns,
			"notStream":  !rpc.StreamsRequest && !rpc.StreamsReturns,
			"streamBody": streamServer,
			"logicPkg":   logicPkg,
		})
		if err != nil {
			return nil, nil, err
		}

		functionList = append(functionList, buffer.String())
	}
	return functionList, extraImports, nil
}

// upperFirst returns s with its first rune converted to uppercase.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
