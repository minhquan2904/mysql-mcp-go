package tools

import "github.com/modelcontextprotocol/go-sdk/mcp"

// Register adds every tool to s.
func Register(s *mcp.Server, d Deps) {
	addListSources(s, d)
	addExecuteSQL(s, d)
	addGetQueryPlan(s, d)
	addListTables(s, d)
	addListActiveQueries(s, d)
	addListAllLocks(s, d)
	addListTableFragmentation(s, d)
	addListTablesMissingUniqueIndexes(s, d)
	addListTableStats(s, d)
	addShowQueryStats(s, d)
}
