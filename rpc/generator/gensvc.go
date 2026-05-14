package generator

import (
	_ "embed"
	"fmt"
	"path/filepath"

	conf "github.com/qqz14/zctl/config"
	"github.com/qqz14/zctl/rpc/parser"
	"github.com/qqz14/zctl/util"
	"github.com/qqz14/zctl/util/format"
	"github.com/qqz14/zctl/util/pathx"
)

//go:embed svc.tpl
var svcTemplate string

// GenSvc generates the servicecontext.go file. The output is intentionally
// minimal: just Config + sentinel regions. ent infrastructure (DB/Tx/dsn/...)
// is filled in lazily by EnsureEntInfra() — invoked from any zctl subcommand
// that adds ent dependencies (rpc ent / rpc dao).
//
// Rationale: at scaffold time, neither the ent ORM package nor any *_dao.go
// exists yet, so referencing them would break `go build`. Keeping svc minimal
// lets `go build` succeed immediately after `zctl rpc new`.
func (g *Generator) GenSvc(ctx DirContext, _ parser.Proto, cfg *conf.Config, _ *ZRpcContext) error {
	dir := ctx.GetSvc()
	svcFilename, err := format.FileNamingFormat(cfg.NamingFormat, "service_context")
	if err != nil {
		return err
	}

	fileName := filepath.Join(dir.Filename, svcFilename+".go")
	text, err := pathx.LoadTemplate(category, svcTemplateFile, svcTemplate)
	if err != nil {
		return err
	}

	imports := fmt.Sprintf(`"%v"`, ctx.GetConfig().Package)

	return util.With("svc").GoFmt(true).Parse(text).SaveTo(map[string]any{
		"imports": imports,
	}, fileName, false)
}
