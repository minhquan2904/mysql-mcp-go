// Ported from mcp-toolbox@0a7c1b1c110cc97ea9652d2fb1a7a7476b4be988 internal/tools/mysql/mysqllisttablefragmentation (Apache-2.0)

package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

// table_schema and table_name are aliased: without it MySQL 8 reports the
// upper-case information_schema column names.
const listTableFragmentationSQL = `
    SELECT
        table_schema AS table_schema,
        table_name AS table_name,
        data_length AS data_size,
        index_length AS index_size,
        data_free AS data_free,
        ROUND((data_free / (data_length + index_length)) * 100, 2) AS fragmentation_percentage
    FROM
        information_schema.tables
    WHERE
        table_schema NOT IN ('sys', 'performance_schema', 'mysql', 'information_schema')
        AND (COALESCE(?, '') = '' OR table_schema = ?)
        AND (COALESCE(?, '') = '' OR table_name = ?)
        AND data_free >= ?
    ORDER BY
        fragmentation_percentage DESC,
        table_schema,
        table_name
    LIMIT ?;
`

type listTableFragmentationArgs struct {
	SourceArg
	TableSchema            string `json:"table_schema,omitempty" jsonschema:"(Optional) The database where fragmentation check is to be executed. Check all tables visible to the current user if not specified"`
	TableName              string `json:"table_name,omitempty" jsonschema:"(Optional) Name of the table to be checked. Check all tables visible to the current user if not specified."`
	DataFreeThresholdBytes *int   `json:"data_free_threshold_bytes,omitempty" jsonschema:"(Optional) Only show tables with at least this much free space in bytes. Default is 1"`
	Limit                  *int   `json:"limit,omitempty" jsonschema:"(Optional) Max rows to return, default is 10"`
}

func addListTableFragmentation(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_table_fragmentation",
		Description: "Read-only. List tables with free space (fragmentation) in their data files, most fragmented first, " +
			"from information_schema.tables. " + untrusted,
		Annotations: readOnly(),
	}, instrument(d, "list_table_fragmentation", func(ctx context.Context, in listTableFragmentationArgs) (source.Result, *source.Result, error) {
		threshold := 1
		if in.DataFreeThresholdBytes != nil {
			threshold = max(*in.DataFreeThresholdBytes, 0)
		}
		res, err := run(ctx, d, in.Source, listTableFragmentationSQL,
			in.TableSchema, in.TableSchema, in.TableName, in.TableName, threshold, clampLimit(in.Limit, 10, d.MaxRows))
		if err != nil {
			return source.Result{}, nil, err
		}
		return *res, res, nil
	}))
}
