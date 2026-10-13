#!/usr/bin/env bash
# test-report.sh — run every test layer once and write the results to
# docs/test-report.md plus the summary block between the README markers
# <!-- TESTS:START --> / <!-- TESTS:END --> (replaced strictly between them;
# the script fails loudly if the marker pair is missing).
#
# Runs ON the Docker host, in the repo root, after `scripts/dev.sh sync`/`up`
# (`scripts/dev.sh test-report` = sync + `scripts/test-report.sh`).
#
# Layers:
#   1. unit:        go test -json ./...            (golang image, with coverage)
#   2. integration: go test -json -tags=integration ./...   (compose `it`, DB up)
#   3. fuzz:        go test -fuzz=$FUZZ_NAME        (default FuzzCheck, 10s)
#   4. govulncheck: pinned like scripts/dev.sh (VULN_VERSION)
#   5. e2e:         scripts/e2e.sh --json  -> one JSON array of
#                   {"step": 1..12, "name": "...", "ok": true|false} on stdout
#
# The report shows FAIL (and the script exits non-zero) when any layer fails.
#
# Env knobs (all optional):
#   GO_IMAGE=golang:1.26  VULN_VERSION=v1.8.0
#   FUZZ_PKG=./internal/guard/  FUZZ_NAME=FuzzCheck  FUZZ_TIME=10s
#   COMMIT_SHA=<sha>  commit being tested (the synced tree on this host has no
#                     .git; without it the report records "unknown")
set -euo pipefail

cd "$(dirname "$0")/.."

# ---------------------------------------------------------------------------
# prerequisites
for dep in docker jq curl awk; do
  command -v "$dep" >/dev/null 2>&1 || {
    echo "test-report: missing dependency '$dep'. Install it on this host (e.g. apt-get install -y docker.io jq curl)" >&2
    exit 1
  }
done
docker compose version >/dev/null 2>&1 || {
  echo "test-report: 'docker compose' is unavailable on this host" >&2
  exit 1
}
[ -f .env ] || { echo "test-report: .env not found in $(pwd); create it from .env.example (see README)" >&2; exit 1; }
set -a
. ./.env
set +a
for var in MCP_AUTH_TOKEN MYSQL_ROOT_PASSWORD MCP_RO_PASSWORD; do
  [ -n "${!var:-}" ] || { echo "test-report: $var is empty in .env; generate it (see .env.example)" >&2; exit 1; }
done

GO_IMAGE=${GO_IMAGE:-golang:1.26}
VULN_VERSION=${VULN_VERSION:-v1.8.0}
FUZZ_PKG=${FUZZ_PKG:-./internal/guard/}
FUZZ_NAME=${FUZZ_NAME:-FuzzCheck}
FUZZ_TIME=${FUZZ_TIME:-10s}
U=http://127.0.0.1:8080   # loopback only: the repo is public

# .testreport, not .bench: running the report after scripts/bench.sh must not
# wipe the benchmark artifacts the report is read next to.
WORK=$PWD/.testreport
mkdir -p "$WORK" docs

log() { echo "test-report: $*"; }

gorun() {
  docker run --rm -u "$(id -u):$(id -g)" \
    -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod \
    -v "$HOME/.cache/mtg:/cache" -v "$PWD:/src" -w /src "$GO_IMAGE" "$@"
}

# ---------------------------------------------------------------------------
# README marker check first: a missing marker must not be discovered after a
# long run.
for pair in "BENCH:START BENCH:END" "TESTS:START TESTS:END"; do
  set -- $pair
  sc=$(grep -cF "<!-- $1 -->" README.md || true)
  ec=$(grep -cF "<!-- $2 -->" README.md || true)
  [ "$sc" = 1 ] && [ "$ec" = 1 ] || {
    echo "test-report: README.md must contain exactly one '<!-- $1 -->' / '<!-- $2 -->' pair (found $sc/$ec)" >&2
    exit 1
  }
done

# inject <content-file> between the START/END markers of <target>, replacing
# whatever is in between (and nothing else).
inject() { # $1=START-marker $2=END-marker $3=content-file $4=target
  awk -v s="<!-- $1 -->" -v e="<!-- $2 -->" -v c="$3" '
    BEGIN { while ((getline line < c) > 0) body = body line "\n" }
    index($0, s) { print; printf "%s", body; skip = 1; next }
    skip && index($0, e) { skip = 0 }
    !skip { print }
  ' "$4" > "$4.tmp" && mv "$4.tmp" "$4"
}

# ---------------------------------------------------------------------------
commit=${COMMIT_SHA:-${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}}
commit=$(printf '%s' "$commit" | sed 's/[^0-9a-f]//g')
[ -n "$commit" ] || commit=unknown
timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
go_version=$(docker run --rm "$GO_IMAGE" go version | awk '{ print $3 }')

# ---------------------------------------------------------------------------
# 1. unit tests + coverage
log "unit tests: go test -json -coverprofile ./..."
unit_rc=0
gorun go test -json -count=1 -coverprofile=/src/.testreport/unit-cover.out ./... \
  > "$WORK/unit.json" 2>"$WORK/unit.err" || unit_rc=$?
if [ -s "$WORK/unit.err" ]; then sed 's/^/test-report:   /' "$WORK/unit.err" >&2; fi

unit_total=$(gorun go tool cover -func=/src/.testreport/unit-cover.out 2>/dev/null \
  | awk '/^total:/ { print $NF }' || true)

# ---------------------------------------------------------------------------
# 2. integration tests
log "integration tests: go test -json -tags=integration ./... (compose it)"
int_rc=0
HOST_UID=$(id -u) HOST_GID=$(id -g) docker compose --profile test run --rm it \
  go test -json -tags=integration -count=1 -p 1 ./... \
  > "$WORK/integration.json" 2>"$WORK/integration.err" || int_rc=$?
if [ -s "$WORK/integration.err" ]; then sed 's/^/test-report:   /' "$WORK/integration.err" >&2; fi

# ---------------------------------------------------------------------------
# 3. fuzz
log "fuzz: go test -fuzz=$FUZZ_NAME -fuzztime=$FUZZ_TIME $FUZZ_PKG"
fuzz_rc=0
gorun go test -run='^$' -fuzz="$FUZZ_NAME" -fuzztime="$FUZZ_TIME" "$FUZZ_PKG" \
  > "$WORK/fuzz.log" 2>&1 || fuzz_rc=$?
fuzz_line=$(tr '\r' '\n' < "$WORK/fuzz.log" | awk '/fuzz: elapsed:/ { last = $0 } END { print last }')
fuzz_execs=$(printf '%s' "$fuzz_line" | sed -nE 's/.*execs: ([0-9]+).*/\1/p')
fuzz_elapsed=$(printf '%s' "$fuzz_line" | sed -nE 's/.*elapsed: ([^,]+),.*/\1/p')
fuzz_execs=${fuzz_execs:-0}
fuzz_elapsed=${fuzz_elapsed:-$FUZZ_TIME}

# ---------------------------------------------------------------------------
# 4. govulncheck
log "govulncheck $VULN_VERSION"
vuln_rc=0
gorun go run "golang.org/x/vuln/cmd/govulncheck@$VULN_VERSION" ./... \
  > "$WORK/vuln.log" 2>&1 || vuln_rc=$?
vuln_found=$(grep -oE 'Found [0-9]+ vulnerabilit' "$WORK/vuln.log" | head -1 || true)

# ---------------------------------------------------------------------------
# 5. e2e (its --json contract: one JSON array of {step, name, ok} on stdout)
# e2e.sh talks to a running stack and exits 2 when nothing listens on :8080,
# so bring mysql+mcp up (and give the server a moment) before calling it.
docker compose up -d mysql mcp >/dev/null
for _ in $(seq 1 60); do
  curl -fsS -o /dev/null "$U/healthz" 2>/dev/null && break
  sleep 0.5
done
log "e2e: scripts/e2e.sh --json"
e2e_rc=0
scripts/e2e.sh --json > "$WORK/e2e.json" 2>"$WORK/e2e.err" || e2e_rc=$?
if [ -s "$WORK/e2e.err" ]; then sed 's/^/test-report:   /' "$WORK/e2e.err" >&2; fi
e2e_ok=0
e2e_total=0
if jq -e 'type == "array" and length > 0 and all(.[]; has("step") and has("name") and (.ok | type == "boolean"))' \
    "$WORK/e2e.json" >/dev/null 2>&1; then
  e2e_total=$(jq 'length' "$WORK/e2e.json")
  e2e_ok=$(jq '[.[] | select(.ok)] | length' "$WORK/e2e.json")
# tripwire: e2e.sh documents exactly 12 steps; a different count means the
# contract changed (or the run was cut short) and must not read as a pass.
[ "$e2e_total" = 12 ] || e2e_rc=1
else
  e2e_rc=1
fi

# ---------------------------------------------------------------------------
# parse go test -json: per-package pass/fail/skip and per-package coverage
pkg_rows() { # $1 = go test -json file -> "package<TAB>pass<TAB>fail<TAB>skip<TAB>coverage"
  jq -rs '
    [ .[] | select(.Test != null and (.Action == "pass" or .Action == "fail" or .Action == "skip")) ] as $t
    | [ .[] | select(.Test == null and .Action == "fail") | .Package ] | unique as $pkgfail
    | ($t | group_by(.Package) | map([.[0].Package,
        ([.[] | select(.Action == "pass")] | length),
        ([.[] | select(.Action == "fail")] | length),
        ([.[] | select(.Action == "skip")] | length)])) as $rows
    | $rows + [ $pkgfail[] as $p | select([$rows[][0]] | index($p) | not) | [$p, 0, 1, 0] ]
    | .[] | @tsv' "$1" 2>/dev/null \
  | while IFS=$'\t' read -r pkg pass fail skip; do
      cov=$(jq -r 'select(.Action == "output") | .Package + "\t" + (.Output | gsub("\n"; ""))' "$1" \
        | grep -F "${pkg}"$'\t' | sed -nE "s/.*coverage: ([0-9.]+)%.*/\1/p" | tail -1 || true)
      printf '%s\t%s\t%s\t%s\t%s\n' "$pkg" "$pass" "$fail" "$skip" "${cov:-—}"
    done
}
pkg_rows "$WORK/unit.json" > "$WORK/unit-table.tsv"
pkg_rows "$WORK/integration.json" > "$WORK/integration-table.tsv"

table_md() { # $1 = tsv file -> markdown table on stdout
  echo "| Package | Pass | Fail | Skip | Coverage |"
  echo "|---|---|---|---|---|"
  if [ -s "$1" ]; then
    while IFS=$'\t' read -r pkg pass fail skip cov; do
      echo "| \`$pkg\` | $pass | $fail | $skip | $cov |"
    done < "$1"
  else
    echo "| — | 0 | 0 | 0 | — |"
  fi
}

sum_col() { awk -F'\t' -v c="$2" '{ s += $c } END { print s + 0 }' "$1"; }
u_pass=$(sum_col "$WORK/unit-table.tsv" 2); u_fail=$(sum_col "$WORK/unit-table.tsv" 3); u_skip=$(sum_col "$WORK/unit-table.tsv" 4)
i_pass=$(sum_col "$WORK/integration-table.tsv" 2); i_fail=$(sum_col "$WORK/integration-table.tsv" 3); i_skip=$(sum_col "$WORK/integration-table.tsv" 4)

verdict=PASS
[ "$unit_rc" = 0 ] && [ "$u_fail" = 0 ] || verdict=FAIL
[ "$int_rc" = 0 ] && [ "$i_fail" = 0 ] || verdict=FAIL
[ "$fuzz_rc" = 0 ] || verdict=FAIL
[ "$vuln_rc" = 0 ] || verdict=FAIL
[ "$e2e_rc" = 0 ] && [ "$e2e_ok" = "$e2e_total" ] && [ "$e2e_total" -gt 0 ] || verdict=FAIL

# ---------------------------------------------------------------------------
# render docs/test-report.md
{
  echo "# Test report"
  echo
  echo "Generated by \`scripts/test-report.sh\` on $timestamp · commit \`$commit\` · $go_version"
  echo
  echo "## Unit tests (\`go test ./...\`)"
  echo
  table_md "$WORK/unit-table.tsv"
  echo
  echo "Totals: $u_pass pass, $u_fail fail, $u_skip skip · coverage total: ${unit_total:-—} — **$([ "$unit_rc" = 0 ] && echo PASS || echo FAIL)**"
  echo
  echo "## Integration tests (\`go test -tags=integration ./...\`, compose \`it\`)"
  echo
  table_md "$WORK/integration-table.tsv"
  echo
  echo "Totals: $i_pass pass, $i_fail fail, $i_skip skip — **$([ "$int_rc" = 0 ] && echo PASS || echo FAIL)**"
  echo
  echo "## Fuzz (\`$FUZZ_NAME\`, $FUZZ_TIME)"
  echo
  echo "| Duration | Executions | Result |"
  echo "|---|---|---|"
  echo "| $fuzz_elapsed | $fuzz_execs | **$([ "$fuzz_rc" = 0 ] && echo PASS || echo FAIL)** |"
  if [ "$fuzz_rc" != 0 ]; then
    echo
    echo '```'
    sed -n '1,20p' "$WORK/fuzz.log"
    echo '```'
  fi
  echo
  echo "## govulncheck ($VULN_VERSION)"
  echo
  echo "**$([ "$vuln_rc" = 0 ] && echo PASS || echo FAIL)** — ${vuln_found:-$(tail -1 "$WORK/vuln.log" 2>/dev/null || true)}"
  echo
  echo "## E2E (\`scripts/e2e.sh --json\`)"
  echo
  echo "| # | Step | Result |"
  echo "|---|---|---|"
  if [ -s "$WORK/e2e.json" ] && jq -e 'type == "array"' "$WORK/e2e.json" >/dev/null 2>&1; then
    jq -r '.[] | "| \(.step) | \(.name) | \(if .ok then "PASS" else "**FAIL**" end) |"' "$WORK/e2e.json"
  else
    echo "| — | could not parse e2e output | **FAIL** |"
  fi
  echo
  echo "e2e: $e2e_ok/$e2e_total passed — **$([ "$e2e_rc" = 0 ] && echo PASS || echo FAIL)**"
  echo
  echo "## Verdict: **$verdict**"
} > docs/test-report.md

# ---------------------------------------------------------------------------
# README summary between <!-- TESTS:START --> and <!-- TESTS:END -->
{
  echo "| Metric | Value |"
  echo "|---|---|"
  echo "| Unit tests | $u_pass pass / $u_fail fail / $u_skip skip |"
  echo "| Coverage (total) | ${unit_total:-—} |"
  echo "| Integration tests | $i_pass pass / $i_fail fail / $i_skip skip |"
  echo "| E2E | $e2e_ok/$e2e_total |"
  echo "| Fuzz (\`$FUZZ_NAME\`, $FUZZ_TIME) | $fuzz_execs executions |"
  echo "| govulncheck | $([ "$vuln_rc" = 0 ] && echo clean || echo "${vuln_found:-FAIL}") |"
  echo
  echo "**Verdict: $verdict** — full report: [docs/test-report.md](docs/test-report.md) · commit \`$commit\` · $timestamp"
} > "$WORK/tests-summary.md"
inject "TESTS:START" "TESTS:END" "$WORK/tests-summary.md" README.md

# no IP addresses/hostname-shaped values in what we publish (repo is public)
for f in docs/test-report.md "$WORK/tests-summary.md"; do
  leaks=$(grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' "$f" | sort -u | grep -v '^127\.0\.0\.1$' || true)
  [ -z "$leaks" ] || {
    echo "test-report: $f contains non-loopback IP address(es), refusing to publish:" >&2
    echo "$leaks" >&2
    exit 1
  }
done

log "done. Generated: docs/test-report.md, README.md (TESTS block). Verdict: $verdict"
log "pull the results back to your local checkout:"
log "  rsync -az \"\$VM:\$DIR/docs/\" docs/ && rsync -az \"\$VM:\$DIR/README.md\" README.md"
log "(VM and DIR as in .dev.env / scripts/dev.sh)"

[ "$verdict" = PASS ] || exit 1
