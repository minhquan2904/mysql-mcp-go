# mysql-mcp-go

A **read-only** MySQL [MCP](https://modelcontextprotocol.io) server written in Go with the official
[go-sdk](https://github.com/modelcontextprotocol/go-sdk), inspired by the MySQL tools of
[googleapis/mcp-toolbox](https://github.com/googleapis/mcp-toolbox). It runs with Docker Compose.

> **Status: work in progress.** Only the scaffold exists so far: dev loop, MySQL test database with seed data, and a
> minimal read-only user. The server, tools, benchmarks and full docs come in later commits.

## Goals
- MCP over Streamable HTTP (`/mcp`), with a required bearer token.
- 10 tools, close to parity with mcp-toolbox's MySQL tools: `execute_sql`, `list_tables`, `get_query_plan`,
  `list_active_queries`, `list_all_locks`, `list_table_fragmentation`, `list_tables_missing_unique_indexes`,
  `list_table_stats`, `show_query_stats`, and `list_sources`.
- Multiple databases via `MYSQL_DSN_<NAME>` environment variables.
- **Read-only enforced in depth**, not just hinted:
  1. A SQL guard: MySQL-accurate lexer, statement allowlist and dangerous-function denylist.
  2. Read-only session (`transaction_read_only=1`) plus a `READ ONLY` transaction that is always rolled back.
  3. A single-use connection per query, so no locks, variables or temp tables leak between calls.
  4. Server-side `max_execution_time`, a client timeout, and `KILL QUERY` on timeout or cancel.
  5. Row, cell, response-size and concurrency caps.
  6. Minimal grants: one schema plus specific `performance_schema` tables, never `*.*` or `mysql.*`.
  7. A startup privilege self-check that refuses to run with a user that can write.

## Layout
```
deploy/mysql/init/   # seed schema + read-only user for the test database
docker-compose.yml   # mysql:8.4 (+ test runner profile); MCP service added later
scripts/dev.sh       # dev loop: edit locally, run Go/Docker on a remote Docker host
```

## Dev loop
You need a Linux host with Docker. Go is not required locally; everything runs in containers.

```bash
echo 'VM=user@your-docker-host' > .dev.env   # untracked
scripts/dev.sh go version                     # sync + run go in a container on the host
scripts/dev.sh up                             # start the compose stack
scripts/dev.sh test                           # unit tests
scripts/dev.sh it                             # integration tests against the compose MySQL
```

On the Docker host, create `.env` from `.env.example` and generate the secrets:
```bash
cp .env.example .env
# MYSQL_ROOT_PASSWORD / MCP_RO_PASSWORD: openssl rand -hex 16
# MCP_AUTH_TOKEN:                         openssl rand -hex 32
```

## Credits
The SQL for the toolbox-equivalent tools will be ported from
[googleapis/mcp-toolbox](https://github.com/googleapis/mcp-toolbox) (Apache-2.0); attribution will be added with that code.
