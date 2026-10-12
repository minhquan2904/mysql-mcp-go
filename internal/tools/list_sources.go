package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

type listSourcesOut struct {
	Sources []string `json:"sources"`
}

func addListSources(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sources",
		Description: "Read-only. List the names of the configured MySQL sources (never connection strings). " + untrusted,
		Annotations: readOnly(),
	}, instrument(d, "list_sources", func(ctx context.Context, _ noArgs) (listSourcesOut, *source.Result, error) {
		return listSourcesOut{Sources: append([]string{}, d.Reg.Names()...)}, nil, nil
	}))
}
