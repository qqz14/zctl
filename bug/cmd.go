package bug

import (
	"github.com/spf13/cobra"
	"github.com/qqz14/zctl/internal/cobrax"
)

// Cmd describes a bug command.
var Cmd = cobrax.NewCommand("bug", cobrax.WithRunE(runE), cobrax.WithArgs(cobra.NoArgs))
