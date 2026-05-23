# zctl

基于 goctl fork 的 CLI 工具，用于生成 go-zero + entgo gRPC 微服务。一条命令初始化项目，自动集成 DAO 分层、统一错误处理、国际化、日志、拦截器链、Prometheus 指标、Mock 测试、协议管理等最佳实践。

## 快速开始

```bash
make run
```

## 开发流程

```bash
# 1. 新建 ent schema，编辑字段
make gen-ent-new name=User
# → 编辑 ent/schema/user.go 定义 Fields / Edges / Indexes

# 2. 生成 ent ORM 代码
make gen-ent

# 3. 一键生成模块全套代码（DAO + logic + errcode + test + desc proto + pb + server）
make gen-rpc-ent-logic model=User

# 4. 整理依赖 & 运行
make tidy
make run
```

## 常用命令

详见 [zctl-commands.md](./doc/zctl-commands.md)

## Proto 协议管理

### 最佳实践

所有微服务的 proto 协议由**统一的 proto 仓库**集中管理（包括本服务），各微服务通过 `proto.yaml` 配置同步协议。

统一仓库结构示例：

```
proto-definitions/             # 统一 proto 仓库
├── base/
│   └── base.proto             # 公共 message（Empty / PageInfo 等）
├── myservice/                 # 本服务的协议
│   ├── ping/ping.proto
│   └── userinfo/user_info.proto
├── other-service/             # 其他服务的协议
│   └── ...
└── ...
```

### 配置

项目根目录 `proto.yaml`（初始化时已创建，填入实际值即可）：

```yaml
remote:
  repo: git@github.com:your-org/proto-definitions.git
  ref: main              # 开发阶段跟 main 分支；发版用 tag 如 v1.2.0
  path: myservice/      # 本服务在远程仓库中的子目录
  target: desc/          # 同步到本地 desc/
```

### 协议开发流程

```bash
# 1. 拉取远程最新协议到本地 desc/
make pull-proto

# 2. 修改 desc/ 中的 proto 文件（新增接口、修改 message 等）

# 3. 本地重新生成代码
make gen-rpc

# 4. 调整 logic 代码适配新签名，本地验证
make tidy
go build ./...
make test

# 5. 提交协议变更到统一 proto 仓库
cd /path/to/proto-definitions
cp -r /path/to/myservice/desc/* myservice/
git add .
git commit -m "feat(myservice): add getUserList pagination"
git tag v1.3.0
git push origin main --tags

# 6. 回到服务仓库，更新 proto.yaml 的 ref（如果用 tag 锁版本）
#    ref: v1.3.0
# 7. 提交服务仓库
git add .
git commit -m "feat: update proto to v1.3.0"
git push
```

### CI/CD 协议一致性保证

CI pipeline 中加入校验步骤，确保部署时使用的协议与 proto 仓库一致：

```yaml
# .github/workflows/ci.yaml (示例)
steps:
  - name: Pull proto
    run: make pull-proto

  - name: Verify proto consistency
    run: |
      make gen-rpc
      if [ -n "$(git status --porcelain types/ myservice.proto internal/server/ myservice_client/)" ]; then
        echo "ERROR: proto out of sync with proto repo"
        git diff --stat
        exit 1
      fi

  - name: Test
    run: make test

  - name: Build
    run: make build-linux
```

**核心原则**：

- **proto 仓库是 single source of truth**，所有服务从同一个仓库拉取
- **`proto.yaml` 中的 `ref` 锁定版本**，CI 用 tag（如 `v1.3.0`），开发可用 `main`
- **CI 验证无 diff** = 部署代码与协议版本一致
- **先改 proto 仓库，再改业务仓库**，确保协议变更有独立的版本历史和 review

### 协议文档生成

proto 协议可一键转换为表格式 API 文档（含字段说明、类型、必填标记、JSON 示例）：

```bash
make proto-doc      # 生成 doc/{service}_api.md
```

文档中每个接口包含请求/响应参数表格和 JSON 示例，嵌套字段用 `info.username` 格式展示，方便协作和 review。

proto 字段的注释（ent schema 的 `.Comment("xxx")`）会自动成为文档中的"说明"列。

## 测试与调试

### 浏览器调试（Swagger）

启动服务后，一行命令打开 gRPC Web UI，浏览器中可查看所有接口、填写参数、直接调试：

```bash
make run             # 先启动服务
make swagger         # 自动打开浏览器 gRPC 调试 UI
```

> 仅非 prod 环境可用（prod 环境 gRPC 反射已关闭）。

### 单接口测试（命令行）

用 grpcurl 快速调用指定接口：

```bash
make grpc-test method=myservice.myservice/Ping req='{}'
```

### 单元测试（Mock）

zctl 自动为每个 DAO 生成 testify mock（`internal/dao/mock/`），同时为每个模块生成测试骨架。

```bash
# 运行全部单测
make test

# 运行指定模块的单测
make test-module module=userinfo

# 运行单个测试函数
make test-func func=TestCreateUserInfo pkg=./internal/logic/userinfo/user_info/...
```

测试骨架位于 `internal/logic/{group}/{model}/{model}_test.go`，含 `t.Skip()` 占位，填充断言后即可使用。

## 服务发现

默认不依赖 Etcd。K8s 用 headless service + DNS：

```yaml
myserviceRpc:
  Target: dns:///myservice-rpc-svc:8080
```

本地开发直连：

```yaml
myserviceRpc:
  Target: direct://127.0.0.1:8080
```

```bash
make help   # 查看所有可用命令
```

## 数据库 schema 变更（DDL）

| 环境       | DDL 落地方式 |
| ---------- | ------------ |
| dev/stage  | 服务启动时 `entClient.Schema.Create()` 自动同步 |
| uat/prod   | **手工执行**：`make gen-ddl` 输出差量 SQL → DBA 评审 → 人工 apply |

```bash
make gen-ddl                              # 默认 name=auto
make gen-ddl name=add_role_cid_to_user   # 推荐：语义化命名
# → migrations/{YYYYMMDD_HHMMSS}_{name}.sql
```

底层调 `cmd/migrate-ddl`，用 ent 的 `Schema.WriteTo(file, WithDropColumn(true), WithDropIndex(true))` 把 `ent/schema/*.go` 与 DB 现状的差量 DDL **写入文件**（走 `schema.WriteDriver`，物理上不会落到 DB）。

**DBA 评审 checklist**：

- [ ] `DROP COLUMN` 是真删字段，还是重命名？是重命名 → 改写成 `ALTER TABLE ... CHANGE COLUMN` 保留数据
- [ ] `DROP INDEX` 在大表上是否触发慢查询，是否走 OnlineDDL / pt-osc / gh-ost
- [ ] 唯一键列变化前是否清理重复数据（`GROUP BY ... HAVING COUNT(*)>1`）
- [ ] 灰度顺序：先发应用代码兼容新旧 schema → apply DDL → 下线兼容代码

`migrations/*.sql` 全部纳入 git，作为生产 DDL 的可追溯真相。

## 项目结构

```
myservice/
├── myservice.go                # 主入口
├── myservice.proto             # 合并后的 proto（自动生成，勿编辑）
├── proto.yaml                 # 远程 proto 配置（可选）
├── Makefile
├── Dockerfile
├── desc/                      # proto 源文件（按业务域分子目录）
│   ├── base.proto
│   └── {group}/
├── types/                     # protoc 生成的 pb 文件
├── ent/schema/                # entgo 表结构定义
├── internal/
│   ├── config/
│   ├── svc/                  # ServiceContext（DAO 实例注入）
│   ├── server/               # gRPC server 实现
│   ├── logic/                # 业务逻辑（层级对齐 desc/）
│   ├── middleware/            # 拦截器
│   │   ├── validate_interceptor.go
│   │   ├── log_module.go
│   │   ├── log_interceptor.go
│   │   ├── metrics_interceptor.go
│   │   ├── grpc_status_interceptor.go
│   │   └── i18n_interceptor.go
│   ├── dao/                  # DAO 接口
│   ├── dao/impl/             # DAO 实现（OceanBase）
│   └── dao/mock/            # DAO Mock（自动生成）
├── pkg/
│   ├── errcode/              # 错误码 + 错误类型
│   ├── ctxutil/              # ctx 工具 + 日志
│   ├── model/                # 公共模型（PageInfo 等）
│   ├── consts/               # 常量
│   ├── i18n/                 # 国际化
│   └── metrics/              # Prometheus 指标
└── myservice_client/         # RPC 客户端 SDK
```

## 拦截器链

```
请求 → Validate → Module → Log → Metrics → I18n → GRPCStatus → Logic
```

| 拦截器 | 作用 |
|--------|------|
| ValidateInterceptor | proto 参数校验 |
| ModuleInterceptor | 从 FullMethod 提取模块名注入 ctx（日志自动携带） |
| LogInterceptor | 统一错误日志 + 强制校验（error 必须是 *errcode.Err） |
| MetricsInterceptor | Prometheus 指标上报（成功/失败/status error/biz error） |
| I18nInterceptor | 错误消息国际化（方案 A：只翻译业务错误，其他透传） |
| GRPCStatusInterceptor | *errcode.Err → gRPC transport format |

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
| 拦截器链（6 个） | ❌ | ✅ |
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
