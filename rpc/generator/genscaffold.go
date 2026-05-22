package generator

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qqz14/zctl/util/ctx"
	"github.com/qqz14/zctl/util/pathx"
)

// GenScaffold generates the best-practice scaffold: pkg/, middleware/, dao/ skeleton
func (g *Generator) GenScaffold(abs string, projectCtx *ctx.ProjectContext, zctx *ZRpcContext) error {
	modulePath := projectCtx.Path // e.g. "github.com/xxx/passport"
	serviceName := filepath.Base(abs)

	// pkg/errcode (unified: error type + codes + grpc transport)
	if err := g.genPkgErrcode(abs, modulePath); err != nil {
		return err
	}
	// pkg/ctxutil
	if err := g.genPkgCtxutil(abs, modulePath); err != nil {
		return err
	}
	// pkg/i18n
	if err := g.genPkgI18n(abs); err != nil {
		return err
	}
	// pkg/model
	if err := g.genPkgModel(abs); err != nil {
		return err
	}
	// pkg/metrics (Prometheus counters for RPC success/fail/status/biz errors)
	if err := g.genPkgMetrics(abs, modulePath); err != nil {
		return err
	}
	// internal/middleware
	if err := g.genMiddleware(abs, modulePath); err != nil {
		return err
	}
	// internal/dao skeleton
	if err := pathx.MkdirIfNotExist(filepath.Join(abs, "internal", "dao")); err != nil {
		return err
	}
	if err := pathx.MkdirIfNotExist(filepath.Join(abs, "internal", "dao", "impl")); err != nil {
		return err
	}
	if err := pathx.MkdirIfNotExist(filepath.Join(abs, "internal", "dao", "mock")); err != nil {
		return err
	}
	// internal/service skeleton
	if err := pathx.MkdirIfNotExist(filepath.Join(abs, "internal", "service")); err != nil {
		return err
	}
	// pkg/consts skeleton
	if err := pathx.MkdirIfNotExist(filepath.Join(abs, "pkg", "consts")); err != nil {
		return err
	}
	// Makefile
	if err := g.genMakefile(abs, serviceName, zctx); err != nil {
		return err
	}
	// Dockerfile
	if err := g.genDockerfile(abs, serviceName); err != nil {
		return err
	}
	// entrypoint.sh
	if err := g.genEntrypoint(abs); err != nil {
		return err
	}
	// etc/xxx.yaml.template
	if err := g.genEtcTemplate(abs, serviceName, zctx); err != nil {
		return err
	}
	// .gitignore
	if err := g.genGitignore(abs); err != nil {
		return err
	}
	// cmd/migrate-ddl/main.go (offline DDL diff tool: ent schema vs DB → migrations/*.sql)
	if err := g.genCmdMigrateDDL(abs, serviceName, modulePath); err != nil {
		return err
	}
	// migrations/ directory placeholder (gen-ddl outputs .sql here)
	if err := pathx.MkdirIfNotExist(filepath.Join(abs, "migrations")); err != nil {
		return err
	}
	// README.md
	if err := g.genProjectReadme(abs, serviceName, zctx); err != nil {
		return err
	}
	// zctl-commands.md
	if err := g.genCommandsDoc(abs, serviceName); err != nil {
		return err
	}
	// desc/ directory
	if err := g.genDescDir(abs, serviceName); err != nil {
		return err
	}
	// merge_proto.sh
	//
	// 历史脚本：早期版本由 zctl 生成的本地兜底合并脚本；现在 `make gen-rpc` 走
	// `zctl rpc merge-proto`（Go 实现），sh 脚本已无用途。为避免新项目里产出
	// 容易被误调的死脚本，这里通过常量开关默认关闭；如需临时回退，把该常量
	// 改为 true 即会重新生成 merge_proto.sh。
	const enableMergeProtoScript = false
	if enableMergeProtoScript {
		if err := g.genMergeProtoScript(abs, serviceName); err != nil {
			return err
		}
	}
	// proto.yaml (remote proto config, optional)
	if err := g.genProtoYaml(abs); err != nil {
		return err
	}
	// proto/buf/validate/validate.proto (protovalidate dependency)
	if err := g.genValidateProto(abs); err != nil {
		return err
	}
	// proto/google/api/{annotations,http}.proto (grpc-transcoding dependency)
	// 用于在 desc/*.proto 中使用 option (google.api.http) = {...} 定义 HTTP 路由。
	if err := g.genGoogleAPIProto(abs); err != nil {
		return err
	}
	// types/ directory
	if err := pathx.MkdirIfNotExist(filepath.Join(abs, "types")); err != nil {
		return err
	}

	return nil
}

// ==================== pkg/errcode ====================

func (g *Generator) genPkgErrcode(abs, modulePath string) error {
	dir := filepath.Join(abs, "pkg", "errcode")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	// errcode.go — unified error type with grpcCode for HTTP status control
	if err := writeIfNotExist(filepath.Join(dir, "errcode.go"), `package errcode

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Err is the business error, implements error interface.
// By default, gRPC status code is OK (→ HTTP 200), business code carried in message.
// Use WithGRPC() to override for 401/403 etc.
type Err struct {
	code     int        // business error code (e.g. 95001)
	msg      string     // error message (may be i18n-translated)
	origin   string     // original error message before i18n translation
	grpcCode codes.Code // gRPC status code, default 0 = OK → HTTP 200
}

func (e *Err) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("[%d] %s", e.code, e.msg)
}

// Code returns the business error code.
func (e *Err) Code() int {
	if e == nil {
		return 0
	}
	return e.code
}

// Msg returns the error message.
func (e *Err) Msg() string {
	if e == nil {
		return ""
	}
	return e.msg
}

// GRPCCode returns the gRPC status code (0 means OK).
func (e *Err) GRPCCode() codes.Code {
	if e == nil {
		return codes.OK
	}
	return e.grpcCode
}

// Origin returns the original error message before i18n translation.
// Returns empty string if not translated.
func (e *Err) Origin() string {
	if e == nil {
		return ""
	}
	return e.origin
}

// WithGRPC sets the gRPC status code for HTTP status override.
// Usage: errcode.Newf(95003, "token expired").WithGRPC(codes.Unauthenticated) → HTTP 401
func (e *Err) WithGRPC(c codes.Code) *Err {
	e.grpcCode = c
	return e
}

// WithOrigin preserves the original error message before i18n translation.
func (e *Err) WithOrigin(origin string) *Err {
	e.origin = origin
	return e
}

// ──── Constructors ────

// Newf creates a business error (gRPC OK → HTTP 200 by default).
func Newf(code int, format string, args ...interface{}) *Err {
	return &Err{code: code, msg: fmt.Sprintf(format, args...)}
}

// Wrapf creates a business error wrapping an internal cause.
func Wrapf(code int, format string, args ...interface{}) *Err {
	return &Err{code: code, msg: fmt.Sprintf(format, args...)}
}

// ──── Helpers ────

// ExtractErr tries to extract *Err from any error.
func ExtractErr(err error) (*Err, bool) {
	if err == nil {
		return nil, false
	}
	if e, ok := err.(*Err); ok {
		return e, true
	}
	return nil, false
}

// ExtractCode extracts business code from any error.
func ExtractCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*Err); ok {
		return e.code
	}
	if s, ok := status.FromError(err); ok {
		return int(s.Code())
	}
	return InternalError
}

// Is checks whether err matches the given business code.
func Is(err error, code int) bool { return ExtractCode(err) == code }
`); err != nil {
		return err
	}

	// grpc.go — encode/decode for gRPC transport (JSON in status message)
	if err := writeIfNotExist(filepath.Join(dir, "grpc.go"), `package errcode

import (
	"encoding/json"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcPayload is the JSON structure encoded in gRPC status message.
// Gateway (e.g. APISIX) reads this to build the final HTTP response body.
type grpcPayload struct {
	Code int    `+"`"+`json:"code"`+"`"+`
	Msg  string `+"`"+`json:"msg"`+"`"+`
}

// ToGRPCStatus converts *Err to gRPC status for transport.
//   - gRPC code: e.grpcCode (default OK → HTTP 200)
//   - message:   JSON {"code":95001,"msg":"用户不存在"}
//
// Called by GRPCStatusInterceptor, NOT by business code.
func ToGRPCStatus(e *Err) *status.Status {
	payload, _ := json.Marshal(grpcPayload{Code: e.code, Msg: e.msg})
	return status.New(e.grpcCode, string(payload))
}

// StatusError converts *Err to a gRPC error for transport.
// NOTE: when grpcCode is OK, gRPC returns nil (by design). This is expected —
// GRPCStatusInterceptor handles this case via grpc.SetTrailer.
func StatusError(e *Err) error {
	return ToGRPCStatus(e).Err()
}

// FromGRPCStatus decodes *Err from a gRPC status (for client-side or testing).
func FromGRPCStatus(st *status.Status) (*Err, bool) {
	if st == nil {
		return nil, false
	}
	var p grpcPayload
	if err := json.Unmarshal([]byte(st.Message()), &p); err != nil {
		return nil, false
	}
	return &Err{code: p.Code, msg: p.Msg, grpcCode: st.Code()}, true
}

// FromGRPCError decodes *Err from a gRPC error.
func FromGRPCError(err error) (*Err, bool) {
	if err == nil {
		return nil, false
	}
	st, ok := status.FromError(err)
	if !ok {
		return nil, false
	}
	return FromGRPCStatus(st)
}

// IsOKCode returns true if grpcCode is OK (business error, HTTP 200).
func IsOKCode(c codes.Code) bool {
	return c == codes.OK
}
`); err != nil {
		return err
	}

	// common.go — only int constants, no *Err variables
	content := `package errcode

// ──── Common error codes ────
// Codes only. Messages come from i18n (pkg/i18n/locale/{lang}.json → key "errcode.{code}").
const (
	OK             = 0
	InternalError  = 95000
	InvalidParam   = 95001
	NotFound       = 95002
	Unauthorized   = 95003
	Forbidden      = 95004
	// ContractViolation indicates the caller did not honor the declared proto constraints
	// (buf.validate.field rules). This is a CALLER BUG — distinct from InvalidParam which
	// represents legitimate business-level parameter validation failures.
	// gRPC code: InvalidArgument (HTTP 400).
	ContractViolation = 95005

	// DB error codes (used by DAO layer via errcode.Wrapf)
	DBQueryFailed  = 10006
	DBInsertFailed = 10007
	DBUpdateFailed = 10008
	DBDeleteFailed = 10009
)
`
	return writeIfNotExist(filepath.Join(dir, "common.go"), content)
}

// ==================== pkg/ctxutil ====================

func (g *Generator) genPkgCtxutil(abs, modulePath string) error {
	dir := filepath.Join(abs, "pkg", "ctxutil")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	// ctxutil.go
	if err := writeIfNotExist(filepath.Join(dir, "ctxutil.go"), `package ctxutil

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

type contextKey string

const moduleKey contextKey = "module"

func WithModule(ctx context.Context, module string) context.Context {
	return context.WithValue(ctx, moduleKey, module)
}

func GetModule(ctx context.Context) string {
	if v, ok := ctx.Value(moduleKey).(string); ok {
		return v
	}
	return "unknown"
}

// ClientIP gets the real client IP from gRPC context.
// Priority: x-real-ip > x-forwarded-for[0] > peer address.
func ClientIP(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		if vals := md.Get("x-real-ip"); len(vals) > 0 {
			if ip := strings.TrimSpace(vals[0]); ip != "" {
				return ip
			}
		}
		if vals := md.Get("x-forwarded-for"); len(vals) > 0 {
			parts := strings.Split(vals[0], ",")
			if len(parts) > 0 {
				if ip := strings.TrimSpace(parts[0]); ip != "" {
					return ip
				}
			}
		}
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr := p.Addr.String()
		if idx := strings.LastIndex(addr, ":"); idx > 0 {
			return addr[:idx]
		}
		return addr
	}
	return "unknown"
}

// MetaValue gets the first non-empty value from gRPC metadata keys.
func MetaValue(ctx context.Context, keys ...string) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, key := range keys {
		if vals := md.Get(key); len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	return ""
}
`); err != nil {
		return err
	}

	// log.go
	return writeIfNotExist(filepath.Join(dir, "log.go"), `package ctxutil

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
)

// L returns a logger with module field from ctx.
func L(ctx context.Context) logx.Logger {
	return logx.WithContext(ctx).WithFields(logx.Field("module", GetModule(ctx)))
}

// ErrField creates a logx field for error.
func ErrField(err error) logx.LogField {
	return logx.Field("error", err.Error())
}

// IDField creates a logx field for id (any type).
func IDField(id interface{}) logx.LogField {
	return logx.Field("id", id)
}

// CountField creates a logx field for count.
func CountField(count int) logx.LogField {
	return logx.Field("count", count)
}

// KV creates a logx field for any key-value pair.
func KV(key string, val interface{}) logx.LogField {
	return logx.Field(key, val)
}

// Infof logs a formatted info message with module context.
// Usage: ctxutil.Infof(ctx, "user %s login from %s", username, ip)
func Infof(ctx context.Context, format string, args ...interface{}) {
	L(ctx).Info(fmt.Sprintf(format, args...))
}

// Errorf logs a formatted error message with module context.
// Usage: ctxutil.Errorf(ctx, "failed to create user %s: %v", username, err)
func Errorf(ctx context.Context, format string, args ...interface{}) {
	L(ctx).Error(fmt.Sprintf(format, args...))
}

// Slowf logs a formatted slow message with module context.
// Usage: ctxutil.Slowf(ctx, "query took %dms for user %s", cost, uid)
func Slowf(ctx context.Context, format string, args ...interface{}) {
	L(ctx).Slow(fmt.Sprintf(format, args...))
}
`)
}

// ==================== pkg/i18n ====================

func (g *Generator) genPkgI18n(abs string) error {
	dir := filepath.Join(abs, "pkg", "i18n", "locale")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	en := `{
  "errcode": {
	"95000": "Internal server error",
    "95001": "Invalid parameter",
    "95002": "Resource not found",
    "95003": "Unauthorized",
    "95004": "Forbidden",
    "95005": "Caller violated the API contract: request parameters do not satisfy the declared proto constraints",
    "10006": "Database query failed",
    "10007": "Database insert failed",
    "10008": "Database update failed",
    "10009": "Database delete failed"
  }
}
`
	zh := `{
  "errcode": {
	"95000": "内部服务器错误",
    "95001": "参数校验失败",
    "95002": "资源不存在",
    "95003": "未授权",
    "95004": "禁止访问",
    "95005": "调用方未按接口约定传参，请检查请求字段是否符合 proto 定义的约束",
    "10006": "数据库查询失败",
    "10007": "数据库插入失败",
    "10008": "数据库更新失败",
    "10009": "数据库删除失败"
  }
}
`
	if err := writeIfNotExist(filepath.Join(dir, "en.json"), en); err != nil {
		return err
	}
	if err := writeIfNotExist(filepath.Join(dir, "zh.json"), zh); err != nil {
		return err
	}

	// pkg/i18n/i18n.go — loader + translator (no lock, template vars, fallback chain)
	i18nDir := filepath.Join(abs, "pkg", "i18n")
	return writeIfNotExist(filepath.Join(i18nDir, "i18n.go"), `package i18n

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Bundle holds loaded locale data.
// Designed for load-once-read-many: no lock needed after Load() completes.
// JSON format: {"errcode": {"95001": "参数校验失败"}, "ui": {"welcome": "欢迎 {{.Name}}"}}
type Bundle struct {
	locales map[string]map[string]map[string]string // lang → section → key → message
}

var defaultBundle = &Bundle{
	locales: make(map[string]map[string]map[string]string),
}

// Load reads all JSON files from localeDir.
// Each file named {lang}.json (e.g. en.json, zh.json, zh-CN.json).
// MUST be called at startup before serving requests.
func Load(localeDir string) error {
	return defaultBundle.Load(localeDir)
}

func (b *Bundle) Load(localeDir string) error {
	entries, err := os.ReadDir(localeDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		lang := entry.Name()[:len(entry.Name())-5]
		data, err := os.ReadFile(filepath.Join(localeDir, entry.Name()))
		if err != nil {
			continue
		}
		var m map[string]map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		b.locales[lang] = m
	}
	return nil
}

// Translate looks up a message. Supports fallback chain: zh-CN → zh → en.
// Supports template variables: "hello {{.Name}}" + Args{"Name": "张三"} → "hello 张三"
func Translate(lang, section, key string, args ...map[string]string) string {
	return defaultBundle.Translate(lang, section, key, args...)
}

func (b *Bundle) Translate(lang, section, key string, args ...map[string]string) string {
	msg := b.lookup(lang, section, key)
	if msg == "" {
		return ""
	}
	if len(args) > 0 && args[0] != nil {
		for k, v := range args[0] {
			msg = strings.ReplaceAll(msg, "{{."+k+"}}", v)
		}
	}
	return msg
}

func (b *Bundle) lookup(lang, section, key string) string {
	// 1. Exact match
	if msg := b.get(lang, section, key); msg != "" {
		return msg
	}
	// 2. Base language (zh-CN → zh)
	if idx := strings.IndexAny(lang, "-_"); idx > 0 {
		if msg := b.get(lang[:idx], section, key); msg != "" {
			return msg
		}
	}
	// 3. Fallback to en
	if lang != "en" {
		return b.get("en", section, key)
	}
	return ""
}

func (b *Bundle) get(lang, section, key string) string {
	if s, ok := b.locales[lang]; ok {
		if k, ok := s[section]; ok {
			return k[key]
		}
	}
	return ""
}

// TranslateErrcode translates an error code.
func TranslateErrcode(lang string, code int, args ...map[string]string) string {
	return Translate(lang, "errcode", fmt.Sprintf("%d", code), args...)
}

// MustLoad panics if loading fails.
func MustLoad(localeDir string) {
	if err := Load(localeDir); err != nil {
		panic("failed to load i18n locales: " + err.Error())
	}
}
`)
}

// ==================== pkg/metrics ====================

func (g *Generator) genPkgMetrics(abs, modulePath string) error {
	dir := filepath.Join(abs, "pkg", "metrics")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	return writeIfNotExist(filepath.Join(dir, "metrics.go"), `package metrics

import "github.com/zeromicro/go-zero/core/metric"

// ──── RPC Metrics ────
//
// Design rationale — memory impact analysis:
//
// ┌────────────────────────────┬──────────────────┬────────────┬────────────────────────────────┐
// │ Library                    │ Inc latency      │ Alloc/op   │ Notes                          │
// ├────────────────────────────┼──────────────────┼────────────┼────────────────────────────────┤
// │ go-zero core/metric        │ ~3.3 ns          │ 0 B        │ Thin wrapper over prom client  │
// │ prometheus/client_golang    │ ~3.3 ns          │ 0 B        │ Pre-resolve WithLabelValues    │
// │ VictoriaMetrics/metrics     │ ~3.5 ns          │ 0 B        │ Simpler API, fewer deps        │
// └────────────────────────────┴──────────────────┴────────────┴────────────────────────────────┘
//
// Conclusion: all three have identical hot-path perf (~3 ns, 0 alloc).
// We choose go-zero core/metric because:
//   1. Already a transitive dependency — zero extra deps or binary bloat
//   2. Labels are pre-registered → no map lookup / fmt.Sprintf in hot path
//   3. Fully compatible with go-zero's built-in Prometheus endpoint
//   4. CounterVec uses prometheus.MustRegister under the hood → auto-exposed
//
// Memory considerations:
//   - Each unique label combination creates ONE Counter (~200 bytes).
//   - We bound cardinality: method × {success/fail} is O(N) where N = API count.
//   - grpc_code has ≤15 possible values; biz_code is bounded by errcode constants.
//   - Total memory: ~200 bytes × (N×2 + N×15 + N×K) — typically < 100 KB for
//     a service with 50 APIs and 20 error codes.

var (
	// RPCTotal counts every RPC call, partitioned by method and success/fail.
	// Use for overall success rate dashboards.
	RPCTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "rpc",
		Subsystem: "server",
		Name:      "requests_total",
		Help:      "Total RPC requests by method and result (success/fail)",
		Labels:    []string{"method", "result"},
	})

	// RPCStatusTotal counts RPC errors by gRPC status code.
	// Use for HTTP-level error monitoring (4xx/5xx mapping).
	// Only incremented on error (grpc_code != OK for status errors).
	RPCStatusTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "rpc",
		Subsystem: "server",
		Name:      "status_errors_total",
		Help:      "Total RPC errors by method and gRPC status code (maps to HTTP status)",
		Labels:    []string{"method", "grpc_code"},
	})

	// RPCBizErrorTotal counts business-layer errors by errcode.
	// Use for business error monitoring and alerting.
	// Only incremented when grpc_code is OK (HTTP 200) but biz code != 0.
	RPCBizErrorTotal = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "rpc",
		Subsystem: "server",
		Name:      "biz_errors_total",
		Help:      "Total business errors by method and business error code (grpc_code=OK, HTTP 200)",
		Labels:    []string{"method", "biz_code"},
	})
)
`)
}

// ==================== pkg/model ====================

func (g *Generator) genPkgModel(abs string) error {
	dir := filepath.Join(abs, "pkg", "model")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	return writeIfNotExist(filepath.Join(dir, "common.go"), `package model

// PageInfo is a common pagination request.
type PageInfo struct {
	Page     int `+"`"+`json:"page"`+"`"+`
	PageSize int `+"`"+`json:"pageSize"`+"`"+`
}
`)
}

// ==================== internal/middleware ====================

func (g *Generator) genMiddleware(abs, modulePath string) error {
	dir := filepath.Join(abs, "internal", "middleware")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}

	// log_module.go
	logModule := fmt.Sprintf(`package middleware

import (
	"context"
	"strings"

	"google.golang.org/grpc"

	"%s/pkg/ctxutil"
)

// ModuleInterceptor injects the module name (from gRPC FullMethod) into ctx.
func ModuleInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		module := extractModule(info.FullMethod)
		ctx = ctxutil.WithModule(ctx, module)
		return handler(ctx, req)
	}
}

// extractModule extracts the gRPC method name from FullMethod.
// Example: "/demo.Demo/CreateUser" → "CreateUser"
func extractModule(fullMethod string) string {
	// FullMethod format: /{package}.{Service}/{Method}
	if idx := strings.LastIndex(fullMethod, "/"); idx >= 0 && idx+1 < len(fullMethod) {
		return fullMethod[idx+1:]
	}
	return "unknown"
}
`, modulePath)
	if err := writeIfNotExist(filepath.Join(dir, "log_module.go"), logModule); err != nil {
		return err
	}

	// i18n_interceptor.go — translates error messages, returns *errcode.Err (no status conversion)
	i18nInterceptor := fmt.Sprintf(`package middleware

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"%s/pkg/errcode"
	"%s/pkg/i18n"
)

// I18nInterceptor translates business error messages to the client's language.
// Input and output are both *errcode.Err — it only replaces the msg field.
// The original msg is preserved via WithOrigin() for logging purposes.
//
// For non-errcode errors (should not happen if LogInterceptor enforces),
// wraps as InternalError with translated message.
func I18nInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			return resp, nil
		}

		lang := extractLang(ctx)

		// *errcode.Err → translate msg, return *errcode.Err
		if e, ok := errcode.ExtractErr(err); ok {
			if msg := i18n.TranslateErrcode(lang, e.Code()); msg != "" {
				return nil, errcode.Newf(e.Code(), msg).WithGRPC(e.GRPCCode()).WithOrigin(e.Msg())
			}
			return nil, err
		}

		// Non-errcode error → wrap as InternalError
		msg := "internal error"
		if translated := i18n.TranslateErrcode(lang, errcode.InternalError); translated != "" {
			msg = translated
		}
		return nil, errcode.Newf(errcode.InternalError, msg).WithGRPC(codes.Internal)
	}
}

func extractLang(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if langs := md.Get("x-lang"); len(langs) > 0 {
			return langs[0]
		}
		if langs := md.Get("accept-language"); len(langs) > 0 {
			return langs[0]
		}
	}
	return "en"
}
`, modulePath, modulePath)
	if err := writeIfNotExist(filepath.Join(dir, "i18n_interceptor.go"), i18nInterceptor); err != nil {
		return err
	}

	// log_interceptor.go — logs all RPC calls (success + biz error + status error) with enforcement
	logInterceptor := fmt.Sprintf(`package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/zeromicro/go-zero/core/logx"

	"%s/pkg/ctxutil"
	"%s/pkg/errcode"
	"%s/pkg/i18n"
)

const maxLogBodyLen = 1024 // truncate request body in log if too long

// LogInterceptor logs all RPC calls: success (Info) and errors (Error/Info).
// At this point the error msg is already translated by I18nInterceptor,
// and ctx already contains module name from ModuleInterceptor.
//
// Enforcement rules (fail-fast in dev):
//  1. Every error MUST be *errcode.Err — non-errcode errors → panic.
//  2. Business errors (grpcCode == OK) MUST have i18n translation → panic if missing.
func LogInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		resp, err := handler(ctx, req)

		duration := time.Since(start)
		clientIP := ctxutil.ClientIP(ctx)
		lang := ctxutil.MetaValue(ctx, "x-lang", "accept-language")

		reqStr := marshalRequest(req)

		if err == nil {
			logx.WithContext(ctx).Infow("rpc ok",
				logx.Field("method", info.FullMethod),
				logx.Field("cost(ms)", duration.Milliseconds()),
				logx.Field("client_ip", clientIP),
				logx.Field("lang", lang),
				logx.Field("request", reqStr),
			)
			return resp, nil
		}

		// ── Rule 1: error MUST be *errcode.Err ──
		e, ok := errcode.ExtractErr(err)
		if !ok {
			logx.Severef(
				"[FATAL] %%s returned non-errcode error: %%v — "+
					"wrap it with errcode.Newf/Wrapf in your logic",
				info.FullMethod, err,
			)
			panic(fmt.Sprintf(
				"[errcode enforcement] %%s returned a raw error instead of *errcode.Err: %%v\n"+
					"Fix: use errcode.Newf(code, msg) or errcode.Wrapf(code, msg) in your logic layer.",
				info.FullMethod, err,
			))
		}

		// ── Rule 2: business error (HTTP 200) MUST have i18n msg ──
		grpcCode := e.GRPCCode()
		bizCode := e.Code()
		if errcode.IsOKCode(grpcCode) && bizCode != errcode.OK {
			if msg := i18n.TranslateErrcode("en", bizCode); msg == "" {
				logx.Severef(
					"[FATAL] %%s returned errcode %%d with no i18n translation — "+
						"add key \"errcode.%%d\" to pkg/i18n/locale/*.json",
					info.FullMethod, bizCode, bizCode,
				)
				panic(fmt.Sprintf(
					"[i18n enforcement] %%s: errcode %%d has no i18n translation.\n"+
						"Fix: add \"errcode\".\""+fmt.Sprintf("%%d", bizCode)+"\" to all locale JSON files.",
					info.FullMethod, bizCode,
				))
			}
		}

		// ── Log: business error vs status error ──
		origin := e.Origin() // original error msg before i18n translation

		if errcode.IsOKCode(grpcCode) {
			payload, _ := json.Marshal(struct {
				Code int    `+"`"+`json:"code"`+"`"+`
				Msg  string `+"`"+`json:"msg"`+"`"+`
			}{Code: bizCode, Msg: e.Msg()})
			fields := []logx.LogField{
				logx.Field("method", info.FullMethod),
				logx.Field("error", string(payload)),
				logx.Field("cost(ms)", duration.Milliseconds()),
				logx.Field("client_ip", clientIP),
				logx.Field("request", reqStr),
			}
			if origin != "" {
				fields = append(fields, logx.Field("origin", origin))
			}
			logx.WithContext(ctx).Infow("rpc biz_error", fields...)
		} else {
			fields := []logx.LogField{
				logx.Field("method", info.FullMethod),
				logx.Field("code", bizCode),
				logx.Field("grpc_code", grpcCode.String()),
				logx.Field("error", e.Msg()),
				logx.Field("cost(ms)", duration.Milliseconds()),
				logx.Field("client_ip", clientIP),
				logx.Field("lang", lang),
				logx.Field("request", reqStr),
			}
			if origin != "" {
				fields = append(fields, logx.Field("origin", origin))
			}
			logx.WithContext(ctx).Errorw("rpc status_error", fields...)
		}

		return nil, err
	}
}

func marshalRequest(req interface{}) string {
	reqStr := fmt.Sprintf("%%+v", req)
	if m, ok := req.(proto.Message); ok {
		if b, err := protojson.Marshal(m); err == nil {
			reqStr = string(b)
		}
	}
	if len(reqStr) > maxLogBodyLen {
		reqStr = reqStr[:maxLogBodyLen] + "...(truncated)"
	}
	return reqStr
}
`, modulePath, modulePath, modulePath)
	if err := writeIfNotExist(filepath.Join(dir, "log_interceptor.go"), logInterceptor); err != nil {
		return err
	}

	// grpc_status_interceptor.go — converts *errcode.Err into gRPC transport format (outermost)
	grpcStatusInterceptor := fmt.Sprintf(`package middleware

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"%s/pkg/errcode"
)

// GRPCStatusInterceptor converts *errcode.Err into gRPC transport format.
// This is the last interceptor to touch the response before gRPC framework sends it.
//
// For status errors (grpcCode != OK, e.g. 401/403/500):
//   Returns standard gRPC error → gateway maps to corresponding HTTP status.
//
// For business errors (grpcCode == OK, HTTP 200):
//   gRPC status.New(OK, msg).Err() returns nil by design.
//   We encode the JSON payload into grpc trailer "x-biz-error" so gateway
//   can read it, and return (resp, nil) as a successful gRPC response.
func GRPCStatusInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			return resp, nil
		}

		e, ok := errcode.ExtractErr(err)
		if !ok {
			return nil, err
		}

		// Status error (401/403/500): standard gRPC error
		if !errcode.IsOKCode(e.GRPCCode()) {
			return nil, errcode.StatusError(e)
		}

		// Business error (HTTP 200): encode JSON into trailer, return success
		st := errcode.ToGRPCStatus(e)
		grpc.SetTrailer(ctx, metadata.Pairs("x-biz-error", st.Message()))
		return resp, nil
	}
}
`, modulePath)
	if err := writeIfNotExist(filepath.Join(dir, "grpc_status_interceptor.go"), grpcStatusInterceptor); err != nil {
		return err
	}

	// metrics_interceptor.go — auto-report success/fail + status errors + biz errors
	metricsInterceptor := fmt.Sprintf(`package middleware

import (
	"context"
	"strconv"

	"google.golang.org/grpc"

	"%s/pkg/errcode"
	"%s/pkg/metrics"
)

// MetricsInterceptor records per-RPC success/failure counters.
//
// At this point err is still the original *errcode.Err (already translated),
// so we can distinguish status errors vs biz errors.
//
// Metrics reported:
//
//  1. rpc_server_requests_total{method, result}
//     - result="success" on nil error
//     - result="fail"    on any error
//
//  2. rpc_server_status_errors_total{method, grpc_code}
//     - Only for status errors: grpcCode != OK (maps to HTTP 4xx/5xx)
//
//  3. rpc_server_biz_errors_total{method, biz_code}
//     - Only for business errors: grpcCode == OK (HTTP 200) but code != 0
func MetricsInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, err := handler(ctx, req)

		method := info.FullMethod

		if err == nil {
			metrics.RPCTotal.Inc(method, "success")
			return resp, nil
		}

		metrics.RPCTotal.Inc(method, "fail")

		e, ok := errcode.ExtractErr(err)
		if !ok {
			metrics.RPCStatusTotal.Inc(method, "Internal")
			return nil, err
		}

		grpcCode := e.GRPCCode()
		bizCode := e.Code()

		if errcode.IsOKCode(grpcCode) {
			metrics.RPCBizErrorTotal.Inc(method, strconv.Itoa(bizCode))
		} else {
			metrics.RPCStatusTotal.Inc(method, grpcCode.String())
		}

		return nil, err
	}
}
`, modulePath, modulePath)
	if err := writeIfNotExist(filepath.Join(dir, "metrics_interceptor.go"), metricsInterceptor); err != nil {
		return err
	}

	// validate_interceptor.go — uses buf.build/go/protovalidate to enforce
	// (buf.validate.field) constraints declared in *.proto files at runtime via
	// protoreflect, without any codegen step. Modules `go mod tidy` will pull
	// `buf.build/go/protovalidate` automatically on first build.
	validateInterceptor := fmt.Sprintf(`package middleware

import (
	"context"

	"buf.build/go/protovalidate"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"%s/pkg/errcode"
)

// validator is the singleton protovalidate validator.
// Initialized once at process start; reuse across requests is safe and recommended.
var validator protovalidate.Validator

func init() {
	v, err := protovalidate.New()
	if err != nil {
		// Only happens if proto registry is broken / CEL rule fails to compile;
		// surface immediately at startup, unrecoverable.
		panic("protovalidate: failed to init validator: " + err.Error())
	}
	validator = v
}

// ValidateInterceptor validates incoming requests using buf.build/go/protovalidate.
// Reads (buf.validate.field) constraints declared in *.proto files at runtime via
// protoreflect, no codegen required.
//
// On validation failure, returns *errcode.Err with:
//   - code     = errcode.ContractViolation (95005), a dedicated code for
//     "caller did not honor the proto-declared constraints" — semantically
//     distinct from InvalidParam (95001) which is for legitimate business
//     parameter validation. This is treated as a CALLER BUG.
//   - grpcCode = codes.InvalidArgument (HTTP 400), so the gateway / client
//     can clearly tell apart "caller violated contract" (4xx) from
//     "business rejected the request" (200 + biz code).
//
// The detailed protovalidate field-level error is logged here for engineers
// to locate which field violated which rule. The msg returned to the client
// will be replaced by I18nInterceptor with the localized text declared at
// i18n key "errcode.95005".
func ValidateInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if msg, ok := req.(proto.Message); ok {
			if err := validator.Validate(msg); err != nil {
				// Detailed field-level violation goes to log only — clients receive
				// the unified i18n text, not internal field details.
				logx.WithContext(ctx).Errorw("rpc contract_violation",
					logx.Field("method", info.FullMethod),
					logx.Field("detail", err.Error()),
				)
				return nil, errcode.Newf(errcode.ContractViolation, err.Error()).
					WithGRPC(codes.InvalidArgument)
			}
		}
		return handler(ctx, req)
	}
}
`, modulePath)
	return writeIfNotExist(filepath.Join(dir, "validate_interceptor.go"), validateInterceptor)
}

// ==================== Makefile ====================

func (g *Generator) genMakefile(abs, serviceName string, zctx *ZRpcContext) error {
	port := 8080
	if zctx != nil && zctx.Port > 0 {
		port = zctx.Port
	}
	svcLower := strings.ToLower(serviceName)
	// Use GoPascal so dashed names like "cs-agent-rpc" become "CsAgentRpc" (a valid Go identifier),
	// matching the service ident emitted by mergeproto.go (single source of truth).
	svcCamel := GoPascal(serviceName)

	content := fmt.Sprintf(`# Custom configuration | 独立配置
# Service name | 项目名称
SERVICE=%s
# Service name in specific style | 项目经过style格式化的名称
SERVICE_STYLE=%s
# Service name in lowercase | 项目名称全小写格式
SERVICE_LOWER=%s
# Service name in dash format | 项目名称短杠格式
SERVICE_DASH=%s

# The project version, if you don't use git, you should set it manually | 项目版本
VERSION=$(shell git describe --tags --always 2>/dev/null || echo "dev")

# The project file name style | 项目文件命名风格
PROJECT_STYLE=go_zero

# Whether to use i18n | 是否启用 i18n
PROJECT_I18N=true

# Ent enabled features | Ent 启用的官方特性
# - sql/execquery: 暴露底层 ExecContext/QueryContext，便于自定义裸 SQL
# - intercept:     启用 Interceptor 机制（cachex 等中间件依赖此特性）
# - sql/upsert:    启用 Create.OnConflict / CreateBulk.OnConflict（编译为 INSERT ... ON DUPLICATE KEY UPDATE），
#                  在 MySQL/OceanBase 下针对所有 UNIQUE KEY 冲突生效，提供原子的幂等写入语义，
#                  适用于配置同步 / 批量导入去重 / 避免"先 Get 再 Create/Update"的竞态
ENT_FEATURE=sql/execquery,intercept,sql/upsert

# The service port | 服务端口
PORT=%d

# The arch of the build | 构建的架构
GOARCH=amd64

# The docker image repo | Docker 仓库地址
DOCKER_REPO=docker.io/xxx

# ---- You may not need to modify the codes below | 下面的代码大概率不需要更改 ----

GO ?= go
GOFMT ?= gofmt "-s"
GOFILES := $(shell find . -name "*.go")
LDFLAGS := -s -w

# Default model (for single module generation)
model ?= all

# ==================== Proto ====================

.PHONY: pull-proto
pull-proto: # Pull proto from remote repo | 从远程仓库拉取 proto (需配置 proto.yaml)
	@if [ ! -f proto.yaml ]; then echo "proto.yaml not found, using local desc/ directly"; exit 0; fi
	@REPO=$$(grep 'repo:' proto.yaml | head -1 | awk '{print $$2}'); \
	REF=$$(grep 'ref:' proto.yaml | head -1 | awk '{print $$2}'); \
	REMOTE_PATH=$$(grep 'path:' proto.yaml | head -1 | awk '{print $$2}'); \
	TARGET=$$(grep 'target:' proto.yaml | head -1 | awk '{print $$2}'); \
	TARGET=$${TARGET:-desc/}; \
	if [ -z "$$REPO" ]; then echo "ERROR: repo not configured in proto.yaml"; exit 1; fi; \
	TMPDIR=$$(mktemp -d); \
	echo "[pull-proto] Cloning $$REPO @ $$REF ..."; \
	git clone --depth 1 --branch "$$REF" "$$REPO" "$$TMPDIR" 2>/dev/null || \
		(echo "ERROR: failed to clone $$REPO @ $$REF"; rm -rf "$$TMPDIR"; exit 1); \
	mkdir -p "$$TARGET"; \
	echo "[pull-proto] Copying $$TMPDIR/$$REMOTE_PATH → $$TARGET"; \
	cp -r "$$TMPDIR/$$REMOTE_PATH"* "$$TARGET/"; \
	rm -rf "$$TMPDIR"; \
	echo "[pull-proto] Done. Proto version: $$REF"

.PHONY: gen-rpc
gen-rpc: # Generate RPC files from proto | 合并 desc/ → 根 proto → protoc → types/
	@zctl rpc merge-proto
	zctl rpc protoc ./$(SERVICE_STYLE).proto --go_out=./types --go-grpc_out=./types --zrpc_out=. --style=$(PROJECT_STYLE) -I=./proto -I=.
	@echo "Generate RPC files successfully"

# ==================== Ent ====================

.PHONY: gen-ent
gen-ent: # Generate Ent codes | 生成 Ent 的代码
	@go get entgo.io/ent@latest 2>/dev/null; true
	@test -d ent/schema || (echo "ERROR: ent/schema not found, run 'make gen-ent-new name=XXX' first"; exit 1)
	go run -mod=mod entgo.io/ent/cmd/ent generate ./ent/schema --feature $(ENT_FEATURE)
	@echo "Generate Ent files successfully"

.PHONY: gen-ent-new
gen-ent-new: # Create new ent schema | 新建 ent schema (usage: make gen-ent-new name=User)
	$(eval _ENT_NAME := $(if $(name),$(name),$(word 2,$(MAKECMDGOALS))))
	@if [ -z "$(_ENT_NAME)" ]; then echo "Usage: make gen-ent-new name=User (or: make gen-ent-new User)"; exit 1; fi
	@go get entgo.io/ent@latest 2>/dev/null; true
	@mkdir -p ent/schema
	go run -mod=mod entgo.io/ent/cmd/ent new $(_ENT_NAME)
	@# Rename ent-generated lowercase filename to snake_case (e.g. iamapp.go → iam_app.go)
	@_LOWER=$$(echo "$(_ENT_NAME)" | tr '[:upper:]' '[:lower:]'); \
	_SNAKE=$$(echo "$(_ENT_NAME)" | sed 's/\([A-Z]\)/_\1/g' | sed 's/^_//' | tr '[:upper:]' '[:lower:]'); \
	if [ "$$_LOWER" != "$$_SNAKE" ] && [ -f "ent/schema/$$_LOWER.go" ]; then \
		mv "ent/schema/$$_LOWER.go" "ent/schema/$$_SNAKE.go"; \
		echo "Renamed ent/schema/$$_LOWER.go → ent/schema/$$_SNAKE.go"; \
	fi
	@echo "Created ent schema: $(_ENT_NAME)"

.PHONY: gen-ddl
gen-ddl: # Generate DDL diff (review-only, NOT executed) | 输出 ent schema 与 DB 的差量 DDL 到 migrations/ (usage: make gen-ddl [name=add_role_cid])
	$(eval _DDL_NAME := $(if $(name),$(name),auto))
	@mkdir -p migrations
	go run ./cmd/migrate-ddl -f etc/$(SERVICE_STYLE).yaml -dir migrations -name "$(_DDL_NAME)"
	@echo "[gen-ddl] Done. Review the latest file under migrations/ before applying to prod."

# Allow positional args like "make gen-ent-new User" without error
%%:
	@:

# ==================== Code Generation ====================

.PHONY: gen-rpc-ent-logic
gen-rpc-ent-logic: # Generate CRUD+DAO+proto+logic from Ent | 一键生成全套 (usage: make gen-rpc-ent-logic model=User)
	zctl rpc ent --schema=./ent/schema --style=$(PROJECT_STYLE) --service_name=$(SERVICE) --model=$(model)
	@$(MAKE) gen-rpc
	@echo "Generate successfully (ent: dao + desc proto; gen-rpc: merge + protoc + logic/server)"

.PHONY: gen-dao-sql
gen-dao-sql: # Generate DAO method from SQL | 根据SQL生成DAO方法 (usage: make gen-dao-sql sql="SELECT ...")
	@if [ -z "$(sql)" ]; then echo "ERROR: please specify sql, e.g.: make gen-dao-sql sql=\"SELECT * FROM user WHERE status=1\""; exit 1; fi
	zctl rpc dao --schema=./ent/schema --sql="$(sql)" --style=$(PROJECT_STYLE) --overwrite
	@echo "Generate DAO method from SQL successfully"

# ==================== Test ====================

.PHONY: test
test: # Run all tests | 运行全部单测
	go test -v --cover ./internal/...

.PHONY: test-module
test-module: # Run tests for a module | 运行某模块的单测 (usage: make test-module module=userinfo)
	go test -v --cover ./internal/logic/$(module)/... ./internal/dao/...

.PHONY: test-func
test-func: # Run a single test function | 运行单个测试函数 (usage: make test-func func=TestXxx pkg=./internal/...)
	@if [ -z "$(func)" ] || [ -z "$(pkg)" ]; then echo "Usage: make test-func func=TestXxx pkg=./internal/logic/xxx/..."; exit 1; fi
	go test -v -run $(func) $(pkg)

.PHONY: grpc-test
grpc-test: # Call a gRPC method via grpcurl | 单接口测试 (usage: make grpc-test method=xxx.Xxx/Xxx req='{}')
	@if [ -z "$(method)" ]; then echo "Usage: make grpc-test method=demo.Demo/Ping req='{}'"; exit 1; fi
	grpcurl -plaintext -d '$(req)' localhost:$(PORT) $(method)

# ==================== Run & Build ====================

.PHONY: run
run: # Run the service locally | 本地运行
	go run $(SERVICE_STYLE).go -f etc/$(SERVICE_STYLE).yaml

.PHONY: build
build: # Build for current platform | 构建当前平台可执行文件
	go build -ldflags "$(LDFLAGS)" -trimpath -o $(SERVICE_STYLE) $(SERVICE_STYLE).go
	@echo "Build successfully"

.PHONY: build-linux
build-linux: # Build project for Linux | 构建Linux下的可执行文件
	env CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -trimpath -o $(SERVICE_STYLE) $(SERVICE_STYLE).go
	@echo "Build project for Linux successfully"

.PHONY: build-mac
build-mac: # Build project for MacOS | 构建MacOS下的可执行文件
	env CGO_ENABLED=0 GOOS=darwin GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -trimpath -o $(SERVICE_STYLE) $(SERVICE_STYLE).go
	@echo "Build project for MacOS successfully"

.PHONY: build-win
build-win: # Build project for Windows | 构建Windows下的可执行文件
	env CGO_ENABLED=0 GOOS=windows GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -trimpath -o $(SERVICE_STYLE).exe $(SERVICE_STYLE).go
	@echo "Build project for Windows successfully"

# ==================== Docker ====================

.PHONY: docker
docker: # Build the docker image | 构建 docker 镜像
	docker build -t $(DOCKER_REPO)/$(SERVICE_DASH):$(VERSION) .
	@echo "Build docker successfully"

.PHONY: publish-docker
publish-docker: # Publish docker image | 发布 docker 镜像
	docker push $(DOCKER_REPO)/$(SERVICE_DASH):$(VERSION)
	@echo "Publish docker successfully"

# ==================== Doc & Debug ====================

.PHONY: swagger
swagger: # Launch gRPC Web UI for debugging | 启动可调试的 gRPC Web UI（需服务运行中）
	@echo "Launching gRPC Web UI (browser will open automatically)"
	@echo "Requires service running at localhost:$(PORT) with reflection enabled (non-prod)."
	grpcui -plaintext localhost:$(PORT)

.PHONY: proto-doc
proto-doc: # Generate API doc with field table + JSON examples | 生成表格式 API 文档
	@mkdir -p doc
	protoc -I=. -I=./proto --doc_out=./doc --doc_opt=json,$(SERVICE_STYLE).json $(SERVICE_STYLE).proto
	zctl rpc proto-doc --input=doc/$(SERVICE_STYLE).json
	@echo "Generated doc/$(SERVICE_STYLE)_api.md"

# ==================== Misc ====================

.PHONY: tidy
tidy: # Go mod tidy | 整理依赖
	go mod tidy

.PHONY: fmt
fmt: # Format the codes | 格式化代码
	$(GOFMT) -w $(GOFILES)

.PHONY: lint
lint: # Run go linter | 运行代码错误分析
	golangci-lint run -D staticcheck

.PHONY: tools
tools: # Install the necessary tools | 安装必要的工具
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	$(GO) install github.com/qqz14/zctl@latest
	$(GO) install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest
	$(GO) install github.com/fullstorydev/grpcui/cmd/grpcui@latest
	$(GO) install github.com/pseudomuto/protoc-gen-doc/cmd/protoc-gen-doc@latest

.PHONY: health
health: # Check gRPC health status | 检查 gRPC 健康状态
	grpcurl -plaintext localhost:$(PORT) grpc.health.v1.Health/Check

.PHONY: grpc-list
grpc-list: # List all gRPC services | 列出所有 gRPC 服务
	grpcurl -plaintext localhost:$(PORT) list

.PHONY: help
help: # Show help | 显示帮助
	@grep -E '^[a-zA-Z0-9 -]+:.*#'  Makefile | sort | while read -r l; do printf "\033[1;32m$$(echo $$l | cut -f 1 -d':')\\033[00m:$$(echo $$l | cut -f 2- -d'#')\\n"; done
`, svcCamel, svcLower, svcLower, svcLower, port)

	return writeIfNotExist(filepath.Join(abs, "Makefile"), content)
}

// ==================== Dockerfile ====================

func (g *Generator) genDockerfile(abs, serviceName string) error {
	svcLower := strings.ToLower(serviceName)
	content := fmt.Sprintf(`# ==========================================
# Stage 1: Build
# ==========================================
FROM golang:1.25-alpine AS builder

ENV GO111MODULE=on \
    GOPROXY=https://goproxy.cn,direct \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -ldflags="-s -w" -o %s ./%s.go

# ==========================================
# Stage 2: Run
# ==========================================
FROM alpine:3.22

WORKDIR /app

ENV TZ=UTC

RUN apk update --no-cache && \
    apk add --no-cache tzdata bash gettext ca-certificates jq && \
    update-ca-certificates

COPY --from=builder /build/%s ./
COPY ./entrypoint.sh ./entrypoint.sh
RUN chmod +x ./entrypoint.sh

COPY ./etc/%s.yaml.template ./etc/%s.yaml.template

# i18n locale files (required at runtime by %s.go — see pkg/i18n/locale/*.json)
COPY --from=builder /build/pkg/i18n/locale ./pkg/i18n/locale

ENTRYPOINT ["./entrypoint.sh"]
`, svcLower, svcLower, svcLower, svcLower, svcLower, svcLower)

	return writeIfNotExist(filepath.Join(abs, "Dockerfile"), content)
}

// ==================== entrypoint.sh ====================

func (g *Generator) genEntrypoint(abs string) error {
	content := `#!/bin/bash
set -e

# Load secrets from JSON file (e.g. AWS Secrets Manager mount)
json_file_path="${PATH_TO_SECRET_FILE:-}"
if [ -n "$json_file_path" ] && [ -f "$json_file_path" ]; then
    if command -v jq >/dev/null 2>&1; then
        eval "$(jq -r 'to_entries | .[] | "export \(.key)=\"\(.value)\""' "$json_file_path")"
        echo "[entrypoint] Loaded secrets from $json_file_path"
    fi
fi

# Find config template and render with envsubst
CONFIG_TEMPLATE=$(find ./etc -name "*.yaml.template" | head -1)
if [ -z "$CONFIG_TEMPLATE" ]; then
    echo "[entrypoint] ERROR: no .yaml.template found in ./etc/"
    exit 1
fi

CONFIG_FILE="${CONFIG_TEMPLATE%.template}"
echo "[entrypoint] Rendering: $CONFIG_TEMPLATE -> $CONFIG_FILE"
envsubst < "$CONFIG_TEMPLATE" > "$CONFIG_FILE"

# Find and run the binary
BINARY=$(find . -maxdepth 1 -type f -executable ! -name "*.sh" | head -1)
if [ -z "$BINARY" ]; then
    echo "[entrypoint] ERROR: no executable binary found"
    exit 1
fi

echo "[entrypoint] Starting $BINARY ..."
exec "$BINARY" -f "$CONFIG_FILE" "$@"
`
	return writeIfNotExist(filepath.Join(abs, "entrypoint.sh"), content)
}

// ==================== etc/xxx.yaml.template ====================

func (g *Generator) genEtcTemplate(abs, serviceName string, zctx *ZRpcContext) error {
	svcLower := strings.ToLower(serviceName)
	port := 8080
	if zctx != nil && zctx.Port > 0 {
		port = zctx.Port
	}
	portStr := fmt.Sprintf("%d", port)
	upperSvc := strings.ToUpper(serviceName)

	content := fmt.Sprintf(`Name: %s.rpc
ListenOn: 0.0.0.0:${%s_RPC_PORT:-%s}

# Environment: dev | stage | uat | prod
# Only dev/stage allow auto-migrate (create tables automatically)
Env: ${ENV:-dev}

DatabaseConf:
  Type: mysql
  Host: ${DB_HOST:-127.0.0.1}
  Port: ${DB_PORT:-3306}
  DBName: ${DB_NAME:-%s}
  Username: ${DB_USER:-root}
  Password: "${DB_PASSWORD:-password}"
  MaxOpenConn: 50
  SSLMode: disable
  CacheTime: 5

Log:
  ServiceName: %sLogger
  Mode: console
  Level: ${LOG_LEVEL:-info}
  Encoding: plain
  StackCoolDownMillis: 100

RedisConf:
  Host: ${REDIS_HOST:-127.0.0.1:6379}
  Db: 0

Prometheus:
  Host: 0.0.0.0
  Port: ${PROMETHEUS_PORT:-4000}
  Path: /metrics

#Telemetry:
#  Name: %s-rpc
#  Endpoint: localhost:4317
#  Sampler: 1.0
#  Batcher: otlpgrpc
`, svcLower, upperSvc, portStr, svcLower, svcLower, svcLower)

	dir := filepath.Join(abs, "etc")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	return writeIfNotExist(filepath.Join(dir, svcLower+".yaml.template"), content)
}

// ==================== .gitignore ====================

func (g *Generator) genGitignore(abs string) error {
	content := `# Binaries
*.exe
*.exe~
*.dll
*.so
*.dylib

# Test binary
*.test

# Output of the go coverage tool
*.out

# IDE
.idea/
.vscode/
*.swp
*.swo

# OS
.DS_Store
Thumbs.db

# Build output
/bin/
/dist/

# Env / Secrets
*.env
secret_manager*.json

# Generated etc config (keep .template only)
etc/*.yaml
!etc/*.yaml.template
`
	return writeIfNotExist(filepath.Join(abs, ".gitignore"), content)
}

// ==================== README.md ====================

func (g *Generator) genProjectReadme(abs, serviceName string, zctx *ZRpcContext) error {
	svcLower := strings.ToLower(serviceName)
	port := 8080
	if zctx != nil && zctx.Port > 0 {
		port = zctx.Port
	}

	content := fmt.Sprintf(`# %s

基于 go-zero + entgo 的 gRPC 微服务。

## 快速开始

`+"```"+`bash
make run
`+"```"+`

## 开发流程

`+"```"+`bash
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
`+"```"+`

## 常用命令

详见 [zctl-commands.md](./zctl-commands.md)

## Proto 协议管理

### 最佳实践

所有微服务的 proto 协议由**统一的 proto 仓库**集中管理（包括本服务），各微服务通过 `+"`"+`proto.yaml`+"`"+` 配置同步协议。

统一仓库结构示例：

`+"```"+`
proto-definitions/             # 统一 proto 仓库
├── base/
│   └── base.proto             # 公共 message（Empty / PageInfo 等）
├── %s/                        # 本服务的协议
│   ├── ping/ping.proto
│   └── userinfo/user_info.proto
├── other-service/             # 其他服务的协议
│   └── ...
└── ...
`+"```"+`

### 配置

项目根目录 `+"`"+`proto.yaml`+"`"+`（初始化时已创建，填入实际值即可）：

`+"```"+`yaml
remote:
  repo: git@github.com:your-org/proto-definitions.git
  ref: main              # 开发阶段跟 main 分支；发版用 tag 如 v1.2.0
  path: %s/              # 本服务在远程仓库中的子目录
  target: desc/          # 同步到本地 desc/
`+"```"+`

### 协议开发流程

`+"```"+`bash
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
cp -r /path/to/%s/desc/* %s/
git add .
git commit -m "feat(%s): add getUserList pagination"
git tag v1.3.0
git push origin main --tags

# 6. 回到服务仓库，更新 proto.yaml 的 ref（如果用 tag 锁版本）
#    ref: v1.3.0
# 7. 提交服务仓库
git add .
git commit -m "feat: update proto to v1.3.0"
git push
`+"```"+`

### CI/CD 协议一致性保证

CI pipeline 中加入校验步骤，确保部署时使用的协议与 proto 仓库一致：

`+"```"+`yaml
# .github/workflows/ci.yaml (示例)
steps:
  - name: Pull proto
    run: make pull-proto

  - name: Verify proto consistency
    run: |
      make gen-rpc
      if [ -n "$(git status --porcelain types/ %s.proto internal/server/ %s_client/)" ]; then
        echo "ERROR: proto out of sync with proto repo"
        git diff --stat
        exit 1
      fi

  - name: Test
    run: make test

  - name: Build
    run: make build-linux
`+"```"+`

**核心原则**：

- **proto 仓库是 single source of truth**，所有服务从同一个仓库拉取
- **`+"`"+`proto.yaml`+"`"+` 中的 `+"`"+`ref`+"`"+` 锁定版本**，CI 用 tag（如 `+"`"+`v1.3.0`+"`"+`），开发可用 `+"`"+`main`+"`"+`
- **CI 验证无 diff** = 部署代码与协议版本一致
- **先改 proto 仓库，再改业务仓库**，确保协议变更有独立的版本历史和 review

### 协议文档生成

proto 协议可一键转换为表格式 API 文档（含字段说明、类型、必填标记、JSON 示例）：

`+"```"+`bash
make proto-doc      # 生成 doc/{service}_api.md
`+"```"+`

文档中每个接口包含请求/响应参数表格和 JSON 示例，嵌套字段用 `+"`"+`info.username`+"`"+` 格式展示，方便协作和 review。

proto 字段的注释（ent schema 的 `+"`"+`.Comment("xxx")`+"`"+`）会自动成为文档中的"说明"列。

## 测试与调试

### 浏览器调试（Swagger）

启动服务后，一行命令打开 gRPC Web UI，浏览器中可查看所有接口、填写参数、直接调试：

`+"```"+`bash
make run             # 先启动服务
make swagger         # 自动打开浏览器 gRPC 调试 UI
`+"```"+`

> 仅非 prod 环境可用（prod 环境 gRPC 反射已关闭）。

### 单接口测试（命令行）

用 grpcurl 快速调用指定接口：

`+"```"+`bash
make grpc-test method=%s.%s/Ping req='{}'
`+"```"+`

### 单元测试（Mock）

zctl 自动为每个 DAO 生成 testify mock（`+"`"+`internal/dao/mock/`+"`"+`），同时为每个模块生成测试骨架。

`+"```"+`bash
# 运行全部单测
make test

# 运行指定模块的单测
make test-module module=userinfo

# 运行单个测试函数
make test-func func=TestCreateUserInfo pkg=./internal/logic/userinfo/user_info/...
`+"```"+`

测试骨架位于 `+"`"+`internal/logic/{group}/{model}/{model}_test.go`+"`"+`，含 `+"`"+`t.Skip()`+"`"+` 占位，填充断言后即可使用。

## 服务发现

默认不依赖 Etcd。K8s 用 headless service + DNS：

`+"```"+`yaml
%sRpc:
  Target: dns:///%s-rpc-svc:%d
`+"```"+`

本地开发直连：

`+"```"+`yaml
%sRpc:
  Target: direct://127.0.0.1:%d
`+"```"+`

`+"```"+`bash
make help   # 查看所有可用命令
`+"```"+`

## 数据库 schema 变更（DDL）

| 环境       | DDL 落地方式 |
| ---------- | ------------ |
| dev/stage  | 服务启动时 `+"`"+`entClient.Schema.Create()`+"`"+` 自动同步 |
| uat/prod   | **手工执行**：`+"`"+`make gen-ddl`+"`"+` 输出差量 SQL → DBA 评审 → 人工 apply |

`+"```"+`bash
make gen-ddl                              # 默认 name=auto
make gen-ddl name=add_role_cid_to_user_role  # 推荐：语义化命名
# → migrations/{YYYYMMDD_HHMMSS}_{name}.sql
`+"```"+`

底层调 `+"`"+`cmd/migrate-ddl`+"`"+`，用 ent 的 `+"`"+`Schema.WriteTo(file, WithDropColumn(true), WithDropIndex(true))`+"`"+` 把 `+"`"+`ent/schema/*.go`+"`"+` 与 DB 现状的差量 DDL **写入文件**（走 `+"`"+`schema.WriteDriver`+"`"+`，物理上不会落到 DB）。

**DBA 评审 checklist**：

- [ ] `+"`"+`DROP COLUMN`+"`"+` 是真删字段，还是重命名？是重命名 → 改写成 `+"`"+`ALTER TABLE ... CHANGE COLUMN`+"`"+` 保留数据
- [ ] `+"`"+`DROP INDEX`+"`"+` 在大表上是否触发慢查询，是否走 OnlineDDL / pt-osc / gh-ost
- [ ] 唯一键列变化前是否清理重复数据（`+"`"+`GROUP BY ... HAVING COUNT(*)>1`+"`"+`）
- [ ] 灰度顺序：先发应用代码兼容新旧 schema → apply DDL → 下线兼容代码

`+"`"+`migrations/*.sql`+"`"+` 全部纳入 git，作为生产 DDL 的可追溯真相。

## 项目结构

`+"```"+`
%s/
├── %s.go                # 主入口
├── %s.proto             # 合并后的 proto（自动生成，勿编辑）
├── proto.yaml             # 远程 proto 配置（可选）
├── Makefile
├── Dockerfile
├── desc/                  # proto 源文件（按业务域分子目录）
│   ├── base.proto
│   └── {group}/
├── types/                 # protoc 生成的 pb 文件
├── ent/schema/            # entgo 表结构定义
├── internal/
│   ├── config/
│   ├── svc/               # ServiceContext（DAO 实例注入）
│   ├── server/            # gRPC server 实现
│   ├── logic/             # 业务逻辑（层级对齐 desc/）
│   ├── middleware/         # 拦截器
│   ├── dao/               # DAO 接口
│   ├── dao/impl/          # DAO 实现（OceanBase）
│   └── dao/mock/          # DAO Mock（自动生成）
├── pkg/
│   ├── errcode/           # 错误码 + 错误类型
│   ├── ctxutil/           # ctx 工具 + 日志
│   ├── model/             # 公共模型（PageInfo 等）
│   ├── consts/            # 常量
│   ├── i18n/              # 国际化
│   └── metrics/           # Prometheus 指标
└── %s_client/             # RPC 客户端 SDK
`+"```"+`
`, serviceName,
		svcLower, svcLower,
		svcLower, svcLower, svcLower,
		svcLower, svcLower,
		svcLower, serviceName,
		svcLower, svcLower, port,
		svcLower, port,
		svcLower, svcLower, svcLower,
		svcLower)

	return writeIfNotExist(filepath.Join(abs, "README.md"), content)
}

// ==================== zctl-commands.md ====================

func (g *Generator) genCommandsDoc(abs, serviceName string) error {
	return GenCommandsDoc(abs, serviceName)
}

// GenCommandsDoc generates/overwrites zctl-commands.md documentation.
// Exported so that every subcommand (ent, dao, merge-proto, enum) can refresh it.
func GenCommandsDoc(abs, serviceName string) error {
	svcLower := strings.ToLower(serviceName)

	content := strings.ReplaceAll(`# zctl 桩命令使用说明

> 本文档由 zctl 自动生成（每次运行 zctl 桩命令时自动覆盖更新），详细介绍每个桩命令的功能、参数、生成的文件、注意事项及使用示例。

---

## 目录

1. [make gen-rpc](#1-make-gen-rpc) — 合并 proto + 生成 pb + 自动扫描 enum
2. [make gen-ent-new](#2-make-gen-ent-new) — 创建 Ent Schema
3. [make gen-ent](#3-make-gen-ent) — 生成 Ent ORM 代码
4. [make gen-rpc-ent-logic](#4-make-gen-rpc-ent-logic) — 根据 Ent 生成全套模块代码
5. [make gen-dao-sql](#5-make-gen-dao-sql) — 根据 SQL 生成自定义 DAO 方法
6. [zctl rpc enum](#6-zctl-rpc-enum) — 手动生成枚举常量
7. [zctl rpc merge-proto](#7-zctl-rpc-merge-proto) — 合并 desc 下所有 proto

---

## 1. ~make gen-rpc~

**做什么**：将 ~desc/~ 目录下所有 .proto 合并成根 proto，调用 protoc 生成 pb 文件到 ~types/~，同时自动完成以下工作：

1. 合并 ~desc/**/*.proto~ → 根 proto
2. 自动扫描 proto 中的 ~enum~ 定义 → 生成 Go 枚举助手
3. protoc 生成 pb 文件 → ~types/{package}/~
4. 自动为 ~desc/~ 下每个子目录（模块）生成对应的 **pkg/model/{module}.go**、**pkg/consts/{module}.go**、**pkg/errcode/{module}.go** 占位文件
5. 日志自动注入 ~module~ 字段：通过 ~ModuleInterceptor~ 从 gRPC FullMethod 提取模块名注入到 ctx，后续所有 ~ctxutil.L(ctx)~ 打出的日志都自动携带 ~module~ 字段，方便按模块过滤日志

**执行命令**：
~~~bash
make gen-rpc
~~~

**内部调用链**：
1. ~zctl rpc merge-proto~ → 扫描 ~desc/**/*.proto~ → 合并到 ~{{SERVICE}}.proto~
2. 自动扫描 ~desc/~ 中的 ~enum~ → 生成 ~pkg/enums/{snake_name}.go~
3. ~zctl rpc protoc {{SERVICE}}.proto~ → protoc → ~types/{package}/~
4. 扫描 ~desc/~ 子目录 → 生成 ~pkg/model/~、~pkg/consts/~、~pkg/errcode/~ 模块占位文件

**生成/修改的文件**：

| 文件路径 | 操作 | 说明 |
|----------|------|------|
| ~{{SERVICE}}.proto~ | 覆盖 | 合并后的根 proto（只读，不要手动编辑） |
| ~types/{{SERVICE}}/*.pb.go~ | 覆盖 | protoc 生成的 Go pb 文件 |
| ~types/{{SERVICE}}/*_grpc.pb.go~ | 覆盖 | protoc 生成的 gRPC 桩代码 |
| ~internal/server/*_server.go~ | 新建/跳过已有 | gRPC server 实现（按 service 分包） |
| ~internal/logic/**/*_logic.go~ | 新建/跳过已有 | Logic 层（层级对齐 desc/ 目录） |
| ~pkg/enums/{snake_name}.go~ | 覆盖 | proto enum → Go 枚举助手 |
| ~pkg/model/{module}.go~ | 新建/跳过已有 | 模块 VO/DTO 占位（按 desc 子目录名） |
| ~pkg/consts/{module}.go~ | 新建/跳过已有 | 模块常量占位 |
| ~pkg/errcode/{module}.go~ | 新建/跳过已有 | 模块错误码占位 |

**日志 module 自动注入**：

拦截器链中的 ~ModuleInterceptor~ 会自动从 gRPC FullMethod（如 ~/demo.User/CreateUser~）提取模块名（~User~）并注入 ctx。后续代码只需用 ~ctxutil.L(ctx).Infow(...)~ 打日志，输出自动包含：

~~~json
{"module": "User", "msg": "dao.User.Create ok", "id": 123}
~~~

无需手动传递 module 参数，按 ~module=User~ 即可过滤某模块的所有日志。

**注意事项**：
- ~{{SERVICE}}.proto~ 是合并生成的，**设为只读（0444）权限**，所有 proto 修改都应在 ~desc/~ 子目录下进行。
- ~internal/logic/~ 和 ~internal/server/~ 下已有文件不会被覆盖，只新增缺失的。
- proto 中的 ~enum~ 命名需遵循 ~ENUM_NAME_VALUE~ 格式（如 ~USER_STATUS_NORMAL~），才能被自动扫描。

**示例**：假设 ~desc/user/user.proto~ 中定义了：
~~~protobuf
enum UserStatus {
  USER_STATUS_NORMAL = 0;
  USER_STATUS_DISABLED = 1;
  USER_STATUS_LOCKED = 2;
}
~~~

运行 ~make gen-rpc~ 后自动生成 ~pkg/enums/user_status.go~：
~~~go
type UserStatus int32
const (
    UserStatusNormal   UserStatus = 0
    UserStatusDisabled UserStatus = 1
    UserStatusLocked   UserStatus = 2
)
func (e UserStatus) String() string { ... }
func (e UserStatus) IsValid() bool { ... }
func ParseUserStatus(s string) (UserStatus, error) { ... }
~~~

---

## 2. ~make gen-ent-new~

**做什么**：在 ~ent/schema/~ 下创建新的 ent schema 文件。

**执行命令**：
~~~bash
make gen-ent-new name=User
# 或位置参数写法
make gen-ent-new User
~~~

参数：

| 参数 | 必填 | 说明 |
|------|------|------|
| ~name~ | 是 | Schema 名称，大驼峰（如 ~User~, ~UserToken~） |

生成的文件：

| 文件路径 | 操作 | 说明 |
|----------|------|------|
| ~ent/schema/{snake_name}.go~ | 新建 | Ent schema 模板 |

**注意事项**：
- 名称必须用 **大驼峰** 格式（~User~ 而非 ~user~），ent 要求首字母大写。
- 生成后需 **手动编辑** schema 文件，添加 Fields() 和 Edges()。
- 如果 ~ent/schema/~ 目录不存在会自动创建。

**下一步**：编辑 schema → ~make gen-ent~ → ~make gen-rpc-ent-logic model=User~

---

## 3. ~make gen-ent~

**做什么**：根据 ~ent/schema/~ 下的所有 schema 定义，运行 ~go run entgo.io/ent/cmd/ent generate~ 生成 ent ORM 代码。

**执行命令**：
~~~bash
make gen-ent
~~~

**生成/修改的文件**：

| 目录/文件 | 操作 | 说明 |
|----------|------|------|
| ~ent/client.go~ | 覆盖 | ent Client（CRUD 入口） |
| ~ent/{model}.go~ | 覆盖 | 实体结构体 |
| ~ent/{model}_create.go~ | 覆盖 | Create 构建器 |
| ~ent/{model}_update.go~ | 覆盖 | Update 构建器 |
| ~ent/{model}_query.go~ | 覆盖 | Query 构建器 |
| ~ent/{model}_delete.go~ | 覆盖 | Delete 构建器 |
| ~ent/{model}/where.go~ | 覆盖 | Where predicate（查询条件） |
| ~ent/migrate/schema.go~ | 覆盖 | 数据库迁移 schema |
| ~ent/hook/~ | 覆盖 | Hook 工具 |
| ~ent/intercept/~ | 覆盖 | 查询拦截器 |

**注意事项**：
- ~ent/~ 目录下除 ~ent/schema/~ 以外的所有文件 **每次都会被覆盖**，不要手动修改。
- 修改 schema 后必须重新运行此命令。
- 启用的 ent features 在 Makefile 的 ~ENT_FEATURE~ 变量中配置（默认 ~sql/execquery,intercept,sql/upsert~）。

**前置条件**：至少有一个 schema（先运行 ~make gen-ent-new name=XXX~）。

---

## 4. ~make gen-rpc-ent-logic~

**做什么**：根据 ent schema 自动生成一个模块的全套代码：DAO 接口 + DAO 实现 + CRUD Logic + 错误码 + 测试骨架 + desc proto。

**执行命令**：
~~~bash
# 生成指定模块
make gen-rpc-ent-logic model=User

# 生成所有模块
make gen-rpc-ent-logic model=all
~~~

**参数**：

| 参数 | 必填 | 说明 |
|------|------|------|
| ~model~ | 是 | ent schema 名称（大驼峰）或 ~all~ |

**生成/修改的文件**（以 ~model=User~ 为例）：

| 文件路径 | 操作 | 说明 | 你需要做什么 |
|----------|------|------|------------|
| ~internal/dao/user_dao.go~ | 新建/跳过 | DAO 接口（5 个方法） | 可追加自定义方法 |
| ~internal/dao/impl/user_oceanbase.go~ | 新建/跳过 | DAO 实现（ent ORM） | 可追加自定义方法 |
| ~internal/dao/mock/user_dao_mock.go~ | 覆盖 | DAO Mock（testify/mock） | 自动同步，无需手动 |
| ~internal/logic/user/user/user_test.go~ | 新建/跳过 | 测试骨架（含 Skip） | 填充断言 |
| ~pkg/errcode/user.go~ | 新建/跳过 | 模块错误码 | 按需添加错误码 |
| ~pkg/model/user.go~ | 新建/跳过 | VO/DTO 占位 | 定义业务模型 |
| ~pkg/consts/user.go~ | 新建/跳过 | 常量占位 | 定义业务常量 |
| ~desc/user/user.proto~ | 新建/跳过 | CRUD proto（含 5 个 rpc + 全套 message） | 按需添加自定义 rpc |

> **注意：Logic 文件不由此命令生成**。运行 ~make gen-rpc~ 后，protoc 会根据 proto 中的 rpc 定义自动生成 logic 骨架（~internal/logic/user/user/create_user_logic.go~ 等），签名自动对齐 pb 类型（如 ~*demo.CreateUserReq~），无需手动处理类型转换。

**生成的 desc proto 详细说明**（~desc/user/user.proto~）：

| 内容 | 说明 |
|------|------|
| ~UserInfo~ message | 核心详情结构，字段从 ent schema 自动推导（含 id/created_at/updated_at + 所有业务字段） |
| ~Create{Model}Req~ / ~Create{Model}Resp~ | 创建请求（内嵌 UserInfo）和返回（id） |
| ~Update{Model}Req~ | 更新请求（内嵌 UserInfo） |
| ~Get{Model}ByIDReq~ | 按 ID 查询请求 |
| ~Get{Model}By{UniqueField}Req~ | 按唯一字段查询请求（根据 DAO 方法动态生成） |
| ~Update{Model}By{UniqueField}Req~ | 按唯一字段更新请求（根据 DAO 方法动态生成） |
| ~Delete{Model}Req~ | 删除请求（支持批量 ids，仅有 deleted_at 字段时生成） |
| ~Get{Model}ListReq~ / ~Get{Model}ListResp~ | 分页列表请求和返回 |
| ~service~ 块 | 方法数量根据 DAO 接口动态生成（大写开头，Go 规范） |

注意：**如果 DAO 文件已存在，proto 不会再次生成**（即 DAO 是"只生成一次"的判断依据）。如需重新生成 proto，请先手动删除对应的 DAO 文件或使用 ~--overwrite~ 参数。

**生成的 proto 命名规范**（以 ~User~ 为例）：

~~~protobuf
// 复用 UserInfo 作为详情载体
message UserInfo { ... }

// 每个 rpc 方法有独立的 Req/Resp，命名 = 方法名 + Req/Resp（大写开头，Go 规范）
rpc CreateUser       (CreateUserReq)       returns (CreateUserResp);
rpc GetUserByID      (GetUserByIDReq)      returns (UserInfo);
rpc GetUserByEmail   (GetUserByEmailReq)   returns (UserInfo);
rpc UpdateUser       (UpdateUserReq)       returns (Empty);
rpc UpdateUserByEmail(UpdateUserByEmailReq) returns (Empty);
rpc DeleteUser       (DeleteUserReq)       returns (Empty);
rpc GetUserList      (GetUserListReq)      returns (GetUserListResp);
~~~

规则：rpc 方法名大写开头（Go 规范），不使用 ~IDReq~、~IDsReq~、~BaseIDResp~ 等通用命名。空返回复用 ~Empty~，详情复用 ~UserInfo~。proto 方法根据 DAO 接口动态生成，参数与 DAO 方法匹配。
~~~go
type UserDao interface {
    Create(ctx context.Context, data *ent.User) (*ent.User, error)
    GetByID(ctx context.Context, id int) (*ent.User, error)
    Update(ctx context.Context, data *ent.User) (*ent.User, error)
    Delete(ctx context.Context, id int) error
    List(ctx context.Context, page, pageSize int) ([]*ent.User, int, error)
}
~~~

**注意事项**：
- 文件已存在时**不会覆盖**（除非加 ~--overwrite~），只创建缺失文件。
- ~group~ 名称默认取 schema 小写名（~user~），决定 ~desc/{group}/~ 和 ~logic/{group}/~ 路径。
- 生成后务必运行 ~make gen-rpc~ 来合并新的 proto 并重新生成 pb 文件。
- DAO impl 使用 ~ctxutil.L(ctx)~ 打日志，~errcode.Wrapf~ 包装错误。

**完整流程示例**：
~~~bash
# 1. 创建 schema
make gen-ent-new name=UserToken

# 2. 编辑 ent/schema/user_token.go 定义字段

# 3. 生成 ent ORM
make gen-ent

# 4. 生成模块全套
make gen-rpc-ent-logic model=UserToken

# 5. 合并 proto + 生成 pb
make gen-rpc

# 6. 编译验证
go build ./...
~~~

---

## 5. ~make gen-dao-sql~

**做什么**：当 CRUD 五件套不够用时，根据 SQL 语句自动生成自定义 DAO 方法。支持完整的 SQL 解析，自动推导方法名、参数类型、返回值，并生成完整的 ent predicate 调用。

**执行命令**：
~~~bash
make gen-dao-sql sql="你的SQL语句"
~~~

**参数**：

| 参数 | 必填 | 说明 |
|------|------|------|
| ~sql~ | 是 | SQL 语句，用 ~?~ 作占位符 |

**生成/修改的文件**：

| 文件路径 | 操作 | 说明 |
|----------|------|------|
| ~internal/dao/{model}_dao.go~ | 追加 | 在接口定义末尾追加方法签名 |
| ~internal/dao/impl/{model}_oceanbase.go~ | 追加 | 在文件末尾追加完整方法实现 |

### 支持的 SQL 语法

| 类型 | SQL 示例 | 生成方法名 |
|------|----------|-----------|
| 查询列表 | ~SELECT * FROM user WHERE status = ?~ | ~FindByStatus~ |
| 查询单条 | ~SELECT * FROM user WHERE email = ? LIMIT 1~ | ~GetByEmail~ |
| 计数 | ~SELECT COUNT(*) FROM user WHERE status = ?~ | ~CountByStatus~ |
| 分页 | ~SELECT * FROM user WHERE status = ? LIMIT ? OFFSET ?~ | ~FindByStatus~ + page/pageSize |
| 分组 | ~SELECT status, COUNT(*) FROM user GROUP BY status~ | ~GroupByAll~ |
| JOIN | ~SELECT u.* FROM user u LEFT JOIN order o ON u.id = o.user_id WHERE u.status = ?~ | ~FindByStatus~ + TODO 注释 |
| NULL 判断 | ~SELECT * FROM user WHERE deleted_at IS NULL AND status = ?~ | ~FindByDeletedAtIsNilAndStatus~ |
| 更新 | ~UPDATE user SET status = ? WHERE id = ?~ | ~UpdateStatusById~ |
| 删除 | ~DELETE FROM user WHERE user_id = ?~ | ~DeleteByUserId~ |
| 插入 | ~INSERT INTO user (name, email) VALUES (?, ?)~ | ~InsertUser~ |

### 支持的 WHERE 操作符

| 操作符 | SQL 示例 | 方法名后缀 | 生成的 ent predicate |
|--------|----------|-----------|---------------------|
| ~=~ | ~status = ?~ | 无后缀 | ~user.StatusEQ(status)~ |
| ~!=~ / ~<>~ | ~status != ?~ | ~Neq~ | ~user.StatusNEQ(status)~ |
| ~>~ | ~age > ?~ | ~Gt~ | ~user.AgeGT(age)~ |
| ~>=~ | ~created_at >= ?~ | ~Gte~ | ~user.CreatedAtGTE(created_at)~ |
| ~<~ | ~score < ?~ | ~Lt~ | ~user.ScoreLT(score)~ |
| ~<=~ | ~score <= ?~ | ~Lte~ | ~user.ScoreLTE(score)~ |
| ~IN~ | ~id IN (?, ?)~ | ~In~ | ~user.IdIn(idList...)~ |
| ~LIKE~ | ~name LIKE ?~ | ~Like~ | ~user.NameContains(name)~ |
| ~IS NULL~ | ~deleted_at IS NULL~ | ~IsNil~ | ~user.DeletedAtIsNil()~（无参数） |
| ~IS NOT NULL~ | ~email IS NOT NULL~ | ~NotNil~ | ~user.EmailNotNil()~（无参数） |
| ~BETWEEN~ | ~age BETWEEN ? AND ?~ | ~Between~ | ~user.AgeGTE(ageMin), user.AgeLTE(ageMax)~ |

### 支持的高级特性

| 特性 | 说明 |
|------|------|
| JOIN | 解析 INNER/LEFT/RIGHT JOIN，生成 TODO 注释提示用 ent edge query |
| GROUP BY | 解析 GROUP BY 子句，方法名前缀为 ~GroupBy~ |
| HAVING | 解析 HAVING 子句，作为注释保留 |
| ORDER BY | 解析 ORDER BY 子句，作为注释保留 |
| Ent Schema 类型推导 | 自动读取 ~ent/schema~ 推导参数的 Go 类型（替代 ~interface{}~） |
| 重复检测 | 方法已存在则跳过，不会重复追加 |

> **⚠️ 不支持 JSON 字段查询/修改**
>
> Ent ORM 对 JSON 类型字段（如 ~field.JSON~）的查询和修改需要使用 ~ValueScannerField~ 或原生 SQL，**gen-dao-sql 目前无法自动生成 JSON 字段的 predicate**。如果你的 SQL 包含 JSON 字段操作（如 ~JSON_EXTRACT~、~->~、~->>~），请手动编写 DAO 方法，使用 ~client.User.Query().Where(func(s *sql.Selector){ ... })~ 方式实现。

### 示例

**例 1：按条件查询列表**
~~~bash
make gen-dao-sql sql="SELECT * FROM user WHERE status = ? AND created_at > ?"
~~~
生成方法：~FindByStatusAndCreatedAtGt(ctx context.Context, status int, created_at time.Time) ([]*ent.User, error)~

**例 2：单条查询**
~~~bash
make gen-dao-sql sql="SELECT * FROM user WHERE email = ? LIMIT 1"
~~~
生成方法：~GetByEmail(ctx context.Context, email string) (*ent.User, error)~

**例 3：计数**
~~~bash
make gen-dao-sql sql="SELECT COUNT(*) FROM user WHERE status = ?"
~~~
生成方法：~CountByStatus(ctx context.Context, status int) (int, error)~

**例 4：分页查询**
~~~bash
make gen-dao-sql sql="SELECT * FROM user WHERE status = ? ORDER BY id DESC LIMIT ? OFFSET ?"
~~~
生成方法：~FindByStatus(ctx context.Context, status int, page int, pageSize int) ([]*ent.User, error)~

**例 5：IN 查询**
~~~bash
make gen-dao-sql sql="SELECT * FROM user WHERE id IN (?, ?, ?)"
~~~
生成方法：~FindByIdIn(ctx context.Context, idList []int) ([]*ent.User, error)~

**例 6：BETWEEN 查询**
~~~bash
make gen-dao-sql sql="SELECT * FROM user WHERE created_at BETWEEN ? AND ?"
~~~
生成方法：~FindByCreatedAtBetween(ctx context.Context, created_atMin time.Time, created_atMax time.Time) ([]*ent.User, error)~

**例 7：UPDATE 指定字段**
~~~bash
make gen-dao-sql sql="UPDATE user SET status = ?, updated_at = ? WHERE id = ?"
~~~
生成方法：~UpdateStatusAndUpdatedAtById(ctx context.Context, id int) (int, error)~

**例 8：LEFT JOIN 查询**
~~~bash
make gen-dao-sql sql="SELECT u.* FROM user u LEFT JOIN order o ON u.id = o.user_id WHERE u.status = ?"
~~~
生成方法：~FindByStatus(ctx context.Context, status int) ([]*ent.User, error)~
生成实现中包含 TODO 注释提示将 JOIN 替换为 ent edge query（如 ~.QueryOrders()~）。

**例 9：NULL 判断**
~~~bash
make gen-dao-sql sql="SELECT * FROM user WHERE deleted_at IS NULL AND status = ?"
~~~
生成方法：~FindByDeletedAtIsNilAndStatus(ctx context.Context, status int) ([]*ent.User, error)~
~IS NULL~ 条件不生成参数，直接映射为 ~user.DeletedAtIsNil()~。

### 注意事项

- **前置条件**：必须先运行 ~make gen-rpc-ent-logic~ 生成初始 DAO 文件，否则无文件可追加。
- SQL 中用 ~?~ 作为参数占位符（不支持命名参数 ~:name~）。
- 表名从 SQL 中提取后转换为 ent 模型名（~user~ → ~User~, ~user_token~ → ~UserToken~）。
- 如果 ~ent/schema~ 目录存在，会自动加载 schema 推导参数类型；加载失败时 fallback 为 ~interface{}~。
- 支持 backtick 引用的表名和列名（如 ~'user'~ 和 ~'status'~ 写成反引号形式）。
- 同一方法名不会重复追加，再次运行会显示 ~⊘ Method xxx already exists~。
- JOIN 查询会生成基于主表的查询，**需要手动替换为 ent edge query**。

---

## 6. ~zctl rpc enum~

**做什么**：手动生成纯 Go 枚举类型（适用于不在 proto 中定义的业务常量），含 String/IsValid/Parse/Values/Int32 等方法。

**执行命令**：
~~~bash
zctl rpc enum --name=OrderStatus --values=pending,paid,shipped,done
~~~

**参数**：

| 参数 | 必填 | 说明 |
|------|------|------|
| ~--name~ | 是 | 枚举类型名（大驼峰，如 ~OrderStatus~） |
| ~--values~ | 是 | 逗号分隔的值名（小写，如 ~pending,paid,shipped,done~） |

**生成的文件**：

| 文件路径 | 操作 | 说明 |
|----------|------|------|
| ~pkg/enums/{snake_name}.go~ | 新建/覆盖 | 枚举类型 + 常量 + 辅助方法 |

**生成内容预览**（~--name=OrderStatus --values=pending,paid,shipped,done~）：
~~~go
type OrderStatus int32

const (
    OrderStatusPending  OrderStatus = 0
    OrderStatusPaid     OrderStatus = 1
    OrderStatusShipped  OrderStatus = 2
    OrderStatusDone     OrderStatus = 3
)

func (e OrderStatus) String() string { ... }
func (e OrderStatus) IsValid() bool { ... }
func OrderStatusValues() []OrderStatus { ... }
func ParseOrderStatus(s string) (OrderStatus, error) { ... }
func (e OrderStatus) Int32() int32 { ... }
~~~

**注意事项**：
- ~--name~ 必须大驼峰，~--values~ 用小写逗号分隔。
- proto 中的 ~enum~ 由 ~make gen-rpc~ **自动**生成，不需要手动用此命令。
- 此命令用于**不在 proto 中定义**的业务枚举（如内部状态机、配置项等）。
- 两种枚举输出到同一目录 ~pkg/enums/~，注意不要命名冲突。

---

## 7. ~zctl rpc merge-proto~

**做什么**：扫描 ~desc/**/*.proto~ 所有子文件，合并 message/enum/service 定义到根 ~{{SERVICE}}.proto~。通常不需要单独调用，~make gen-rpc~ 会自动调用。

**执行命令**：
~~~bash
zctl rpc merge-proto
# 或通过 make
make gen-rpc  # 内部会先调用 merge-proto
~~~

**生成/修改的文件**：

| 文件路径 | 操作 | 说明 |
|----------|------|------|
| ~{{SERVICE}}.proto~ | 覆盖 | 合并后的根 proto 文件 |

**合并规则**：
1. ~desc/base.proto~ 中读取 ~package~ 和 ~go_package~ 作为根 proto 头部。
2. 按字母序扫描 ~desc/~ 下所有 ~.proto~ 文件（含子目录）。
3. 去掉每个文件的 ~syntax~, ~package~, ~option~, ~import~ 行。
4. 其余内容（message/enum/service）按文件追加到根 proto 中。

**注意事项**：
- ~desc/base.proto~ 必须存在，它定义了 ~package~ 和 ~go_package~。
- 每个 ~desc/{group}/{model}.proto~ 文件中不要写 ~syntax~, ~option go_package~ 等头部（合并时会自动去除，但建议保持简洁）。
- 根 proto 文件（~{{SERVICE}}.proto~）是自动生成的，**不要手动编辑**。

---

## 项目目录结构

~~~
{{SERVICE}}/
├── {{SERVICE}}.go              # 主入口
├── {{SERVICE}}.proto           # 合并后的 proto（自动生成，勿编辑）
├── Makefile
├── Dockerfile
├── entrypoint.sh
├── desc/                       # proto 源文件（按业务域分子目录）
│   ├── base.proto              # 基础 message（Empty/IDReq/BaseResp 等）
│   └── {group}/                # 业务模块 proto
│       └── {model}.proto
├── types/                      # protoc 生成的 pb 文件（自动生成，勿编辑）
│   └── {package}/
├── ent/
│   └── schema/                 # entgo 表结构定义（手动编辑）
├── etc/                        # 配置
│   ├── {{SERVICE}}.yaml        # 运行时配置（.gitignore 忽略）
│   └── {{SERVICE}}.yaml.template  # 配置模板（提交到 git）
├── internal/
│   ├── config/                 # 配置结构体
│   ├── svc/                    # ServiceContext（DAO 实例注入）
│   ├── server/                 # gRPC server 实现
│   ├── logic/                  # 业务逻辑（层级对齐 desc/）
│   │   └── {group}/
│   │   └── {group}/            # 如 user/
│   │       └── {model}/        # 如 user/create_user_logic.go
│   ├── middleware/              # 拦截器
│   ├── dao/                    # DAO 接口（手动 + gen-dao-sql 追加）
│   │   └── {model}_dao.go
│   ├── dao/impl/               # DAO 实现
│   │   └── {model}_oceanbase.go
│   └── dao/mock/               # DAO Mock（自动生成，与接口同步）
│       └── {model}_dao_mock.go
├── pkg/
│   ├── errcode/                # 错误码 + 错误类型
│   ├── ctxutil/                # ctx 工具 + 日志
│   ├── consts/                 # 常量（按模块）
│   ├── model/                  # VO/DTO 模型（按模块）
│   ├── enums/                  # 枚举（proto enum + 手动 enum）
│   ├── i18n/                   # 国际化
│   └── entlog/                 # ent SQL 日志
├── zctl-commands.md            # 桩命令使用说明（自动生成）
└── {{SERVICE}}client/          # RPC 客户端 SDK
~~~

---

## 完整开发流程

~~~bash
# 1. 创建项目（含 ent 集成）
zctl rpc new myservice --ent

# 2. 新建 ent schema
make gen-ent-new name=User

# 3. 编辑 ent/schema/user.go 定义字段和 Edge

# 4. 生成 ent ORM 代码
make gen-ent

# 5. 生成模块全套代码（DAO + logic + errcode + test + desc proto）
make gen-rpc-ent-logic model=User

# 6. 合并 proto + 生成 pb + 自动生成 enum
make gen-rpc

# 7. 编译验证
go build ./...

# 8. 需要自定义查询？用 gen-dao-sql 追加 DAO 方法
make gen-dao-sql sql="SELECT * FROM user WHERE status = ? AND created_at > ?"

# 9. 需要业务枚举？
zctl rpc enum --name=OrderStatus --values=pending,paid,shipped,done

# 10. 运行
make run
~~~

### 常用命令速查

| 命令 | 说明 |
|------|------|
| ~make gen-rpc~ | 合并 proto + 生成 pb + enum |
| ~make gen-ent~ | 生成 Ent ORM 代码 |
| ~make gen-ent-new name=X~ | 新建 ent schema |
| ~make gen-rpc-ent-logic model=X~ | 生成指定模块全套代码 |
| ~make gen-rpc-ent-logic model=all~ | 生成所有模块 |
| ~make gen-dao-sql sql="..."~ | 根据 SQL 追加 DAO 方法 |
| ~make test~ | 运行全量测试 |
| ~make test-module module=user~ | 运行指定模块测试 |
| ~make run~ | 本地运行 |
| ~make build-linux~ | 交叉编译 Linux |
| ~make docker~ | 构建 Docker 镜像 |
| ~make health~ | gRPC 健康检查 |
| ~make help~ | 显示所有命令 |
`, "~", "`")

	content = strings.ReplaceAll(content, "{{SERVICE}}", svcLower)
	content = strings.ReplaceAll(content, "~~~", "```")

	// Always overwrite: this is auto-generated documentation that should stay in sync with the tool version.
	return os.WriteFile(filepath.Join(abs, "zctl-commands.md"), []byte(strings.TrimLeft(content, "\n")), 0644)
}

// RefreshCommandsDoc auto-detects the service name from the working directory
// and regenerates zctl-commands.md. Called by subcommands (ent, dao, merge-proto, enum)
// that don't know the service name upfront.
func RefreshCommandsDoc(abs string) {
	serviceName := filepath.Base(abs)
	// Try to read SERVICE_STYLE from Makefile for more accurate name
	if data, err := os.ReadFile(filepath.Join(abs, "Makefile")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "SERVICE_STYLE=") {
				if v := strings.TrimSpace(strings.TrimPrefix(line, "SERVICE_STYLE=")); v != "" {
					serviceName = v
					break
				}
			}
		}
	}
	_ = GenCommandsDoc(abs, serviceName)
}

// ==================== Module placeholder files from desc/ ====================

// GenModuleFiles scans desc/ subdirectories and creates module placeholder files
// (pkg/model/{file}.go, pkg/consts/{file}.go, pkg/errcode/{file}.go)
//
// File-name rule (must align with DAO/schema naming):
//   - desc/ subdirectory uses DirName (all-lowercase, no underscores) — e.g. "csuserprofile"
//   - placeholder file names MUST use FileSnake (snake_case) — e.g. "cs_user_profile.go"
//
// Recovery strategy: scan ent/schema/*.go to build a {DirName: FileSnake} map.
// When a desc/ subdirectory matches a schema's DirName, use that schema's
// snake_case file name; otherwise (e.g. "user", "base") fall back to the
// directory name itself (which is already a valid file name).
func (g *Generator) GenModuleFiles(abs string) error {
	descDir := filepath.Join(abs, "desc")
	if _, err := os.Stat(descDir); os.IsNotExist(err) {
		return nil
	}

	entries, err := os.ReadDir(descDir)
	if err != nil {
		return nil
	}

	// Build {dirName -> snakeName} map from ent/schema/*.go
	// e.g. "cs_user_profile.go" → {"csuserprofile": "cs_user_profile"}
	dirToSnake := make(map[string]string)
	schemaDir := filepath.Join(abs, "ent", "schema")
	if schemaEntries, e := os.ReadDir(schemaDir); e == nil {
		for _, se := range schemaEntries {
			if se.IsDir() || !strings.HasSuffix(se.Name(), ".go") {
				continue
			}
			snake := strings.TrimSuffix(se.Name(), ".go")
			// derive dir name = snake without underscores
			dir := strings.ReplaceAll(snake, "_", "")
			dirToSnake[dir] = snake
		}
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := entry.Name()
		// Resolve file base name: prefer schema-derived snake_case, else fall back to dir name
		fileBase := dir
		if snake, ok := dirToSnake[dir]; ok {
			fileBase = snake
		}

		// pkg/model/{fileBase}.go
		modelDir := filepath.Join(abs, "pkg", "model")
		pathx.MkdirIfNotExist(modelDir)
		modelFile := filepath.Join(modelDir, fileBase+".go")
		if !pathx.FileExists(modelFile) {
			os.WriteFile(modelFile, []byte(fmt.Sprintf("package model\n\n// ──── %s module models ────\n", dir)), 0644)
		}

		// pkg/consts/{fileBase}.go
		constsDir := filepath.Join(abs, "pkg", "consts")
		pathx.MkdirIfNotExist(constsDir)
		constsFile := filepath.Join(constsDir, fileBase+".go")
		if !pathx.FileExists(constsFile) {
			os.WriteFile(constsFile, []byte(fmt.Sprintf("package consts\n\n// ──── %s module constants ────\n", dir)), 0644)
		}

		// pkg/errcode/{fileBase}.go
		errcodeDir := filepath.Join(abs, "pkg", "errcode")
		pathx.MkdirIfNotExist(errcodeDir)
		errcodeFile := filepath.Join(errcodeDir, fileBase+".go")
		if !pathx.FileExists(errcodeFile) {
			os.WriteFile(errcodeFile, []byte(fmt.Sprintf("package errcode\n\n// ──── %s module error codes ────\n// Add constants here. Messages come from i18n.\n", dir)), 0644)
		}
	}

	return nil
}

// ==================== desc/ directory + proto merge script ====================

func (g *Generator) genDescDir(abs, serviceName string) error {
	descDir := filepath.Join(abs, "desc")
	if err := pathx.MkdirIfNotExist(descDir); err != nil {
		return err
	}
	return nil
}

func (g *Generator) genMergeProtoScript(abs, serviceName string) error {
	svcLower := strings.ToLower(serviceName) // raw lower (may contain '-')
	protoPkg := ProtoPkg(serviceName)        // valid proto3 ident (no '-'/space)

	content := fmt.Sprintf(`#!/bin/bash
# merge_proto.sh — Merge all desc/**/*.proto into root %s.proto
# Usage: ./merge_proto.sh
# Called by: make gen-rpc (before protoc)
set -e

SERVICE="%s"
# PROTO_PKG: proto3 package identifier (only [A-Za-z0-9_]). Service names with
# dashes (e.g. "cs-agent-rpc") are normalized to "csagentrpc" by zctl at scaffold time.
PROTO_PKG="%s"
ROOT_PROTO="./${SERVICE}.proto"
DESC_DIR="./desc"

# Extract package and go_package from base.proto (authoritative source)
PKG=$(grep '^package ' "${DESC_DIR}/base.proto" 2>/dev/null | head -1 | sed 's/package //;s/;//')
GO_PKG=$(grep 'go_package' "${DESC_DIR}/base.proto" 2>/dev/null | head -1 | sed 's/.*"\(.*\)".*/\1/')

if [ -z "$PKG" ]; then
  PKG="${PROTO_PKG}"
fi
if [ -z "$GO_PKG" ]; then
  GO_PKG="./${PROTO_PKG}"
fi

# Header
cat > "$ROOT_PROTO" <<EOF
syntax = "proto3";

package ${PKG};
option go_package = "${GO_PKG}";

EOF

# Collect all .proto files under desc/ (base.proto first, then others sorted)
FILES=$(find "$DESC_DIR" -name "*.proto" | sort)

# Extract enum + message + service blocks from all files
for f in $FILES; do
  echo "// ---- from ${f} ----" >> "$ROOT_PROTO"
  echo "" >> "$ROOT_PROTO"
  # Skip syntax/package/option/import lines, keep everything else
  grep -v '^syntax\s' "$f" | grep -v '^package\s' | grep -v '^option\s' | grep -v '^import\s' >> "$ROOT_PROTO"
  echo "" >> "$ROOT_PROTO"
done

echo "[merge_proto] Generated ${ROOT_PROTO} from $(echo $FILES | wc -w | tr -d ' ') proto files"
`, svcLower, svcLower, protoPkg)

	scriptPath := filepath.Join(abs, "merge_proto.sh")
	if err := writeIfNotExist(scriptPath, content); err != nil {
		return err
	}
	// Make executable
	return os.Chmod(scriptPath, 0755)
}

// ==================== helpers ====================

// ==================== proto.yaml ====================

func (g *Generator) genProtoYaml(abs string) error {
	content := `# proto.yaml — 远程 proto 仓库配置
# 所有微服务的 proto 协议由统一仓库管理，本地 desc/ 通过此配置同步远程最新协议。
# 不配置此文件时 make pull-proto 会跳过，直接使用本地 desc/。

remote:
  # 统一 proto 仓库地址
  repo: ""
  # 协议版本（git tag 或 branch），CI/CD 用 tag 锁版本，开发用 branch 跟踪最新
  ref: ""
  # 本服务 proto 在远程仓库中的子目录路径
  path: ""
  # 拉取到本地的目标目录（一般不改）
  target: desc/
`
	return writeIfNotExist(filepath.Join(abs, "proto.yaml"), content)
}

// ==================== proto/buf/validate/validate.proto ====================

//go:embed validate.proto
var validateProtoContent string

func (g *Generator) genValidateProto(abs string) error {
	dir := filepath.Join(abs, "proto", "buf", "validate")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	return writeIfNotExist(filepath.Join(dir, "validate.proto"), validateProtoContent)
}

// ==================== proto/google/api/{annotations,http}.proto ====================
//
// grpc-transcoding 依赖：在 desc/*.proto 中可使用
//   option (google.api.http) = {get: "/v1/xxx"};
// 来声明 gRPC↔HTTP 路由映射；annotations.proto 内部 import http.proto，
// 二者必须同时落盘。文件源自 grpc-ecosystem/grpc-gateway third_party/googleapis，
// Apache-2.0 许可，原样转写。

//go:embed annotations.proto
var googleAPIAnnotationsProtoContent string

//go:embed http.proto
var googleAPIHTTPProtoContent string

// ensureGoogleAPIProtoFiles 把 annotations.proto + http.proto 写入 proto/google/api/
// （幂等：已存在则跳过）。供 Generator.genGoogleAPIProto（创建新项目时）与
// EnsureGoogleAPIProtoIfReferenced（已存项目 merge-proto 触发时）共用。
func ensureGoogleAPIProtoFiles(abs string) error {
	dir := filepath.Join(abs, "proto", "google", "api")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	if err := writeIfNotExist(filepath.Join(dir, "annotations.proto"), googleAPIAnnotationsProtoContent); err != nil {
		return err
	}
	return writeIfNotExist(filepath.Join(dir, "http.proto"), googleAPIHTTPProtoContent)
}

func (g *Generator) genGoogleAPIProto(abs string) error {
	return ensureGoogleAPIProtoFiles(abs)
}

// EnsureGoogleAPIProtoIfReferenced 由 zctl rpc merge-proto 在合并完成后调用：
// 扫描合并产物（rootProto），若发现 google.api.http annotation 或对
// google/api/annotations.proto 的 import，则自动把所需 proto 文件写入项目；
// 否则什么也不做（避免给不需要 HTTP transcoding 的项目留无用文件）。
func EnsureGoogleAPIProtoIfReferenced(abs, rootProto string) error {
	data, err := os.ReadFile(rootProto)
	if err != nil {
		return err
	}
	content := string(data)
	if !strings.Contains(content, "google.api.http") &&
		!strings.Contains(content, "google/api/annotations.proto") {
		return nil
	}
	return ensureGoogleAPIProtoFiles(abs)
}

// ==================== cmd/migrate-ddl/main.go ====================

// genCmdMigrateDDL writes cmd/migrate-ddl/main.go — an offline DDL diff tool.
//
// 设计要点：
//   - 工具读取 etc/{svcLower}.yaml 的 DatabaseConf 拼 DSN 连 DB（与服务运行同源配置）；
//   - 用 ent client.Schema.WriteTo(file, WithDropColumn, WithDropIndex) 把
//     "ent/schema 真相 vs DB 现状" 的差量 DDL 写到 migrations/{stamp}_{name}.sql，
//     底层走 schema.WriteDriver，io.Writer 透传 SQL，物理上不会写到 DB；
//   - 显式开启 WithDropColumn / WithDropIndex：否则只能输出 ADD，重命名 / 唯一键调整等
//     需要 DROP+ADD 的场景会丢 DROP，DBA 拿到的脚本不完整。
//
// 模板渲染：用 strings.ReplaceAll 替换 __SVC_LOWER__ / __MODULE_PATH__ 两个占位符；
// 不用 fmt.Sprintf 是因为模板里大量 % 字符（%q/%v/%-30s 等）转义后极易出错。
func (g *Generator) genCmdMigrateDDL(abs, serviceName, modulePath string) error {
	svcLower := strings.ToLower(serviceName)

	tmpl := `// Package main 提供一个【纯输出】DDL 的离线命令：
//
//	它会：
//	  1) 读取 etc/__SVC_LOWER__.yaml 中的 DatabaseConf（与服务运行使用同一份配置，避免环境漂移）；
//	  2) 用 entClient.Schema.WriteTo(file, WithDropColumn, WithDropIndex)
//	     把 "ent schema 真相 vs 数据库当前状态" 的差量 DDL 写到一个 .sql 文件里；
//	  3) 输出文件命名为 migrations/{YYYYMMDD_HHMMSS}_{name}.sql。
//
// 它不会：
//   - 执行任何 DDL：底层走 ent 的 schema.WriteDriver，所有 SQL 都进 io.Writer，不进 DB；
//   - 修改任何代码 / schema 文件 / 应用启动行为。
//
// 设计要点：
//   - 必须显式开启 WithDropColumn / WithDropIndex：否则只能看到 "加列/加索引"，
//     "应该删除的旧列、旧索引" 会被静默忽略，导致 DBA 拿到一份不完整的迁移脚本（典型场景：
//     重命名字段、调整唯一键列顺序——必须 DROP+ADD 才能完整表达，不带这两个开关时漏 DROP）。
//   - 使用方式: ` + "`" + `make gen-ddl [name=<short_desc>]` + "`" + `，name 可选，缺省为 ` + "`" + `auto` + "`" + `，详见 Makefile::gen-ddl。
//   - 输出文件一律纳入 git，作为评审与生产回放的真相源。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/schema"

	entsql "entgo.io/ent/dialect/sql"

	_ "github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/conf"

	"__MODULE_PATH__/ent"
	"__MODULE_PATH__/internal/config"
)

// migrateConfig 只解析 DDL 输出所需字段。
//
// 不直接复用 internal/config.Config 的原因：
//   - Config 内嵌 zrpc.RpcServerConf（要求 ListenOn / Etcd 等），但本工具不起 RPC 服务，
//     强行复用会因为 ListenOn 等必填字段缺失而 panic；
//   - 这里只需要 DatabaseConf（且字段与 internal/config.DatabaseConf 完全一致），
//     用一个最小化结构体就够，避免无关依赖。
type migrateConfig struct {
	DatabaseConf config.DatabaseConf
}

func main() {
	var (
		cfgPath = flag.String("f", "etc/__SVC_LOWER__.yaml", "service config file (DatabaseConf is read from here)")
		outDir  = flag.String("dir", "migrations", "output directory for generated .sql")
		name    = flag.String("name", "auto", "short kebab/snake description (e.g. add_xxx_to_yyy); default: auto")
	)
	flag.Parse()

	// name 仅作为输出文件名后缀，缺省时用 "auto"，方便快速生成。
	// 仅做字符白名单校验，避免出现路径穿越/空格等。
	if strings.TrimSpace(*name) == "" {
		*name = "auto"
	}
	if !nameRe.MatchString(*name) {
		log.Fatalf("[migrate-ddl] invalid -name=%q, allowed chars: [a-z0-9_-]", *name)
	}

	// 1) 读配置（复用 internal/config.DatabaseConf 的字段定义，保证与服务一致）
	var c migrateConfig
	if err := conf.Load(*cfgPath, &c); err != nil {
		log.Fatalf("[migrate-ddl] load config %s failed: %v", *cfgPath, err)
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=UTC",
		c.DatabaseConf.Username, c.DatabaseConf.Password,
		c.DatabaseConf.Host, c.DatabaseConf.Port, c.DatabaseConf.DBName)

	// 2) 连 DB（仅用于 inspect 当前 schema，本工具不会写 DB——见 step 4 注释）
	drv, err := entsql.Open(dialect.MySQL, dsn)
	if err != nil {
		log.Fatalf("[migrate-ddl] open mysql failed: %v", err)
	}
	client := ent.NewClient(ent.Driver(drv))
	defer client.Close()

	// 3) 准备输出文件
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("[migrate-ddl] mkdir %s failed: %v", *outDir, err)
	}
	stamp := time.Now().Format("20060102_150405")
	fname := filepath.Join(*outDir, fmt.Sprintf("%s_%s.sql", stamp, *name))
	f, err := os.OpenFile(fname, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		log.Fatalf("[migrate-ddl] open output %s failed: %v", fname, err)
	}
	defer f.Close()

	// 头部注释：让 DBA 一眼看清楚来源、时间、警告事项
	header := fmt.Sprintf(` + "`" + `-- ╔══════════════════════════════════════════════════════════════════╗
-- ║ AUTO-GENERATED BY: make gen-ddl name=%-30s║
-- ║ GENERATED AT     : %-48s║
-- ║ SOURCE OF TRUTH  : ent/schema/*.go                                  ║
-- ║ DROP_COLUMN/DROP_INDEX = ON  (full diff, including removals)        ║
-- ║                                                                     ║
-- ║ ⚠ DO NOT auto-apply this file in production.                        ║
-- ║ ⚠ DBA must review every DROP statement before execution.            ║
-- ║ ⚠ Rename columns appears as DROP+ADD here — manually rewrite to     ║
-- ║   ALTER TABLE ... CHANGE COLUMN <old> <new> ... to preserve data.   ║
-- ╚══════════════════════════════════════════════════════════════════╝
` + "`" + `, *name, time.Now().Format(time.RFC3339))
	if _, err := f.WriteString(header); err != nil {
		log.Fatalf("[migrate-ddl] write header failed: %v", err)
	}
	headerLen := int64(len(header))

	// 4) 调 ent 的 WriteTo —— 仅 diff 写文件，不执行
	//    ent 内部用 schema.WriteDriver 包装真实 driver：
	//    所有 SQL 都进 io.Writer，物理上不可能写到 DB（go ent 源码可查）。
	ctx := context.Background()
	if err := client.Schema.WriteTo(ctx, f,
		schema.WithDropColumn(true),
		schema.WithDropIndex(true),
		schema.WithForeignKeys(false), // 走业务唯一键 + 软删除，不依赖物理外键
	); err != nil {
		log.Fatalf("[migrate-ddl] write schema diff failed: %v", err)
	}

	stat, _ := f.Stat()
	if stat != nil && stat.Size() <= headerLen {
		log.Printf("[migrate-ddl] ✅ no schema diff detected (DB already aligned with ent/schema)")
		log.Printf("[migrate-ddl]    file: %s (header only, no DDL)", fname)
		return
	}
	log.Printf("[migrate-ddl] ✅ DDL diff written → %s", fname)
	log.Printf("[migrate-ddl]    review the file, then hand it to DBA for execution.")
}

var nameRe = regexp.MustCompile(` + "`" + `^[a-z0-9_-]+$` + "`" + `)
`

	content := strings.ReplaceAll(tmpl, "__SVC_LOWER__", svcLower)
	content = strings.ReplaceAll(content, "__MODULE_PATH__", modulePath)

	dir := filepath.Join(abs, "cmd", "migrate-ddl")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	return writeIfNotExist(filepath.Join(dir, "main.go"), content)
}

// ==================== helpers ====================

func writeIfNotExist(filename, content string) error {
	if pathx.FileExists(filename) {
		return nil
	}
	dir := filepath.Dir(filename)
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	return os.WriteFile(filename, []byte(strings.TrimLeft(content, "\n")), 0644)
}
