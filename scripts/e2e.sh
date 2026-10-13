#!/usr/bin/env bash
# End-to-end check for the read-only MySQL MCP server. Runs on the Docker host
# from the repo root against the running stack (scripts/dev.sh up). Needs curl
# and jq on the host; without them run the whole script inside a throwaway
# container instead:
#
#   docker run --rm --network host -v "$PWD":/work -w /work \
#     alpine sh -c 'apk add -q curl jq bash >/dev/null && bash scripts/e2e.sh'
#
# Usage: scripts/e2e.sh [--json]
#   --json: also emit a machine-readable summary on stdout, one JSON array of
#   {"step": <n>, "name": <string>, "ok": <bool>}; human lines then go to
#   stderr so stdout carries only the JSON. Without --json the human lines go
#   to stdout and no JSON is printed.
#
# Exit: 0 all steps pass, 1 some step fails (JSON summary still printed),
# 2 setup problem (missing tool/secret, stack not running).

set -euo pipefail

JSON_OUT=false
for arg in "$@"; do
  case "$arg" in
    --json) JSON_OUT=true ;;
    *) echo "usage: scripts/e2e.sh [--json]" >&2; exit 2 ;;
  esac
done
HUMAN_FD=1
$JSON_OUT && HUMAN_FD=2

log() { printf '%s\n' "$*" >&"$HUMAN_FD"; }
die() { printf 'e2e: %s\n' "$*" >&2; exit 2; }

for tool in curl jq docker; do
  command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done
docker compose version >/dev/null 2>&1 || die "missing required tool: docker compose (v2 plugin)"

cd "$(dirname "$0")/.."
[[ -f .env ]] || die ".env not found in $(pwd); copy .env.example to .env and set the secrets"
# shellcheck disable=SC1091
source .env

[[ -n "${MCP_RO_PASSWORD:-}" ]] || die "MCP_RO_PASSWORD is empty; set it in .env"
[[ -n "${MCP_AUTH_TOKEN:-}" ]] || die "MCP_AUTH_TOKEN is empty; set it in .env"
[[ -n "${MYSQL_ROOT_PASSWORD:-}" ]] || die "MYSQL_ROOT_PASSWORD is empty; set it in .env"

U=http://127.0.0.1:8080
BODY_FILE=$(mktemp)
BIG_FILE=$(mktemp)
RESULTS_FILE=$(mktemp)
cleanup() {
  command -v docker >/dev/null 2>&1 && docker rm -f mcp2 >/dev/null 2>&1 || true
  rm -f "$BODY_FILE" "$BIG_FILE" "$RESULTS_FILE"
}
trap cleanup EXIT

# The stack must be up and healthy before step 1 asserts on it. Bring it up
# here rather than assume: `scripts/dev.sh it` stops mysql when it finishes, so
# an e2e run straight after integration tests would otherwise race the restart
# (503 on /healthz, "database unavailable" from the tools). Same pattern as
# scripts/test-report.sh.
docker compose up -d mysql mcp >/dev/null 2>&1 \
  || die "cannot start the stack (docker compose up -d mysql mcp)"
ready=false
for _ in $(seq 1 60); do
  if [[ "$(curl -sS -o /dev/null -w '%{http_code}' "$U/healthz" 2>/dev/null)" == 200 ]]; then
    ready=true
    break
  fi
  sleep 0.5
done
$ready || die "nothing healthy on $U after 30s; start the stack first (scripts/dev.sh up)"

# ---- helpers ---------------------------------------------------------------

# rpc <json>: one plain POST per call (stateless + JSONResponse: no session id).
rpc_at() { # <base-url> <json>
  curl -sS -X POST "$1/mcp" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -H "Authorization: Bearer $MCP_AUTH_TOKEN" \
    -d "$2"
}
rpc() { rpc_at "$U" "$1"; }

tool_body() { # <tool> <arguments-json> -> JSON-RPC tools/call body
  jq -cn --arg name "$1" --argjson args "$2" \
    '{jsonrpc:"2.0",id:1,method:"tools/call",params:{name:$name,arguments:$args}}'
}

is_err() { jq -e '.result.isError == true' >/dev/null 2>&1 <<<"$1"; }
err_text() { jq -r '.result.content[0].text // ""' <<<"$1"; }
# Tool output: structuredContent, falling back to the JSON in content[0].text.
result_json() {
  jq -c '(.result.structuredContent // (.result.content[0].text | fromjson))' <<<"$1"
}

wait_healthz() { # <base-url> <seconds>
  local url=$1 limit=$(( $2 * 2 )) i=0
  while (( i < limit )); do
    if [[ "$(curl -sS -o /dev/null -w '%{http_code}' "$url/healthz" 2>/dev/null)" == 200 ]]; then
      return 0
    fi
    sleep 0.5
    i=$(( i + 1 ))
  done
  return 1
}

# ---- steps -----------------------------------------------------------------

step_healthz() {
  local code
  code=$(curl -sS -o "$BODY_FILE" -w '%{http_code}' "$U/healthz") \
    || { log "  healthz request failed"; return 1; }
  [[ "$code" == 200 ]] || { log "  HTTP $code, want 200"; return 1; }
  jq -e '. == {"status":"ok"}' "$BODY_FILE" >/dev/null \
    || { log "  body is not {\"status\":\"ok\"}"; return 1; }
  if grep -q 'testdb' "$BODY_FILE"; then
    log "  body leaked a source name"
    return 1
  fi
  return 0
}

step_auth_rejections() {
  local body='{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' code
  # 1. no token -> 401
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$U/mcp" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d "$body") || { log "  no-token request failed"; return 1; }
  [[ "$code" == 401 ]] || { log "  no token: HTTP $code, want 401"; return 1; }
  # 2. wrong token -> 401
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$U/mcp" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H 'Authorization: Bearer wrong-token' -d "$body") || { log "  wrong-token request failed"; return 1; }
  [[ "$code" == 401 ]] || { log "  wrong token: HTTP $code, want 401"; return 1; }
  # 3. disallowed browser Origin with a valid token -> 403 ("null" is the
  #    opaque origin and can never be in ALLOWED_ORIGINS, which allows only
  #    http(s)://host[:port])
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$U/mcp" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H "Authorization: Bearer $MCP_AUTH_TOKEN" -H 'Origin: null' -d "$body") \
    || { log "  bad-origin request failed"; return 1; }
  [[ "$code" == 403 ]] || { log "  disallowed origin: HTTP $code, want 403"; return 1; }
  # 4. 2MB body -> 413 (the server caps request bodies at 1MB)
  head -c 2097152 /dev/zero >"$BIG_FILE"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$U/mcp" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H "Authorization: Bearer $MCP_AUTH_TOKEN" --data-binary "@$BIG_FILE") \
    || { log "  large-body request failed"; return 1; }
  [[ "$code" == 413 ]] || { log "  2MB body: HTTP $code, want 413"; return 1; }
  return 0
}

step_tools_list() {
  local resp n
  resp=$(rpc '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}') \
    || { log "  tools/list request failed"; return 1; }
  n=$(jq -r '.result.tools | length' <<<"$resp" 2>/dev/null) \
    || { log "  unparseable tools/list response"; return 1; }
  [[ "$n" == 10 ]] || { log "  got $n tools, want 10"; return 1; }
  jq -e '[.result.tools[].annotations.readOnlyHint] | all' <<<"$resp" >/dev/null \
    || { log "  not every tool has annotations.readOnlyHint"; return 1; }
  return 0
}

count_customers() { # sets $count; returns 1 when the call fails
  local resp
  resp=$(rpc "$(tool_body execute_sql '{"sql":"SELECT COUNT(*) c FROM customers"}')") || return 1
  is_err "$resp" && { log "  execute_sql errored: $(err_text "$resp")"; return 1; }
  count=$(result_json "$resp" | jq -r '.rows[0].c | tonumber' 2>/dev/null) || return 1
  return 0
}

step_select_count() {
  local count
  count_customers || { log "  COUNT(*) call failed"; return 1; }
  [[ "$count" == 20 ]] || { log "  COUNT(*) = $count, want 20"; return 1; }
  return 0
}

step_delete_rejected() {
  local resp count
  resp=$(rpc "$(tool_body execute_sql '{"sql":"DELETE FROM customers"}')") \
    || { log "  DELETE request failed"; return 1; }
  is_err "$resp" || { log "  DELETE was not rejected"; return 1; }
  count_customers || { log "  COUNT(*) after DELETE failed"; return 1; }
  [[ "$count" == 20 ]] || { log "  COUNT(*) after DELETE = $count, want 20"; return 1; }
  return 0
}

step_list_tables() {
  local resp
  resp=$(rpc "$(tool_body list_tables '{"output_format":"simple"}')") \
    || { log "  list_tables request failed"; return 1; }
  is_err "$resp" && { log "  list_tables errored: $(err_text "$resp")"; return 1; }
  result_json "$resp" | jq -e '.rows | tostring | contains("\"orders\"")' >/dev/null \
    || { log "  listing has no orders table"; return 1; }
  return 0
}

step_query_timeout() {
  local keep=${QUERY_TIMEOUT:-30s} rc=0 elapsed
  QUERY_TIMEOUT=2s docker compose up -d mcp >/dev/null 2>&1 \
    || { log "  cannot restart mcp with QUERY_TIMEOUT=2s"; return 1; }
  if ! wait_healthz "$U" 30; then
    log "  mcp not healthy after restart"
    QUERY_TIMEOUT="$keep" docker compose up -d mcp >/dev/null 2>&1 || true
    return 1
  fi
  elapsed=$(curl -sS -o "$BODY_FILE" -w '%{time_total}' -X POST "$U/mcp" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H "Authorization: Bearer $MCP_AUTH_TOKEN" \
    -d "$(tool_body execute_sql '{"sql":"SELECT SLEEP(30)"}')") || rc=1
  if (( rc == 0 )); then
    is_err "$(cat "$BODY_FILE")" || { log "  SLEEP(30) was not an error"; rc=1; }
  fi
  if (( rc == 0 )) && ! jq -ne --argjson t "$elapsed" '$t <= 4' >/dev/null 2>&1; then
    log "  timeout after ${elapsed}s, want <= 4s"
    rc=1
  fi
  QUERY_TIMEOUT="$keep" docker compose up -d mcp >/dev/null 2>&1 \
    || { log "  cannot restore QUERY_TIMEOUT=$keep"; rc=1; }
  return "$rc"
}

step_no_secrets_in_logs() {
  local logs
  logs=$(docker compose logs mcp 2>&1) || { log "  cannot read mcp logs"; return 1; }
  if grep -qF -e "$MCP_RO_PASSWORD" -e "$MCP_AUTH_TOKEN" <<<"$logs"; then
    log "  secrets found in mcp logs"
    return 1
  fi
  return 0
}

step_no_dsn_refused() {
  local out rc=0
  out=$(docker run --rm -e "MCP_AUTH_TOKEN=$MCP_AUTH_TOKEN" mysql-toolbox-go 2>&1) || rc=$?
  (( rc != 0 )) || { log "  server started without any MYSQL_DSN"; return 1; }
  grep -q 'no MYSQL_DSN' <<<"$out" \
    || { log "  output does not mention no MYSQL_DSN"; return 1; }
  return 0
}

step_short_token_refused() {
  local out rc=0
  out=$(docker run --rm -e MCP_AUTH_TOKEN=short \
    -e 'MYSQL_DSN_X=u:p@tcp(127.0.0.1:3306)/testdb' mysql-toolbox-go 2>&1) || rc=$?
  (( rc != 0 )) || { log "  server started with a 5-character token"; return 1; }
  grep -q 'MCP_AUTH_TOKEN' <<<"$out" \
    || { log "  output does not mention MCP_AUTH_TOKEN"; return 1; }
  return 0
}

step_root_dsn_refused() {
  local out rc=0
  out=$(docker compose run --rm \
    -e "MYSQL_DSN_TESTDB=root:$MYSQL_ROOT_PASSWORD@tcp(mysql:3306)/testdb" mcp 2>&1) || rc=$?
  (( rc != 0 )) || { log "  server accepted the root DSN"; return 1; }
  grep -q 'privilege' <<<"$out" \
    || { log "  output does not mention privilege"; return 1; }
  return 0
}

step_multi_source() {
  local u2=http://127.0.0.1:18080 resp
  docker rm -f mcp2 >/dev/null 2>&1 || true
  docker compose run -d --name mcp2 -p 127.0.0.1:18080:8080 \
    -e "MYSQL_DSN_COPY=mcp_ro:${MCP_RO_PASSWORD}@tcp(mysql:3306)/testdb" mcp >/dev/null 2>&1 \
    || { log "  cannot start mcp2"; return 1; }
  wait_healthz "$u2" 30 || { log "  mcp2 not healthy"; return 1; }
  # Two sources configured: an ambiguous execute_sql must name both.
  resp=$(rpc_at "$u2" "$(tool_body execute_sql '{"sql":"SELECT 1 AS ok"}')") \
    || { log "  execute_sql request failed"; return 1; }
  is_err "$resp" || { log "  execute_sql without source did not fail"; return 1; }
  err_text "$resp" | grep -q 'copy, testdb' \
    || { log "  error does not list 'copy, testdb'"; return 1; }
  resp=$(rpc_at "$u2" "$(tool_body list_sources '{}')") \
    || { log "  list_sources request failed"; return 1; }
  is_err "$resp" && { log "  list_sources errored: $(err_text "$resp")"; return 1; }
  result_json "$resp" | jq -e '.sources == ["copy","testdb"]' >/dev/null \
    || { log "  sources are not [copy,testdb]"; return 1; }
  if grep -q '@tcp(' <<<"$resp"; then
    log "  list_sources leaked a connection string"
    return 1
  fi
  return 0
}

# ---- run -------------------------------------------------------------------

STEP_N=0
FAILED=0
run_step() { # <name> <fn>
  local name=$1 fn=$2 ok=true
  STEP_N=$(( STEP_N + 1 ))
  if "$fn"; then
    log "PASS $STEP_N $name"
  else
    ok=false
    FAILED=1
    log "FAIL $STEP_N $name"
  fi
  jq -cn --argjson step "$STEP_N" --arg name "$name" --argjson ok "$ok" \
    '{step:$step,name:$name,ok:$ok}' >>"$RESULTS_FILE"
}

run_step healthz            step_healthz
run_step auth-rejections    step_auth_rejections
run_step tools-list         step_tools_list
run_step select-count       step_select_count
run_step delete-rejected    step_delete_rejected
run_step list-tables        step_list_tables
run_step query-timeout      step_query_timeout
run_step no-secrets-in-logs step_no_secrets_in_logs
run_step no-dsn-refused     step_no_dsn_refused
run_step short-token-refused step_short_token_refused
run_step root-dsn-refused   step_root_dsn_refused
run_step multi-source       step_multi_source

if $JSON_OUT; then
  jq -s . "$RESULTS_FILE"
fi
if (( FAILED )); then
  log "FAIL: $STEP_N steps run, at least one failed"
  exit 1
fi
log "PASS: $STEP_N steps"
exit 0
