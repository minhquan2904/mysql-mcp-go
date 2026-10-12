package source

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/minhquan2904/mysql-mcp-go/internal/config"
)

// maxUserConnections is the MAX_USER_CONNECTIONS of the read-only account
// (deploy/mysql/init/02-readonly-user.sh). One Source opens at most
// MaxConcurrent+2 query connections plus killerConns killer connections.
const maxUserConnections = 20

const (
	killerConns = 2

	pingTimeout = 10 * time.Second
	killTimeout = 5 * time.Second

	errQueryTimeoutNumber = 3024 // ER_QUERY_TIMEOUT: max_execution_time exceeded
	errUnknownThreadID    = 1094 // ER_NO_SUCH_THREAD: the query already ended
)

// ErrQueryTimeout is returned (wrapped) when a query ran past the configured
// timeout, whether the client deadline or the server's max_execution_time
// fired first.
var ErrQueryTimeout = errors.New("query exceeded timeout")

// Source is one MySQL database: a query pool plus a separate pool that only
// issues KILL QUERY, so a saturated query pool can never block a kill.
type Source struct {
	Name string
	// Database is the schema named in the DSN (Open requires one). Tools that
	// read server-wide statistics scope themselves to it.
	Database string

	// PerfSchemaVerified is set by the tools once they have seen
	// performance_schema enabled: the variable cannot change while the server
	// runs, so the check is not repeated.
	PerfSchemaVerified atomic.Bool

	db     *sql.DB
	killer *sql.DB
	lim    Limits
	log    *slog.Logger

	kills atomic.Int64 // KILL QUERY statements issued (read by tests)
}

// Open connects to the source and pings both pools. The DSN in cfg must
// already be normalized by the config package. Errors never contain the DSN:
// they name the source by its redacted form.
func Open(ctx context.Context, cfg config.Source, lim Limits, log *slog.Logger) (*Source, error) {
	if lim.MaxRows <= 0 || lim.MaxCellBytes <= 0 || lim.MaxResultBytes <= 0 ||
		lim.MaxConcurrent <= 0 || lim.Timeout <= 0 {
		return nil, errors.New("source limits must all be positive")
	}
	if lim.MaxResultBytes < lim.MaxCellBytes {
		// One cell is buffered whole before it is capped; a result cap below
		// the cell cap would make every large cell drop its row.
		return nil, fmt.Errorf("MaxResultBytes (%d) must be at least MaxCellBytes (%d)", lim.MaxResultBytes, lim.MaxCellBytes)
	}
	if need := lim.MaxConcurrent + 2 + killerConns; need > maxUserConnections {
		return nil, fmt.Errorf("MaxConcurrent %d needs %d connections, over the read-only user's MAX_USER_CONNECTIONS limit of %d",
			lim.MaxConcurrent, need, maxUserConnections)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	database, err := dsnDatabase(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", cfg.Name, err)
	}

	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("source %s: invalid DSN", cfg.Name)
	}
	// Every query takes its own connection and discards it, so nothing is
	// idle; the pool only bounds how many can be open at once. The extra two
	// leave room for control statements next to MaxConcurrent queries.
	db.SetMaxOpenConns(lim.MaxConcurrent + 2)
	db.SetMaxIdleConns(0)

	killer, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("source %s: invalid DSN", cfg.Name)
	}
	killer.SetMaxOpenConns(killerConns)
	killer.SetMaxIdleConns(killerConns)

	s := &Source{Name: cfg.Name, Database: database, db: db, killer: killer, lim: lim, log: log}
	for _, p := range []*sql.DB{db, killer} {
		pctx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := p.PingContext(pctx)
		cancel()
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("source %s (%s): ping: %w", cfg.Name, cfg.Redacted(), err)
		}
	}
	// Keep both killer connections open so the first timeout does not have to
	// dial (and possibly fail) while a query is hanging.
	if err := s.prewarmKiller(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("source %s (%s): killer pool: %w", cfg.Name, cfg.Redacted(), err)
	}
	return s, nil
}

func (s *Source) prewarmKiller(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	conns := make([]*sql.Conn, 0, killerConns)
	defer func() {
		for _, c := range conns {
			c.Close() // back to the idle pool
		}
	}()
	for range killerConns {
		c, err := s.killer.Conn(ctx)
		if err != nil {
			return err
		}
		conns = append(conns, c)
	}
	return nil
}

// Ping checks that the query pool can reach the server.
func (s *Source) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close closes both pools.
func (s *Source) Close() error {
	var errs []error
	if s.db != nil {
		errs = append(errs, s.db.Close())
	}
	if s.killer != nil {
		errs = append(errs, s.killer.Close())
	}
	return errors.Join(errs...)
}

// RunReadOnly runs one statement on a dedicated connection inside a READ ONLY
// transaction that is always rolled back, and then discards the physical
// connection so session state (locks, temporary tables, user variables)
// cannot outlive the call. It does not validate the SQL: that is the guard's
// job.
//
// Whenever the context is done after the statement (deadline or cancel), the
// running query is killed synchronously through the killer pool and the call
// fails, even if rows came back. A server-side max_execution_time error maps
// to ErrQueryTimeout too.
func (s *Source) RunReadOnly(ctx context.Context, query string, args ...any) (*Result, error) {
	eff := effectiveTimeout(ctx, s.lim.Timeout)
	ctx, cancel := context.WithTimeout(ctx, s.lim.Timeout)
	defer cancel()
	start := time.Now()

	var connID int64
	res, err := s.run(ctx, query, args, &connID)
	if err != nil {
		err = s.classify(ctx, connID, eff, err)
		s.log.DebugContext(ctx, "query failed", "source", s.Name, "conn_id", connID,
			"duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	s.log.DebugContext(ctx, "query done", "source", s.Name, "conn_id", connID,
		"duration_ms", time.Since(start).Milliseconds(), "rows", res.RowCount, "truncated", res.Truncated)
	return res, nil
}

// run executes the statement; connID is set as soon as it is known.
func (s *Source) run(ctx context.Context, query string, args []any, connID *int64) (*Result, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer discard(conn)

	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(connID); err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	cols := make([]colInfo, len(types))
	for i, ct := range types {
		cols[i] = colInfo{Name: ct.Name(), DBType: ct.DatabaseTypeName()}
	}
	res, err := collect(rows, cols, s.lim)
	if err != nil {
		return nil, err
	}
	// Judge the deadline here, before the deferred rows.Close/Rollback/discard
	// run, so their time cannot turn a good result into a timeout. A result
	// that finished at the deadline is not trusted: when the server's
	// max_execution_time (same value as the client timeout) interrupts SLEEP()
	// it returns 1 as a normal row instead of error 3024.
	if cerr := ctxDone(ctx); cerr != nil {
		return nil, cerr
	}
	if res.stoppedEarly {
		// Rows are still pending: kill the statement so the deferred
		// rows.Close reads an error packet instead of draining millions of
		// rows. The truncated result is a success whatever that takes.
		s.kill(*connID)
	}
	return res, nil
}

// classify turns a failed run into the error returned to callers, killing the
// server-side query first when the context ended.
// eff is the effective timeout (parent deadline or the configured limit,
// whichever is shorter), reported in the error.
func (s *Source) classify(ctx context.Context, connID int64, eff time.Duration, err error) error {
	if cerr := ctxDone(ctx); cerr != nil {
		if connID != 0 {
			s.kill(connID)
		}
		if errors.Is(cerr, context.DeadlineExceeded) {
			return fmt.Errorf("%w %s", ErrQueryTimeout, eff.Round(time.Millisecond))
		}
		return cerr
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == errQueryTimeoutNumber {
		return fmt.Errorf("%w %s: %w", ErrQueryTimeout, eff.Round(time.Millisecond), err)
	}
	return err
}

// ctxDone returns the context's error, also when its deadline has passed but
// the runtime timer has not marked the context done yet (that can lag by a
// millisecond or more, which is enough to lose a race with the server).
func ctxDone(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl) {
		return context.DeadlineExceeded
	}
	return nil
}

// kill stops the running statement of connection id. It is synchronous and
// uses its own context: the caller's is already done.
func (s *Source) kill(id int64) {
	s.kills.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	if _, err := s.killer.ExecContext(ctx, fmt.Sprintf("KILL QUERY %d", id)); err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == errUnknownThreadID {
			return // the connection is already gone
		}
		s.log.Warn("KILL QUERY failed", "source", s.Name, "conn_id", id, "err", err)
	}
}

// discard closes the physical connection instead of returning it to the pool:
// Raw reporting driver.ErrBadConn makes database/sql close the driver conn.
func discard(c *sql.Conn) {
	_ = c.Raw(func(any) error { return driver.ErrBadConn })
	_ = c.Close()
}

// effectiveTimeout is the time a call can actually take: the configured limit,
// or less when the caller's context expires sooner.
func effectiveTimeout(ctx context.Context, limit time.Duration) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return min(limit, max(time.Until(dl), 0))
	}
	return limit
}

// dsnDatabase returns the schema named in a DSN. A DSN that does not parse or
// names no database is an error: the tools that read server-wide statistics
// scope themselves to this schema and must not fall back to "any schema".
func dsnDatabase(dsn string) (string, error) {
	c, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", errors.New("invalid DSN")
	}
	if c.DBName == "" {
		return "", errors.New("the DSN must name a database (user:pass@tcp(host:port)/dbname)")
	}
	return c.DBName, nil
}
