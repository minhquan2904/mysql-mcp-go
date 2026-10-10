#!/bin/bash
set -euo pipefail
[[ "$MCP_RO_PASSWORD" =~ ^[A-Za-z0-9_-]{16,}$ ]] || { echo "MCP_RO_PASSWORD invalid" >&2; exit 1; }
export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
mysql -uroot <<SQL
CREATE USER 'mcp_ro'@'%' IDENTIFIED BY '${MCP_RO_PASSWORD}' WITH MAX_USER_CONNECTIONS 20;
GRANT SELECT, SHOW VIEW ON testdb.* TO 'mcp_ro'@'%';
GRANT PROCESS ON *.* TO 'mcp_ro'@'%';
-- performance_schema tables: added per tool in phase 6 (each line: GRANT SELECT ON performance_schema.<t>)
SQL
