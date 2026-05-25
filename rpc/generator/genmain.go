package generator

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"

	conf "github.com/qqz14/zctl/config"
	"github.com/qqz14/zctl/rpc/parser"
	"github.com/qqz14/zctl/util"
	"github.com/qqz14/zctl/util/pathx"
)

//go:embed main.tpl
var mainTemplate string

type MainServiceTemplateData struct {
	GRPCService string
	Service     string
	ServerPkg   string
	Pkg         string
}

// GenMain generates the main file of the rpc service, which is an rpc service program call entry
func (g *Generator) GenMain(ctx DirContext, proto parser.Proto, cfg *conf.Config,
	c *ZRpcContext) error {
	// File-name policy (see naming-spec.md):
	//   • The root main file ({input}.go) and the etc/{input}.yaml path must
	//     mirror the **user's raw input** verbatim.
	//   • The project root directory was created from that raw input by
	//     `zctl rpc new <name>`, so filepath.Base(WorkDir) recovers it.
	//   • cfg.NamingFormat (e.g. "go_zero") would re-snake-case the proto
	//     package ident and thus differ from the user input — we therefore
	//     ignore it for these two file names only.
	mainFilename := filepath.Base(ctx.GetMain().Filename)

	fileName := filepath.Join(ctx.GetMain().Filename, fmt.Sprintf("%v.go", mainFilename))
	imports := make([]string, 0)
	pbImport := fmt.Sprintf(`"%v"`, ctx.GetPb().Package)
	svcImport := fmt.Sprintf(`"%v"`, ctx.GetSvc().Package)
	configImport := fmt.Sprintf(`"%v"`, ctx.GetConfig().Package)
	imports = append(imports, configImport, pbImport, svcImport)

	var serviceNames []MainServiceTemplateData
	for _, e := range proto.Service {
		var (
			remoteImport string
			serverPkg    string
		)
		if !c.Multiple {
			serverPkg = "server"
			remoteImport = fmt.Sprintf(`"%v"`, ctx.GetServer().Package)
		} else {
			childPkg, err := ctx.GetServer().GetChildPackage(e.Name)
			if err != nil {
				return err
			}

			serverPkg = filepath.Base(childPkg + "Server")
			remoteImport = fmt.Sprintf(`%s "%v"`, serverPkg, childPkg)
		}
		imports = append(imports, remoteImport)
		// Normalize the service ident through ServiceGoIdent so that even when the
		// proto we read carries a stale "CsAgentRPC"-style name (e.g. produced by
		// an older zctl, or hand-written by the user with strcase initialism),
		// the symbols we reference here (`Register{X}Server` / `New{X}Server`)
		// always match what the current protoc-gen-go-grpc emits — i.e. "Rpc"
		// is **not** treated as an acronym.
		svcIdent := ServiceGoIdent(e.Name)
		serviceNames = append(serviceNames, MainServiceTemplateData{
			GRPCService: svcIdent,
			Service:     svcIdent,
			ServerPkg:   serverPkg,
			Pkg:         proto.PbPackage,
		})
	}

	text, err := pathx.LoadTemplate(category, mainTemplateFile, mainTemplate)
	if err != nil {
		return err
	}

	// etc/{input}.yaml — same raw-input rule as above.
	etcFileName := mainFilename

	return util.With("main").GoFmt(true).Parse(text).SaveTo(map[string]any{
		"serviceName":   etcFileName,
		"imports":       strings.Join(imports, pathx.NL),
		"pkg":           proto.PbPackage,
		"serviceNames":  serviceNames,
		"hasMiddleware": true,
		"middlewarePkg": ctx.GetMain().Package + "/internal/middleware",
		"i18nPkg":       ctx.GetMain().Package + "/pkg/i18n",
		// domainName: 用于注册 DomainInterceptor 时标识本服务的 domain。
		// 取 go-module 路径的最后一段（与 go.mod 中 module 名一致），
		// e.g. "github.com/qqz14/passport" → "passport"。
		"domainName": filepath.Base(ctx.GetMain().Package),
	}, fileName, false)
}
