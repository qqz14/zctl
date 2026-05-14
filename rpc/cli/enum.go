package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/qqz14/zctl/rpc/generator"
	"github.com/spf13/cobra"
)

var (
	VarStringEnumName   string
	VarStringEnumValues string
)

// EnumGen handles `zctl rpc enum --name=OrderStatus --values=pending,paid,shipped,done`
// Generates pkg/enums/{snake_name}.go with constants, String(), IsValid(), Parse(), Values()
func EnumGen(_ *cobra.Command, _ []string) error {
	name := VarStringEnumName
	valuesStr := VarStringEnumValues

	if name == "" {
		return fmt.Errorf("--name is required")
	}
	if valuesStr == "" {
		return fmt.Errorf("--values is required (comma separated)")
	}

	values := strings.Split(valuesStr, ",")
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}

	if err := generator.GenEnumFile(".", name, values); err != nil {
		return err
	}

	// Refresh zctl-commands.md on every subcommand run
	abs, _ := filepath.Abs(".")
	generator.RefreshCommandsDoc(abs)
	return nil
}
