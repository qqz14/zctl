package env

import (
	"fmt"
	"strings"
	"time"

	"github.com/qqz14/zctl/pkg/env"
	"github.com/qqz14/zctl/pkg/protoc"
	"github.com/qqz14/zctl/pkg/protocgengo"
	"github.com/qqz14/zctl/pkg/protocgengogrpc"
	"github.com/qqz14/zctl/util/console"
	"github.com/spf13/cobra"
)

type bin struct {
	name   string
	exists bool
	get    func(cacheDir string) (string, error)
}

var bins = []bin{
	{
		name:   "protoc",
		exists: protoc.Exists(),
		get:    protoc.Install,
	},
	{
		name:   "protoc-gen-go",
		exists: protocgengo.Exists(),
		get:    protocgengo.Install,
	},
	{
		name:   "protoc-gen-go-grpc",
		exists: protocgengogrpc.Exists(),
		get:    protocgengogrpc.Install,
	},
}

func check(_ *cobra.Command, _ []string) error {
	return Prepare(boolVarInstall, boolVarForce, boolVarVerbose)
}

func Prepare(install, force, verbose bool) error {
	log := console.NewColorConsole(verbose)
	pending := true
	log.Info("[zctl-env]: preparing to check env")
	defer func() {
		if p := recover(); p != nil {
			log.Error("%+v", p)
			return
		}
		if pending {
			log.Success("\n[zctl-env]: congratulations! your zctl environment is ready!")
		} else {
			log.Error(`
[zctl-env]: check env finish, some dependencies is not found in PATH, you can execute
command 'zctl env check --install' to install it, for details, please execute command 
'zctl env check --help'`)
		}
	}()
	for _, e := range bins {
		time.Sleep(200 * time.Millisecond)
		log.Info("")
		log.Info("[zctl-env]: looking up %q", e.name)
		if e.exists {
			log.Success("[zctl-env]: %q is installed", e.name)
			continue
		}
		log.Warning("[zctl-env]: %q is not found in PATH", e.name)
		if install {
			install := func() {
				log.Info("[zctl-env]: preparing to install %q", e.name)
				path, err := e.get(env.Get(env.GoctlCache))
				if err != nil {
					log.Error("[zctl-env]: an error interrupted the installation: %+v", err)
					pending = false
				} else {
					log.Success("[zctl-env]: %q is already installed in %q", e.name, path)
				}
			}
			if force {
				install()
				continue
			}
			console.Info("[zctl-env]: do you want to install %q [y: YES, n: No]", e.name)
			for {
				var in string
				fmt.Scanln(&in)
				var brk bool
				switch {
				case strings.EqualFold(in, "y"):
					install()
					brk = true
				case strings.EqualFold(in, "n"):
					pending = false
					console.Info("[zctl-env]: %q installation is ignored", e.name)
					brk = true
				default:
					console.Error("[zctl-env]: invalid input, input 'y' for yes, 'n' for no")
				}
				if brk {
					break
				}
			}
		} else {
			pending = false
		}
	}
	return nil
}
