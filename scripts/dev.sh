#!/usr/bin/env bash
# Dev loop helper: edit locally, run everything (Go, Docker) on the VM.
set -euo pipefail

# Local, untracked settings (e.g. VM=user@host). See README "Dev loop".
[ -f "$(dirname "$0")/../.dev.env" ] && . "$(dirname "$0")/../.dev.env"
VM=${VM:?set VM=user@host in .dev.env or the environment}
DIR=${DIR:-mysql-toolbox-go}
# Pinned govulncheck version (update deliberately).
VULN_VERSION=${VULN_VERSION:-v1.8.0}
GO_IMAGE=${GO_IMAGE:-golang:1.26}

cd "$(dirname "$0")/.."

# Runs on the VM, inside $DIR.
GO_RUN="docker run --rm -u \"\$(id -u):\$(id -g)\" -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -v \"\$HOME/.cache/mtg:/cache\" -v \"\$PWD:/src\" -w /src $GO_IMAGE"

# Quote args for the remote shell.
q() { [ $# -eq 0 ] || printf '%q ' "$@"; }

rsh() { ssh -o BatchMode=yes "$VM" "mkdir -p ~/.cache/mtg && cd $DIR && $1"; }

sync_up() {
  ssh -o BatchMode=yes "$VM" "mkdir -p ~/.cache/mtg $DIR"
  rsync -az --delete --exclude .git --exclude .env --exclude .dev.env --exclude .DS_Store \
    --exclude testdata/fuzz ./ "$VM:$DIR/"
}

pull_mod() {
  rsync -az --include=go.mod --include=go.sum --exclude='*' "$VM:$DIR/" ./ 2>/dev/null || true
}

pull_fuzz() {
  rsync -azm --include='*/' --include='**/testdata/fuzz/**' --exclude='*' "$VM:$DIR/" ./ || true
}

cmd=${1:-}
[ $# -gt 0 ] && shift

case "$cmd" in
  sync)
    sync_up
    ;;
  test)
    sync_up
    if [ $# -gt 0 ]; then
      rsh "$GO_RUN go test $(q "$@")"
    else
      rsh "$GO_RUN go test ./..."
    fi
    ;;
  go)
    sync_up
    rc=0
    rsh "$GO_RUN go $(q "$@")" || rc=$?
    pull_mod
    exit $rc
    ;;
  vuln)
    sync_up
    rsh "$GO_RUN go run golang.org/x/vuln/cmd/govulncheck@$VULN_VERSION ./..."
    ;;
  fuzz)
    [ $# -ge 2 ] || { echo "usage: dev.sh fuzz <pkg> <FuzzName>" >&2; exit 2; }
    sync_up
    rc=0
    rsh "$GO_RUN go test -run='^\$' -fuzz=$(q "$2") -fuzztime=30s $(q "$1")" || rc=$?
    if [ $rc -ne 0 ]; then pull_fuzz; fi
    exit $rc
    ;;
  up)
    sync_up
    rsh "docker compose up -d $(q "$@")"
    ;;
  down)
    rsh "docker compose down $(q "$@")"
    ;;
  stop)
    rsh "docker compose stop"
    ;;
  logs)
    rsh "docker compose logs $(q "$@")"
    ;;
  it)
    sync_up
    # The test DB starts on demand (depends_on) and is stopped afterwards to
    # free memory on a shared host; set KEEP_DB=1 to leave it running.
    status=0
    rsh "HOST_UID=\$(id -u) HOST_GID=\$(id -g) docker compose --profile test run --rm it go test -tags=integration -count=1 -p 1 ./... $(q "$@")" || status=$?
    [ "${KEEP_DB:-0}" = 1 ] || rsh "docker compose stop mysql >/dev/null 2>&1" || true
    exit $status
    ;;
  e2e)
    sync_up
    rsh "scripts/e2e.sh"
    ;;
  *)
    echo "usage: dev.sh {sync|test [args]|go <args>|vuln|fuzz <pkg> <Fuzz>|up|down [-v]|stop|logs|it [args] (KEEP_DB=1 keeps the DB up)|e2e}" >&2
    exit 2
    ;;
esac
