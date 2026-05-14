package gogen

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/qqz14/zctl/api/spec"
	"github.com/qqz14/zctl/config"
	"github.com/qqz14/zctl/internal/version"
	"github.com/qqz14/zctl/util/format"
	"github.com/qqz14/zctl/util/pathx"
	"github.com/qqz14/zctl/vars"
)

const contextFilename = "service_context"

//go:embed svc.tpl
var contextTemplate string

func genServiceContext(dir, rootPkg, projectPkg string, cfg *config.Config, api *spec.ApiSpec) error {
	filename, err := format.FileNamingFormat(cfg.NamingFormat, contextFilename)
	if err != nil {
		return err
	}

	var middlewareStr string
	var middlewareAssignment string
	middlewares := getMiddleware(api)

	for _, item := range middlewares {
		middlewareStr += fmt.Sprintf("%s rest.Middleware\n", item)
		name := strings.TrimSuffix(item, "Middleware") + "Middleware"
		middlewareAssignment += fmt.Sprintf("%s: %s,\n", item,
			fmt.Sprintf("middleware.New%s().%s", strings.Title(name), "Handle"))
	}

	configImport := "\"" + pathx.JoinPackages(rootPkg, configDir) + "\""
	if len(middlewareStr) > 0 {
		configImport += "\n\t\"" + pathx.JoinPackages(rootPkg, middlewareDir) + "\""
		configImport += fmt.Sprintf("\n\t\"%s/rest\"", vars.ProjectOpenSourceURL)
	}

	return genFile(fileGenConfig{
		dir:             dir,
		subdir:          contextDir,
		filename:        filename + ".go",
		templateName:    "contextTemplate",
		category:        category,
		templateFile:    contextTemplateFile,
		builtinTemplate: contextTemplate,
		data: map[string]string{
			"configImport":         configImport,
			"config":               "config.Config",
			"middleware":           middlewareStr,
			"middlewareAssignment": middlewareAssignment,
			"projectPkg":           projectPkg,
			"version":              version.BuildVersion,
		},
	})
}
