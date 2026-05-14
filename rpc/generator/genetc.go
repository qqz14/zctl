package generator

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"

	conf "github.com/qqz14/zctl/config"
	"github.com/qqz14/zctl/rpc/parser"
	"github.com/qqz14/zctl/util"
	"github.com/qqz14/zctl/util/format"
	"github.com/qqz14/zctl/util/pathx"
	"github.com/qqz14/zctl/util/stringx"
)

//go:embed etc.tpl
var etcTemplate string

// GenEtc generates the yaml configuration file of the rpc service.
func (g *Generator) GenEtc(ctx DirContext, _ parser.Proto, cfg *conf.Config, zctx *ZRpcContext) error {
	dir := ctx.GetEtc()
	etcFilename, err := format.FileNamingFormat(cfg.NamingFormat, ctx.GetServiceName().Source())
	if err != nil {
		return err
	}

	fileName := filepath.Join(dir.Filename, fmt.Sprintf("%v.yaml", etcFilename))

	text, err := pathx.LoadTemplate(category, etcTemplateFileFile, etcTemplate)
	if err != nil {
		return err
	}

	port := 8080
	if zctx != nil && zctx.Port > 0 {
		port = zctx.Port
	}

	return util.With("etc").Parse(text).SaveTo(map[string]any{
		"serviceName": strings.ToLower(stringx.From(ctx.GetServiceName().Source()).ToCamel()),
		"port":        port,
	}, fileName, false)
}
