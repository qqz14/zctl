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

{{if .hasMiddleware}}// localDomain 本服务在 errcode/ErrorInfo.Domain 上的标识。
// 单一来源：DomainInterceptor 用它给本服务出口错误盖章；
// I18nInterceptor 用它判定"本域错（翻译）/ 跨域错（透传）"。
// 修改服务名时只改这一处。
const localDomain = "{{.domainName}}"

{{end}}func main() {
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

{{if .hasMiddleware}}	// 拦截器链（列表顺序 = 请求执行顺序 = 洋葱外→内；响应/错误路径反向由内→外传回）
	//
	// 请求方向（外→内）: GRPCStatus → Domain → Module → Log → Metrics → I18n → Validate → handler
	// 错误返回方向（内→外）: handler → Validate → I18n → Metrics → Log → Module → Domain → GRPCStatus
	//
	// ┌─ 返回相关（写在上面，包住所有人的错误）─────────────────────────────────────────
	// │  GRPCStatus → Domain → Module → Log → Metrics → I18n
	// │  Module 必须在 Log 上面：Log 打印时才能从 ctx 读到模块名
	// │  Domain 必须在 GRPCStatus 下面：错误先打 domain 标记，再被 GRPCStatus 转格式发出
	// │  I18n 必须在 Domain 下面（更内层）：本服务原生错入 I18n 时 Domain 还为空，
	// │    用 localDomain 判定本域翻译；下游错入 I18n 时 Domain 已是下游 domain → 透传
	// ├─ 请求链路相关（写在下面，按依赖顺序）──────────────────────────────────────────
	// │  Validate 最内层紧贴 handler，只管请求进来时的协议层硬性校验
	// └────────────────────────────────────────────────────────────────────────────────
	s.AddUnaryInterceptors(
		middleware.GRPCStatusInterceptor(),         // 0. 错误转成 gRPC Status 格式发出（最外层）
		middleware.DomainInterceptor(localDomain),  // 1. 给本服务原生错误打上 domain=localDomain
		middleware.ModuleInterceptor(),             // 2. 注入模块名到 ctx（Log 依赖此字段）
		middleware.LogInterceptor(),                // 3. 打印请求日志（含模块名、错误信息）
		middleware.MetricsInterceptor(),            // 4. 指标上报（区分 biz/status err）
		middleware.I18nInterceptor(localDomain),    // 5. 错误消息国际化翻译（本域错翻译；跨域错透传）
		middleware.ValidateInterceptor(),           // 6. 接口协议层硬性校验（最内层，紧贴 handler）
	)
{{end}}
	fmt.Printf("Starting rpc server at %s...\n", c.ListenOn)
	s.Start()
}
