# Retrieval pilot plan (increment 4)

Status: plan only, not run. This is increment 4 of the
[project retrieval plan](project-retrieval-plan.md#4-controlled-pilot-and-savings-report). Running
it spends subscription plan quota. `hrn eval` runs only after a maintainer's yes to the run count
below, and never on the `api` backend or with an API key.

## Question

On held-out tasks in this repository, does required text retrieval (`simple-or-exact-v3`, or its
successor once #791 lands) lower the known model tokens per successful task, at equal or better
success? The answer settles two things:

- whether retrieval stays opt-in for one backend or can be offered as a default there;
- the keyword baseline that increment 6 (pgvector) must beat by at least 10% lower median tokens at
  equal success.

On subscription plans, fewer tokens stretch the quota rather than lower a bill. The report states
token and quota figures. Its USD figures are estimates, labelled as such.

## Step 0: the free dry run

Every setup step below ran on a loopback control service at
`014b9290d9eba27dea226832da6a11cf72162186`, with no model call: PostgreSQL, the service, a tenant
and a project, a corpus import and activation, ten `hrn knowledge search` previews, and a manifest
check with `hrn retrieval compare` (it validated 30 expected pairs). The service and container
were removed afterwards. What it found:

| Finding | Effect | Where |
|---|---|---|
| `hrn knowledge markdown` emits identifiers (`.env`, `.env.example`) and one entry (an `Authorization: Bearer` example) that the import deny list refuses. The service names one refusal per attempt. | The import failed 7 times. The review step dropped 6 identifiers and 1 entry: 98 entries became 97. | #825 |
| Search ORs every query word with the `simple` config (stop words kept) and has no score floor. | All 10 previews got a full packet of 7.6–8.1 KB, including the three tasks with no answer in the corpus. The answer's file was in the packet for 3 of the 7 corpus-answerable tasks. The answer text was there for 1. | #791 |
| Claude CLI runs carry token counts but no `usage_metadata`. `hrn retrieval compare` counts only attempts with reported metadata. | A claude arm would show 0/30 comparable pairs, so no token verdict. | #826 |
| Nothing builds the samples file from eval records, and nothing checks `corpus_digest` against the knowledge version. | Samples would be built by hand. | #827 |
| A `measured` comparison always reports `supplied_unverified`, and a pass becomes `pending`. | The tool alone cannot say "pass". | Decision 2 |

With retrieval as it is today, the pilot would mostly measure the cost of adding about 2,000 tokens
to every prompt.

## Prerequisites and go/no-go

- **#791, before runs:** drop common query words and return no context below a score floor.
- **#827, before the comparison:** `hrn retrieval samples` and `hrn retrieval corpus-digest`. It can land
  while runs are under way.
- **#825, not blocking:** until it lands, the review step drops refused identifiers and entries by
  hand and lists them in the report.
- **#826:** only for a later claude arm.
- **Go/no-go:** rerun step 0 at the pilot revision (still free). Go only if at least 4 of the 7
  corpus-answerable tasks get their answer's file in the packet, and at least one of the three
  no-answer tasks gets an empty packet. Otherwise report the preview and stop: the runs would
  measure retrieval noise.

## Fixed inputs

**Revision.** `R` is the full 40-character SHA of the `main` commit the pilot starts from, after its
prerequisites merge: `R=$(git rev-parse <commit>^{commit})`. The service, the knowledge revision,
the batch base and the manifest all compare it exactly. The corpus, the checkout the agents work
in, and the `hrn` binary used for import, search and eval all come from `R`.

**Corpus.** These 11 operator guides at `R`, imported as one version (at most 100 entries per
import; the dry run gave 98):
`AGENTS.md`, `CONTRIBUTING.md`, `README.md`, `server/README.md`, `docs/commands.md`,
`docs/batches.md`, `docs/evaluations.md`, `docs/backends.md`, `docs/permissions.md`,
`docs/run-records.md`, `docs/workspaces.md`. Default word limits. The rest of `docs/` stays out:
history (`PLAN.md`, `docs/plan-history.md`), the retrieval plans, this file, and guides left out
on purpose so that some answers fall outside the corpus. No entry is written for a task.

`corpus_digest` is the SHA-256 of a JSON array of `[key, source, sha256(content)]` rows, one per
entry, with content hashed as UTF-8 and written in lowercase hex. The array is encoded exactly:

- rows sorted by key, then source, then content hash, comparing UTF-8 bytes;
- compact: no spaces, `,` and `:` as separators;
- no HTML escaping, so `>` stays `>` (in Go: `SetEscapeHTML(false)`);
- non-ASCII characters written as themselves, in UTF-8;
- no trailing newline.

The digest is computed twice: over the reviewed `seed.json` that was imported, and over the
entries `hrn knowledge inspect` returns for the version. The two must be equal. In the dry run
they were.

**Tasks.** 10 read-only questions, each checked by a deterministic `expect` pattern (Go RE2):

- 7 have their answer in the corpus;
- 1 is answered only in Go code;
- 1 is answered in a guide outside the corpus;
- 1 asks about something that does not exist.

The task file is kept outside the repository until the report. A checkout contains everything
committed at `R`, and a read-only agent that searches for the question's words would find
committed prompts and answers. Its SHA-256 is
`438e83bbf6b6615ffd14c93f3d5ca0617583ba44324c65601293f0ab4aacc827`, and the report publishes the
file. The patterns were tested against right and wrong sample answers. Known limits: the
architecture question passes an answer that names two components, one of them right; the
no-such-command question passes an answer that invents flags and still says "does not exist"; the
duration patterns accept odd spellings such as "0.15 m". The report reads every passing answer to
those three tasks by hand and lists any it would fail. If an answer changes by `R`, the task is
replaced before the runs and the hash updated here, never after.

| Group A | Group B |
|---|---|
| `gate-timeout` | `markdown-max-words` |
| `fixture-port` | `job-cost-cap` |
| `postgres-boundary` | `retrieval-record` |
| `slot-lock-wait` | `watch-log-rotation` |
| `query-bytes` | `absent-command` |

**Variants.** `off` (`retrieval: off`) and `text` (`retrieval: required`, `retrieval_version` set to
the imported version). Both name themselves explicitly: they share backend and model, so their
default names would collide. Both use backend `codex`, model `gpt-6.1-sol`, the default effort
and lean setting, `mode: readonly`, `max_turns: 30`, `timeout: 10m`, `parallel: 1`, the default
`rerun_unstarted` (only cells whose backend never started run again), and no judge.

Of the two plans in use, only codex runs report usage the comparison accepts (agy's do too, but
agy is not part of this pilot). A claude arm (`sonnet`, 60 more runs) can follow #826 as a
separate pilot. The `api` backend stays out.

**Retry workflow experiment.** Increment 4 also asks for a separately labelled experiment with
the same finite retry policy in both variants. It is deferred to its own plan (decision 4): eval
cells run one attempt each by design, and the retrieval repair path of batch retries has not been
measured.

**Isolation failure.** A sample is an isolation failure in any of three cases:

- the cell changed or committed files in its read-only worktree;
- its packet came from outside the pilot's tenant, project or version;
- its tool calls read the pilot's own files (`$PILOT`, `$SECRETS`, the task file).

`compare --runs` already refuses the second case. The sample builder sets the first from the
batch record. The report checks the third by searching each run's `events.jsonl` for those
paths. Codex's read-only sandbox can read the whole disk, so separate folders lower this risk but
do not remove it.

## Counterbalancing and cache

The eval runner orders cells by task, then variant, then repeat, so a single eval would always run
`off` first, right before `text` warms the same prefix. The pilot therefore runs six evals of 10
cells, each with `repeats: 1`:

| Eval | Tasks | Matrix order | Manifest repeat |
|---|---|---|---|
| A0 | group A | off, text | 0 |
| B0 | group B | text, off | 0 |
| A1 | group A | text, off | 1 |
| B1 | group B | off, text | 1 |
| A2 | group A | off, text | 2 |
| B2 | group B | text, off | 2 |

Over the pilot, 15 pairs run off first and 15 run text first. Every task runs in both orders,
split 2:1. Evals run one after another, one cell at a time. Retrieval appends its packet after the
task text, so both variants share the cached system prefix.

For every cell the report records `cache_read_tokens / input_tokens` (codex counts cache inside
input) and its position in the eval. The first cell of an eval that starts after at least 15 idle
minutes is labelled cold. The report gives the median cached share per variant and per order. If
the variants' medians differ by more than 10 percentage points, the uncached input medians are
reported next to the gate figures, and the savings claim says so.

## Run count and stop rule

**60 runs:** 10 tasks × 2 variants × 3 repeats, all on the codex plan. The PR that adds this plan
gives the token and time estimate from recent run records. Stop rule: A0 runs alone first. If its
10 cells use more than 5 million model tokens together, stop and report before the other five evals.

## Commands

Git Bash on Windows. There are three separate folders, none inside another:

- `$CHECKOUT`: the detached worktree at `R`; the agents' cell worktrees go under it.
- `$PILOT`: specs, manifest, records and binaries. The specs hold the `expect` patterns.
- `$SECRETS`: credentials.

`$TASKS` is the private task file, kept outside `$CHECKOUT`. Credentials stay in files: they
never appear in a command line, a request file or the agents' environment.

Checkout, binaries and a separate hrn home (so `compare --runs` reads only pilot records):

```bash
R=$(git rev-parse "<commit>^{commit}")
git worktree add --detach "$CHECKOUT" "$R"
mkdir -p "$PILOT/bin" "$PILOT/home" "$PILOT/specs" "$PILOT/evals" "$PILOT/search"
(cd "$CHECKOUT" && go build -o "$PILOT/bin/" ./cmd/hrn && cd server && go build -o "$PILOT/bin/" ./cmd/hrn-server)
HRN="$PILOT/bin/hrn"; export HRN_HOME="$(cygpath -m "$PILOT/home")"
test "$(sha256sum < "$TASKS" | cut -d' ' -f1)" = 438e83bbf6b6615ffd14c93f3d5ca0617583ba44324c65601293f0ab4aacc827 || { echo "task file hash mismatch"; exit 1; }
codex login status   # must show a plan sign-in, not an API key
```

Service on loopback:

```bash
mkdir -p "$SECRETS" && for f in pgpass operator scaler; do python -I -c "import secrets;print(secrets.token_hex(24))" > "$SECRETS/$f"; done
printf 'POSTGRES_PASSWORD=%s\nPOSTGRES_DB=pilot\n' "$(cat "$SECRETS/pgpass")" > "$SECRETS/pg.env"
printf 'Authorization: Bearer %s\n' "$(cat "$SECRETS/operator")" > "$SECRETS/operator.header"
printf 'postgres://postgres:%s@127.0.0.1:55441/pilot?sslmode=disable\n' "$(cat "$SECRETS/pgpass")" > "$SECRETS/dburl"
docker run -d --name retrieval-pilot-pg --env-file "$SECRETS/pg.env" -p 127.0.0.1:55441:5432 postgres:16
export HRN_DATABASE_URL_FILE="$SECRETS/dburl" HRN_OPERATOR_TOKEN_FILE="$SECRETS/operator" HRN_SCALER_TOKEN_FILE="$SECRETS/scaler" HRN_LISTEN=127.0.0.1:8098
until docker exec retrieval-pilot-pg pg_isready -q -h 127.0.0.1 -U postgres; do sleep 1; done   # TCP: the image's init server answers only on its socket
"$PILOT/bin/hrn-server" migrate && { "$PILOT/bin/hrn-server" serve & SERVER=$!; }
(cd "$CHECKOUT" && bash scripts/wait-for.sh 127.0.0.1 8098 30)
export HRN_SERVER_URL=http://127.0.0.1:8098
```

Tenant and project. The responses carry credentials, so they go to `$SECRETS`, and only the IDs
and the tenant token are read out of them:

```bash
curl -s -H @"$SECRETS/operator.header" -d '{"name":"retrieval-pilot"}' "$HRN_SERVER_URL/v1/tenants" > "$SECRETS/tenant.json"
TENANT=$(python -I -c "import json,sys;d=json.load(open(sys.argv[1]));open(sys.argv[2],'w').write(d['tenant_token']);print(d['id'])" "$SECRETS/tenant.json" "$SECRETS/tenant_token")
printf '{"tenant_id":"%s","name":"harness","repository_url":"https://github.com/cristiangirlea/harness","base_ref":"%s","backend":"codex"}' "$TENANT" "$R" > "$PILOT/project.json"
curl -s -H @"$SECRETS/operator.header" -d @"$PILOT/project.json" "$HRN_SERVER_URL/v1/projects" > "$SECRETS/project.json"
PROJECT=$(python -I -c "import json,sys;print(json.load(open(sys.argv[1]))['project']['id'])" "$SECRETS/project.json")
```

Corpus: build, review, import, activate and digest:

```bash
printf '{"tenant_id":"%s","project_id":"%s","backend":"codex","model":"gpt-6.1-sol","revision":"%s"}' "$TENANT" "$PROJECT" "$R" > "$PILOT/scope.json"
python -I -c "import json,sys;d=json.load(open(sys.argv[1]));d.update(origin='reviewed_source',reviewed_by='maintainer',review_evidence='docs/retrieval-pilot.md corpus list at '+d['revision']);json.dump(d,open(sys.argv[2],'w'))" "$PILOT/scope.json" "$PILOT/base.json"
(cd "$CHECKOUT" && "$HRN" knowledge markdown -request "$PILOT/base.json" AGENTS.md CONTRIBUTING.md README.md server/README.md docs/commands.md docs/batches.md docs/evaluations.md docs/backends.md docs/permissions.md docs/run-records.md docs/workspaces.md) > "$PILOT/seed.json"
HRN_OPERATOR_TOKEN="$(cat "$SECRETS/operator")" "$HRN" knowledge import "$PILOT/seed.json" > "$PILOT/import.json"
VERSION=$(python -I -c "import json,sys;print(json.load(open(sys.argv[1]))['version_id'])" "$PILOT/import.json")
HRN_TENANT_TOKEN="$(cat "$SECRETS/tenant_token")" "$HRN" knowledge active "$PILOT/scope.json" > "$PILOT/active.json"
python -I -c "import json,sys;d=json.load(open(sys.argv[1]));d.update(operation='activate',version_id=sys.argv[3],expected_generation=json.load(open(sys.argv[2]))['generation']);json.dump(d,open(sys.argv[4],'w'))" "$PILOT/scope.json" "$PILOT/active.json" "$VERSION" "$PILOT/change.json"
HRN_OPERATOR_TOKEN="$(cat "$SECRETS/operator")" "$HRN" knowledge activate "$PILOT/change.json"
python -I -c "import json,sys;d=json.load(open(sys.argv[1]));d['version_id']=sys.argv[2];json.dump(d,open(sys.argv[3],'w'))" "$PILOT/scope.json" "$VERSION" "$PILOT/inspect.json"
HRN_TENANT_TOKEN="$(cat "$SECRETS/tenant_token")" "$HRN" knowledge inspect "$PILOT/inspect.json" > "$PILOT/inspect-out.json"
digest() { python -I -X utf8 -c "import json,sys,hashlib;e=json.load(open(sys.argv[1],encoding='utf-8'))['entries'];rows=sorted([x['key'],x['source'],hashlib.sha256(x['content'].encode()).hexdigest()] for x in e);print(hashlib.sha256(json.dumps(rows,separators=(',',':'),ensure_ascii=False).encode()).hexdigest())" "$1"; }
DIGEST=$(digest "$PILOT/inspect-out.json"); test "$DIGEST" = "$(digest "$PILOT/seed.json")" || { echo "stored version differs from the reviewed request"; exit 1; }
```

Until #825 lands, the import may answer 422 naming one refused identifier or entry. Drop it from
`seed.json`, add it to the report's list, and import again. Then step 0, one search per task, and
the go/no-go check on the recorded sources, sizes and answers:

```bash
python -I -X utf8 -c "import json,sys;s=json.load(open(sys.argv[1]));[json.dump(dict(s,query=t['prompt'],version_id=sys.argv[3]),open(sys.argv[4]+'/'+t['id']+'.json','w',encoding='utf-8'),ensure_ascii=False) for t in json.load(open(sys.argv[2],encoding='utf-8'))]" "$PILOT/scope.json" "$TASKS" "$VERSION" "$PILOT/search"
for f in "$PILOT"/search/*.json; do HRN_TENANT_TOKEN="$(cat "$SECRETS/tenant_token")" "$HRN" knowledge search "$f" > "${f%.json}.out"; done
```

The six specs and the manifest. `dir` must be a Windows path inside JSON, hence `cygpath -m`:

```bash
export PILOT_DIR="$(cygpath -m "$CHECKOUT")" TENANT PROJECT R VERSION DIGEST
python -I - "$TASKS" "$PILOT" <<'EOF'
import json, os, sys; E = os.environ; t = {x["id"]: x for x in json.load(open(sys.argv[1], encoding="utf-8"))}
A = ["gate-timeout", "fixture-port", "postgres-boundary", "slot-lock-wait", "query-bytes"]; B = [k for k in t if k not in A]
off = {"name": "off", "backend": "codex", "model": "gpt-6.1-sol", "retrieval": "off"}; txt = dict(off, name="text", retrieval="required", retrieval_version=E["VERSION"])
base = {"dir": E["PILOT_DIR"], "mode": "readonly", "parallel": 1, "timeout": "10m", "max_turns": 30, "retrieval_tenant": E["TENANT"], "retrieval_project": E["PROJECT"], "retrieval_revision": E["R"]}
for n, g, m in [("A0", A, [off, txt]), ("B0", B, [txt, off]), ("A1", A, [txt, off]), ("B1", B, [off, txt]), ("A2", A, [off, txt]), ("B2", B, [txt, off])]:
    json.dump(dict(base, repeats=1, matrix=m, tasks=[t[k] for k in g]), open(f"{sys.argv[2]}/specs/{n}.json", "w", encoding="utf-8"), indent=2, ensure_ascii=False)
json.dump({"id": "retrieval-pilot-v1", "evidence_kind": "measured", "revision": E["R"], "corpus_digest": E["DIGEST"], "held_out": True, "cache_conditions": "Six evals swap off/text order by group and repeat (15/15 pairs); cached share and position recorded per cell; cold = first cell after 15 idle minutes.", "spec": dict(base, repeats=3, matrix=[off, txt], tasks=list(t.values()))}, open(f"{sys.argv[2]}/manifest.json", "w", encoding="utf-8"), indent=2, ensure_ascii=False)
EOF
echo '[]' > "$PILOT/empty.json"; "$HRN" retrieval compare --manifest "$PILOT/manifest.json" --samples "$PILOT/empty.json"   # validates: 30 expected pairs
```

The runs, only after the maintainer's yes. A0 runs alone for the stop rule. The variants name
`codex`, so no default backend applies. A token in the environment is withheld from the agent
processes (`_TOKEN` suffix):

```bash
export HRN_TENANT_TOKEN="$(cat "$SECRETS/tenant_token")"
"$HRN" eval --json "$PILOT/specs/A0.json" > "$PILOT/evals/A0.json"
# Stop rule. Codex input already includes cached tokens, so input plus output is the model-token total.
A0_TOKENS=$(python -I -c "import json,sys;r=json.load(open(sys.argv[1]));print(sum(a['result'].get('input_tokens',0)+a['result'].get('output_tokens',0) for t in r['results'] for a in t.get('attempts',[])))" "$PILOT/evals/A0.json")
test "$A0_TOKENS" -le 5000000 || { echo "A0 used $A0_TOKENS tokens: stop and report"; exit 1; }
for e in B0 A1 B1 A2 B2; do "$HRN" eval --json "$PILOT/specs/$e.json" > "$PILOT/evals/$e.json" || break; done
```

Samples and comparison (#827). Every sample's `online` is
`{"model_tokens": 0, "cost_usd": 0, "duration_ns": <packet search time, 0 for off>}`. Text
search makes no model call, and an absent value would make every pair non-comparable. Both
preparation files are zero for the same reason (import and activation make no model call):

```bash
echo '{"model_tokens":0,"cost_usd":0,"duration_ns":0}' > "$PILOT/prep.json"
"$HRN" retrieval samples --manifest "$PILOT/manifest.json" $(for e in A0:0 B0:0 A1:1 B1:1 A2:2 B2:2; do printf -- '--eval %s:%s ' "$(python -I -c "import json,sys;print(json.load(open(sys.argv[1]))['id'])" "$PILOT/evals/${e%:*}.json")" "${e#*:}"; done) > "$PILOT/samples.json"
"$HRN" retrieval compare --manifest "$PILOT/manifest.json" --samples "$PILOT/samples.json" --baseline-preparation "$PILOT/prep.json" --candidate-preparation "$PILOT/prep.json" --runs --json > "$PILOT/compare.json"
"$HRN" version > "$PILOT/hrn-version.txt"; git -C "$CHECKOUT" rev-parse HEAD >> "$PILOT/hrn-version.txt"
```

A later `hrn` may be used for `samples` and `compare` only (they make no model call). `--runs`
checks every cell against its run and batch records in `$HRN_HOME`: backend, model, original
prompt, revision, version pin and scope, and the recorded check outcome.

Tear down:

```bash
kill "$SERVER"; docker rm -f retrieval-pilot-pg
git worktree remove --force "$CHECKOUT"; rm -rf "$SECRETS"
```

## How the report decides

The report reads `compare.json`. Counts are pairs (task × repeat, 30), not runs. Its `gate` stays
`pending` for measured input (decision 2), so the verdict comes from `gate`, `reason` and the
counts:

| Comparison says | Verdict |
|---|---|
| `gate: reject` (a pass in `off` became a fail in `text`, or an isolation failure) | Retrieval stays off for codex. List the pairs. |
| Reason starts "incomplete comparison coverage" | Pairs are missing. Run only the cells that never started, at the same `R`, then read again. A cell that ran and failed is a result and is never rerun. |
| Reason "insufficient comparable usage or improvement" with fewer than 30 comparable pairs | No token verdict. List the pairs without reported usage (for example a codex timeout before its usage event). |
| Same reason, both variants at zero successes | Neither variant answers these tasks: the task set is wrong, not retrieval. Replace the tasks and repeat the pilot. |
| Reason starts "higher success" | Supported: retrieval raises success. One extra passing cell is enough for this reason, so the report states the margin. |
| Reason "equal success and a N percent reduction … above the threshold" | Supported: token saving of N% for codex `gpt-6.1-sol` on these tasks. The threshold is the larger of 10% and the baseline's movement between repeats. |
| Equal success below the threshold, or the baseline passed in fewer than two repeats | Inconclusive: retrieval stays opt-in. Next, 20 tasks and 5 repeats, not a broad claim. |

"Supported" also needs these binding checks, each recorded in the report:

1. The digest of the reviewed `seed.json` equals the digest of the entries `hrn knowledge inspect`
   returns, and both equal the manifest's `corpus_digest`.
2. The task file's SHA-256 equals the one in this plan.
3. `compare --runs` succeeded, and `hrn-version.txt` shows a build of `R`.
4. The cached-share check above holds, or its deviation is stated.

The report also gives, per variant: success rate, median and p95 model tokens, tokens per success
(failures counted), attempts, p50/p95 time, packet bytes, cached share, and the relevant-context
hit rate (the answer's file among the packet sources) per task kind. The USD figures are labelled
as plan estimates. The report goes to `docs/retrieval-pilot-report.md`, with the task file,
manifest, samples and comparison JSON beside it. Run records stay local, and their IDs are listed.

## Maintainer decisions

1. **Run yes:** 60 codex runs as above, after #791 and #827 and a passing go/no-go. Running at
   today's ranking means overriding the go/no-go, which failed in the dry run.
2. **Binding:** the comparison downgrades measured input to `pending` until corpus and check
   binding is built. Either accept the four recorded checks for this pilot (recommended: `--runs`
   already verifies run identity, pins, scope and check outcomes) or build the binding first.
3. **Claude arm:** after #826, as a second 60-run pilot on the Claude plan.
4. **Retry workflow experiment:** its own plan after this pilot reports, or dropped.
