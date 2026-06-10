// Package checker — fix_hints.go
//
// 集中维护"静态检查 tab → 一键/工具链修复建议"映射。
//
// 设计原则：
//  1. 仅当该 tab 检测到 issue 时才在报告中渲染（避免无问题时干扰用户）。
//  2. 命令必须是【外部独立工具或 golangci-lint 子命令】，要么真能一键修复（例如
//     fieldalignment -fix / gofmt -w），要么至少能把检查范围限定到该 tab 对应的
//     单个 linter，便于人工聚焦修复。
//  3. 不可逆类操作必须在 Notes 中提示先 git commit 再执行。
package checker

// fixHintFor returns the FixHint for a given tab ID, or nil if no automated
// fix is known for that tab. Tab IDs must align with those defined in
// BuildReportData (panic-typeassert / err-drop / sty-fmt / cx-cyclo / ...).
func fixHintFor(tabID string) *FixHint {
	if h, ok := fixHints[tabID]; ok {
		return h
	}
	return nil
}

// commitFirstNote 通用提示：批量自动修复前先提交一次，便于回滚 diff。
const commitFirstNote = "执行任何 -fix / -w 命令前请先 `git add -A && git commit -m \"snapshot before auto-fix\"`，便于失败时回滚。"

// fixHints 按 IssueTab.ID 索引。仅覆盖确实有第三方一键修复方案、
// 或可借助 golangci-lint --fix 收敛的 tab；其余（人工重构类）保持 nil。
var fixHints = map[string]*FixHint{

	// ── ⚡ Critical · Panic 风险 ──────────────────────────────────────────
	"panic-typeassert": {
		Title:   "💡 修复建议：forcetypeassert",
		Summary: "无 comma-ok 的强制类型断言无法自动修复，需人工把 a := b.(T) 改写为 a, ok := b.(T) 并处理 !ok 分支。golangci-lint 可单独跑该 linter 快速复核。",
		Steps: []FixStep{
			{Desc: "仅运行 forcetypeassert 检查，聚焦本 tab 列出的位置：",
				Command: "golangci-lint run --no-config --disable-all --enable=forcetypeassert ./..."},
			{Desc: "对每条 issue 改写为带 ok 判断的安全断言后，重新扫描验证：",
				Command: "zctl perf scan --dir=."},
		},
	},
	"panic-nilnil": {
		Title:   "💡 修复建议：nilnil",
		Summary: "不应同时返回 (nil, nil)。需人工选择：①确实无错就返回 (val, nil)；②确实失败就返回 (nil, fmt.Errorf(...))。",
		Steps: []FixStep{
			{Desc: "聚焦扫描该 linter：",
				Command: "golangci-lint run --no-config --disable-all --enable=nilnil ./..."},
		},
	},
	"panic-nilderef": {
		Title:   "💡 修复建议：staticcheck SA5011",
		Summary: "Nil 指针解引用属于硬 bug，无法自动修复。staticcheck 可单独跑，定位后人工补 nil 检查。",
		Steps: []FixStep{
			{Desc: "安装 staticcheck（独立运行更快）：",
				Command: "go install honnef.co/go/tools/cmd/staticcheck@latest"},
			{Desc: "只跑 SA5011：",
				Command: "staticcheck -checks SA5011 ./..."},
		},
	},

	// ── ⚡ Critical · Error 处理缺陷 ─────────────────────────────────────
	"err-drop": {
		Title:   "💡 修复建议：errcheck",
		Summary: "errcheck 不支持自动修复（无法决定如何处理 error）。但可独立运行得到精确清单。",
		Steps: []FixStep{
			{Desc: "安装 errcheck：",
				Command: "go install github.com/kisielk/errcheck@latest"},
			{Desc: "只看本仓库错误处理缺失：",
				Command: "errcheck ./..."},
			{Desc: "对每条 _ = func() 显式判断 err 后再决定 log/return/continue。"},
		},
	},
	"err-swallow": {
		Title:   "💡 修复建议：nilerr",
		Summary: "已检查 err != nil 却 return nil — 必须人工决定：是返回 err 还是 wrap。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=nilerr ./..."},
		},
	},
	"err-wrap": {
		Title:   "💡 修复建议：wrapcheck",
		Summary: "外部包 error 应包装。若团队统一用 `fmt.Errorf(\"...: %w\", err)` 或 `errors.Wrap`，可批量改写。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=wrapcheck ./..."},
			{Desc: "在每个直接 return err 处加上 fmt.Errorf 包装上下文，例如：return fmt.Errorf(\"GetUser: %w\", err)"},
		},
	},
	"err-wasted": {
		Title:   "💡 修复建议：wastedassign",
		Summary: "赋值后从未使用，wastedassign 通常意味着遗漏了对该变量的检查。无法自动修复，但可单独运行收敛清单。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=wastedassign ./..."},
		},
	},

	// ── 🐛 Bug · 资源泄漏 ─────────────────────────────────────────────────
	"leak-body": {
		Title:   "💡 修复建议：bodyclose",
		Summary: "缺少 defer resp.Body.Close()。bodyclose 不支持自动修复，但 IDE 可批量补。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=bodyclose ./..."},
			{Desc: "对每个 resp, err := http.XXX(...) 之后立即加：if err == nil { defer resp.Body.Close() }"},
		},
	},
	"leak-rows": {
		Title:   "💡 修复建议：sqlclosecheck",
		Summary: "缺少 defer rows.Close()。需人工补 defer。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=sqlclosecheck ./..."},
		},
	},
	"leak-rowserr": {
		Title:   "💡 修复建议：rowserrcheck",
		Summary: "for rows.Next() 之后必须 if err := rows.Err(); err != nil { ... }。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=rowserrcheck ./..."},
		},
	},

	// ── 🐛 Bug · 并发安全 ─────────────────────────────────────────────────
	"conc-ctx": {
		Title:   "💡 修复建议：contextcheck",
		Summary: "goroutine 使用了非继承 context，导致无法被取消。需人工把 context.Background() 改为传入的 ctx。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=contextcheck ./..."},
		},
	},
	"conc-noctx": {
		Title:   "💡 修复建议：noctx",
		Summary: "http.NewRequest 没有 ctx → 无法超时/取消。改用 http.NewRequestWithContext(ctx, ...)。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=noctx ./..."},
		},
	},
	"conc-loopvar": {
		Title:   "💡 修复建议：copyloopvar (Go 1.22+ 默认已修复)",
		Summary: "Go 1.22+ 已自动修复循环变量捕获。若 go.mod 已 ≥1.22 可直接消除该问题；旧版需在循环内显式 v := v 拷贝一次。",
		Steps: []FixStep{
			{Desc: "查看当前 go 版本：",
				Command: "go version"},
			{Desc: "升级 go.mod 语义版本到 1.22+（确认 CI/Docker 镜像已支持后再改）：",
				Command: "go mod edit -go=1.22 && go mod tidy"},
		},
		Notes: []string{"升级 go.mod 前请确认部署环境/镜像 Go 版本 ≥1.22。"},
	},

	// ── 🐛 Bug · 其他缺陷 ─────────────────────────────────────────────────
	"defect-enum": {
		Title:   "💡 修复建议：exhaustive",
		Summary: "switch 未覆盖所有枚举值。可在每个 switch 加 default 分支，或补齐缺少的 case。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=exhaustive ./..."},
		},
	},
	"defect-unparam": {
		Title:   "💡 修复建议：unparam",
		Summary: "函数参数声明但从未使用。可选择删除参数或改名为 _。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=unparam ./..."},
		},
	},
	"defect-vet": {
		Title:   "💡 修复建议：go vet（含 fieldalignment）",
		Summary: "go vet 包含 printf 格式 / 锁拷贝 / fieldalignment（结构体字段未按内存对齐排序）等子检查；其中 fieldalignment 支持 -fix 自动重排字段。",
		Steps: []FixStep{
			{Desc: "安装 fieldalignment（用于自动重排结构体字段，减小 padding）：",
				Command: "go install golang.org/x/tools/go/analysis/passes/fieldalignment/cmd/fieldalignment@latest"},
			{Desc: "执行自动修复（会修改源码，先 commit！）：",
				Command: "fieldalignment -fix ./..."},
			{Desc: "再跑一次 go vet 收敛剩余问题：",
				Command: "go vet ./..."},
		},
		Notes: []string{commitFirstNote, "fieldalignment -fix 会重排所有结构体字段顺序，可能影响序列化布局；如有 unsafe 偏移依赖请先评估。"},
	},

	// ── 🐌 Performance · 代码层性能 ───────────────────────────────────────
	"perf-slice": {
		Title:   "💡 修复建议：prealloc",
		Summary: "循环 append 应预分配 capacity：xs := make([]T, 0, len(src))。无法自动改写，需人工。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=prealloc ./..."},
		},
	},
	"perf-ctx": {
		Title:   "💡 修复建议：fatcontext",
		Summary: "循环内 context.WithValue 会形成嵌套 context 链。把 WithValue 移到循环外，或重构为单次构造。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=fatcontext ./..."},
		},
	},
	"perf-huge": {
		Title:   "💡 修复建议：gocritic hugeParam",
		Summary: "大结构体应改成传指针 *T。gocritic 不提供自动修复，但 IDE 可快速重构。",
		Steps: []FixStep{
			{Desc: "聚焦扫描（仅 performance 类规则）：",
				Command: "golangci-lint run --no-config --disable-all --enable=gocritic ./..."},
		},
	},
	"perf-sprint": {
		Title:   "💡 一键修复：perfsprint",
		Summary: "perfsprint 支持 --fix，可把 fmt.Sprintf 自动重写为更快的 strconv/string 拼接。",
		Steps: []FixStep{
			{Desc: "通过 golangci-lint 一键修复：",
				Command: "golangci-lint run --no-config --disable-all --enable=perfsprint --fix ./..."},
			{Desc: "或单独安装并使用 perfsprint：",
				Command: "go install github.com/catenacyber/perfsprint@latest && perfsprint -fix ./..."},
		},
		Notes: []string{commitFirstNote},
	},
	"perf-select": {
		Title:   "💡 修复建议：unqueryvet (SELECT *)",
		Summary: "SELECT * 应改为显式列。无法自动修复，建议结合 ORM 自动生成的强类型模型重构。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=unqueryvet ./..."},
		},
	},

	// ── 🔒 Security · 代码安全（gosec 不支持自动修复，统一给定位命令） ──
	"sec-sql": {
		Title:   "💡 修复建议：gosec G201/G202 (SQL 注入)",
		Summary: "禁止用 fmt.Sprintf 拼接 SQL；改用参数占位符 (? / $1) 或 ORM。",
		Steps: []FixStep{
			{Desc: "安装 gosec 单独运行：",
				Command: "go install github.com/securego/gosec/v2/cmd/gosec@latest"},
			{Desc: "只看 SQL 注入：",
				Command: "gosec -include=G201,G202 ./..."},
		},
	},
	"sec-rand": {
		Title:   "💡 修复建议：gosec G401/G404 (弱随机数)",
		Summary: "math/rand 在加密/鉴权场景必须换成 crypto/rand。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "gosec -include=G401,G404 ./..."},
		},
	},
	"sec-hardcode": {
		Title:   "💡 修复建议：gosec G101 (硬编码密钥)",
		Summary: "把密钥/密码移到环境变量或 KMS。务必同步从 git 历史中清理（git filter-repo）。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "gosec -include=G101 ./..."},
		},
		Notes: []string{"如已 push，必须使用 git filter-repo 清理历史并轮换泄露密钥。"},
	},
	"sec-perm": {
		Title:   "💡 修复建议：gosec G302/G306 (文件权限过宽)",
		Summary: "把 0777 / 0666 改为 0600 / 0644。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "gosec -include=G302,G306 ./..."},
		},
	},

	// ── 🔒 Security · 代码注入风险 ───────────────────────────────────────
	"trojan-bidi": {
		Title:   "💡 修复建议：bidichk (Trojan Source)",
		Summary: "源码中存在不可见的 Unicode 双向字符 (CVE-2021-42574)。需人工删除这些不可见字符。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=bidichk ./..."},
			{Desc: "用 grep 定位（PERL 模式匹配 U+202A..U+202E / U+2066..U+2069）：",
				Command: "grep -RP '[\\x{202A}-\\x{202E}\\x{2066}-\\x{2069}]' ."},
		},
	},

	// ── 📐 Quality · 复杂度（人工重构，提供测量命令） ─────────────────
	"cx-cyclo": {
		Title:   "💡 修复建议：gocyclo (圈复杂度 ≤10)",
		Summary: "无法自动修复，必须人工拆函数。可独立运行 gocyclo 看 top-N。",
		Steps: []FixStep{
			{Desc: "安装 gocyclo：",
				Command: "go install github.com/fzipp/gocyclo/cmd/gocyclo@latest"},
			{Desc: "列出复杂度最高的 20 个函数：",
				Command: "gocyclo -top 20 ."},
		},
	},
	"cx-cognit": {
		Title:   "💡 修复建议：gocognit (认知复杂度 ≤15)",
		Summary: "通过提前 return、抽取小函数、消除嵌套来降低。",
		Steps: []FixStep{
			{Desc: "安装 gocognit：",
				Command: "go install github.com/uudashr/gocognit/cmd/gocognit@latest"},
			{Desc: "列出 top-20：",
				Command: "gocognit -top 20 ."},
		},
	},
	"cx-funlen": {
		Title:   "💡 修复建议：funlen (函数行数 ≤200)",
		Summary: "过长函数应按业务步骤拆分为多个子函数。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=funlen ./..."},
		},
	},
	"cx-nestif": {
		Title:   "💡 修复建议：nestif (嵌套深度 ≤5)",
		Summary: "把深层 if 改为提前 return / guard clause。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=nestif ./..."},
		},
	},
	"cx-maintidx": {
		Title:   "💡 修复建议：maintidx (可维护性指数 ≥20)",
		Summary: "综合指标，需同时降低复杂度+缩短函数+提升注释率。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=maintidx ./..."},
		},
	},

	// ── 📐 Quality · 代码风格 ─────────────────────────────────────────────
	"sty-fmt": {
		Title:   "💡 一键修复：gofmt",
		Summary: "gofmt 是 Go 官方格式化工具，-w 直接覆盖写回，可一键修复全部格式问题。",
		Steps: []FixStep{
			{Desc: "一键格式化整个仓库：",
				Command: "gofmt -w ."},
			{Desc: "（推荐）使用 goimports 同时整理 import：",
				Command: "go install golang.org/x/tools/cmd/goimports@latest && goimports -w ."},
			{Desc: "再次扫描验证：",
				Command: "zctl perf scan --dir=."},
		},
		Notes: []string{commitFirstNote},
	},
	"sty-mnd": {
		Title:   "💡 修复建议：mnd (魔法数字)",
		Summary: "不可自动修复。需把字面量提取为有名 const，或加注释说明含义。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=mnd ./..."},
		},
	},
	"sty-const": {
		Title:   "💡 修复建议：goconst (重复字符串)",
		Summary: "把出现 3+ 次的字符串提取为 const。",
		Steps: []FixStep{
			{Desc: "聚焦扫描：",
				Command: "golangci-lint run --no-config --disable-all --enable=goconst ./..."},
		},
	},
	"sty-lll": {
		Title:   "💡 一键修复：golines (行长度 ≤120)",
		Summary: "golines 是社区公认的 Go 长行自动换行工具，支持 -w 直接修改源码。",
		Steps: []FixStep{
			{Desc: "安装 golines：",
				Command: "go install github.com/segmentio/golines@latest"},
			{Desc: "一键把超长行折行（最大 120 列）：",
				Command: "golines -w -m 120 ."},
			{Desc: "再次跑 gofmt 兼容：",
				Command: "gofmt -w ."},
		},
		Notes: []string{commitFirstNote, "golines 重排可能影响 git blame，建议在独立 commit 中执行。"},
	},
	"sty-spell": {
		Title:   "💡 一键修复：misspell",
		Summary: "misspell 支持 -w 直接改写英文拼写错误，安全可一键。",
		Steps: []FixStep{
			{Desc: "安装 misspell：",
				Command: "go install github.com/client9/misspell/cmd/misspell@latest"},
			{Desc: "一键修复全仓拼写：",
				Command: "misspell -w ."},
		},
		Notes: []string{commitFirstNote},
	},
	"sty-todo": {
		Title:   "💡 修复建议：godox (TODO/FIXME 残留)",
		Summary: "无法自动修复。处理掉 TODO 后删除标记，或迁移到 issue tracker。",
		Steps: []FixStep{
			{Desc: "列出所有 TODO/FIXME：",
				Command: "grep -RIn -E 'TODO|FIXME|HACK|XXX' --include='*.go' ."},
		},
	},

	// ── 📐 Quality · 测试质量 ─────────────────────────────────────────────
	"test-testify": {
		Title:   "💡 一键修复：testifylint",
		Summary: "testifylint 支持 -fix，可批量修正 testify 断言写法（如 Equal(t,nil,err) → NoError(t,err)）。",
		Steps: []FixStep{
			{Desc: "安装 testifylint：",
				Command: "go install github.com/Antonboom/testifylint/cmd/testifylint@latest"},
			{Desc: "一键修复：",
				Command: "testifylint -fix ./..."},
			{Desc: "或通过 golangci-lint：",
				Command: "golangci-lint run --no-config --disable-all --enable=testifylint --fix ./..."},
		},
		Notes: []string{commitFirstNote},
	},
}
