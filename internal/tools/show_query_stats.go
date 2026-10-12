// Ported from mcp-toolbox@0a7c1b1c110cc97ea9652d2fb1a7a7476b4be988 internal/tools/mysql/mysqlshowquerystats (Apache-2.0)

package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

// Only DIGEST_TEXT (the statement with literals already replaced by ?) is
// selected, never QUERY_SAMPLE_TEXT, which holds a real statement with values.
const showQueryStatsSQL = `
SELECT
    schema_name AS 'db',
    digest_text AS 'query',
    count_star AS 'execution_count',
    ROUND(sum_timer_wait / 1000000000, 2) AS 'total_latency_ms',
    ROUND(avg_timer_wait / 1000000000, 2) AS 'average_latency_ms',
    ROUND(max_timer_wait / 1000000000, 2) AS 'max_latency_ms',
    sum_rows_sent AS 'total_rows_sent',
    sum_rows_examined AS 'total_rows_examined',
    sum_no_index_used AS 'full_table_scan_count',
    sum_no_good_index_used AS 'inefficient_index_used_count',
    last_seen AS 'last_executed'
FROM performance_schema.events_statements_summary_by_digest
WHERE schema_name NOT IN ('information_schema', 'performance_schema', 'mysql', 'sys')
AND (schema_name = COALESCE(NULLIF(?, ''), NULLIF(DATABASE(), '')) OR COALESCE(NULLIF(?, ''), NULLIF(DATABASE(), '')) IS NULL)
ORDER BY sum_timer_wait DESC
LIMIT ?;
`

type showQueryStatsArgs struct {
	SourceArg
	TableSchema string `json:"table_schema,omitempty" jsonschema:"(Optional) The database where query statistics is to be executed. Check all queries visible to the current user if not specified"`
	Limit       *int   `json:"limit,omitempty" jsonschema:"(Optional) Max rows to return, default is 10"`
}

func addShowQueryStats(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "show_query_stats",
		Description: "Read-only. Show the most expensive statement digests (normalized statement text without literal values, execution " +
			"count, latency, rows examined) from performance_schema.events_statements_summary_by_digest. " + untrusted,
		Annotations: readOnly(),
	}, instrument(d, "show_query_stats", func(ctx context.Context, in showQueryStatsArgs) (source.Result, *source.Result, error) {
		if err := checkSchema(d, in.Source, in.TableSchema); err != nil {
			return source.Result{}, nil, err
		}
		if err := requirePerfSchema(ctx, d, in.Source); err != nil {
			return source.Result{}, nil, err
		}
		res, err := run(ctx, d, in.Source, showQueryStatsSQL,
			in.TableSchema, in.TableSchema, clampLimit(in.Limit, 10, d.MaxRows))
		if err != nil {
			return source.Result{}, nil, err
		}
		// digest_text is already normalized; redacting it again changes nothing
		// but keeps a literal that slipped through (or a cut digest) out.
		redactColumn(res, "query")
		return *res, res, nil
	}))
}
