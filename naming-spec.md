
# zctl 命名规则草案 v1（待评审）

## 一、命名三铁律

| # | 对象 | 规则 | 例（输入 `cs-agent-rpc` / `cs_agent_rpc` / `CsAgentRpc`） |
|---|---|---|---|
| 1 | **所有文件名**（含 `.go` / `.proto` / `.yaml` / `.yaml.template` / `.pb.go` / `_grpc.pb.go` / `.json` / `.md` 的服务名部分） | **保持用户输入原样** | `cs-agent-rpc.go` / `cs_agent_rpc.go` / `CsAgentRpc.go` |
| 2 | **所有目录名**（含项目根目录、`types/{pkg}/`、`{name}_client/` 中的 `{name}` 部分、`internal/server/` 文件中的服务前缀等） | **一律转为 `csagentrpc` 形式**：去掉 `-` / `_`，统一 lowercase，无任何分隔符 | 三种输入统一 → `csagentrpc` |
| 3 | **服务标识符**（proto `service Xxx`、Go 类型名、Makefile `SERVICE=`） | **PascalCase**（首字母大写驼峰 + initialism） | 三种输入统一 → `CsAgentRpc` |

> 衍生约定：
> - proto `package` = 目录规则 = `csagentrpc`
> - proto `option go_package = "./csagentrpc"`
> - Go 内部包名（`config` / `svc` / `server` / `dao` / 各 `pkg/xxx`）= 用途名，不带服务名，**不受输入影响**
> - Makefile 三态变量：`SERVICE=CsAgentRpc`（PascalCase）；`SERVICE_STYLE` = 文件名风格 = **原样输入**；`SERVICE_DIR=csagentrpc`（目录形式）；`SERVICE_DASH=cs-agent-rpc`（docker tag 用，固定 dash）

---

## 二、`zctl rpc new cs-agent-rpc` 完整文件树

```text
cs-agent-rpc/                                 ← 项目根目录 = 用户输入原样
├── .gitignore
├── .gitlab-ci.yml
├── Dockerfile
├── Makefile
├── README.md
├── cs-agent-rpc.go                           ← 根 main 文件 = 输入原样
├── cs-agent-rpc.proto                        ← 根 proto = 输入原样（合并产物）
├── entrypoint.sh
├── go.mod
├── go.sum
├── proto.yaml
├── zctl-commands.md
│
├── cmd/
│   └── migrate-ddl/
│       └── main.go
│
├── csagentrpc_client/                        ← client 目录 = 目录规则 csagentrpc + _client
│   └── cs-agent-rpc.go                       ← client 文件 = 输入原样
│
├── desc/
│   ├── base.proto
│   └── ping/
│       └── ping.proto
│
├── doc/
│   ├── cs-agent-rpc.json                     ← protoc-gen-doc 输出 = 输入原样
│   └── cs-agent-rpc_api.md                   ← API 文档 = 输入原样
│
├── ent/
│   └── schema/                               ← ent schema 业务表（用户后加）
│
├── etc/
│   ├── cs-agent-rpc.yaml                     ← 运行时配置 = 输入原样
│   └── cs-agent-rpc.yaml.template            ← 配置模板 = 输入原样
│
├── internal/
│   ├── config/
│   │   └── config.go
│   ├── dao/
│   │   ├── entlog/entlog.go
│   │   ├── entx/tx.go
│   │   ├── hook/                             ← 业务表 hook（model 加后）
│   │   ├── impl/                             ← oceanbase impl
│   │   └── mock/                             ← mock
│   ├── logic/                                ← rpc 业务 logic（按 model 分子目录）
│   ├── middleware/
│   │   ├── grpc_status_interceptor.go
│   │   ├── i18n_interceptor.go
│   │   ├── identity_interceptor.go
│   │   ├── log_interceptor.go
│   │   ├── log_module.go
│   │   ├── metrics_interceptor.go
│   │   ├── permission_interceptor.go
│   │   └── validate_interceptor.go
│   ├── server/
│   │   └── cs-agent-rpc_server.go            ← server 文件 = 输入原样 + _server
│   ├── service/                              ← 业务 service 层（可选）
│   └── svc/
│       └── service_context.go
│
├── migrations/                               ← gen-ddl 输出 *.sql
│
├── pkg/
│   ├── casbinx/
│   ├── consts/
│   ├── ctxutil/
│   ├── enums/
│   ├── errcode/
│   ├── grpcx/
│   ├── i18n/
│   │   ├── i18n.go
│   │   └── locale/{en.json, zh.json}
│   ├── identity/
│   ├── metrics/
│   ├── model/
│   └── sliceutil/
│
├── proto/
│   ├── buf/validate/validate.proto
│   └── google/api/{annotations,http}.proto
│
└── types/
    └── csagentrpc/                           ← pb 包目录 = 目录规则 csagentrpc
        ├── cs-agent-rpc.pb.go                ← pb 文件 = 输入原样
        └── cs-agent-rpc_grpc.pb.go           ← grpc pb 文件 = 输入原样
```

**关键内容片段**：

`cs-agent-rpc.proto`：
```proto
syntax = "proto3";
package csagentrpc;                           // ← 目录规则
option go_package = "./csagentrpc";           // ← 目录规则
service CsAgentRpc { ... }                    // ← 服务标识符 PascalCase
```

`cs-agent-rpc.go`（main）：
```go
var configFile = flag.String("f", "etc/cs-agent-rpc.yaml", ...)   // ← 原样
```

`Makefile`：
```makefile
SERVICE=CsAgentRpc
SERVICE_STYLE=cs-agent-rpc
SERVICE_DIR=csagentrpc
SERVICE_DASH=cs-agent-rpc
```

---

## 三、`zctl rpc new cs_agent_rpc` 完整文件树

```text
cs_agent_rpc/                                 ← 项目根目录 = 输入原样
├── .gitignore
├── .gitlab-ci.yml
├── Dockerfile
├── Makefile
├── README.md
├── cs_agent_rpc.go                           ← 输入原样
├── cs_agent_rpc.proto                        ← 输入原样
├── entrypoint.sh
├── go.mod
├── go.sum
├── proto.yaml
├── zctl-commands.md
│
├── cmd/
│   └── migrate-ddl/main.go
│
├── csagentrpc_client/                        ← 目录规则 csagentrpc + _client
│   └── cs_agent_rpc.go                       ← 输入原样
│
├── desc/
│   ├── base.proto
│   └── ping/ping.proto
│
├── doc/
│   ├── cs_agent_rpc.json
│   └── cs_agent_rpc_api.md
│
├── ent/schema/
│
├── etc/
│   ├── cs_agent_rpc.yaml
│   └── cs_agent_rpc.yaml.template
│
├── internal/
│   ├── config/config.go
│   ├── dao/{entlog, entx, hook, impl, mock}/
│   ├── logic/
│   ├── middleware/{8 个文件，同上}
│   ├── server/
│   │   └── cs_agent_rpc_server.go            ← 输入原样 + _server
│   ├── service/
│   └── svc/service_context.go
│
├── migrations/
│
├── pkg/{casbinx, consts, ctxutil, enums, errcode, grpcx, i18n, identity, metrics, model, sliceutil}/
│
├── proto/
│   ├── buf/validate/validate.proto
│   └── google/api/{annotations,http}.proto
│
└── types/
    └── csagentrpc/                           ← 目录规则
        ├── cs_agent_rpc.pb.go                ← 输入原样
        └── cs_agent_rpc_grpc.pb.go           ← 输入原样
```

**关键内容片段**：

`cs_agent_rpc.proto`：
```proto
package csagentrpc;
option go_package = "./csagentrpc";
service CsAgentRpc { ... }
```

`Makefile`：
```makefile
SERVICE=CsAgentRpc
SERVICE_STYLE=cs_agent_rpc
SERVICE_DIR=csagentrpc
SERVICE_DASH=cs-agent-rpc
```

---

## 四、`zctl rpc new CsAgentRpc` 完整文件树

```text
CsAgentRpc/                                   ← 项目根目录 = 输入原样
├── .gitignore
├── .gitlab-ci.yml
├── Dockerfile
├── Makefile
├── README.md
├── CsAgentRpc.go                             ← 输入原样
├── CsAgentRpc.proto                          ← 输入原样
├── entrypoint.sh
├── go.mod
├── go.sum
├── proto.yaml
├── zctl-commands.md
│
├── cmd/
│   └── migrate-ddl/main.go
│
├── csagentrpc_client/                        ← 目录规则 csagentrpc + _client
│   └── CsAgentRpc.go                         ← 输入原样
│
├── desc/
│   ├── base.proto
│   └── ping/ping.proto
│
├── doc/
│   ├── CsAgentRpc.json
│   └── CsAgentRpc_api.md
│
├── ent/schema/
│
├── etc/
│   ├── CsAgentRpc.yaml
│   └── CsAgentRpc.yaml.template
│
├── internal/
│   ├── config/config.go
│   ├── dao/{entlog, entx, hook, impl, mock}/
│   ├── logic/
│   ├── middleware/{8 个文件，同上}
│   ├── server/
│   │   └── CsAgentRpc_server.go              ← 输入原样 + _server
│   ├── service/
│   └── svc/service_context.go
│
├── migrations/
│
├── pkg/{casbinx, consts, ctxutil, enums, errcode, grpcx, i18n, identity, metrics, model, sliceutil}/
│
├── proto/
│   ├── buf/validate/validate.proto
│   └── google/api/{annotations,http}.proto
│
└── types/
    └── csagentrpc/                           ← 目录规则
        ├── CsAgentRpc.pb.go                  ← 输入原样
        └── CsAgentRpc_grpc.pb.go             ← 输入原样
```

**关键内容片段**：

`CsAgentRpc.proto`：
```proto
package csagentrpc;
option go_package = "./csagentrpc";
service CsAgentRpc { ... }
```

`Makefile`：
```makefile
SERVICE=CsAgentRpc
SERVICE_STYLE=CsAgentRpc
SERVICE_DIR=csagentrpc
SERVICE_DASH=cs-agent-rpc                     ← 由 PascalCase 切词后用 - 拼接
```

---

## 五、需要你确认的边界问题

1. **client 目录名是 `csagentrpc_client/`（目录规则）还是 `cs-agent-rpc_client/`（保留输入原样 + `_client`）？** —— 我目前按你说的"目录一律 csagentrpc"写成了 `csagentrpc_client/`，但通常 client 目录算"用户可见入口"，可能也希望保留原样。
2. **`internal/server/{?}_server.go`** —— 这是文件名，按规则 1 应保留输入原样：`cs-agent-rpc_server.go` / `cs_agent_rpc_server.go` / `CsAgentRpc_server.go`。但 `cs-agent-rpc_server.go` 这种文件名比较丑（dash + underscore 混用），是否要把 `-` 转 `_` 后再拼 `_server`？
3. **`types/csagentrpc/{?}.pb.go`** —— pb 文件名由 protoc 决定（取根 proto 文件名前缀），所以输入 `cs-agent-rpc` 时就是 `cs-agent-rpc.pb.go`。这个**没法在 zctl 内改**（除非生成完后 rename）。是否要 rename？
4. **`SERVICE_STYLE` 在 Makefile 里的语义**：原本它叫"项目经过 style 格式化的名称"，现在你的规则下它就 = 输入原样。`make run` 里用 `go run $(SERVICE_STYLE).go -f etc/$(SERVICE_STYLE).yaml` —— 这跟规则 1 一致，OK。
5. **`PascalCase` 输入的特殊性**：`CsAgentRpc.go` 这种文件名虽然合法但极不符合 Go 社区习惯（Go 文件名约定为 lowercase + `_`）。如果用户敢输入 PascalCase，是否要在 zctl 里直接报错拒绝？（推荐：拒绝 PascalCase 输入，只允许 `cs-agent-rpc` / `cs_agent_rpc` 两种风格。）

请逐条 review，确认或纠正后我再去改 zctl 代码。
