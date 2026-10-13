#!/usr/bin/env bash
# bench.sh — measure the read-only MySQL MCP server end to end and write the raw
# numbers that `cmd/benchreport` renders into docs/benchmarks.md.
#
# Runs ON the Docker host, in the repo root, after `scripts/dev.sh sync`/`up`
# (`scripts/dev.sh bench` = sync + `scripts/bench.sh`). All Go/Docker work runs
# here; the local checkout is only updated manually (command printed at the end).
#
# Usage: scripts/bench.sh [--repeat N]
#   --repeat N   load-suite repetitions, default 2 (plan risk: shared VM noise).
#                Per-cell p50 deltas >= 20% between the first and last run are
#                recorded in bench-extra.json as "variance"/"variance_note".
#
# Env knobs (all optional):
#   BENCH_DURATION=30s      per-cell measurement window (cmd/bench -duration)
#   BENCH_WARMUP=5s         per-cell warmup (cmd/bench -warmup, 0 disables)
#   BENCH_CONCURRENCY="1 8 32"   concurrency points, space separated
#   GO_IMAGE=golang:1.26    Go build/test image
#   COMMIT_SHA=<sha>        commit being measured. The synced tree on this host
#                           has no .git, so `scripts/dev.sh bench` passes this in;
#                           without it the report records "unknown".
#
# Artifacts (under .bench/ in the repo root; transient on this host, wiped by the
# next `scripts/dev.sh sync --delete`):
#   .bench/bench-results.json  JSON array of load rows — one per cell, of the
#                              LAST run — exactly:
#       {scenario, concurrency, n, rps, p50_ms, p95_ms, p99_ms, max_ms, errors}
#   .bench/bench-meta.json     flat object of environment strings:
#       cpu_model, vcpu, ram_gb, os, docker_version, go_version, mysql_version,
#       gosdk_version, commit, timestamp
#   .bench/bench-extra.json    named metric groups for benchreport:
#       startup_cold_ms / startup_restart_ms [{run, ms}]  (up→first /healthz 200)
#       startup_process_ms [{run, startup_ms}]            (main's "ready" log)
#       startup_{cold,restart,process}_stats {min_ms, p50_ms, p95_ms, max_ms}
#       resources {idle_rss_mb, peak_rss_mb, cpu_pct}     (MiB, peak CPU% during load)
#       image_size {image_mb, binary_bytes}            (shipped /server bytes)
#       micro_benchmarks [{name, ns_per_op, bytes_per_op, allocs_per_op}] with
#         names DirectQuery/<x>, RunReadOnly/<x>, GuardCheck/{short,1KB,max}
#       timeout_accuracy {elapsed_s, processlist_clear_s, expected_s, is_error}
#       variance [{scenario, concurrency, p50_first_ms, p50_last_ms, delta_pct}],
#       variance_note "..."                            (run-to-run p50 deltas)
#
# Never records hostnames or IP addresses (only 127.0.0.1) — the repo is public.
set -euo pipefail

cd "$(dirname "$0")/.."

# ---------------------------------------------------------------------------
# arguments
REPEAT=2
while [ $# -gt 0 ]; do
  case "$1" in
    --repeat) [ $# -ge 2 ] || { echo "bench: --repeat needs a value" >&2; exit 2; }; REPEAT=$2; shift 2 ;;
    --repeat=*) REPEAT=${1#*=}; shift ;;
    *) echo "usage: scripts/bench.sh [--repeat N]" >&2; exit 2 ;;
  esac
done
case "$REPEAT" in ''|*[!0-9]*|0) echo "bench: --repeat must be a positive integer, got '$REPEAT'" >&2; exit 2 ;; esac

# ---------------------------------------------------------------------------
# prerequisites
for dep in docker jq curl awk sort; do
  command -v "$dep" >/dev/null 2>&1 || {
    echo "bench: missing dependency '$dep'. Install it on this host (e.g. apt-get install -y docker.io jq curl)" >&2
    exit 1
  }
done
docker compose version >/dev/null 2>&1 || {
  echo "bench: 'docker compose' is unavailable on this host" >&2
  exit 1
}

[ -f .env ] || { echo "bench: .env not found in $(pwd); create it from .env.example (see README)" >&2; exit 1; }
set -a
. ./.env
set +a
for var in MCP_AUTH_TOKEN MYSQL_ROOT_PASSWORD MCP_RO_PASSWORD; do
  [ -n "${!var:-}" ] || { echo "bench: $var is empty in .env; generate it (see .env.example)" >&2; exit 1; }
done

GO_IMAGE=${GO_IMAGE:-golang:1.26}
DURATION=${BENCH_DURATION:-30s}
WARMUP=${BENCH_WARMUP:-5s}
CONCURRENCY=${BENCH_CONCURRENCY:-"1 8 32"}
STARTUP_RUNS=10
IDLE_SECONDS=30
U=http://127.0.0.1:8080

WORK=$PWD/.bench
mkdir -p "$WORK" docs

log() { echo "bench: $*"; }

# go runs inside the pinned image, with the same caches as scripts/dev.sh; the
# load generator needs host networking to reach the published 127.0.0.1:8080.
gorun() {
  docker run --rm --network host -u "$(id -u):$(id -g)" \
    -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod \
    -e MCP_AUTH_TOKEN \
    -v "$HOME/.cache/mtg:/cache" -v "$PWD:/src" -w /src "$GO_IMAGE" "$@"
}

# summarize: one integer per line on stdin -> "min p50 p95 max" (nearest rank).
summarize() {
  sort -n | awk '
    { a[NR] = $1 }
    END {
      if (NR == 0) { print "0 0 0 0"; exit }
      i50 = int((NR * 50 + 99) / 100); if (i50 < 1) i50 = 1
      i95 = int((NR * 95 + 99) / 100); if (i95 < 1) i95 = 1
      printf "%d %d %d %d\n", a[1], a[i50], a[i95], a[NR]
    }'
}

# wait_healthy: poll /healthz every 10ms, echo elapsed ms to the first 200.
wait_healthy() {
  local t0 t code
  t0=$(date +%s%N)
  while :; do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 "$U/healthz" || true)
    if [ "$code" = 200 ]; then
      t=$(date +%s%N)
      echo $(( (t - t0) / 1000000 ))
      return 0
    fi
    t=$(date +%s%N)
    if [ $(( (t - t0) / 1000000 )) -gt 60000 ]; then
      echo "bench: $U/healthz did not return 200 within 60s (last code '$code')" >&2
      return 1
    fi
    sleep 0.01
  done
}

# mcp_start_ms: run "$@" (start/restart the mcp service), then wait for /healthz.
mcp_start_ms() {
  "$@" >&2
  wait_healthy
}

# mysql_q: one quiet query against the test MySQL, prints rows only.
mysql_q() {
  docker compose exec -T -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql \
    mysql -uroot -N -B -e "$1"
}

# to_bytes: docker stats size string (e.g. "12.34MiB") -> bytes.
to_bytes() {
  awk -v s="$1" 'BEGIN {
    u = s; sub(/[0-9.]+/, "", u); v = s + 0
    if (u == "KiB") v *= 1024
    else if (u == "MiB") v *= 1048576
    else if (u == "GiB") v *= 1073741824
    printf "%.0f\n", v
  }'
}

# ---------------------------------------------------------------------------
# 1. environment metadata (strings only; never hostnames/IPs)
log "collecting environment metadata"
docker compose up -d mysql >/dev/null
for _ in $(seq 1 60); do
  mysql_q 'select 1' >/dev/null 2>&1 && break
  sleep 2
done

cpu_model=$(awk -F': ' '/^model name/ { print $2; exit }' /proc/cpuinfo 2>/dev/null || true)
[ -n "$cpu_model" ] || cpu_model=$(awk -F': ' '/^(Model|model)/ { print $2; exit }' /proc/cpuinfo 2>/dev/null || true)
[ -n "$cpu_model" ] || cpu_model=$(uname -m)
vcpu=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN)
ram_gb=$(awk '/^MemTotal:/ { printf "%.1f", $2 * 1024 / 1000000000 }' /proc/meminfo)
os=$(. /etc/os-release 2>/dev/null && echo "${PRETTY_NAME:-}" || true)
[ -n "$os" ] || os=$(uname -srm)
docker_version=$(docker version --format '{{.Server.Version}}')
go_version=$(docker run --rm "$GO_IMAGE" go version | awk '{ print $3 }')
mysql_version=$(mysql_q 'select version()' | head -1 | sed 's/[^0-9.].*//')
gosdk_version=$(gorun go list -m -f '{{.Version}}' github.com/modelcontextprotocol/go-sdk | tr -d '[:space:]')
commit=${COMMIT_SHA:-${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}}
commit=$(printf '%s' "$commit" | sed 's/[^0-9a-f]//g')
[ -n "$commit" ] || commit=unknown
timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)

jq -n \
  --arg cpu_model "$cpu_model" --arg vcpu "$vcpu" --arg ram_gb "$ram_gb" --arg os "$os" \
  --arg docker_version "$docker_version" --arg go_version "$go_version" \
  --arg mysql_version "$mysql_version" --arg gosdk_version "$gosdk_version" \
  --arg commit "$commit" --arg timestamp "$timestamp" \
  '{cpu_model: $cpu_model, vcpu: $vcpu, ram_gb: $ram_gb, os: $os,
    docker_version: $docker_version, go_version: $go_version,
    mysql_version: $mysql_version, gosdk_version: $gosdk_version,
    commit: $commit, timestamp: $timestamp}' > "$WORK/bench-meta.json"

# ---------------------------------------------------------------------------
# 2. build the image and record artifact sizes
log "building the mcp image"
docker compose build mcp >/dev/null
# Binary size: the runtime image is distroless (no shell to `ls` in), so copy
# /server out of a throwaway container with `docker create` + `docker cp` — the
# exact shipped bytes. Image size comes straight from `docker image inspect`.
image_bytes=$(docker image inspect --format '{{.Size}}' mysql-toolbox-go)
cid=$(docker create mysql-toolbox-go)
docker cp "$cid:/server" "$WORK/server-bin" >/dev/null
docker rm "$cid" >/dev/null
binary_bytes=$(wc -c < "$WORK/server-bin" | tr -d ' ')

jq -n --argjson image_bytes "$image_bytes" --argjson binary_bytes "$binary_bytes" \
  '{image_mb: (($image_bytes / 1000000) * 100 | round / 100),
    binary_bytes: $binary_bytes}' > "$WORK/image-size.json"

# ---------------------------------------------------------------------------
# 3. startup metrics: cold start and restart, N runs each
log "startup: $STARTUP_RUNS cold starts + $STARTUP_RUNS restarts"
rm -f "$WORK/startup-cold.ndjson" "$WORK/startup-restart.ndjson" "$WORK/startup-process.ndjson"
for i in $(seq 1 "$STARTUP_RUNS"); do
  docker compose rm -sf mcp >/dev/null
  ms=$(mcp_start_ms docker compose up -d mcp)
  printf '{"run":%d,"ms":%d}\n' "$i" "$ms" >> "$WORK/startup-cold.ndjson"
  # process start -> listening, from main's structured "ready" log line
  startup_ms=$(docker compose logs --no-log-prefix mcp 2>/dev/null \
    | jq -Rr 'fromjson? | select(.msg == "ready") | .startup_ms' | tail -1 || true)
  case "${startup_ms:-}" in ''|*[!0-9]*) startup_ms=0 ;; esac
  printf '{"run":%d,"startup_ms":%d}\n' "$i" "$startup_ms" >> "$WORK/startup-process.ndjson"
  log "  cold start $i/$STARTUP_RUNS: ${ms}ms (startup_ms ${startup_ms})"
done
for i in $(seq 1 "$STARTUP_RUNS"); do
  ms=$(mcp_start_ms docker compose restart mcp)
  printf '{"run":%d,"ms":%d}\n' "$i" "$ms" >> "$WORK/startup-restart.ndjson"
  log "  restart $i/$STARTUP_RUNS: ${ms}ms"
done

# ---------------------------------------------------------------------------
# 4. idle resources: leave the server alone for 30s, then sample once
log "idle RSS after ${IDLE_SECONDS}s"
docker compose up -d mcp >/dev/null
wait_healthy >/dev/null
sleep "$IDLE_SECONDS"
cid=$(docker compose ps -q mcp)
idle_mem=$(docker stats --no-stream --format '{{.MemUsage}}' "$cid" | awk '{ print $1 }')
idle_rss_mb=$(to_bytes "$idle_mem" | awk '{ printf "%.1f", $1 / 1048576 }')

# ---------------------------------------------------------------------------
# 5. load: S1..S6 x concurrency x --repeat, sampling container stats every 1s
log "load: S1..S6 x {${CONCURRENCY// /,}} x $REPEAT run(s), ${DURATION}/cell (warmup ${WARMUP})"
rm -f "$WORK/stats.log"
sampler_ticks=0
(
  # Bounded: an orphaned sampler can never spin forever if the script dies
  # mid-load (14400 ticks at 1s is far past the longest load suite).
  while [ ! -f "$WORK/stop-sampler" ] && [ "$sampler_ticks" -lt 14400 ]; do
    docker stats --no-stream --format '{{.CPUPerc}} {{.MemUsage}}' "$cid" >> "$WORK/stats.log" 2>/dev/null || true
    sampler_ticks=$((sampler_ticks + 1))
    sleep 1
  done
) &
sampler=$!
# On exit SIGNAL the sampler to stop; never delete the flag here — deleting it
# would re-arm a sampler that is already running.
trap 'touch "$WORK/stop-sampler"' EXIT

rm -f "$WORK"/load-runs.ndjson
for k in $(seq 1 "$REPEAT"); do
  rows=$WORK/rows-run-$k.ndjson
  rm -f "$rows"
  # tee keeps a failing run's output instead of losing it inside a pipe
  gorun go run ./cmd/bench \
    -url "$U/mcp" \
    -scenario S1,S2,S3,S4,S5,S6 \
    -concurrency "${CONCURRENCY// /,}" \
    -duration "$DURATION" -warmup "$WARMUP" \
    -out "/src/.bench/rows-run-$k.ndjson" 2>&1 | tee "$WORK/client-run-$k.log"
  jq -cs --argjson run "$k" 'map(. + {run: $run})[]' "$rows" >> "$WORK/load-runs.ndjson"
  log "  run $k/$REPEAT: $(wc -l < "$rows" | tr -d ' ') cells"
done

touch "$WORK/stop-sampler"
wait "$sampler" 2>/dev/null || true
rm -f "$WORK/stop-sampler"

# peak RSS / peak CPU% while under load
read -r peak_rss_mb cpu_pct <<EOF
$(awk '
  {
    cpu = $1; sub(/%/, "", cpu)
    v = $2 + 0
    u = $2; sub(/[0-9.]+/, "", u)
    if (u == "B") b = v
    else if (u == "KiB") b = v * 1024
    else if (u == "MiB") b = v * 1048576
    else if (u == "GiB") b = v * 1073741824
    else b = v
    if (b > peak) peak = b
    if (cpu + 0 > cpeak) cpeak = cpu + 0
  }
  END { printf "%.1f %.2f\n", peak / 1048576, cpeak }' "$WORK/stats.log")
EOF

jq -n --arg idle_rss_mb "$idle_rss_mb" --argjson peak_rss_mb "$peak_rss_mb" \
  --argjson cpu_pct "$cpu_pct" \
  '{idle_rss_mb: ($idle_rss_mb | tonumber), peak_rss_mb: $peak_rss_mb, cpu_pct: $cpu_pct}' \
  > "$WORK/resources.json"

# bench-results.json: the last run's rows, exactly the contracted fields
jq -cs 'map({scenario, concurrency, n, rps, p50_ms, p95_ms, p99_ms, max_ms, errors})' \
  "$WORK/rows-run-$REPEAT.ndjson" > "$WORK/bench-results.json"

# variance: per-cell p50 of run 1 vs the last run
jq -cs '
  group_by(.run) as $runs
  | ($runs[0] // []) as $first
  | ($runs[-1] // []) as $last
  | [ $first[] as $f
      | ($last[] | select(.scenario == $f.scenario and .concurrency == $f.concurrency)) as $l
      | (($l.p50_ms - $f.p50_ms) / (if $f.p50_ms > 0 then $f.p50_ms else 1 end) * 100) as $d
      | (if $d < 0 then -$d else $d end) as $abs
      | select($abs >= 20)
      | {scenario: $f.scenario, concurrency: $f.concurrency,
         p50_first_ms: $f.p50_ms, p50_last_ms: $l.p50_ms, delta_pct: (($d * 100 | round) / 100)} ]
' "$WORK/load-runs.ndjson" > "$WORK/variance.json"
if [ "$(jq 'length' "$WORK/variance.json")" -gt 0 ]; then
  variance_note="p50 differs by >= 20% between runs for $(jq -r 'map("\(.scenario)@\(.concurrency)") | join(", ")' "$WORK/variance.json") (see the variance table). The host is shared/noisy and MySQL runs on the same machine as the server: treat absolute numbers as indicative, not as a capacity claim."
else
  variance_note=""
fi

# ---------------------------------------------------------------------------
# 6. micro-benchmarks
log "micro-benchmark: internal/guard"
gorun go test -bench=. -run='^$' -benchmem ./internal/guard/ 2>&1 | tee "$WORK/micro-guard.txt" >/dev/null
log "micro-benchmark: internal/source (integration, read-only overhead)"
HOST_UID=$(id -u) HOST_GID=$(id -g) docker compose --profile test run --rm it \
  go test -tags=integration -bench=. -run='^$' -benchmem ./internal/source/ 2>&1 | tee "$WORK/micro-source.txt" >/dev/null

# "BenchmarkCheck/short-8  123  812.3 ns/op  320 B/op  3 allocs/op" -> JSON
bench_json() {
  awk '
    /^Benchmark/ {
      name = $1; sub(/-[0-9]+$/, "", name)
      ns = 0; b = 0; a = 0
      for (i = 2; i < NF; i++) {
        if ($(i + 1) == "ns/op") ns = $i
        else if ($(i + 1) == "B/op") b = $i
        else if ($(i + 1) == "allocs/op") a = $i
      }
      printf "%s\t%s\t%s\t%s\n", name, ns, b, a
    }' \
  | jq -Rn '[inputs | split("\t") | select(length >= 4)
      | {name: .[0], ns_per_op: (.[1] | tonumber), bytes_per_op: (.[2] | tonumber), allocs_per_op: (.[3] | tonumber)}]'
}
bench_json < "$WORK/micro-guard.txt" > "$WORK/micro-guard.json"
bench_json < "$WORK/micro-source.txt" > "$WORK/micro-source.json"

# Map benchmark names onto the report contract: BenchmarkDirectQuery/<x> ->
# DirectQuery/<x>, BenchmarkRunReadOnly/<x> -> RunReadOnly/<x>, and the three
# guard sizes onto GuardCheck/{short,1KB,max} (medium is the ~1KB SQL).
jq -n --slurpfile g "$WORK/micro-guard.json" --slurpfile s "$WORK/micro-source.json" '
  [ ($s[0] + $g[0])[]
    | { name: (.name | if . == "BenchmarkCheck/short" then "GuardCheck/short"
                       elif . == "BenchmarkCheck/medium" then "GuardCheck/1KB"
                       elif . == "BenchmarkCheck/max" then "GuardCheck/max"
                       else sub("^Benchmark"; "") end),
        ns_per_op: .ns_per_op, bytes_per_op: .bytes_per_op, allocs_per_op: .allocs_per_op } ]' \
  > "$WORK/micro-benchmarks.json"

# ---------------------------------------------------------------------------
# 7. timeout accuracy: QUERY_TIMEOUT=2s against SELECT SLEEP(30)
log "timeout accuracy: QUERY_TIMEOUT=2s vs SELECT SLEEP(30)"
restored=1
restore_timeout() {
  if [ "$restored" -eq 0 ]; then
    docker compose up -d mcp >/dev/null 2>&1 || true
    wait_healthy >/dev/null 2>&1 || true
    restored=1
  fi
}
trap 'touch "$WORK/stop-sampler"; restore_timeout' EXIT

restored=0
QUERY_TIMEOUT=2s docker compose up -d --force-recreate mcp >/dev/null
wait_healthy >/dev/null

payload='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute_sql","arguments":{"sql":"SELECT SLEEP(30)"}}}'
t0=$(date +%s%N)
body=$(curl -sS -X POST "$U/mcp" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "Authorization: Bearer $MCP_AUTH_TOKEN" \
  --max-time 60 -d "$payload")
t1=$(date +%s%N)
error_s=$(awk -v n=$(( t1 - t0 )) 'BEGIN { printf "%.2f", n / 1000000000 }')
is_error=$(printf '%s' "$body" | jq -r '.result.isError // false')

# when does the killed query disappear from the server-side process list?
processlist_clear_s=0
for _ in $(seq 1 150); do
  n=$(mysql_q "select count(*) from information_schema.processlist where INFO like 'SELECT SLEEP%'" | head -1 || true)
  if [ "${n:-1}" = 0 ]; then
    t2=$(date +%s%N)
    processlist_clear_s=$(awk -v n=$(( t2 - t0 )) 'BEGIN { printf "%.2f", n / 1000000000 }')
    break
  fi
  sleep 0.2
done

jq -n --arg elapsed_s "$error_s" --arg processlist_clear_s "$processlist_clear_s" \
  --arg expected_s 2 --arg is_error "$is_error" \
  '{elapsed_s: ($elapsed_s | tonumber), processlist_clear_s: ($processlist_clear_s | tonumber),
    expected_s: ($expected_s | tonumber), is_error: ($is_error == "true")}' > "$WORK/timeout.json"

restore_timeout

# ---------------------------------------------------------------------------
# 8. assemble bench-extra.json and render the report

# min/p50/p95/max (nearest rank) over each startup series
startup_stats() { # $1 = ndjson file, $2 = value field -> JSON {min_ms,p50_ms,p95_ms,max_ms}
  local mn p50 p95 mx
  read -r mn p50 p95 mx <<<"$(jq -r ".$2" "$1" | summarize)"
  jq -n --argjson min_ms "$mn" --argjson p50_ms "$p50" --argjson p95_ms "$p95" --argjson max_ms "$mx" \
    '{min_ms: $min_ms, p50_ms: $p50_ms, p95_ms: $p95_ms, max_ms: $max_ms}'
}
startup_stats "$WORK/startup-cold.ndjson" ms > "$WORK/startup-cold-stats.json"
startup_stats "$WORK/startup-restart.ndjson" ms > "$WORK/startup-restart-stats.json"
startup_stats "$WORK/startup-process.ndjson" startup_ms > "$WORK/startup-process-stats.json"
jq -n \
  --slurpfile cold "$WORK/startup-cold.ndjson" \
  --slurpfile restart "$WORK/startup-restart.ndjson" \
  --slurpfile process "$WORK/startup-process.ndjson" \
  --slurpfile cold_stats "$WORK/startup-cold-stats.json" \
  --slurpfile restart_stats "$WORK/startup-restart-stats.json" \
  --slurpfile process_stats "$WORK/startup-process-stats.json" \
  --slurpfile size "$WORK/image-size.json" \
  --slurpfile resources "$WORK/resources.json" \
  --slurpfile micro "$WORK/micro-benchmarks.json" \
  --slurpfile timeout "$WORK/timeout.json" \
  --slurpfile variance "$WORK/variance.json" \
  --arg variance_note "$variance_note" \
  '{startup_cold_ms: $cold, startup_restart_ms: $restart, startup_process_ms: $process,
    startup_cold_stats: $cold_stats[0], startup_restart_stats: $restart_stats[0],
    startup_process_stats: $process_stats[0],
    image_size: $size[0], resources: $resources[0], micro_benchmarks: $micro[0],
    timeout_accuracy: $timeout[0], variance: $variance[0], variance_note: $variance_note}' \
  > "$WORK/bench-extra.json"

log "rendering docs/benchmarks.md + README (BENCH markers) via cmd/benchreport"
gorun go run ./cmd/benchreport \
  -rows .bench/bench-results.json -meta .bench/bench-meta.json -extra .bench/bench-extra.json \
  -out docs/benchmarks.md -readme README.md

# the report must never leak hostnames/IPs (the repo is public)
for f in docs/benchmarks.md "$WORK/bench-meta.json" "$WORK/bench-extra.json"; do
  leaks=$(grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' "$f" | sort -u | grep -v '^127\.0\.0\.1$' || true)
  [ -z "$leaks" ] || {
    echo "bench: $f contains non-loopback IP address(es), refusing to publish:" >&2
    echo "$leaks" >&2
    exit 1
  }
done

log "done. Generated: docs/benchmarks.md, README.md (BENCH block), $WORK/{bench-results,bench-meta,bench-extra}.json"
if [ -n "$variance_note" ]; then log "variance: $variance_note"; fi
log "pull the results back to your local checkout:"
log "  rsync -az \"\$VM:\$DIR/docs/\" docs/ && rsync -az \"\$VM:\$DIR/README.md\" README.md"
log "(VM and DIR as in .dev.env / scripts/dev.sh)"
