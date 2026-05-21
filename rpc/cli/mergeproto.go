package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qqz14/zctl/rpc/generator"
	"github.com/spf13/cobra"
)

// MergeProto merges all desc/**/*.proto into one root proto file
func MergeProto(_ *cobra.Command, _ []string) error {
	abs, err := filepath.Abs(".")
	if err != nil {
		return err
	}

	descDir := filepath.Join(abs, "desc")
	if _, err := os.Stat(descDir); os.IsNotExist(err) {
		return fmt.Errorf("desc/ directory not found in current directory")
	}

	// Find project name from directory name
	projectName := filepath.Base(abs)

	// Find root proto file (project name.proto)
	rootProto := filepath.Join(abs, projectName+".proto")

	// Also check if there's a Makefile with SERVICE_STYLE
	if data, err := os.ReadFile(filepath.Join(abs, "Makefile")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "SERVICE_STYLE=") {
				style := strings.TrimPrefix(line, "SERVICE_STYLE=")
				style = strings.TrimSpace(style)
				if style != "" {
					rootProto = filepath.Join(abs, style+".proto")
					projectName = style
				}
				break
			}
		}
	}

	fmt.Printf("[zctl] Merging desc/ → %s.proto ...\n", projectName)
	if err := generator.MergeDescProtos(descDir, rootProto, projectName); err != nil {
		return err
	}
	fmt.Printf("[zctl] Done. Merged into %s\n", rootProto)

	// 按需注入 google/api 依赖：当合并产物里出现 google.api.http annotation 时，
	// 自动把 proto/google/api/{annotations,http}.proto 写入项目（幂等：已存在则跳过），
	// 避免使用方手动从 googleapis 拷贝。机制与 buf/validate 类似但按需触发。
	if err := generator.EnsureGoogleAPIProtoIfReferenced(abs, rootProto); err != nil {
		fmt.Printf("[zctl] Warning: ensure google/api proto failed: %v\n", err)
	}

	// Auto-generate enum helpers from proto enum definitions in desc/
	if err := generator.GenEnumsFromProto(abs); err != nil {
		fmt.Printf("[zctl] Warning: enum generation failed: %v\n", err)
	}

	// Refresh zctl-commands.md on every subcommand run
	generator.RefreshCommandsDoc(abs)

	return nil
}
