package upgrade

import (
	"fmt"
	"runtime"

	"github.com/qqz14/zctl/rpc/execx"
	"github.com/spf13/cobra"
)

// upgrade gets the latest zctl by
// go install github.com/qqz14/zctl@latest
func upgrade(_ *cobra.Command, _ []string) error {
	cmd := `go install github.com/qqz14/zctl@latest`
	if runtime.GOOS == "windows" {
		cmd = `go install github.com/qqz14/zctl@latest`
	}
	info, err := execx.Run(cmd, "")
	if err != nil {
		return err
	}

	fmt.Print(info)
	return nil
}
