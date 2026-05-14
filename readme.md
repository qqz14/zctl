# zctl

基于 goctl fork 的 CLI 工具，用于生成 go-zero + entgo gRPC 微服务。一条命令初始化项目，自动集成 DAO 分层、统一错误处理、国际化、日志、拦截器链、Prometheus 指标、Mock 测试、协议管理等最佳实践。

## 安装

```bash
go install github.com/qqz14/zctl@latest
zctl --version
```

## 30 秒上手

```bash
# 1. 创建项目
zctl rpc new myservice --module github.com/xxx/myservice --port 8080
cd myservice

# 2. 新建表 + 生成全套代码
make gen-ent-new name=User
# → 编辑 ent/schema/user.go 定义字段
make gen-ent
make gen-rpc-ent-logic model=User

# 3. 运行
make tidy
make run

# 4. 浏览器调试
make swagger
```

`make gen-rpc-ent-logic` 一条命令完成：DAO 接口 + OceanBase 实现 + Mock + Hook + errcode + 测试骨架 + desc proto + merge proto + protoc + logic/server 生成。

## 核心功能

### 代码生成

| 命令 | 说明 |
|------|------|
| `zctl rpc new <name>` | 创建完整项目脚手架 |
| `zctl rpc ent` | 从 ent schema 生成全套模块代码 |
| `zctl rpc dao` | 从 SQL 语句生成自定义 DAO 方法 |
| `zctl rpc protoc` | 从 proto 生成 pb + server + logic |
| `zctl rpc merge-proto` | 合并 desc/ 下所有 proto 到根 proto |
| `zctl rpc enum` | 生成 Go 枚举类型 |
| `zctl rpc proto-doc` | 生成表格式 API 文档（含 JSON 示例） |

### 智能 DAO 生成

从 ent schema 自动分析字段属性，智能生成 DAO：

- **唯一键**：字段标记 `.Unique()` → 自动生成 `GetByXxx` / `UpdateByXxx`
- **索引字段**：唯一索引 + 显式索引 → 自动生成 `ListFilter` 结构体（只含带索引的字段）
- **分页**：`List` 方法使用公共 `model.PageInfo`，nil 不分页，非 nil 分页
- **软删除**：有 `deleted_at` 字段 → 生成 `DeleteByID`（软删除），无则不生成 Delete
- **Hook**：每个 schema 自动生成 `dao/hook/{model}_hook.go` 骨架

### SQL → DAO

```bash
# 支持 SELECT/INSERT/UPDATE/DELETE + JOIN/GROUP BY/HAVING/IN/LIKE/BETWEEN/IS NULL
make gen-dao-sql sql="SELECT * FROM user WHERE status=? AND created_at > ?"
```

自动推导：方法名、参数类型（从 ent schema）、ent predicate、返回类型。

### Proto 协议管理

- `proto.yaml` 配置远程 proto 仓库（统一管理所有微服务协议）
- `make pull-proto` 按版本（tag/branch）拉取协议
- CI/CD 一致性校验（`make gen-rpc` + diff 检查）
- `make proto-doc` 生成表格式 API 文档 - 自动从ent schema提取command备注proto - 自动从proto备注写入表格的描述

### 测试与调试

- `make swagger` — 浏览器 gRPC 调试 UI（grpcui）
- `make grpc-test` — 命令行单接口测试（grpcurl）
- `make test` / `make test-module` / `make test-func` — 单测
- 自动生成 DAO Mock（testify/mock）+ 测试骨架

## 生成的项目结构

```
myservice/
├── myservice.go           # 主入口（含拦截器链注册）
├── myservice.proto        # 合并后的 proto（自动生成）
├── proto.yaml             # 远程 proto 配置（可选）
├── Makefile               # 全量 make 命令
├── Dockerfile             # 多阶段构建
├── entrypoint.sh          # K8s 部署入口（envsubst 渲染配置）
├── desc/                  # proto 源文件（按业务域分子目录）
├── types/                 # protoc 生成的 pb 文件
├── ent/schema/            # entgo 表结构定义
├── internal/
│   ├── config/            # 配置（含 Env/DatabaseConf）
│   ├── svc/               # ServiceContext（ent + DAO 注入）
│   ├── server/            # gRPC server
│   ├── logic/             # 业务逻辑（层级对齐 desc/）
│   ├── middleware/         # 拦截器链
│   │   ├── validate_interceptor.go
│   │   ├── log_module.go
│   │   ├── error_log_interceptor.go
│   │   ├── metrics_interceptor.go
│   │   └── i18n_interceptor.go
│   ├── dao/               # DAO 接口 + ListFilter
│   ├── dao/impl/          # DAO 实现（OceanBase）
│   ├── dao/mock/          # DAO Mock（自动生成）
│   ├── dao/hook/          # DAO Hook（每表一文件）
│   ├── dao/entlog/        # ent SQL 日志装饰器
│   └── dao/entx/          # ent 事务管理器
├── pkg/
│   ├── errcode/           # 错误码 + grpc transport
│   ├── ctxutil/           # ctx 工具 + 日志
│   ├── model/             # 公共模型（PageInfo）
│   ├── consts/            # 常量
│   ├── i18n/              # 国际化（多语言 JSON）
│   ├── metrics/           # Prometheus 指标
│   └── enums/             # 枚举（proto enum + 手动 enum）
├── doc/                   # API 文档（make proto-doc 生成）
└── myservice_client/      # RPC 客户端 SDK
```

## 拦截器链

```
请求 → Validate → Module → ErrorLog → Metrics → I18n → Logic
```

| 拦截器 | 作用 |
|--------|------|
| ValidateInterceptor | proto 参数校验 |
| ModuleInterceptor | 从 FullMethod 提取模块名注入 ctx（日志自动携带） |
| ErrorLogInterceptor | 统一错误日志 + 强制校验（error 必须是 *errcode.Err，必须有 i18n） |
| MetricsInterceptor | Prometheus 指标上报（成功/失败/status error/biz error） |
| I18nInterceptor | 错误消息国际化 + 统一转 gRPC status JSON |

## 错误处理

```go
// 业务错误（HTTP 200，code 在 JSON body 中）
return errcode.Newf(errcode.UserNotFound, "user not found: id=%d", id)

// 需要 HTTP 401/403
return errcode.Newf(errcode.Unauthorized, "token expired").WithGRPC(codes.Unauthenticated)
```

网关收到的 gRPC status message：`{"code":11001,"msg":"用户不存在"}`

## 服务发现

默认不依赖 Etcd。K8s 用 headless service + DNS：

```yaml
# 调用方
MyServiceRpc:
  Target: dns:///myservice-rpc-svc:8080
```

本地直连：

```yaml
MyServiceRpc:
  Target: direct://127.0.0.1:8080
```

## 健康检查

go-zero 内置 gRPC 标准健康检查，K8s v1.24+ 直接用：

```yaml
livenessProbe:
  grpc:
    port: 8080
readinessProbe:
  grpc:
    port: 8080
```

本地验证：`make health`

## zctl vs goctl

| 能力 | goctl | zctl |
|------|:-----:|:----:|
| RPC/API 代码生成 | ✅ | ✅ |
| entgo ORM 集成 | ❌ | ✅ |
| DAO 分层（接口 + impl + mock + hook） | ❌ | ✅ |
| SQL → DAO 自动生成 | ❌ | ✅ |
| 唯一键/索引自动识别生成 | ❌ | ✅ |
| 统一错误处理（errcode + grpc transport） | ❌ | ✅ |
| 拦截器链（5 个） | ❌ | ✅ |
| Prometheus 指标 | ❌ | ✅ |
| 国际化（i18n） | ❌ | ✅ |
| proto 协议管理（远程仓库 + 版本） | ❌ | ✅ |
| API 文档生成（表格 + JSON 示例） | ❌ | ✅ |
| 浏览器调试（grpcui） | ❌ | ✅ |
| 生产级 Dockerfile + entrypoint | 基础 | ✅ |
| 兼容 goctl 原有命令 | — | ✅ 100% |

## 帮助

```bash
zctl rpc --help          # 查看所有子命令
zctl rpc ent --help      # 查看 ent 生成命令详情
zctl rpc dao --help      # 查看 SQL→DAO 命令详情
```

每个生成的项目内含 `zctl-commands.md`，详细说明每个 make 命令的用法和生成文件。
