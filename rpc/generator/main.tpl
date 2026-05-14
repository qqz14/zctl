package main

import (
	"flag"
	"fmt"

	{{.imports}}

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
{{if .hasMiddleware}}	"{{.middlewarePkg}}"
	"{{.i18nPkg}}"
{{end}})

var configFile = flag.String("f", "etc/{{.serviceName}}.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

{{if .hasMiddleware}}	// Load i18n locale files
	i18n.MustLoad("pkg/i18n/locale")
{{end}}
	ctx := svc.NewServiceContext(c)

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
{{range .serviceNames}}       {{.Pkg}}.Register{{.GRPCService}}Server(grpcServer, {{.ServerPkg}}.New{{.Service}}Server(ctx))
{{end}}
		// Enable gRPC reflection for non-prod environments (required for grpcui/grpcurl)
		if c.Env != "prod" {
			reflection.Register(grpcServer)
		}
	})
	defer s.Stop()

{{if .hasMiddleware}}	s.AddUnaryInterceptors(
		middleware.ValidateInterceptor(),
		middleware.ModuleInterceptor(),
		middleware.ErrorLogInterceptor(),
		middleware.MetricsInterceptor(),
		middleware.I18nInterceptor(),
	)
{{end}}
	fmt.Printf("Starting rpc server at %s...\n", c.ListenOn)
	s.Start()
}
