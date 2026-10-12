// Ported from mcp-toolbox@0a7c1b1c110cc97ea9652d2fb1a7a7476b4be988 internal/tools/mysql/mysqllistalllocks (Apache-2.0)

package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

// The toolbox's MySQL 8+ statement. The statement text is not cut to 100
// characters in SQL; the handler redacts and cuts it (redactColumn).
// LOCK_DATA (key values) is not selected. MySQL 5.7 has no
// performance_schema.data_locks and is not supported.
const listAllLocksSQL = `
SELECT
  dl.thread_id AS thread_id,
  it.TRX_MYSQL_THREAD_ID AS process_id,
  dl.object_schema AS table_schema,
  dl.object_name AS table_name,
  dl.lock_type AS lock_type,
  dl.lock_mode AS lock_mode,
  dl.LOCK_STATUS AS lock_status,
  it.TRX_STATE AS transaction_state,
  it.TRX_OPERATION_STATE AS current_operation,
  LEFT(it.TRX_QUERY, 4096) AS query
FROM
  performance_schema.data_locks dl
INNER JOIN information_schema.innodb_trx it
  ON dl.ENGINE_TRANSACTION_ID = it.TRX_ID
WHERE
  (dl.object_schema = COALESCE(NULLIF(?, ''), NULLIF(DATABASE(), '')) OR COALESCE(NULLIF(?, ''), NULLIF(DATABASE(), '')) IS NULL)
  AND (COALESCE(?, '') = '' OR dl.object_name = ?)
ORDER BY TRX_STARTED
LIMIT ?;
`

type listAllLocksArgs struct {
	SourceArg
	TableSchema string `json:"table_schema,omitempty" jsonschema:"(Optional) The database where locked object is detected. Check all databases if not specified."`
	TableName   string `json:"table_name,omitempty" jsonschema:"(Optional) Name of the table to be checked. Check all tables visible to the current user if not specified."`
	Limit       *int   `json:"limit,omitempty" jsonschema:"(Optional) Max rows to return, default is 10"`
}

func addListAllLocks(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_all_locks",
		Description: "Read-only. List the InnoDB locks currently held or waited for, with the owning transaction, " +
			"from performance_schema.data_locks. Statement text is shown with literals replaced by ?. " + untrusted,
		Annotations: readOnly(),
	}, instrument(d, "list_all_locks", func(ctx context.Context, in listAllLocksArgs) (source.Result, *source.Result, error) {
		if err := checkSchema(d, in.Source, in.TableSchema); err != nil {
			return source.Result{}, nil, err
		}
		if err := requirePerfSchema(ctx, d, in.Source); err != nil {
			return source.Result{}, nil, err
		}
		res, err := run(ctx, d, in.Source, listAllLocksSQL,
			in.TableSchema, in.TableSchema, in.TableName, in.TableName, clampLimit(in.Limit, 10, d.MaxRows))
		if err != nil {
			return source.Result{}, nil, err
		}
		redactColumn(res, "query")
		return *res, res, nil
	}))
}
