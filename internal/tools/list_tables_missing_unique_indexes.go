// Ported from mcp-toolbox@0a7c1b1c110cc97ea9652d2fb1a7a7476b4be988 internal/tools/mysql/mysqllisttablesmissinguniqueindexes (Apache-2.0)

package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

const listTablesMissingUniqueIndexesSQL = `
    SELECT
        tab.table_schema AS table_schema,
        tab.table_name AS table_name
    FROM
        information_schema.tables tab
        LEFT JOIN
        information_schema.table_constraints tco
        ON
            tab.table_schema = tco.table_schema
            AND tab.table_name = tco.table_name
            AND tco.constraint_type IN ('PRIMARY KEY', 'UNIQUE')
    WHERE
        tco.constraint_type IS NULL
        AND tab.table_schema NOT IN('mysql', 'information_schema', 'performance_schema', 'sys')
        AND tab.table_type = 'BASE TABLE'
        AND (COALESCE(?, '') = '' OR tab.table_schema = ?)
    ORDER BY
        tab.table_schema,
        tab.table_name
    LIMIT ?;
`

type listTablesMissingUniqueIndexesArgs struct {
	SourceArg
	TableSchema string `json:"table_schema,omitempty" jsonschema:"(Optional) The database where the check is to be performed. Check all tables visible to the current user if not specified"`
	Limit       *int   `json:"limit,omitempty" jsonschema:"(Optional) Max rows to return, default is 50"`
}

func addListTablesMissingUniqueIndexes(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_tables_missing_unique_indexes",
		Description: "Read-only. List base tables that have neither a primary key nor a unique constraint, " +
			"from information_schema. " + untrusted,
		Annotations: readOnly(),
	}, instrument(d, "list_tables_missing_unique_indexes", func(ctx context.Context, in listTablesMissingUniqueIndexesArgs) (source.Result, *source.Result, error) {
		res, err := run(ctx, d, in.Source, listTablesMissingUniqueIndexesSQL,
			in.TableSchema, in.TableSchema, clampLimit(in.Limit, 50, d.MaxRows))
		if err != nil {
			return source.Result{}, nil, err
		}
		return *res, res, nil
	}))
}
