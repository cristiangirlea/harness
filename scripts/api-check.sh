#!/usr/bin/env bash
# name: api-check
# summary: Exercise real API runs, continuation and context editing at the owner's expense.
# effects: read, write-workspace, exec, network
# parameters: SMALL_MODEL:string=claude-haiku-4-5; MODEL:string=claude-sonnet-5; MAX_COST:number=1; ANTHROPIC_API_KEY:secret=optional; ANTHROPIC_AUTH_TOKEN:secret=optional
# example: none (spends money)
# hash: none
# The api backend's first real runs, in one command, for the day a key exists.
# Everything the backend does has only been tested against a fake server; this
# answers three open questions in PLAN.md at once:
#
#   1. does a plain read-only run work against the real API,
#   2. does --continue pick a finished run back up,
#   3. does server-side context editing help or hurt: the same long task with
#      it on and off, compared on cache reads and cost, with a count of the
#      reports the server sent about what it cleared.
#
# Usage: ANTHROPIC_API_KEY=... scripts/api-check.sh
# Optional: SMALL_MODEL (steps 1-2, default claude-haiku-4-5), MODEL (step 3,
# default claude-sonnet-5), MAX_COST per step-3 run in USD (default 1).
# Worst case is about 2*MAX_COST + 0.5 USD. Runs are recorded in a temporary
# HRN_HOME, printed at the end, so they never mix with your own.
set -euo pipefail

if [ -z "${ANTHROPIC_API_KEY:-}${ANTHROPIC_AUTH_TOKEN:-}" ]; then
	echo "set ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN) first" >&2
	exit 2
fi
cd "$(dirname "$0")/.."

work="$(mktemp -d)"
export HRN_HOME="$work/home"
hrn="$work/hrn"
go build -o "$hrn" ./cmd/hrn

small="${SMALL_MODEL:-claude-haiku-4-5}"
model="${MODEL:-claude-sonnet-5}"
cap="${MAX_COST:-1}"

# run runs hrn, shows the end of its output, and leaves the run id in $id.
# The id is read from the output rather than from the newest directory: two
# runs started in the same second sort by their random suffix.
id=""
run() {
	local out
	out="$("$hrn" run "$@" 2>&1)" || true
	printf '%s\n' "$out" | tail -n 3
	id="$(printf '%s\n' "$out" | awk '$1 == "run" && NF == 2 { last = $2 } END { print last }')"
	if [ -z "$id" ]; then
		echo "no run id in the output above; stopping" >&2
		exit 1
	fi
}

# reports counts the context-management reports the server sent in a run.
reports() { grep -c 'server context management' "$HRN_HOME/runs/$1/events.jsonl" || true; }

echo "== 1. a read-only run on $small"
run -b api -m "$small" --mode readonly --max-turns 10 --max-cost 0.25 \
	"List the Go packages under internal/, one line each: the package and what it is for."
first="$id"

echo
echo "== 2. continue run $first"
run --continue "$first" --max-turns 10 --max-cost 0.25 \
	"Which of those packages has the most _test.go files? Answer with the package and the count."

echo
echo "== 3. context editing on and off, on $model (at most \$$cap each)"
long="Read every non-test Go file under internal/agent and internal/batch, one file at a time, \
then list each exported function with a one-line description of what it does."
for edits in on off; do
	# A plain string, not an array: an empty array is "unbound" under set -u
	# in bash before 4.4, which is the bash macOS ships.
	flag=""
	[ "$edits" = off ] && flag="--no-context-edits"
	echo "-- context editing $edits"
	run -b api -m "$model" --mode readonly --max-turns 40 --max-cost "$cap" ${flag:+"$flag"} "$long"
	echo "   server reports of cleared context: $(reports "$id")"
done

echo
echo "Compare the two step-3 summary lines on cache read and cost. Records: $HRN_HOME"
echo "Replay one with: HRN_HOME=$HRN_HOME $hrn show <run-id>"
