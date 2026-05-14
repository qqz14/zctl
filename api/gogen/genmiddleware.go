package gogen

import (
	_ "embed"
	"strings"

	"github.com/qqz14/zctl/api/spec"
	"github.com/qqz14/zctl/config"
	"github.com/qqz14/zctl/internal/version"
	"github.com/qqz14/zctl/util/format"
)

//go:embed middleware.tpl
var middlewareImplementCode string

func genMiddleware(dir string, cfg *config.Config, api *spec.ApiSpec) error {
	middlewares := getMiddleware(api)
	for _, item := range middlewares {
		middlewareFilename := strings.TrimSuffix(strings.ToLower(item), "middleware") + "_middleware"
		filename, err := format.FileNamingFormat(cfg.NamingFormat, middlewareFilename)
		if err != nil {
			return err
		}

		name := strings.TrimSuffix(item, "Middleware") + "Middleware"
		err = genFile(fileGenConfig{
			dir:             dir,
			subdir:          middlewareDir,
			filename:        filename + ".go",
			templateName:    "contextTemplate",
			category:        category,
			templateFile:    middlewareImplementCodeFile,
			builtinTemplate: middlewareImplementCode,
			data: map[string]string{
				"name":    strings.Title(name),
				"version": version.BuildVersion,
			},
		})
		if err != nil {
			return err
		}
	}

	return nil
}
