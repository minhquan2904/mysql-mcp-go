package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/guard"
	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

type sqlArgs struct {
	SourceArg
	SQL string `json:"sql" jsonschema:"a single read-only statement: SELECT, WITH, SHOW, DESCRIBE or EXPLAIN"`
}

func addExecuteSQL(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "execute_sql",
		Description: "Read-only. Run one SELECT, WITH, SHOW, DESCRIBE or EXPLAIN statement and return columns and rows " +
			"(capped by row, cell and total size; `truncated` says when). Anything that could write is rejected. " + untrusted,
		Annotations: readOnly(),
	}, instrument(d, "execute_sql", func(ctx context.Context, in sqlArgs) (source.Result, *source.Result, error) {
		if err := guard.Check(in.SQL); err != nil {
			return source.Result{}, nil, err
		}
		res, err := run(ctx, d, in.Source, in.SQL)
		if err != nil {
			return source.Result{}, nil, err
		}
		return *res, res, nil
	}))
}
