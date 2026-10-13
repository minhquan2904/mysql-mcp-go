# mysql-mcp-go

A **read-only** MySQL [MCP](https://modelcontextprotocol.io) server written in Go with the official
[go-sdk](https://github.com/modelcontextprotocol/go-sdk), inspired by the MySQL tools of
[googleapis/mcp-toolbox](https://github.com/googleapis/mcp-toolbox). It runs with Docker Compose and
exposes a Streamable HTTP endpoint that requires a bearer token.

> **Status:** the server and all 10 tools are implemented. Read-only enforcement, benchmarks and the
> generated reports below are in place; see [Dev loop](#dev-loop).

## What it is
An agent can query your MySQL databases (SELECT/SHOW/DESCRIBE/EXPLAIN only) over MCP. Nothing can
write. It is deliberately close to the toolbox MySQL tool set, but read-only is **enforced in depth**
rather than merely hinted.

- Transport: MCP over Streamable HTTP at `POST /mcp` (JSON response, stateless).
- Auth: `Authorization: Bearer $MCP_AUTH_TOKEN` is required on `/mcp`.
- `GET /healthz` is unauthenticated and returns `{"status":"ok"}` without revealing source names.

## Read-only defense layers
1. **SQL guard** (`internal/guard`): a MySQL-accurate lexer plus statement allowlist and
   dangerous-function denylist. Only `SELECT`, `WITH`, `SHOW`, `DESCRIBE`, `DESC`, `EXPLAIN` (and a
   leading `(`) are accepted; `INTO OUTFILE/DUMPFILE`, `FOR UPDATE`, locking functions, `LOAD_FILE`,
   `BENCHMARK`, etc. are rejected. It is lexed twice (backslash escapes on and off) and both must pass.
2. **Read-only session**: the DSN forces `transaction_read_only=1` and every query runs in a
   `READ ONLY` transaction that is always rolled back.
3. **Single-use connection**: each query gets its own connection which is discarded afterwards, so no
   locks, user variables or temp tables can leak between calls.
4. **Timeout + `KILL QUERY`**: a server-side `max_execution_time`, a client timeout, and a synchronous
   `KILL QUERY` on deadline or cancellation.
5. **Caps**: row, cell, response-size, concurrency and HTTP body limits (see env table).
6. **Minimal grants**: one schema plus specific `performance_schema` tables — never `*.*` or `mysql.*`.
7. **Startup privilege self-check**: on boot the server reads `SHOW GRANTS` per source and refuses to
   start if the configured user can write.

### Residual risks
- **`SQL SECURITY DEFINER` views/procedures** run with the definer's rights. The seed's `v_definer`
  is rejected, but a DEFINER object on a real database can still have non-transactional side effects.
- **`PROCESS` privilege** (needed by `list_active_queries`) exposes other sessions' SQL; the tool
  redacts literals and truncates the text (see `internal/redact`), but the normalized statement shape
  is still visible.

## Configuration
All settings come from environment variables.

| Variable | Default | Purpose |
|---|---|---|
| `MYSQL_DSN_<NAME>` | — (at least 1) | One MySQL source per variable. `<NAME>` matches `[a-z0-9_]+` (case-insensitive). |
| `MCP_AUTH_TOKEN` | — **required** | Bearer token for `/mcp`. ≥32 chars, not the example value. |
| `MCP_RO_PASSWORD` | — (compose) | Password of the read-only MySQL user (goes into the DSN and the init script). |
| `MYSQL_ROOT_PASSWORD` | — (compose) | Root password for the bundled test MySQL. |
| `MAX_ROWS` | 1000 | Max rows per result; extra rows set `truncated: true`. |
| `MAX_CELL_BYTES` | 65536 | Longer cell values are cut with `…[truncated]`. |
| `MAX_RESULT_BYTES` | 4194304 | Total response budget; exceeding it stops reading, `truncated: true`. |
| `MAX_CONCURRENT_QUERIES` | 8 | Server-wide query semaphore. |
| `QUERY_TIMEOUT` | `30s` | Per-query timeout (duration, `1ms`–`1h`); also drives `max_execution_time`. |
| `ALLOWED_ORIGINS` | empty | Comma-separated `scheme://host[:port]` origins. A request carrying an `Origin` not in the list is rejected with 403. |
| `ALLOW_UNSAFE_PRIVILEGES` | `false` | Skip the privilege self-check (not recommended). |
| `LISTEN_ADDR` | `:8080` | Listen address inside the container. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `BIND_IP` | `127.0.0.1` | Compose port-publish address for `8080`. Set to a private/VPN address to expose beyond localhost. |

## Quick start
```bash
cp .env.example .env
# Generate the secrets into .env:
#   MYSQL_ROOT_PASSWORD / MCP_RO_PASSWORD: openssl rand -hex 16
#   MCP_AUTH_TOKEN:                         openssl rand -hex 32
docker compose up -d --build
curl 127.0.0.1:8080/healthz
# {"status":"ok"}
```

## Connecting Claude Code
```bash
claude mcp add --transport http mysql-ro http://<host>:8080/mcp \
  --header "Authorization: Bearer <token>"
```
Replace `<host>` with where the service is reachable (loopback, or your private/VPN address) and
`<token>` with your `MCP_AUTH_TOKEN`.

Manual smoke check (real client):
- Ask "list the tables in testdb" → works.
- Ask "delete customer 1" → rejected.

## Tools
All 10 tools are annotated `readOnlyHint: true`. Every tool takes an optional `source` argument
(required when more than one source is configured).

| Tool | Description |
|---|---|
| `execute_sql` | Run one read-only statement (`sql`) and return columns and rows. |
| `list_tables` | List base tables with columns, constraints, indexes and triggers (`table_names`, `output_format`). |
| `get_query_plan` | Return `EXPLAIN FORMAT=JSON` for one `SELECT`/`WITH` statement (`sql`). |
| `list_active_queries` | List running queries and open transactions across sessions (`min_duration_secs`, `limit`). |
| `list_all_locks` | List held/waited-for InnoDB locks with owning transactions (`table_schema`, `table_name`, `limit`). |
| `list_table_fragmentation` | List tables with free space in their data files (`table_schema`, `table_name`, `data_free_threshold_bytes`, `limit`). |
| `list_tables_missing_unique_indexes` | List base tables with no primary key or unique constraint (`table_schema`, `limit`). |
| `list_table_stats` | Per-table size and I/O statistics (`table_schema`, `table_name`, `sort_by`, `limit`). |
| `show_query_stats` | Most expensive statement digests (`table_schema`, `limit`). |
| `list_sources` | List the configured source names (never connection strings). |

### Differences from mcp-toolbox
- The SQL argument is named `sql` (the toolbox uses `sql_statement`).
- `list_table_stats` is rewritten: the toolbox reads `sys.x$schema_table_statistics` (needs the `sys`
  schema); ours reads `performance_schema.table_io_waits_summary_by_table` joined with
  `information_schema.TABLES` instead. Column names stay close to the toolbox.
- `show_query_stats` returns only `DIGEST_TEXT` (normalized statements), never `QUERY_SAMPLE_TEXT`.
- `list_active_queries` redacts literals and truncates query text before returning it.

## Adding a real database
1. Create a dedicated read-only user using the template in `deploy/mysql/init/02-readonly-user.sh`:
   grant `SELECT` (and `SHOW VIEW` if needed) on **one schema**, plus `PROCESS` and `SELECT` on the
   specific `performance_schema` tables the tools read. Never grant `*.*` or anything on `mysql.*`.
   The template also caps the account with `MAX_USER_CONNECTIONS`.
2. Set `MYSQL_DSN_<NAME>` to that user's DSN.
3. On startup the server self-checks `SHOW GRANTS` and refuses to run with a user that can write.

## HTTPS
The binary serves plain HTTP. Terminate TLS in front of it, e.g. with Tailscale:
```bash
tailscale serve --https=443 http://127.0.0.1:8080
```
Point Claude Code at the HTTPS address. Otherwise keep the service on loopback or a private network.

## Dev loop
You need a Linux host with Docker; Go is not required locally. The host is described generically —
put its connection details in the untracked `.dev.env` file (`VM=user@your-docker-host`).

```bash
scripts/dev.sh sync          # copy the tree to the Docker host
scripts/dev.sh go <args>     # run Go in a container on the host
scripts/dev.sh up            # start the compose stack
scripts/dev.sh down          # stop it
scripts/dev.sh stop          # stop without removing
scripts/dev.sh logs          # tail service logs
scripts/dev.sh test          # unit tests
scripts/dev.sh it            # integration tests against the compose MySQL (stops mysql after)
scripts/dev.sh e2e           # end-to-end HTTP checks (starts the stack if it is down)
scripts/dev.sh vuln          # govulncheck
scripts/dev.sh fuzz          # fuzz tests
scripts/dev.sh bench         # benchmark run (writes bench results)
scripts/dev.sh test-report   # regenerate the reports below
```

## Benchmarks

<!-- BENCH:START -->
| Metric | Value |
|---|---|
| Image size | 4.53 MB (binary 9.1 MB) |
| Cold start → healthy (p50 / p95) | 7 ms / 9 ms |
| Idle RSS / peak RSS | 3.5 MB / 28.2 MB |
| execute_sql SELECT 1 @ c=8 (p50 / p99 / RPS) | 7.4 ms / 33.0 ms / 760.6 rps (38.1% of calls failed) |
| RunReadOnly overhead vs direct query | +0.3 ms (single-use conn) |
| Guard check (1KB SQL) | 31.8 µs |

Load, every cell (RPS / p50 / errors):

| Scenario | c=1 | c=8 | c=32 |
|---|---|---|---|
| S1 list_sources | 2690.7 rps / 0.3 ms / 0 err | 3933.8 rps / 1.8 ms / 0 err | 2884.0 rps / 10.6 ms / 0 err |
| S2 execute_sql SELECT 1 | 654.0 rps / 1.3 ms / 0 err | 760.6 rps / 7.4 ms / 8706 err (38.1%) | 890.2 rps / 31.3 ms / 4126 err (15.4%) |
| S3 execute_sql SELECT * FROM orders | 419.4 rps / 1.6 ms / 1464 err (11.6%) | 755.7 rps / 9.9 ms / 1741 err (7.7%) | 783.2 rps / 27.3 ms / 5096 err (21.7%) |
| S4 execute_sql 1000 rows (order_items cross join) | 66.5 rps / 14.8 ms / 0 err | 223.6 rps / 35.1 ms / 0 err | 231.4 rps / 137.3 ms / 0 err |
| S5 list_tables detailed | 47.7 rps / 19.2 ms / 0 err | 159.0 rps / 49.2 ms / 0 err | 158.5 rps / 201.0 ms / 0 err |
| S6 execute_sql DELETE (guard rejection) | 2615.3 rps / 0.3 ms / 0 err | 3802.7 rps / 1.9 ms / 0 err | 2892.3 rps / 10.6 ms / 0 err |

Measured on: 4 vCPU Intel(R) Core(TM) Ultra 7 265KF, 8.3 GB, MySQL 8.4.11 (same host), commit a48e5a2ac7fbd82d922c6d25f812cb798cf9f84b, 2026-10-13T06:44:03Z. Full report: docs/benchmarks.md

<!-- BENCH:END -->

## Tests

<!-- TESTS:START -->
| Metric | Value |
|---|---|
| Unit tests | 650 pass / 0 fail / 0 skip |
| Coverage (total) | 58.1% |
| Integration tests | 699 pass / 0 fail / 0 skip |
| E2E | 12/12 |
| Fuzz (`FuzzCheck`, 10s) | 24499 executions |
| govulncheck | clean |

**Verdict: PASS** — full report: [docs/test-report.md](docs/test-report.md) · commit `a48e5a2ac7fbd82d922c6d25f812cb798cf9f84b` · 2026-10-13T07:14:35Z
<!-- TESTS:END -->

## Credits
The toolbox-equivalent tool SQL is ported from
[googleapis/mcp-toolbox](https://github.com/googleapis/mcp-toolbox) (Apache-2.0). See `NOTICE` for the
pinned upstream commit and the exact files that were ported.
