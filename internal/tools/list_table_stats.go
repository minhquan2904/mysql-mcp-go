// Ported from mcp-toolbox@0a7c1b1c110cc97ea9652d2fb1a7a7476b4be988 internal/tools/mysql/mysqllisttablestats (Apache-2.0)
// and rewritten: the toolbox reads sys.x$schema_table_statistics, which needs
// the sys schema; this reads performance_schema.table_io_waits_summary_by_table
// joined to information_schema.TABLES instead.

package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

const listTableStatsSQL = `
SELECT
  t.TABLE_SCHEMA AS table_schema,
  t.TABLE_NAME AS table_name,
  t.TABLE_ROWS AS table_rows,
  t.DATA_LENGTH AS data_length,
  t.INDEX_LENGTH AS index_length,
  ROUND(io.SUM_TIMER_WAIT / 1000000000000, 2) AS total_latency_secs,
  io.COUNT_FETCH AS rows_fetched,
  io.COUNT_INSERT AS rows_inserted,
  io.COUNT_UPDATE AS rows_updated,
  io.COUNT_DELETE AS rows_deleted,
  io.COUNT_WRITE AS write_ops
FROM
  information_schema.TABLES AS t
LEFT JOIN
  performance_schema.table_io_waits_summary_by_table AS io
  ON (io.OBJECT_SCHEMA = t.TABLE_SCHEMA AND io.OBJECT_NAME = t.TABLE_NAME)
WHERE
  t.TABLE_SCHEMA NOT IN ('sys', 'information_schema', 'mysql', 'performance_schema')
  AND t.TABLE_TYPE = 'BASE TABLE'
  AND (t.TABLE_SCHEMA = COALESCE(NULLIF(?, ''), NULLIF(DATABASE(), '')) OR COALESCE(NULLIF(?, ''), NULLIF(DATABASE(), '')) IS NULL)
  AND (COALESCE(?, '') = '' OR t.TABLE_NAME = ?)
ORDER BY
  CASE ?
    WHEN 'row_count' THEN t.TABLE_ROWS
    WHEN 'rows_fetched' THEN io.COUNT_FETCH
    WHEN 'rows_inserted' THEN io.COUNT_INSERT
    WHEN 'rows_updated' THEN io.COUNT_UPDATE
    WHEN 'rows_deleted' THEN io.COUNT_DELETE
    ELSE io.SUM_TIMER_WAIT
    END DESC,
  t.TABLE_SCHEMA,
  t.TABLE_NAME
LIMIT ?;
`

// tableStatsSorts are the accepted sort_by values; "" and total_latency sort by
// the total time spent in table I/O.
var tableStatsSorts = map[string]bool{
	"": true, "total_latency": true, "row_count": true,
	"rows_fetched": true, "rows_inserted": true, "rows_updated": true, "rows_deleted": true,
}

type listTableStatsArgs struct {
	SourceArg
	TableSchema string `json:"table_schema,omitempty" jsonschema:"(Optional) The database where statistics  is to be executed. Check all tables visible to the current user if not specified"`
	TableName   string `json:"table_name,omitempty" jsonschema:"(Optional) Name of the table to be checked. Check all tables visible to the current user if not specified."`
	SortBy      string `json:"sort_by,omitempty" jsonschema:"(Optional) The column to sort by: row_count, rows_fetched, rows_inserted, rows_updated, rows_deleted; default is total latency"`
	Limit       *int   `json:"limit,omitempty" jsonschema:"(Optional) Max rows to return, default is 10"`
}

func addListTableStats(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_table_stats",
		Description: "Read-only. List per-table size and I/O statistics (rows fetched, inserted, updated, deleted and write operations) " +
			"since server start, from performance_schema.table_io_waits_summary_by_table and information_schema.TABLES. " + untrusted,
		Annotations: readOnly(),
	}, instrument(d, "list_table_stats", func(ctx context.Context, in listTableStatsArgs) (source.Result, *source.Result, error) {
		if !tableStatsSorts[in.SortBy] {
			return source.Result{}, nil, userError{fmt.Errorf("invalid value for sort_by: %q (use row_count, rows_fetched, rows_inserted, rows_updated or rows_deleted)", in.SortBy)}
		}
		if err := checkSchema(d, in.Source, in.TableSchema); err != nil {
			return source.Result{}, nil, err
		}
		if err := requirePerfSchema(ctx, d, in.Source); err != nil {
			return source.Result{}, nil, err
		}
		res, err := run(ctx, d, in.Source, listTableStatsSQL,
			in.TableSchema, in.TableSchema, in.TableName, in.TableName, in.SortBy, clampLimit(in.Limit, 10, d.MaxRows))
		if err != nil {
			return source.Result{}, nil, err
		}
		return *res, res, nil
	}))
}
