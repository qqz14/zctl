package cli

import (
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/qqz14/zctl/rpc/generator"
	"github.com/qqz14/zctl/rpc/generator/ent"
)

var (
	VarStringSchema      string
	VarStringServiceName string
	VarStringModelName   string
	VarStringGroupName   string
	VarBoolOverwrite     bool
)

// EntCRUDLogic generates CRUD logic + DAO from ent schema.
func EntCRUDLogic(_ *cobra.Command, _ []string) error {
	g := &ent.GenContext{
		Schema:      VarStringSchema,
		Output:      ".",
		ServiceName: VarStringServiceName,
		Style:       VarStringStyle,
		ModelName:   VarStringModelName,
		GroupName:   VarStringGroupName,
		Overwrite:   VarBoolOverwrite,
	}

	if err := g.Validate(); err != nil {
		return err
	}

	if err := ent.GenEntLogic(g); err != nil {
		return err
	}

	// Refresh zctl-commands.md on every subcommand run
	abs, _ := filepath.Abs(".")
	generator.RefreshCommandsDoc(abs)
	return nil
}
