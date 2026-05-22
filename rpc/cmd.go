package rpc

import (
	"github.com/qqz14/zctl/config"
	"github.com/qqz14/zctl/internal/cobrax"
	"github.com/qqz14/zctl/rpc/cli"
	"github.com/spf13/cobra"
)

var (
	// Cmd describes a rpc command.
	Cmd = cobrax.NewCommand("rpc", cobrax.WithRunE(func(command *cobra.Command, strings []string) error {
		return cli.RPCTemplate(true)
	}))
	templateCmd = cobrax.NewCommand("template", cobrax.WithRunE(func(command *cobra.Command, strings []string) error {
		return cli.RPCTemplate(false)
	}))

	newCmd      = cobrax.NewCommand("new", cobrax.WithRunE(cli.RPCNew), cobrax.WithArgs(cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs)))
	protocCmd   = cobrax.NewCommand("protoc", cobrax.WithRunE(cli.ZRPC), cobrax.WithArgs(cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs)))
	entCmd      = cobrax.NewCommand("ent", cobrax.WithRunE(cli.EntCRUDLogic))
	daoCmd      = cobrax.NewCommand("dao", cobrax.WithRunE(cli.DaoFromSQL))
	mergeCmd    = cobrax.NewCommand("merge-proto", cobrax.WithRunE(cli.MergeProto))
	enumCmd     = cobrax.NewCommand("enum", cobrax.WithRunE(cli.EnumGen))
	protoDocCmd = cobrax.NewCommand("proto-doc", cobrax.WithRunE(cli.ProtoDoc))
)

func init() {
	// ──── rpc (parent) ────
	Cmd.Short = "Generate rpc code"
	Cmd.Long = `Generate rpc related code from proto, ent schema, or SQL.

Subcommands:
  new          Create a new rpc service project (scaffold)
  protoc       Generate code from proto via protoc + zrpc plugin
  ent          Generate CRUD DAO + logic + proto from ent schema
  dao          Generate custom DAO method from SQL statement
  merge-proto  Merge desc/**/*.proto into root proto
  enum         Generate Go enum type from name + values
  template     Show or export code generation templates`

	// ──── new ────
	newCmd.Short = "Create a new rpc service project"
	newCmd.Long = `Create a new go-zero + entgo gRPC microservice project with best-practice scaffold.

Example:
  zctl rpc new myservice
  zctl rpc new myservice --port 9090`

	var (
		rpcCmdFlags      = Cmd.Flags()
		newCmdFlags      = newCmd.Flags()
		protocCmdFlags   = protocCmd.Flags()
		templateCmdFlags = templateCmd.Flags()
	)

	rpcCmdFlags.StringVar(&cli.VarStringOutput, "o")
	rpcCmdFlags.StringVar(&cli.VarStringHome, "home")
	rpcCmdFlags.StringVar(&cli.VarStringRemote, "remote")
	rpcCmdFlags.StringVar(&cli.VarStringBranch, "branch")

	newCmdFlags.StringSliceVar(&cli.VarStringSliceGoOpt, "go_opt")
	newCmdFlags.StringSliceVar(&cli.VarStringSliceGoGRPCOpt, "go-grpc_opt")
	newCmdFlags.StringVarWithDefaultValue(&cli.VarStringStyle, "style", config.DefaultFormat)
	newCmdFlags.BoolVar(&cli.VarBoolIdea, "idea")
	newCmdFlags.StringVar(&cli.VarStringHome, "home")
	newCmdFlags.StringVar(&cli.VarStringRemote, "remote")
	newCmdFlags.StringVar(&cli.VarStringBranch, "branch")
	newCmdFlags.StringVar(&cli.VarStringModule, "module")
	newCmdFlags.BoolVarP(&cli.VarBoolVerbose, "verbose", "v")
	newCmdFlags.BoolVar(&cli.VarBoolNameFromFilename, "name-from-filename")
	newCmdFlags.MarkHidden("go_opt")
	newCmdFlags.MarkHidden("go-grpc_opt")
	newCmdFlags.BoolVarPWithDefaultValue(&cli.VarBoolClient, "client", "c", true)
	newCmdFlags.IntVarWithDefaultValue(&cli.VarIntPort, "port", 8080)

	// ──── protoc ────
	protocCmd.Short = "Generate code from proto via protoc"
	protocCmd.Long = `Run protoc to generate pb.go files, then generate zrpc server/logic/client code.

Example:
  zctl rpc protoc demo.proto --go_out=./types --go-grpc_out=./types --zrpc_out=. --style=go_zero`

	protocCmdFlags.BoolVarP(&cli.VarBoolMultiple, "multiple", "m")
	protocCmdFlags.StringSliceVar(&cli.VarStringSliceGoOut, "go_out")
	protocCmdFlags.StringSliceVar(&cli.VarStringSliceGoGRPCOut, "go-grpc_out")
	protocCmdFlags.StringSliceVar(&cli.VarStringSliceGoOpt, "go_opt")
	protocCmdFlags.StringSliceVar(&cli.VarStringSliceGoGRPCOpt, "go-grpc_opt")
	protocCmdFlags.StringSliceVar(&cli.VarStringSlicePlugin, "plugin")
	protocCmdFlags.StringSliceVarP(&cli.VarStringSliceProtoPath, "proto_path", "I")
	protocCmdFlags.StringVar(&cli.VarStringStyle, "style")
	protocCmdFlags.StringVar(&cli.VarStringZRPCOut, "zrpc_out")
	protocCmdFlags.StringVar(&cli.VarStringHome, "home")
	protocCmdFlags.StringVar(&cli.VarStringRemote, "remote")
	protocCmdFlags.StringVar(&cli.VarStringBranch, "branch")
	protocCmdFlags.StringVar(&cli.VarStringModule, "module")
	protocCmdFlags.BoolVarP(&cli.VarBoolVerbose, "verbose", "v")
	protocCmdFlags.BoolVar(&cli.VarBoolNameFromFilename, "name-from-filename")
	protocCmdFlags.MarkHidden("go_out")
	protocCmdFlags.MarkHidden("go-grpc_out")
	protocCmdFlags.MarkHidden("go_opt")
	protocCmdFlags.MarkHidden("go-grpc_opt")
	protocCmdFlags.MarkHidden("plugin")
	protocCmdFlags.MarkHidden("proto_path")
	protocCmdFlags.BoolVarPWithDefaultValue(&cli.VarBoolClient, "client", "c", true)

	// ──── template ────
	templateCmd.Short = "Show or export code generation templates"

	templateCmdFlags.StringVar(&cli.VarStringOutput, "o")
	templateCmdFlags.StringVar(&cli.VarStringHome, "home")
	templateCmdFlags.StringVar(&cli.VarStringRemote, "remote")
	templateCmdFlags.StringVar(&cli.VarStringBranch, "branch")

	// ──── ent ────
	entCmd.Short = "Generate CRUD DAO + logic + proto from ent schema"
	entCmd.Long = `Generate full module code from ent schema: DAO interface + impl + mock + hook +
errcode + test skeleton + desc proto, then auto-run merge-proto + protoc + gen logic/server.

Example:
  zctl rpc ent --schema=./ent/schema --service_name=Demo --model=User
  zctl rpc ent --schema=./ent/schema --service_name=Demo --model=all --overwrite`

	entCmdFlags := entCmd.Flags()
	entCmdFlags.StringVar(&cli.VarStringSchema, "schema")
	entCmdFlags.StringVar(&cli.VarStringServiceName, "service_name")
	entCmdFlags.StringVarWithDefaultValue(&cli.VarStringStyle, "style", config.DefaultFormat)
	entCmdFlags.StringVar(&cli.VarStringModelName, "model")
	entCmdFlags.StringVar(&cli.VarStringGroupName, "group")
	entCmdFlags.BoolVar(&cli.VarBoolOverwrite, "overwrite")

	// ──── dao ────
	daoCmd.Short = "Generate custom DAO method from SQL statement"
	daoCmd.Long = `Parse a SQL statement and generate the corresponding DAO method (interface + impl + mock).
Supports SELECT/INSERT/UPDATE/DELETE with JOIN, GROUP BY, HAVING, IN, LIKE, BETWEEN, IS NULL, etc.

Example:
  zctl rpc dao --sql="SELECT * FROM user WHERE status=? AND created_at > ?" --schema=./ent/schema
  zctl rpc dao --sql="SELECT COUNT(*) FROM order WHERE user_id=?" --schema=./ent/schema`

	daoCmdFlags := daoCmd.Flags()
	daoCmdFlags.StringVar(&cli.VarStringSQL, "sql")
	daoCmdFlags.StringVar(&cli.VarStringSchema, "schema")
	daoCmdFlags.StringVarWithDefaultValue(&cli.VarStringStyle, "style", config.DefaultFormat)
	daoCmdFlags.BoolVar(&cli.VarBoolOverwrite, "overwrite")

	// ──── merge-proto ────
	mergeCmd.Short = "Merge desc/**/*.proto into root proto"
	mergeCmd.Long = `Scan all .proto files under desc/ directory and merge them into a single root proto file.
Usually called automatically by make gen-rpc.

Example:
  zctl rpc merge-proto`

	// ──── enum ────
	enumCmd.Short = "Generate Go enum type from name and values"
	enumCmd.Long = `Generate a pure Go enum type with String/IsValid/Parse/Values helpers.
For enums NOT defined in proto (business-only constants).

Example:
  zctl rpc enum --name=OrderStatus --values=pending,paid,shipped,done`

	enumCmdFlags := enumCmd.Flags()
	enumCmdFlags.StringVar(&cli.VarStringEnumName, "name")
	enumCmdFlags.StringVar(&cli.VarStringEnumValues, "values")

	// ──── proto-doc ────
	protoDocCmd.Short = "Generate Markdown API doc from protoc-gen-doc JSON"
	protoDocCmd.Long = `Parse protoc-gen-doc JSON output and generate a table-style Markdown API document
with field descriptions, types, required flags, and JSON examples.

Example:
  protoc --doc_out=./doc --doc_opt=json,demo.json demo.proto
  zctl rpc proto-doc --input=doc/demo.json`

	protoDocCmdFlags := protoDocCmd.Flags()
	protoDocCmdFlags.StringVar(&cli.VarStringProtoDocInput, "input")

	Cmd.AddCommand(newCmd, protocCmd, templateCmd, entCmd, daoCmd, mergeCmd, enumCmd, protoDocCmd)
}
