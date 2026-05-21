package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qqz14/zctl/rpc/generator"
	"github.com/qqz14/zctl/util"
	"github.com/qqz14/zctl/util/console"
	"github.com/qqz14/zctl/util/pathx"
	"github.com/spf13/cobra"
)

var (
	// VarStringOutput describes the output.
	VarStringOutput string
	// VarStringHome describes the zctl home.
	VarStringHome string
	// VarStringRemote describes the remote git repository.
	VarStringRemote string
	// VarStringBranch describes the git branch.
	VarStringBranch string
	// VarStringSliceGoOut describes the go output.
	VarStringSliceGoOut []string
	// VarStringSliceGoGRPCOut describes the grpc output.
	VarStringSliceGoGRPCOut []string
	// VarStringSlicePlugin describes the protoc plugin.
	VarStringSlicePlugin []string
	// VarStringSliceProtoPath describes the proto path.
	VarStringSliceProtoPath []string
	// VarStringSliceGoOpt describes the go options.
	VarStringSliceGoOpt []string
	// VarStringSliceGoGRPCOpt describes the grpc options.
	VarStringSliceGoGRPCOpt []string
	// VarStringStyle describes the style of output files.
	VarStringStyle string
	// VarStringZRPCOut describes the zRPC output.
	VarStringZRPCOut string
	// VarBoolIdea describes whether idea or not
	VarBoolIdea bool
	// VarBoolVerbose describes whether verbose.
	VarBoolVerbose bool
	// VarBoolMultiple describes whether support generating multiple rpc services or not.
	VarBoolMultiple bool
	// VarBoolClient describes whether to generate rpc client
	VarBoolClient bool
	// VarStringModule describes the module name for go.mod.
	VarStringModule string
	// VarBoolNameFromFilename describes whether to derive service name from proto filename
	// instead of the proto package name. Default is false (uses package name).
	VarBoolNameFromFilename bool
	// VarIntPort describes the service port
	VarIntPort int
)

// RPCNew is to generate rpc greet service, this greet service can speed
// up your understanding of the zrpc service structure
func RPCNew(_ *cobra.Command, args []string) error {
	rpcname := args[0]
	ext := filepath.Ext(rpcname)
	if len(ext) > 0 {
		return fmt.Errorf("unexpected ext: %s", ext)
	}
	style := VarStringStyle
	home := VarStringHome
	remote := VarStringRemote
	branch := VarStringBranch
	verbose := VarBoolVerbose
	if len(remote) > 0 {
		repo, _ := util.CloneIntoGitHome(remote, branch)
		if len(repo) > 0 {
			home = repo
		}
	}
	if len(home) > 0 {
		pathx.RegisterGoctlHome(home)
	}

	projectDir := filepath.Join(".", rpcname)

	// 1. Generate desc/base.proto (only PageInfo)
	descDir := filepath.Join(projectDir, "desc")
	baseProto := filepath.Join(descDir, "base.proto")
	baseProtoAbs, err := filepath.Abs(baseProto)
	if err != nil {
		return err
	}
	err = generator.ProtoTmpl(baseProtoAbs)
	if err != nil {
		return err
	}

	// 2. Generate desc/base/ping.proto (common messages + Ping rpc)
	// Use generator.GoPascal so dashed names like "cs-agent-rpc" become a valid PascalCase
	// proto3 ident ("CsAgentRpc"), matching what MergeDescProtos emits later.
	serviceName := generator.GoPascal(rpcname)
	pingDirRel := filepath.Join(descDir, "ping")
	pingDirAbs, err := filepath.Abs(pingDirRel)
	if err != nil {
		return err
	}
	if err := pathx.MkdirIfNotExist(pingDirAbs); err != nil {
		return err
	}
	pingProto := fmt.Sprintf(`syntax = "proto3";

message PingResp {
  string version = 1;
}

service %s {
  rpc Ping(Empty) returns(PingResp);
}
`, serviceName)
	pingProtoPath := filepath.Join(pingDirAbs, "ping.proto")
	if err := os.WriteFile(pingProtoPath, []byte(pingProto), 0644); err != nil {
		return err
	}

	// 3. Merge all desc/**/*.proto into root {name}.proto
	rootProto := filepath.Join(projectDir, rpcname+".proto")
	rootProtoAbs, err := filepath.Abs(rootProto)
	if err != nil {
		return err
	}
	descDirAbs, err := filepath.Abs(descDir)
	if err != nil {
		return err
	}
	if err := generator.MergeDescProtos(descDirAbs, rootProtoAbs, rpcname); err != nil {
		return err
	}

	// 3. Setup types/ directory for pb output
	typesDir := filepath.Join(projectDir, "types")
	typesAbs, err := filepath.Abs(typesDir)
	if err != nil {
		return err
	}
	if err := pathx.MkdirIfNotExist(typesAbs); err != nil {
		return err
	}

	projectDirAbs, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}

	protoDepDir := filepath.Join(projectDirAbs, "proto")

	var ctx generator.ZRpcContext
	ctx.Src = rootProtoAbs
	ctx.GoOutput = typesAbs
	ctx.GrpcOutput = typesAbs
	ctx.IsGooglePlugin = true
	ctx.Output = projectDirAbs
	ctx.ProtocCmd = fmt.Sprintf("protoc -I=%s -I=%s %s --go_out=%s --go-grpc_out=%s",
		projectDirAbs, protoDepDir, filepath.Base(rootProtoAbs), typesAbs, typesAbs)
	ctx.IsGenClient = VarBoolClient
	ctx.Module = VarStringModule
	ctx.NameFromFilename = VarBoolNameFromFilename
	ctx.ProtoPaths = []string{projectDirAbs, protoDepDir}
	ctx.Port = VarIntPort

	grpcOptList := VarStringSliceGoGRPCOpt
	if len(grpcOptList) > 0 {
		ctx.ProtocCmd += " --go-grpc_opt=" + strings.Join(grpcOptList, ",")
	}

	goOptList := VarStringSliceGoOpt
	if len(goOptList) > 0 {
		ctx.ProtocCmd += " --go_opt=" + strings.Join(goOptList, ",")
	}

	g := generator.NewGenerator(style, verbose)
	return g.Generate(&ctx)
}

// RPCTemplate is the entry for generate rpc template
func RPCTemplate(latest bool) error {
	if !latest {
		console.Warning("deprecated: zctl rpc template -o is deprecated and will be removed in the future, use zctl rpc -o instead")
	}
	protoFile := VarStringOutput
	home := VarStringHome
	remote := VarStringRemote
	branch := VarStringBranch
	if len(remote) > 0 {
		repo, _ := util.CloneIntoGitHome(remote, branch)
		if len(repo) > 0 {
			home = repo
		}
	}
	if len(home) > 0 {
		pathx.RegisterGoctlHome(home)
	}

	if len(protoFile) == 0 {
		return errors.New("missing -o")
	}

	return generator.ProtoTmpl(protoFile)
}
