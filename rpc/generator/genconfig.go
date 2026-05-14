package generator

import (
	_ "embed"
	"os"
	"path/filepath"

	conf "github.com/qqz14/zctl/config"
	"github.com/qqz14/zctl/rpc/parser"
	"github.com/qqz14/zctl/util/format"
	"github.com/qqz14/zctl/util/pathx"
)

//go:embed config.tpl
var configTemplate string

// GenConfig generates the configuration structure definition file of the rpc service.
func (g *Generator) GenConfig(ctx DirContext, _ parser.Proto, cfg *conf.Config, zctx *ZRpcContext) error {
	dir := ctx.GetConfig()
	configFilename, err := format.FileNamingFormat(cfg.NamingFormat, "config")
	if err != nil {
		return err
	}

	fileName := filepath.Join(dir.Filename, configFilename+".go")
	if pathx.FileExists(fileName) {
		return nil
	}

	text, err := pathx.LoadTemplate(category, configTemplateFileFile, configTemplate)
	if err != nil {
		return err
	}

	return os.WriteFile(fileName, []byte(text), os.ModePerm)
}
