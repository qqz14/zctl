package generator

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeromicro/go-zero/core/collection"
	conf "github.com/qqz14/zctl/config"
	"github.com/qqz14/zctl/rpc/parser"
	"github.com/qqz14/zctl/util"
	"github.com/qqz14/zctl/util/format"
	"github.com/qqz14/zctl/util/pathx"
	"github.com/qqz14/zctl/util/stringx"
)

const logicFunctionTemplate = `{{if .hasComment}}{{.comment}}{{end}}
func (l *{{.logicName}}) {{.method}} ({{if .hasReq}}in {{.request}}{{if .stream}},stream {{.streamBody}}{{end}}{{else}}stream {{.streamBody}}{{end}}) ({{if .hasReply}}{{.response}},{{end}} error) {
	// todo: add your logic here and delete this line
	
	return {{if .hasReply}}&{{.responseType}}{},{{end}} nil
}
`

//go:embed logic.tpl
var logicTemplate string

// GenLogic generates the logic file of the rpc service, which corresponds to the RPC definition items in proto.
func (g *Generator) GenLogic(ctx DirContext, proto parser.Proto, cfg *conf.Config,
	c *ZRpcContext) error {
	if !c.Multiple {
		return g.genLogicInCompatibility(ctx, proto, cfg)
	}

	return g.genLogicGroup(ctx, proto, cfg)
}

func (g *Generator) genLogicInCompatibility(ctx DirContext, proto parser.Proto,
	cfg *conf.Config) error {
	abs := ctx.GetMain().Filename
	return GenLogicFiles(abs, cfg.NamingFormat, false)
}

// GroupModel holds the two-level directory info for a proto rpc method.
type GroupModel struct {
	Group string // first-level subdirectory under desc/ (e.g. "user")
	Model string // proto filename without ext (e.g. "user" from "user.proto")
}

// BuildRpcGroupMap scans desc/ directory and maps rpc method names to their
// group + model (two-level directory structure).
// e.g. desc/user/user.proto contains "rpc createUser" → map["createUser"] = {Group:"user", Model:"user"}
// Exported so ent/gen.go and genserver.go can reuse it.
func BuildRpcGroupMap(descDir string) map[string]GroupModel {
	result := make(map[string]GroupModel)
	if _, err := os.Stat(descDir); os.IsNotExist(err) {
		return result
	}

	filepath.Walk(descDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		relPath, _ := filepath.Rel(descDir, path)
		parts := strings.Split(filepath.ToSlash(relPath), "/")
		if len(parts) < 2 {
			// root-level proto in desc/ (like base.proto)
			return nil
		}
		// group = first subdirectory (already DirName format)
		// model = proto filename without ext, normalized to DirName format for directory/package use
		group := parts[0]
		rawModel := strings.TrimSuffix(parts[len(parts)-1], ".proto")
		model := strings.ReplaceAll(rawModel, "_", "") // "user_info" → "userinfo"

		// Parse file for rpc lines
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "rpc ") {
				rest := strings.TrimPrefix(trimmed, "rpc ")
				rest = strings.TrimSpace(rest)
				idx := strings.IndexAny(rest, " (")
				if idx > 0 {
					methodName := rest[:idx]
					result[methodName] = GroupModel{Group: group, Model: model}
				}
			}
		}
		return nil
	})
	return result
}

func (g *Generator) genLogicGroup(ctx DirContext, proto parser.Proto, cfg *conf.Config) error {
	dir := ctx.GetLogic()
	pkgMap := parser.BuildProtoPackageMap(proto.ImportedProtos)
	for _, item := range proto.Service {
		serviceName := item.Name
		for _, rpc := range item.RPC {
			var (
				err           error
				filename      string
				logicName     string
				logicFilename string
				packageName   string
			)

			logicName = fmt.Sprintf("%sLogic", stringx.From(rpc.Name).ToCamel())
			childPkg, err := dir.GetChildPackage(serviceName)
			if err != nil {
				return err
			}

			serviceDir := filepath.Base(childPkg)
			nameJoin := fmt.Sprintf("%s_logic", serviceName)
			packageName = strings.ToLower(stringx.From(nameJoin).ToCamel())
			logicFilename, err = format.FileNamingFormat(cfg.NamingFormat, rpc.Name+"_logic")
			if err != nil {
				return err
			}

			filename = filepath.Join(dir.Filename, serviceDir, logicFilename+".go")
			functions, err := g.genLogicFunction(serviceName, proto.PbPackage, proto.GoPackage, logicName, rpc, pkgMap)
			if err != nil {
				return err
			}

			imports := collection.NewSet[string]()
			imports.Add(fmt.Sprintf(`"%v"`, ctx.GetSvc().Package))
			addLogicImports(imports, ctx.GetPb().Package, proto.PbPackage, proto.GoPackage, rpc, pkgMap)

			text, err := pathx.LoadTemplate(category, logicTemplateFileFile, logicTemplate)
			if err != nil {
				return err
			}

			if err = util.With("logic").GoFmt(true).Parse(text).SaveTo(map[string]any{
				"logicName":   logicName,
				"functions":   functions,
				"packageName": packageName,
				"imports":     strings.Join(imports.Keys(), pathx.NL),
			}, filename, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *Generator) genLogicFunction(serviceName, goPackage, mainGoPackage, logicName string,
	rpc *parser.RPC, pkgMap map[string]parser.ImportedProto) (string, error) {
	functions := make([]string, 0)
	text, err := pathx.LoadTemplate(category, logicFuncTemplateFileFile, logicFunctionTemplate)
	if err != nil {
		return "", err
	}

	comment := parser.GetComment(rpc.Doc())
	streamServer := fmt.Sprintf("%s.%s_%s%s", goPackage, parser.CamelCase(serviceName),
		parser.CamelCase(rpc.Name), "Server")

	reqRef := resolveRPCTypeRef(rpc.RequestType, goPackage, mainGoPackage, pkgMap)
	respRef := resolveRPCTypeRef(rpc.ReturnsType, goPackage, mainGoPackage, pkgMap)

	buffer, err := util.With("fun").Parse(text).Execute(map[string]any{
		"logicName":    logicName,
		"method":       parser.CamelCase(rpc.Name),
		"hasReq":       !rpc.StreamsRequest,
		"request":      "*" + reqRef.GoRef,
		"hasReply":     !rpc.StreamsRequest && !rpc.StreamsReturns,
		"response":     "*" + respRef.GoRef,
		"responseType": respRef.GoRef,
		"stream":       rpc.StreamsRequest || rpc.StreamsReturns,
		"streamBody":   streamServer,
		"hasComment":   len(comment) > 0,
		"comment":      comment,
	})
	if err != nil {
		return "", err
	}

	functions = append(functions, buffer.String())
	return strings.Join(functions, pathx.NL), nil
}

// addLogicImports adds the correct import paths to imports for a single RPC's
// logic file. The main pb package is only included when it is actually referenced
// (i.e. when the request or response type lives in that package, or the RPC streams).
func addLogicImports(imports *collection.Set[string], pbImportPath, goPackage, mainGoPackage string,
rpc *parser.RPC, pkgMap map[string]parser.ImportedProto) {
// Streaming RPCs always reference the main pb package (for the stream type).
if rpc.StreamsRequest || rpc.StreamsReturns {
imports.Add(fmt.Sprintf(`"%s"`, pbImportPath))
return
}

reqRef := resolveRPCTypeRef(rpc.RequestType, goPackage, mainGoPackage, pkgMap)
respRef := resolveRPCTypeRef(rpc.ReturnsType, goPackage, mainGoPackage, pkgMap)

// Add main pb import if any type ref is from the main package (no extra import path).
if reqRef.ImportPath == "" || respRef.ImportPath == "" {
imports.Add(fmt.Sprintf(`"%s"`, pbImportPath))
}
if reqRef.ImportPath != "" {
imports.Add(fmt.Sprintf(`"%s"`, reqRef.ImportPath))
}
if respRef.ImportPath != "" {
imports.Add(fmt.Sprintf(`"%s"`, respRef.ImportPath))
}
}
