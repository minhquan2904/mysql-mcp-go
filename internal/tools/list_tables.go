// Ported from mcp-toolbox@0a7c1b1c110cc97ea9652d2fb1a7a7476b4be988 internal/tools/mysql/mysqllisttables (Apache-2.0)

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

// The statement keeps the toolbox's session variables (@table_names,
// @output_format): they are bound with ? and only live on the single-use
// connection, so they work under transaction_read_only=1.
const listTablesSQL = `
    SELECT
        T.TABLE_SCHEMA AS schema_name,
        T.TABLE_NAME AS object_name,
        CASE
            WHEN @output_format = 'simple' THEN
                JSON_OBJECT('name', T.TABLE_NAME)
            ELSE
                CONVERT(
                    JSON_OBJECT(
                        'schema_name', T.TABLE_SCHEMA,
                        'object_name', T.TABLE_NAME,
                        'object_type', 'TABLE',
                        'owner', (
                            SELECT
                                IFNULL(U.GRANTEE, 'N/A')
                            FROM
                                INFORMATION_SCHEMA.SCHEMA_PRIVILEGES U
                            WHERE
                                U.TABLE_SCHEMA = T.TABLE_SCHEMA
                            LIMIT 1
                        ),
                        'comment', IFNULL(T.TABLE_COMMENT, ''),
                        'columns', (
                            SELECT
                                IFNULL(
                                    JSON_ARRAYAGG(
                                        JSON_OBJECT(
                                            'column_name', C.COLUMN_NAME,
                                            'data_type', C.COLUMN_TYPE,
                                            'ordinal_position', C.ORDINAL_POSITION,
                                            'is_not_nullable', IF(C.IS_NULLABLE = 'NO', TRUE, FALSE),
                                            'column_default', C.COLUMN_DEFAULT,
                                            'column_comment', IFNULL(C.COLUMN_COMMENT, '')
                                        )
                                    ),
                                    JSON_ARRAY()
                                )
                            FROM
                                INFORMATION_SCHEMA.COLUMNS C
                            WHERE
                                C.TABLE_SCHEMA = T.TABLE_SCHEMA AND C.TABLE_NAME = T.TABLE_NAME
                            ORDER BY C.ORDINAL_POSITION
                        ),
                        'constraints', (
                            SELECT
                                IFNULL(
                                    JSON_ARRAYAGG(
                                        JSON_OBJECT(
                                            'constraint_name', TC.CONSTRAINT_NAME,
                                            'constraint_type',
                                            CASE TC.CONSTRAINT_TYPE
                                                WHEN 'PRIMARY KEY' THEN 'PRIMARY KEY'
                                                WHEN 'FOREIGN KEY' THEN 'FOREIGN KEY'
                                                WHEN 'UNIQUE' THEN 'UNIQUE'
                                                ELSE TC.CONSTRAINT_TYPE
                                            END,
                                            'constraint_definition', '',
                                            'constraint_columns', (
                                                SELECT
                                                    IFNULL(JSON_ARRAYAGG(KCU.COLUMN_NAME), JSON_ARRAY())
                                                FROM
                                                    INFORMATION_SCHEMA.KEY_COLUMN_USAGE KCU
                                                WHERE
                                                    KCU.CONSTRAINT_SCHEMA = TC.CONSTRAINT_SCHEMA
                                                    AND KCU.CONSTRAINT_NAME = TC.CONSTRAINT_NAME
                                                    AND KCU.TABLE_NAME = TC.TABLE_NAME
                                                ORDER BY KCU.ORDINAL_POSITION
                                            ),
                                            'foreign_key_referenced_table', IF(TC.CONSTRAINT_TYPE = 'FOREIGN KEY', RC.REFERENCED_TABLE_NAME, NULL),
                                            'foreign_key_referenced_columns', IF(TC.CONSTRAINT_TYPE = 'FOREIGN KEY',
                                                (SELECT IFNULL(JSON_ARRAYAGG(FKCU.REFERENCED_COLUMN_NAME), JSON_ARRAY())
                                                FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE FKCU
                                                WHERE FKCU.CONSTRAINT_SCHEMA = TC.CONSTRAINT_SCHEMA
                                                    AND FKCU.CONSTRAINT_NAME = TC.CONSTRAINT_NAME
                                                    AND FKCU.TABLE_NAME = TC.TABLE_NAME
                                                    AND FKCU.REFERENCED_TABLE_NAME IS NOT NULL
                                                ORDER BY FKCU.ORDINAL_POSITION),
                                                NULL
                                            )
                                        )
                                    ),
                                    JSON_ARRAY()
                                )
                            FROM
                                INFORMATION_SCHEMA.TABLE_CONSTRAINTS TC
                            LEFT JOIN
                                INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS RC
                                ON TC.CONSTRAINT_SCHEMA = RC.CONSTRAINT_SCHEMA
                                AND TC.CONSTRAINT_NAME = RC.CONSTRAINT_NAME
                                AND TC.TABLE_NAME = RC.TABLE_NAME
                            WHERE
                                TC.TABLE_SCHEMA = T.TABLE_SCHEMA AND TC.TABLE_NAME = T.TABLE_NAME
                        ),
                        'indexes', (
                            SELECT
                                IFNULL(
                                    JSON_ARRAYAGG(
                                        JSON_OBJECT(
                                            'index_name', IndexData.INDEX_NAME,
                                            'is_unique', IF(IndexData.NON_UNIQUE = 0, TRUE, FALSE),
                                            'is_primary', IF(IndexData.INDEX_NAME = 'PRIMARY', TRUE, FALSE),
                                            'index_columns', IFNULL(IndexData.INDEX_COLUMNS_ARRAY, JSON_ARRAY())
                                        )
                                    ),
                                    JSON_ARRAY()
                                )
                            FROM (
                                SELECT
                                    S.TABLE_SCHEMA,
                                    S.TABLE_NAME,
                                    S.INDEX_NAME,
                                    MIN(S.NON_UNIQUE) AS NON_UNIQUE,
                                    JSON_ARRAYAGG(S.COLUMN_NAME) AS INDEX_COLUMNS_ARRAY
                                FROM
                                    INFORMATION_SCHEMA.STATISTICS S
                                GROUP BY
                                    S.TABLE_SCHEMA, S.TABLE_NAME, S.INDEX_NAME
                            ) AS IndexData
                            WHERE IndexData.TABLE_SCHEMA = T.TABLE_SCHEMA AND IndexData.TABLE_NAME = T.TABLE_NAME
                            ORDER BY IndexData.INDEX_NAME
                        ),
                        'triggers', (
                            SELECT
                                IFNULL(
                                    JSON_ARRAYAGG(
                                        JSON_OBJECT(
                                            'trigger_name', TR.TRIGGER_NAME,
                                            'trigger_definition', TR.ACTION_STATEMENT
                                        )
                                    ),
                                    JSON_ARRAY()
                                )
                            FROM
                                INFORMATION_SCHEMA.TRIGGERS TR
                            WHERE
                                TR.EVENT_OBJECT_SCHEMA = T.TABLE_SCHEMA AND TR.EVENT_OBJECT_TABLE = T.TABLE_NAME
                            ORDER BY TR.TRIGGER_NAME
                        )
                    )
                USING utf8mb4)
        END AS object_details
    FROM
        INFORMATION_SCHEMA.TABLES T
    CROSS JOIN (SELECT @table_names := ?, @output_format := ?) AS variables
    WHERE
        T.TABLE_SCHEMA NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
        AND (NULLIF(TRIM(@table_names), '') IS NULL OR FIND_IN_SET(T.TABLE_NAME, @table_names))
        AND T.TABLE_TYPE = 'BASE TABLE'
    ORDER BY
        T.TABLE_SCHEMA, T.TABLE_NAME;
`

type listTablesArgs struct {
	SourceArg
	TableNames   string `json:"table_names,omitempty" jsonschema:"Optional: A comma-separated list of table names. If empty, details for all tables will be listed."`
	OutputFormat string `json:"output_format,omitempty" jsonschema:"Optional: Use 'simple' for names only or 'detailed' for full info."`
}

func addListTables(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_tables",
		Description: "Read-only. List the base tables of the connected databases with their columns, constraints, indexes and triggers " +
			"('detailed', the default) or just their names ('simple'). Optionally limited to a comma-separated list of table names. " +
			"object_details is a JSON object. " + untrusted,
		Annotations: readOnly(),
	}, instrument(d, "list_tables", func(ctx context.Context, in listTablesArgs) (source.Result, *source.Result, error) {
		format := in.OutputFormat
		if format == "" {
			format = "detailed"
		}
		if format != "simple" && format != "detailed" {
			return source.Result{}, nil, userError{fmt.Errorf("invalid value for output_format: must be 'simple' or 'detailed', but got %q", format)}
		}
		res, err := run(ctx, d, in.Source, listTablesSQL, in.TableNames, format)
		if err != nil {
			return source.Result{}, nil, err
		}
		parseObjectDetails(res)
		return *res, res, nil
	}))
}

// parseObjectDetails turns the object_details text of every row into a JSON
// object. Text that does not parse (a cell cut by the size cap) is left as is.
func parseObjectDetails(res *source.Result) {
	for _, row := range res.Rows {
		s, ok := row["object_details"].(string)
		if !ok {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader([]byte(s)))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil || obj == nil || dec.More() {
			continue
		}
		row["object_details"] = obj
	}
}
