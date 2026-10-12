#!/bin/bash
set -euo pipefail
[[ "$MCP_RO_PASSWORD" =~ ^[A-Za-z0-9_-]{16,}$ ]] || { echo "MCP_RO_PASSWORD invalid" >&2; exit 1; }
export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
mysql -uroot <<SQL
CREATE USER 'mcp_ro'@'%' IDENTIFIED BY '${MCP_RO_PASSWORD}' WITH MAX_USER_CONNECTIONS 20;
GRANT SELECT, SHOW VIEW ON testdb.* TO 'mcp_ro'@'%';
GRANT PROCESS ON *.* TO 'mcp_ro'@'%';
-- performance_schema: exactly the tables the tools read, one table-level grant each.
-- list_all_locks (LOCK_DATA is never selected):
GRANT SELECT ON performance_schema.data_locks TO 'mcp_ro'@'%';
-- list_table_stats:
GRANT SELECT ON performance_schema.table_io_waits_summary_by_table TO 'mcp_ro'@'%';
-- show_query_stats (DIGEST_TEXT only; QUERY_SAMPLE_TEXT is never selected):
GRANT SELECT ON performance_schema.events_statements_summary_by_digest TO 'mcp_ro'@'%';
SQL
