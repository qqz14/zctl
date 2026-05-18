package generator

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qqz14/zctl/util/pathx"
)

// EnsureEntInfra is the SINGLE source of truth for installing ent infrastructure
// into a project. Every zctl subcommand that may introduce ent imports MUST
// call this function (currently: rpc ent, rpc dao).
//
// The four artifacts below are treated as one logical transaction. They are
// either all present (ready to use) or installed together. Each artifact is
// idempotent:
//
//  1. internal/dao/entlog/entlog.go       — written if missing
//  2. internal/dao/entx/tx.go             — written if missing
//  3. internal/svc/service_context.go     — patched at sentinel regions:
//       <<ENT_IMPORTS>>     ent/entlog/entx/dialect/driver imports
//       <<ENT_FIELDS>>      DB *ent.Client + Tx *entx.Tx
//       <<ENT_INIT>>        dsn + entsql.Open + entlog wrap + Schema.Create
//       <<ENT_FIELDS_INIT>> DB: entClient, Tx: entx.New(entClient)
//  4. DAO sentinel regions in svc (DAOS / DAOS_INIT / DAO imports)
//
// Failure mode: best-effort. On error mid-way, partial writes remain and an
// error is returned; re-running the same command resumes safely thanks to
// idempotency. User-edited code outside sentinel regions is never touched.
func EnsureEntInfra(abs, modulePath string) error {
	// 1. internal/dao/entlog/entlog.go
	if err := writeEntlog(abs); err != nil {
		return fmt.Errorf("write entlog: %w", err)
	}

	// 2. internal/dao/entx/tx.go
	if err := writeEntx(abs, modulePath); err != nil {
		return fmt.Errorf("write entx: %w", err)
	}

	// 3. internal/dao/hook/ directory (model hook files generated per-schema)
	if err := pathx.MkdirIfNotExist(filepath.Join(abs, "internal", "dao", "hook")); err != nil {
		return fmt.Errorf("create hook dir: %w", err)
	}

	// 4 + 5. patch service_context.go (ent + dao sentinel regions)
	if err := patchServiceContext(abs, modulePath); err != nil {
		return fmt.Errorf("patch service_context: %w", err)
	}

	return nil
}

// ─── 1. internal/dao/entlog/entlog.go ───────────────────────────────────────

const entlogContent = `package entlog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	"github.com/zeromicro/go-zero/core/logx"
)

// WithModule injects module name into ctx via logx fields.
func WithModule(ctx context.Context, module string) context.Context {
	return logx.ContextWithFields(ctx, logx.Field("module", module))
}

// ─── Driver ──────────────────────────────────────────────────────────────────────
//
// Driver is a transparent logging decorator for ent dialect.Driver.
// It logs every Query and Exec call with SQL, args, cost, and (for Exec) rows_affected.
//
// IMPORTANT: This driver NEVER modifies the query result (v parameter).
// Replacing v's internal state (e.g. ColumnScanner) would break downstream code
// that does type assertions (such as atlas schema migration).

type Driver struct {
	dialect.Driver
}

func NewDriver(drv dialect.Driver) *Driver {
	return &Driver{Driver: drv}
}

func (d *Driver) Exec(ctx context.Context, query string, args, v any) error {
	start := time.Now()
	err := d.Driver.Exec(ctx, query, args, v)
	cost := time.Since(start)
	logExec(ctx, query, args, v, cost, err)
	return err
}

func (d *Driver) Query(ctx context.Context, query string, args, v any) error {
	start := time.Now()
	err := d.Driver.Query(ctx, query, args, v)
	cost := time.Since(start)
	logQuery(ctx, query, args, cost, err)
	return err
}

func (d *Driver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.Driver.Tx(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, ctx: ctx}, nil
}

// ExecContext exposes the underlying driver's ExecContext for raw-SQL DAO calls
// (e.g. INSERT ... ON DUPLICATE KEY UPDATE batch). Without this method, ent's
// generated client.ExecContext type-asserts c.driver against
// interface{ ExecContext(...) } and fails because *Driver only inherits
// dialect.Driver's method set.
func (d *Driver) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	ex, ok := d.Driver.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	if !ok {
		return nil, fmt.Errorf("entlog.Driver: underlying driver does not support ExecContext")
	}
	start := time.Now()
	res, err := ex.ExecContext(ctx, query, args...)
	cost := time.Since(start)
	logExecRaw(ctx, query, args, res, cost, err)
	return res, err
}

// QueryContext exposes the underlying driver's QueryContext for raw-SQL DAO calls.
func (d *Driver) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q, ok := d.Driver.(interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	})
	if !ok {
		return nil, fmt.Errorf("entlog.Driver: underlying driver does not support QueryContext")
	}
	start := time.Now()
	rows, err := q.QueryContext(ctx, query, args...)
	cost := time.Since(start)
	logQuery(ctx, query, args, cost, err)
	return rows, err
}

// ─── Tx ──────────────────────────────────────────────────────────────────────────

type Tx struct {
	dialect.Tx
	ctx context.Context
}

func (t *Tx) Exec(ctx context.Context, query string, args, v any) error {
	start := time.Now()
	err := t.Tx.Exec(ctx, query, args, v)
	cost := time.Since(start)
	logExec(ctx, query, args, v, cost, err)
	return err
}

func (t *Tx) Query(ctx context.Context, query string, args, v any) error {
	start := time.Now()
	err := t.Tx.Query(ctx, query, args, v)
	cost := time.Since(start)
	logQuery(ctx, query, args, cost, err)
	return err
}

func (t *Tx) Commit() error {
	logx.WithContext(t.ctx).Infow("[SQL] tx commit")
	return t.Tx.Commit()
}

func (t *Tx) Rollback() error {
	logx.WithContext(t.ctx).Infow("[SQL] tx rollback")
	return t.Tx.Rollback()
}

// ExecContext exposes the underlying tx's ExecContext for raw-SQL DAO calls
// inside a transaction. Without this method, ent's generated txDriver
// type-asserts tx against interface{ ExecContext(...) } and fails because
// *Tx only inherits dialect.Tx's method set.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	ex, ok := t.Tx.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	if !ok {
		return nil, fmt.Errorf("entlog.Tx: underlying tx does not support ExecContext")
	}
	start := time.Now()
	res, err := ex.ExecContext(ctx, query, args...)
	cost := time.Since(start)
	logExecRaw(ctx, query, args, res, cost, err)
	return res, err
}

// QueryContext exposes the underlying tx's QueryContext for raw-SQL DAO calls inside a transaction.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q, ok := t.Tx.(interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	})
	if !ok {
		return nil, fmt.Errorf("entlog.Tx: underlying tx does not support QueryContext")
	}
	start := time.Now()
	rows, err := q.QueryContext(ctx, query, args...)
	cost := time.Since(start)
	logQuery(ctx, query, args, cost, err)
	return rows, err
}

// ─── Query logging ───────────────────────────────────────────────────────────────

func logQuery(ctx context.Context, query string, args any, cost time.Duration, err error) {
	log := logx.WithContext(ctx)
	if err != nil {
		log.Error(fmt.Sprintf("[SQL] query error=%s cost[%s] sql=%s args=%v",
			err.Error(), cost.Truncate(time.Millisecond), query, args))
	} else {
		log.Info(fmt.Sprintf("[SQL] query cost[%s] sql=%s args=%v",
			cost.Truncate(time.Millisecond), query, args))
	}
}

// ─── Exec logging ────────────────────────────────────────────────────────────────

func logExec(ctx context.Context, query string, args any, v any, cost time.Duration, err error) {
	log := logx.WithContext(ctx)
	ra, hasRA := extractRowsAffected(v)
	if err != nil {
		if hasRA {
			log.Error(fmt.Sprintf("[SQL] exec rows_affected[%d] error=%s cost[%s] sql=%s args=%v",
				ra, err.Error(), cost.Truncate(time.Millisecond), query, args))
		} else {
			log.Error(fmt.Sprintf("[SQL] exec error=%s cost[%s] sql=%s args=%v",
				err.Error(), cost.Truncate(time.Millisecond), query, args))
		}
	} else {
		if hasRA {
			log.Info(fmt.Sprintf("[SQL] exec rows_affected[%d] cost[%s] sql=%s args=%v",
				ra, cost.Truncate(time.Millisecond), query, args))
		} else {
			log.Info(fmt.Sprintf("[SQL] exec cost[%s] sql=%s args=%v",
				cost.Truncate(time.Millisecond), query, args))
		}
	}
}

func extractRowsAffected(v any) (int64, bool) {
	if v == nil {
		return 0, false
	}
	type rowsAffector interface {
		RowsAffected() (int64, error)
	}
	if r, ok := v.(rowsAffector); ok {
		n, err := r.RowsAffected()
		if err == nil {
			return n, true
		}
	}
	if rp, ok := v.(*sql.Result); ok && rp != nil && *rp != nil {
		n, err := (*rp).RowsAffected()
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

// logExecRaw 记录原生 SQL（ExecContext）执行日志，使用 sql.Result 提取 rows_affected。
func logExecRaw(ctx context.Context, query string, args any, res sql.Result, cost time.Duration, err error) {
	log := logx.WithContext(ctx)
	var ra int64
	hasRA := false
	if err == nil && res != nil {
		if n, e := res.RowsAffected(); e == nil {
			ra, hasRA = n, true
		}
	}
	if err != nil {
		if hasRA {
			log.Error(fmt.Sprintf("[SQL] exec rows_affected[%d] error=%s cost[%s] sql=%s args=%v",
				ra, err.Error(), cost.Truncate(time.Millisecond), query, args))
		} else {
			log.Error(fmt.Sprintf("[SQL] exec error=%s cost[%s] sql=%s args=%v",
				err.Error(), cost.Truncate(time.Millisecond), query, args))
		}
		return
	}
	if hasRA {
		log.Info(fmt.Sprintf("[SQL] exec rows_affected[%d] cost[%s] sql=%s args=%v",
			ra, cost.Truncate(time.Millisecond), query, args))
	} else {
		log.Info(fmt.Sprintf("[SQL] exec cost[%s] sql=%s args=%v",
			cost.Truncate(time.Millisecond), query, args))
	}
}
`

func writeEntlog(abs string) error {
	dir := filepath.Join(abs, "internal", "dao", "entlog")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	target := filepath.Join(dir, "entlog.go")
	if pathx.FileExists(target) {
		// Auto-upgrade: older entlog templates don't expose ExecContext/QueryContext
		// on Driver/Tx, which makes ent's generated client.ExecContext (and
		// txDriver.ExecContext) fall back to "Driver.ExecContext is not supported"
		// / "Tx.ExecContext is not supported" — breaking any DAO that uses raw SQL
		// (e.g. INSERT ... ON DUPLICATE KEY UPDATE batches inside a transaction).
		// Only overwrite when the existing file is the legacy version that lacks
		// the ExecContext passthrough. User-customized files that already added it
		// are left untouched.
		existing, err := os.ReadFile(target)
		if err != nil {
			return err
		}
		if bytes.Contains(existing, []byte("func (d *Driver) ExecContext(")) ||
			bytes.Contains(existing, []byte("func (t *Tx) ExecContext(")) {
			return nil
		}
		fmt.Println("[zctl] upgrading internal/dao/entlog/entlog.go (adds ExecContext/QueryContext passthrough).")
	}
	return os.WriteFile(target, []byte(entlogContent), 0644)
}

// ─── 2. internal/dao/entx/tx.go ─────────────────────────────────────────────

const entxTemplate = `// Package entx provides an ent transaction manager that wraps Begin/Commit/
// Rollback/panic-recovery so business code can write:
//
//	svcCtx.Tx.WithTx(ctx, func(ctx context.Context, tx *ent.Tx) error {
//	    userDao := svcCtx.UserDao.WithTx(tx)
//	    if _, err := userDao.Create(ctx, ...); err != nil { return err }
//	    return nil // → commit; non-nil err or panic → rollback
//	})
package entx

import (
	"context"
	"fmt"

	"%[1]s/ent"
	"%[1]s/pkg/ctxutil"
)

// Tx is the transaction manager.
type Tx struct {
	client *ent.Client
}

// New constructs a transaction manager from the global *ent.Client.
func New(client *ent.Client) *Tx {
	return &Tx{client: client}
}

// WithTx executes fn inside a transaction:
//   - fn returns nil  ⇒ commit
//   - fn returns err  ⇒ rollback
//   - fn panics       ⇒ rollback then re-panic
func (m *Tx) WithTx(ctx context.Context, fn func(ctx context.Context, tx *ent.Tx) error) (err error) {
	tx, err := m.client.Tx(ctx)
	if err != nil {
		ctxutil.L(ctx).Errorw("entx.Begin failed", ctxutil.ErrField(err))
		return fmt.Errorf("entx begin: %%w", err)
	}

	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback()
			ctxutil.L(ctx).Errorw("entx panic, rolled back",
				ctxutil.ErrField(fmt.Errorf("panic: %%v", r)))
			panic(r)
		}
	}()

	if err = fn(ctx, tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			ctxutil.L(ctx).Errorw("entx.Rollback failed", ctxutil.ErrField(rbErr))
			return fmt.Errorf("rollback: %%v (orig: %%w)", rbErr, err)
		}
		return err
	}

	if err = tx.Commit(); err != nil {
		ctxutil.L(ctx).Errorw("entx.Commit failed", ctxutil.ErrField(err))
		return fmt.Errorf("entx commit: %%w", err)
	}
	return nil
}
`

func writeEntx(abs, modulePath string) error {
	dir := filepath.Join(abs, "internal", "dao", "entx")
	if err := pathx.MkdirIfNotExist(dir); err != nil {
		return err
	}
	target := filepath.Join(dir, "tx.go")
	if pathx.FileExists(target) {
		return nil
	}
	content := fmt.Sprintf(entxTemplate, modulePath)
	return os.WriteFile(target, []byte(content), 0644)
}

// ─── 3 + 4. service_context.go sentinel patching ───────────────────────────

// patchServiceContext fills six sentinel regions in service_context.go.
// Each region is filled only if currently empty (i.e. just two adjacent
// sentinel comments). Already-filled regions are left untouched, so user
// edits inside the regions are preserved between runs.
//
// Sentinels:
//
//	// <<ENT_IMPORTS_BEGIN>>      ent/entlog/entx/dialect/driver imports
//	// <<ENT_IMPORTS_END>>
//	// <<ENT_FIELDS_BEGIN>>       struct fields: DB *ent.Client, Tx *entx.Tx
//	// <<ENT_FIELDS_END>>
//	// <<ENT_INIT_BEGIN>>         dsn + entsql.Open + entlog wrap + migrate
//	// <<ENT_INIT_END>>
//	// <<ENT_FIELDS_INIT_BEGIN>>  literal init: DB: entClient, Tx: entx.New(entClient)
//	// <<ENT_FIELDS_INIT_END>>
//	// <<DAOS_BEGIN>>             struct fields: UserDao dao.UserDao, ...
//	// <<DAOS_END>>
//	// <<DAOS_INIT_BEGIN>>        literal init: UserDao: impl.NewUserOceanBaseDao(entClient), ...
//	// <<DAOS_INIT_END>>
func patchServiceContext(abs, modulePath string) error {
	svcFile := filepath.Join(abs, "internal", "svc", "service_context.go")
	data, err := os.ReadFile(svcFile)
	if err != nil {
		// svc not generated yet (e.g. user ran `zctl rpc ent` before `zctl rpc new`).
		return nil
	}
	src := string(data)

	// Bail silently on legacy svc files that lack any sentinel — never alter
	// user-authored code that was not produced by our template.
	if !strings.Contains(src, "<<ENT_INIT_BEGIN>>") {
		return nil
	}

	// ── 3a. Ent init region (4 sentinel pairs) ──
	src = fillIfEmpty(src, "// <<ENT_IMPORTS_BEGIN>>", "// <<ENT_IMPORTS_END>>",
		entImportsBlock(modulePath))

	src = fillIfEmpty(src, "// <<ENT_FIELDS_BEGIN>>", "// <<ENT_FIELDS_END>>",
		"\tDB *ent.Client\n\tTx *entx.Tx\n")

	src = fillIfEmpty(src, "// <<ENT_INIT_BEGIN>>", "// <<ENT_INIT_END>>",
		entInitBlock())

	src = fillIfEmpty(src, "// <<ENT_FIELDS_INIT_BEGIN>>", "// <<ENT_FIELDS_INIT_END>>",
		"\t\tDB: entClient,\n\t\tTx: entx.New(entClient),\n")

	// ── 4. DAO regions ──
	daoNames, err := scanDaoInterfaces(filepath.Join(abs, "internal", "dao"))
	if err != nil {
		return err
	}

	var fieldBlock strings.Builder
	for _, name := range daoNames {
		fmt.Fprintf(&fieldBlock, "\t%s dao.%s\n", name, name)
	}
	var initBlock strings.Builder
	for _, name := range daoNames {
		modelName := strings.TrimSuffix(name, "Dao")
		fmt.Fprintf(&initBlock, "\t\t%s: impl.New%sOceanBaseDao(entClient),\n", name, modelName)
	}

	src = replaceRegion(src, "// <<DAOS_BEGIN>>", "// <<DAOS_END>>", fieldBlock.String())
	src = replaceRegion(src, "// <<DAOS_INIT_BEGIN>>", "// <<DAOS_INIT_END>>", initBlock.String())

	if len(daoNames) > 0 {
		src = ensureImport(src, modulePath+"/internal/dao")
		src = ensureImport(src, modulePath+"/internal/dao/impl")
	}

	return os.WriteFile(svcFile, []byte(src), 0644)
}

func entInitBlock() string {
	return `	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=UTC",
		c.DatabaseConf.Username, c.DatabaseConf.Password,
		c.DatabaseConf.Host, c.DatabaseConf.Port, c.DatabaseConf.DBName)
	drv, err := entsql.Open(dialect.MySQL, dsn)
	if err != nil {
		panic(fmt.Sprintf("failed to open database: %v", err))
	}

	// Wrap with entlog for SQL logging (remove to disable)
	entClient := ent.NewClient(ent.Driver(entlog.NewDriver(drv)))

	// Auto-migrate: only allowed in dev/stage
	if c.Env == "dev" || c.Env == "stage" {
		if err := entClient.Schema.Create(context.Background()); err != nil {
			log.Fatalf("[ent] auto-migrate failed: %v", err)
		}
		log.Printf("[ent] auto-migrate completed (env=%s)", c.Env)
	} else {
		log.Printf("[ent] auto-migrate skipped (env=%s, only dev/stage allowed)", c.Env)
	}
`
}

func entImportsBlock(modulePath string) string {
	return fmt.Sprintf(`	"context"
	"fmt"
	"log"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"%s/ent"
	_ "%s/ent/runtime"
	"%s/internal/dao/entlog"
	"%s/internal/dao/entx"

	_ "github.com/go-sql-driver/mysql"
`, modulePath, modulePath, modulePath, modulePath)
}

// ─── sentinel helpers ───────────────────────────────────────────────────────

// fillIfEmpty inserts body between begin/end markers ONLY if the region is
// currently empty (i.e. only whitespace between markers). If user has already
// added content, this is a no-op so user edits are preserved.
func fillIfEmpty(src, begin, end, body string) string {
	bIdx := strings.Index(src, begin)
	eIdx := strings.Index(src, end)
	if bIdx < 0 || eIdx < 0 || eIdx < bIdx {
		return src
	}
	lineEnd := strings.Index(src[bIdx:], "\n")
	if lineEnd < 0 {
		return src
	}
	bodyStart := bIdx + lineEnd + 1
	bodyEnd := strings.LastIndex(src[:eIdx], "\n")
	if bodyEnd < bodyStart {
		// Markers on adjacent lines — region is empty.
		bodyEnd = bodyStart
	} else {
		bodyEnd++ // include trailing newline
	}
	between := src[bodyStart:bodyEnd]
	if strings.TrimSpace(between) != "" {
		// Already filled (by us or user) → leave alone.
		return src
	}
	return src[:bodyStart] + body + src[bodyEnd:]
}
