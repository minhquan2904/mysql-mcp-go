// Ported from mcp-toolbox@0a7c1b1c110cc97ea9652d2fb1a7a7476b4be988 internal/tools/mysql/mysqllistactivequeries (Apache-2.0)

package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

// Same statement as the toolbox's MySQL variant, except the statement text is
// not cut to 100 characters in SQL: it is cut to 4096 here and then redacted
// and cut to 1024 runes by the handler (redactColumn).
const listActiveQueriesSQL = `
    SELECT
        p.id AS processlist_id,
        LEFT(IFNULL(p.info, t.trx_query), 4096) AS query,
        t.trx_started AS trx_started,
        (UNIX_TIMESTAMP(UTC_TIMESTAMP()) - UNIX_TIMESTAMP(t.trx_started)) AS trx_duration_seconds,
        (UNIX_TIMESTAMP(UTC_TIMESTAMP()) - UNIX_TIMESTAMP(t.trx_wait_started)) AS trx_wait_duration_seconds,
        p.time AS query_time,
        t.trx_state AS trx_state,
        p.state AS process_state,
        IF(p.host IS NULL OR p.host = '', p.user, concat(p.user, '@', SUBSTRING_INDEX(p.host, ':', 1))) AS user,
        t.trx_rows_locked AS trx_rows_locked,
        t.trx_rows_modified AS trx_rows_modified,
        p.db AS db
    FROM
        information_schema.processlist p
        LEFT OUTER JOIN
        information_schema.innodb_trx t
        ON p.id = t.trx_mysql_thread_id
    WHERE
        (? IS NULL OR p.time >= ?)
        AND p.id != CONNECTION_ID()
        AND Command NOT IN ('Binlog Dump', 'Binlog Dump GTID', 'Connect', 'Connect Out', 'Register Slave')
        AND User NOT IN ('system user', 'event_scheduler')
        AND (t.trx_id is NOT NULL OR command != 'Sleep')
    ORDER BY
        t.trx_started
    LIMIT ?;
`

type listActiveQueriesArgs struct {
	SourceArg
	MinDurationSecs *int `json:"min_duration_secs,omitempty" jsonschema:"Optional: Only show queries running for at least this long in seconds"`
	Limit           *int `json:"limit,omitempty" jsonschema:"Optional: The maximum number of rows to return."`
}

func addListActiveQueries(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_active_queries",
		Description: "Read-only. List the currently running queries and open transactions of all sessions on the server. " +
			"Statement text is shown with every string and number literal replaced by ? and is cut to 1024 characters. " + untrusted,
		Annotations: readOnly(),
	}, instrument(d, "list_active_queries", func(ctx context.Context, in listActiveQueriesArgs) (source.Result, *source.Result, error) {
		min := nullableInt(in.MinDurationSecs, 0)
		res, err := run(ctx, d, in.Source, listActiveQueriesSQL, min, min, clampLimit(in.Limit, 100, d.MaxRows))
		if err != nil {
			return source.Result{}, nil, err
		}
		redactColumn(res, "query")
		return *res, res, nil
	}))
}
