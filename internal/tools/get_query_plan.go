package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/guard"
	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

type planOut struct {
	Plan map[string]any `json:"plan"`
}

func addGetQueryPlan(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_query_plan",
		Description: "Read-only. Return the MySQL execution plan (EXPLAIN FORMAT=JSON) of one SELECT or WITH statement " +
			"without running it. " + untrusted,
		Annotations: readOnly(),
	}, instrument(d, "get_query_plan", func(ctx context.Context, in sqlArgs) (planOut, *source.Result, error) {
		if err := guard.Check(in.SQL); err != nil {
			return planOut{}, nil, err
		}
		if !guard.StartsWithSelect(in.SQL) {
			return planOut{}, nil, userError{errors.New("only SELECT statements can be explained")}
		}
		res, err := run(ctx, d, in.Source, "EXPLAIN FORMAT=JSON "+in.SQL)
		if err != nil {
			return planOut{}, nil, err
		}
		plan, err := planFrom(res)
		if err != nil {
			return planOut{}, res, err
		}
		return planOut{Plan: plan}, res, nil
	}))
}

// planFrom extracts the JSON document from the first column of the first row.
// The result is never nil: a nil map would fail the tool's output schema.
func planFrom(res *source.Result) (map[string]any, error) {
	if res.Truncated {
		return nil, userError{errors.New("query plan is too large to return")}
	}
	if len(res.Columns) == 0 || len(res.Rows) == 0 {
		return nil, userError{errors.New("unexpected EXPLAIN output: no rows")}
	}
	switch v := res.Rows[0][res.Columns[0]].(type) {
	case map[string]any: // collect already decoded a JSON-typed column
		if v == nil {
			v = map[string]any{}
		}
		return v, nil
	case string:
		dec := json.NewDecoder(strings.NewReader(v))
		dec.UseNumber()
		plan := map[string]any{}
		if err := dec.Decode(&plan); err != nil {
			return nil, userError{fmt.Errorf("unexpected EXPLAIN output: %w", err)}
		}
		return plan, nil
	default:
		return nil, userError{fmt.Errorf("unexpected EXPLAIN output: first column is %T", v)}
	}
}
