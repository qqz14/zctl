# zctl

基于 go-zero zctl v1.10.1 扩展的代码生成工具，集成 entgo + DAO 适配层 + 统一错误处理 + 国际化 + 日志 module 等最佳实践。

## 安装

```bash
go install github.com/qqz14/zctl@latest

# 验证
zctl --version
```

## 快速上手

### 1. 初始化新服务

```bash
zctl rpc new passport --ent --module github.com/xxx/passport --port 9110
```

### 2. 日常开发流程

```bash
cd passport

# 新建 ent schema
make gen-ent-new name=User
# 编辑 ent/schema/user.go 定义字段...

# 生成 ent ORM 代码
make gen-ent

# 全量生成 RPC（从 proto 重新生成所有代码）
make gen-rpc

# 全量生成所有模块的 CRUD + DAO + errcode（从 ent schema）
make gen-rpc-ent-logic

# 也可以只生成单个模块
make gen-rpc-ent-logic model=User

# 本地运行
make run
```

### 3. 构建 & 部署

```bash
make build-linux    # 交叉编译 Linux
make docker         # 构建 Docker 镜像
make publish-docker # 推送镜像
```

---

## 生成的项目结构

```
passport/
├── passport.go                  # 主入口（含 interceptor 注册）
├── passport.proto
├── Makefile                     # 全量命令
├── Dockerfile                   # 多阶段构建
├── entrypoint.sh               # K8s 部署入口（envsubst 配置渲染）
├── .gitignore
├── etc/
│   ├── passport.yaml            # 本地开发配置
│   └── passport.yaml.template   # 部署模板（环境变量占位）
├── ent/schema/                  # entgo schema
├── internal/
│   ├── config/                  # 含 DBConfig
│   ├── svc/                     # 含 ent.Client + DAO 注入
│   ├── server/
│   ├── logic/
│   ├── middleware/              # ModuleInterceptor + I18nInterceptor
│   ├── dao/ + impl/            # DAO 接口 + OceanBase 实现
│   └── service/                 # 服务编排层
├── pkg/
│   ├── cerr/                    # 统一错误类型
│   ├── errcode/                 # 错误码（按模块文件）
│   ├── ctxutil/                 # ctx 工具 + 日志 helper
│   ├── i18n/locale/             # 国际化
│   └── model/                   # 公共模型
└── passportclient/              # RPC 客户端 SDK
```

---

## Makefile 命令

| 命令 | 说明 |
|------|------|
| `make gen-rpc` | 从 proto 重新生成所有 RPC 代码 |
| `make gen-ent` | 生成 Ent ORM 代码 |
| `make gen-ent-new name=X` | 新建 ent schema |
| `make gen-rpc-ent-logic` | 全量生成所有模块（CRUD + DAO + errcode + 测试骨架） |
| `make gen-rpc-ent-logic model=X` | 只生成指定模块 |
| `make test` | 运行全量测试 |
| `make test-module module=user` | 运行指定模块的测试 |
| `make fmt` | 格式化代码 |
| `make lint` | 代码检查 |
| `make build` | 当前平台编译 |
| `make build-linux` | 交叉编译 Linux |
| `make build-mac` | 交叉编译 macOS |
| `make build-win` | 交叉编译 Windows |
| `make docker` | 构建 Docker 镜像 |
| `make publish-docker` | 推送镜像 |
| `make run` | 本地运行 |
| `make health` | gRPC 健康检查（grpcurl） |
| `make grpc-list` | 列出所有 gRPC 服务（需 reflection） |
| `make tidy` | go mod tidy |
| `make tools` | 安装依赖工具 |
| `make help` | 显示所有命令 |

---

## Health Check（K8s 健康探针）

go-zero v1.10.1 **内置了 gRPC 标准健康检查服务** `grpc.health.v1.Health`，无需额外代码。

### K8s 配置示例（v1.24+）

```yaml
livenessProbe:
  grpc:
    port: 9110
  initialDelaySeconds: 10
  periodSeconds: 10
readinessProbe:
  grpc:
    port: 9110
  initialDelaySeconds: 5
  periodSeconds: 5
```

### 本地验证

```bash
# 安装 grpcurl
make tools

# 检查健康
make health
# 输出: { "status": "SERVING" }

# 列出所有服务（Dev/Test 模式下启用 reflection）
make grpc-list
```

### 为什么不用 HTTP health？

gRPC 服务走的是 HTTP/2 + protobuf 协议，K8s v1.24+ 原生支持 gRPC 探针，不需要额外暴露 HTTP 端口。

---

## 关于 Swagger

**Swagger/OpenAPI 不支持纯 gRPC 服务**。Swagger 是 HTTP/REST 的文档规范，gRPC 用的是 protobuf IDL。

| 场景 | 工具 |
|------|------|
| gRPC 服务调试 | `grpcurl`（命令行）/ Postman（支持 gRPC）/ BloomRPC |
| 接口文档 | proto 文件本身就是文档（强类型 + 注释） |
| 如需 HTTP 网关 + Swagger | 需引入 `grpc-gateway`（不在 zctl 默认范围，按需加） |

---

## 服务发现（不依赖 Etcd）

zctl 生成的项目**默认不配置 Etcd**，yaml 里没有 `Etcd` 段就不会连 Etcd。

### K8s 部署（推荐）

使用 headless service + DNS 轮询，**不需要 Etcd**：

```yaml
# K8s Service（headless）
apiVersion: v1
kind: Service
metadata:
  name: passport-rpc-svc
spec:
  clusterIP: None   # headless
  ports:
    - port: 9110
  selector:
    app: passport-rpc
```

客户端（调用方）配置：

```yaml
# 调用方的 yaml
PassportRpc:
  Target: dns:///passport-rpc-svc:9110
  # 或 k8s://namespace/passport-rpc-svc:9110
```

### 本地开发

直连，无需任何服务发现：

```yaml
PassportRpc:
  Target: direct://127.0.0.1:9110
```

---

## 测试策略

`gen-rpc-ent-logic` 生成模块时会自动创建测试骨架文件 `internal/logic/{module}/{module}_test.go`，包含每个 CRUD 方法的 `t.Skip("TODO")` 占位。

**测试用例如何填充**（桩命令本身不带 AI）：

| 方式 | 说明 |
|------|------|
| 手写 table-driven tests | mock DAO 接口 + 断言返回值 / 错误码 |
| `go generate` + mockgen | `mockgen -source=internal/dao/user_dao.go` 生成 mock |
| IDE AI 辅助 | 用 CodeBuddy / Copilot 根据 logic 代码补全测试 |

流程：`make gen-rpc-ent-logic` → 测试骨架生成 → 填充实现 → `make test-module module=user`

---

## zctl vs zctl 对比

| 能力 | zctl (官方) | zctl |
|------|:-----------:|:----:|
| RPC proto 生成 | ✅ | ✅ |
| API 生成 | ✅ | ✅ |
| Model 生成 | ✅ | ✅ |
| Docker/K8s 生成 | ✅ | ✅ |
| **entgo ORM 集成** | ❌ | ✅ |
| **DAO 适配层自动生成** | ❌ | ✅ |
| **统一错误处理（pkg/cerr）** | ❌ | ✅ |
| **错误码按模块文件约束** | ❌ | ✅ |
| **日志 module 注入（interceptor）** | ❌ | ✅ |
| **错误国际化（i18n）** | ❌ | ✅ |
| **生产级 Makefile** | ❌ | ✅ |
| **Dockerfile + entrypoint.sh** | 基础 | ✅ 生产级 |
| **etc yaml.template（envsubst）** | ❌ | ✅ |
| **从 ent schema 全量生成 CRUD** | ❌ | ✅ |
| **从 ent schema 生成 DAO + errcode** | ❌ | ✅ |
| **pkg 公共包初始化** | ❌ | ✅ |
| 兼容 zctl 原有命令 | — | ✅ 100% |
