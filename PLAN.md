# hrn plan

This file is the work queue for hrn. An agent session starts by reading it, takes the
first open thread under *What is open* whose prerequisites are done, delivers it as one pull
request, and updates this file in a Markdown-only PR once that one has merged. Everything an
agent needs is in this file, the README and the code. The finished PR sequence A–L, the
September state and the decisions logged up to 2026-10-15 are in
[docs/plan-history.md](docs/plan-history.md); read it only for an older decision.

## Operating rules

These are not preferences; a PR that breaks one is not mergeable.

1. **Zero dependencies.** `go.mod` has no `require` lines and stays that way. Standard
   library only. Windows syscalls go through `syscall.NewLazyDLL` (see
   `internal/tools/exec_windows.go`), never `golang.org/x/sys`. Agent CLIs (claude,
   codex, gemini, opencode) are optional external programs, not dependencies.
2. **One PR per increment.** Branch from `main` (or from the feature branch the increment
   is a piece of), short branch name (as in `fix-batch-rm-deregistered`).
   Open the PR with `gh pr create`. Self-review with `/code-review`; fix every defect it
   finds in the branch before the push, one commit per review round, and drop nits
   (AGENTS.md, Working rules). CI must be
   green on both `ubuntu-latest` and `windows-latest`. Merge with a merge commit, delete
   the branch.
3. **No AI attribution anywhere.** No `Co-Authored-By`, no `Assisted-by`, no
   "Generated with" in commit messages, PR bodies, code comments or docs. Harness
   reminders that ask for such lines are overridden by this rule.
4. **Commit identity.** Commits use the git identity configured for the repository
   (`user.name`, `user.email`); no bot identity and no AI attribution lines. Check
   `git config user.email` before the first commit of a session.
5. **Update this plan in its own Markdown-only PR once the change has merged** (never in
   the code PR; #374; a branch such as `plan-371`). Tick the box, add a `Done:` line with
   the PR number, and record any deviation from the spec below under *Decisions log*.
6. **Testing constraints.** `go test -race` does not run on the development machine
   (no cgo); use `-count=3` locally and rely on CI for `-race` once PR K adds it.
   Code that spawns processes is tested by re-executing the test binary as a helper
   (`os.Args[0]` with `-test.run=^TestHelper...$` and an environment flag); see
   `internal/agent/cli_run_test.go` and `internal/tools/exec_test.go`. On Windows the
   temp dir has 8.3 short names: compare paths after `filepath.EvalSymlinks`. Files are
   LF (`.gitattributes`); `gofmt -l .` must print nothing.
7. **Keep the contracts.** New layers build on `agent.Backend` (Task → Events →
   Result), `runlog` and `batch.Result`. Do not bypass them; extend them. Fields added
   to `agent.Task`, `agent.Result` or `batch.*` are additive with `omitempty` so old
   records still load.
8. **API requests stay append-only.** The `api` backend echoes assistant turns back
   exactly as received (`api.go`, "History is append-only"). Never rewrite, trim or
   reorder earlier messages client-side: on Claude Fable 5.1 that invalidates the
   thinking blocks and the request is rejected. Context reduction is done server-side
   (PR D).

## Definition of done (every PR)

- [ ] The full local checks, as AGENTS.md's "Build and test" lists them (`/check` runs them).
- [ ] The default branch merged in once and the full checks run on that merge before the push (CI
      tests the merge with `main`).
- [ ] An independent review (`/code-review`): every defect, security point or missing test it
      finds in the branch's own code fixed in the branch before the push, blocking or not. Nits
      dropped (at most a line in the PR); only work outside the branch or a maintainer's decision
      becomes an issue, never a follow-up of a follow-up. Reviews are text and commits are
      changes: everything found before a commit exists goes into that one commit, and each later
      round's fixes into one more.
- [ ] CI green on ubuntu and windows.
- [ ] The relevant guide in `docs/` updated for any new flag, field, command or status, and
      README when the overview changes.
- [ ] Merged with a merge commit; branch deleted.
- [ ] Then this file, in a separate Markdown-only PR (never in the code PR): box ticked,
      `Done: PR #n, <date>` under the PR heading, deviations in the *Decisions log*; a fix
      with no item here gets a Decisions-log entry naming the issue and the PR.

## What is open

Every lettered increment is done. These are the threads the log leaves behind, in the
order they are worth taking:

1. ~~**One options struct.**~~ Done as PR L (#16).
2. **Measure the context-editing default.** Under consideration, with item 5 (#116).
3. ~~**Record what the server cleared.**~~ Done as PR R, verbatim, since the shape is
   undocumented here. Read the first real reports and decide whether to parse them.
4. ~~**The eval judge.**~~ Done as PR M, with `expect`, a tool-call column and the count
   of passes that changed nothing.
5. **A real api run.** Under consideration, with item 2 (#116).
6. ~~**A `batch` tool in `hrn mcp`.**~~ Done as PR N: `batch`, `batch_show` and
   `batch_merge`.
7. ~~**Release binaries.**~~ Done as PR P. No release has been cut: that, and making the
   repository public, are a maintainer's to do.
8. ~~**`hrn eval diff <a> <b>`.**~~ Done as PR Q, with `hrn eval list`.
9. ~~**A live view.**~~ Done as PR V (#34): `hrn watch`, `hrn show -f`, and `--lan` for a
   phone, read-only and without a login for now.
10. ~~**An access key for `hrn watch --lan`**~~ Done as PR W (#39, closes #33): `--lan=NAME`, a QR
    code of the phone link, phone-sized lists, and the key at the door for anything that would
    act. Viewing stays keyless.
11. **A status bot**: something on screen that says what hrn is doing and asks for the decisions
    it waits on (an approval, a failed gate: retry or stop, merging the branches that passed), and
    acts on the answer. Its eyes are `hrn watch`'s streams (`/stream/index`, `/stream/run/<id>`,
    `/stream/batch/<id>`), its hands `hrn mcp`'s tools (run, batch, batch_show, batch_merge;
    approvals already reach an MCP host by elicitation). The missing piece is a place where
    pending decisions wait: today an approval is a prompt in the terminal of `hrn run` or a
    question to the MCP host, and nothing else can answer it. Its actions go through hrn watch's
    door: they must send `Options.Key` in `X-Hrn-Watch-Key`, which hrn watch does not set yet.
    - ~~Part 1, her eyes and voice.~~ Done: PR #89, 2026-10-11 (closes #59). The bot is **Ada**, a
      female robot scarlet macaw: `hrn bot`, `/bot`, sentences in plain UK English made in Go,
      speech, notifications, skip, snooze, mute, push-to-talk for commands that steer her only.
    - Her purpose is super intelligence. In this order: her own recorded voice (#83); a live map
      of where the work is across the entire workflow, every repository and every stage from idea
      to release (#90); then decisions and actions, behind #38; then planning and improving hrn
      itself (the self-improvement loop).
    - [x] Recording her voice ([#91](https://github.com/cristiangirlea/harness/issues/91), the
      first slice of #83). Done: PR #93, 2026-10-11. `/voice`, for this computer only: 523 lines
      to read (everyday ones mixed with Ada's own, made by her wording functions) and a mode that
      records talk, saved as WAV with `metadata.csv` into a folder the speaker picks. Next in #83,
      each download approved by a maintainer first: a taste with Chatterbox, transcribing talk
      with Whisper, training with Piper, and Ada speaking with the voice.
    - Ada where the user works, after the map of #90 and behind the masking of
      [#304](https://github.com/cristiangirlea/harness/issues/304) (secrets masked in everything
      hrn watch, `hrn show`, the renderer and the bot display): a Claude Code side pane with Ada
      reacting to the live session, the map in short form and the session meter, the session
      recorded under `~/.hrn` so reports and hrn watch include the user's own sessions
      ([#305](https://github.com/cristiangirlea/harness/issues/305)); and Ada gamified, experience
      gathered by working with the AI, from real outcomes only, with Ada's own growth beside it
      ([#306](https://github.com/cristiangirlea/harness/issues/306); the four maintainer
      decisions answered on the issue 2026-10-25, the design in `docs/experience.md`: PR #642,
      2026-10-25; the ledger slice next). #304 done: PR #314, 2026-10-14.
12. **Guard rails**, in this order:
    - ~~[#36](https://github.com/cristiangirlea/harness/issues/36): what agents may do.~~
      Done as PR X (#44, closes #36): the guarded environment for every backend, claude's
      deny rules, and the api loop's guard extended to secret files and the guard's own
      variables. Left: codex's own sandbox under the guard, and per-CLI rules for gemini
      and opencode (their runs say what is unguarded).
    - ~~[#37](https://github.com/cristiangirlea/harness/issues/37): spending.~~ Closed
      2026-10-16 as not needed while every run is on a plan backend (Decisions log).
    - [#38](https://github.com/cristiangirlea/harness/issues/38): control. Confirmations,
      an allow-list and an audit log for the bot's and the phone's actions, before item 11.

13. **Ada OS's engine interface** ([#49](https://github.com/cristiangirlea/harness/issues/49)):
    Ada, a separate private repository, runs hrn as its engine and needs a versioned interface
    instead of terminal output. Four PRs, in this order:
    - [x] Reasoning effort for codex and claude. Done: PR #50, 2026-10-11.
    - [x] Run records surface write failures. Done: PR #51, 2026-10-11.
    - [x] Agent CLIs run in a contained process tree. Done: PR #52, 2026-10-11.
    - [x] `hrn exec --protocol v1`, documented in `docs/exec-protocol.md`. Done: PR #53, 2026-10-11.
14. ~~**Carry on at a plan's usage limit**~~ ([#43](https://github.com/cristiangirlea/harness/issues/43)).
    Done as PR #46, 2026-10-11: a `limited` status, claude and codex carrying on each
    other's runs, reset times remembered in `limits.json`, `no_switch`. See the Decisions
    log, 2026-10-11.
15. **What a run was for, and what it costs** (from classify-sessions, a session analytics tool):
    its purpose rules and its honest cost ("unknown cost is never zero"), brought into hrn. The
    work goes through `hrn batch`, so hrn watch shows it as batches. Three PRs, in this order:
    - [x] A batch's runs link to their batch on hrn watch ([#61](https://github.com/cristiangirlea/harness/issues/61)).
      Done: PR #67, 2026-10-11.
    - [x] `internal/purpose` and `hrn report --by purpose` ([#62](https://github.com/cristiangirlea/harness/issues/62)).
      Done: PR #121, 2026-10-12.
    - [x] hrn watch: what the listed runs were for and what they cost, as a lower bound
      ([#63](https://github.com/cristiangirlea/harness/issues/63)); a first, budget-free step
      towards #37. Done: PR #123, 2026-10-12.
16. **Disk: clear what merged work leaves behind** (after two work drives filled with worktrees
    and Rust build folders). Deterministic, no model involved, and nothing local goes before its
    merge is proven. Three PRs, in this order:
    - [x] `hrn gc`: worktrees whose pull request is proven merged
      ([#78](https://github.com/cristiangirlea/harness/issues/78)). Done: PR #95, 2026-10-12.
    - [x] `hrn gc`: idle build caches (`CACHEDIR.TAG`), oldest first when a drive runs low
      ([#79](https://github.com/cristiangirlea/harness/issues/79)). Done: PR #131, 2026-10-13.
    - [x] `hrn batch merge --rm` ([#80](https://github.com/cristiangirlea/harness/issues/80)).
      Done: PR #221, 2026-10-13.
    - [x] `hrn ws`: reusable workspaces per repository
      ([#133](https://github.com/cristiangirlea/harness/issues/133)). Done: PR #144, 2026-10-13.

17. **Durable project workers and project retrieval** ([#142](https://github.com/cristiangirlea/harness/issues/142)):
    The work adds a separate `server/` module with a PostgreSQL control service and project
    worker, durable checkpoints in the api loop, `hrn remote` and its MCP tools, and
    development charts; it wires approved project knowledge with text retrieval into runs,
    batches and evals.
    Implemented against `634507a` and kept as local snapshot commit `8fc689e`, the work
    lands onto today's main in slices through the feature branch `durable`.
    - [x] [#134](https://github.com/cristiangirlea/harness/issues/134) docs: the accepted spec, the retrieval plan and a PLAN.md item. Done: PR #143, 2026-10-13.
    - [x] [#135](https://github.com/cristiangirlea/harness/issues/135) api: durable execution checkpoints for the api loop. Done: PR #150, 2026-10-13.
    - [x] [#136](https://github.com/cristiangirlea/harness/issues/136) agent: usage metadata with backend-aware token semantics. Done: PR #158, 2026-10-13.
    - [x] [#137](https://github.com/cristiangirlea/harness/issues/137) server: PostgreSQL control service and project worker, a separate module. Done: PR #159, 2026-10-13.
    - [x] [#138](https://github.com/cristiangirlea/harness/issues/138) remote: `hrn remote` and the MCP remote tools. Done: PR #175, 2026-10-13.
    - [x] [#139](https://github.com/cristiangirlea/harness/issues/139) deploy: development charts, image and chart verifier. Done: PR #174, 2026-10-13.
    - [x] [#140](https://github.com/cristiangirlea/harness/issues/140) knowledge: approved project knowledge and text retrieval in the control service. Done: PR #181, 2026-10-13.
    - [x] [#141](https://github.com/cristiangirlea/harness/issues/141) retrieval: wiring in runs, batches, evals and MCP, with the fixture comparison. Done: PR #186, 2026-10-13.
    - [x] `durable` into main. Done: PR #266, 2026-10-14.

18. **Cheaper runs: shape what the agent reads, route by evidence, keep the scripts it retypes**.
    In the sample in `docs/evaluations.md` (nine runs over a week), output is about 2% of the
    tokens moved and cache reads of the growing context are most of the rest, so shorter answers
    can save little. The levers are what the agent reads, which effort and model answer, and what
    it writes twice. In this order, each its own PR:
    - [#295](https://github.com/cristiangirlea/harness/issues/295): measure first, from the run
      records: tool-result bytes by shape, and the inline scripts written again and again.
      Done: PR #330, 2026-10-14. The page says shaping can touch about 10% of the context
      carried (terminal output most of it) and kept scripts under half a percent.
    - [#151](https://github.com/cristiangirlea/harness/issues/151) and
      [#264](https://github.com/cristiangirlea/harness/issues/264): an unknown cost is never
      zero and a cache read is counted once, before any saving is claimed. #151 done:
      PR #313, 2026-10-14. #264 done: PR #321, 2026-10-14.
    - [#296](https://github.com/cristiangirlea/harness/issues/296): large tool results shaped
      per kind (tests, logs, JSON, tables, terminal), the original kept behind a handle,
      invariants checked, measured by an eval in hrn-evals. Done: PR #335, 2026-10-14, for
      terminal, log and test output, off by default; the eval and its run wait for a maintainer.
    - [#297](https://github.com/cristiangirlea/harness/issues/297): a routing table from eval
      results (purpose to backend, model and effort), applied when a run names none and
      recorded in the run; effort first, then model; the AdaNext allocation as the first table,
      local models once a hrn-evals suite has measured them (the eval #224 asks for). Done:
      PR #329, 2026-10-14. The table in force: hrn-evals eval `f353a` of 2026-10-15 (six tasks,
      Sol and Sonnet at medium, Luna and Haiku at low, three repeats, 64 of 72 passed): Sol for
      debug, implement and research, Luna low for test and unknown; recorded in hrn-evals
      PR #12 (`results/2026-10-15-routing.md`), after the codex-only first table of eval
      `b123c` (PR #11). Claude never wins a tie because codex stores no cost data and the
      eval table read that as zero until #378 (PR #383) priced codex at list price too; the
      next hrn-evals run ranks the two on the same footing.
    - [#298](https://github.com/cristiangirlea/harness/issues/298): a header, rules and a
      CI-verified index for the scripts agents write twice, kept under `scripts/`. Done:
      PR #317, 2026-10-14.
    The gate side lives in adanext-exec (output shaping, and the hint when a kept script covers
    a command: adanext-exec #1 and #2); the rule for claiming a saving and the page of where
    each model loses live in hrn-evals (#3 and #4).
    Second round (follow the more mature patterns where they beat ours), each its own PR, in
    this order:
    - [#436](https://github.com/cristiangirlea/harness/issues/436): adversarial probes and a fuzz
      target for the summary invariants, before shaping runs by default. Done: PR #468,
      2026-10-16.
    - [#437](https://github.com/cristiangirlea/harness/issues/437): a bounded originals store
      that refuses rather than evicts, and `read_original` by line range or JSON pointer;
      in the same PR as [#337](https://github.com/cristiangirlea/harness/issues/337), which
      touches the same lookup (originals across a continuation). Done: PR #601, 2026-10-19.
    - [#336](https://github.com/cristiangirlea/harness/issues/336): JSON and tabular summaries.
      Done: PR #487, 2026-10-16.
    - [#435](https://github.com/cristiangirlea/harness/issues/435): hrn owns the terminal for
      every backend, so shaped output reaches claude, codex and agy runs too.
    - [#439](https://github.com/cristiangirlea/harness/issues/439): an advisory Claude Code hook
      that names a kept script covering a command, until the gate does it (adanext-exec #2).
      Done: PR #481, 2026-10-16.
    - [#438](https://github.com/cristiangirlea/harness/issues/438): measure what tool definitions
      cost per api turn, and trim them only if it pays. Measurement and byte budget done: PR
      #492, 2026-10-16; trimming deferred (2026-10-19).
    - hrn-evals #30: a routing suite of real tasks, two or more per purpose, with multi-step
      requirements, so the table rests on more than ten small fixtures. Done: hrn-evals PR #39,
      2026-10-16.
    - [#441](https://github.com/cristiangirlea/harness/issues/441): comments (whole-line comments
      are 19% of non-test Go bytes, paid by every agent that reads a file). The rule and a comment
      check with a baseline land before every slice above, so new code stops growing them; the
      package-by-package cleanup lands after all of them. Done: PR #457, 2026-10-16 (the rule and
      the check); the cleanup in PRs #602 and #604, 2026-10-19, and #615, #617, #618 and #620,
      2026-10-20. Comments are now 8.8% of non-test Go bytes and the baseline holds only its
      header.
    - [#488](https://github.com/cristiangirlea/harness/issues/488): a run loads only the MCP
      servers, plugins and skills its task needs, on claude and codex. Done: PR #595, 2026-10-19.

Left out on purpose: real sandboxing for yolo (a project, not a PR; the guard says it is a guard
rail), a daemon or queue (batch plus cron or the MCP tool covers it), and MCP over HTTP (every host
met so far speaks stdio). A web dashboard was on this list ("report and batch show answer the
questions") until a live view was asked for on 2026-10-10: neither of them follows a run while it
happens.

## Under consideration

Things kept but not active (2026-10-16). Each says what would bring it back.

- **Context editing against the prompt cache**
  ([#116](https://github.com/cristiangirlea/harness/issues/116)), with What is open items 2 and 5.
  The api backend asks for server-side clearing of old tool results by default, and a clear
  invalidates the cached prefix from that point; the balance has never been measured on a real
  long run, and the backend has never run against Anthropic's API (everything in it is covered by
  a fake server). #116 asks for the same long task with edits on and off, a fixed model and
  settings, and a cost cap and decision criteria set before any real call, judged on modelled
  usage and cost, task success and the context trajectory, then a published decision on the
  default. `scripts/api-check.sh` runs that comparison and the first real run in one command once
  a key is set; compare the first real estimates with the console's bill. Back when an API key or
  a real Anthropic endpoint is used: today every run is on plan backends (the claude, codex and
  agy CLIs).
- **Measuring `compact_window`**, the last item of
  [#208](https://github.com/cristiangirlea/harness/issues/208) (closed by PR #450): measure it on
  a long real task (cache reads, turns, result checks) before recommending a default. Back when
  a maintainer approves the plan usage for that run.

## Decisions log
- 2026-10-30: maintainer decisions on four open questions (#462, #464, #562, #671; PRs #711, #712,
  #713). A log summary keeps the two lines after an error and the trace after it up to the next log
  entry, at most 20 lines, with a count valid recomputes; an error word wrapped across terminal
  lines is accepted as a limit of line-based shaping. The eval judge's agent runs with a scratch
  HRN_HOME, leaving worktree names and reading the real home by path as documented limits. Slot
  caches are never pruned per dependency, since Cargo does not touch artifacts it reuses; with
  --slot-cap set, an idle, unlocked, unleased slot cache still above the cap after any incremental
  trim is removed whole.
- 2026-10-30: a contributor's review, @mattabate (#700, #701, #702 and three points sent privately;
  PRs #707-#710). edit_file follows a CRLF file without doubling carriage returns, grep names a
  file a long line cut short, and glob commas outside braces are literal. Display masking covers
  more secret names and token forms, records under HRN_HOME are created owner-only (folders 0700,
  files 0600, existing ones unchanged), and a public watch stream ends when its access token
  expires.
- 2026-10-30: shaping fixes from the adversarial probes (#461, #463, #465, #482, #483, #484, #619;
  PR #694, `docs/tool-shapes.md`). `valid` reads numbers from the colour-stripped original, as
  whole tokens and as digit runs, and exempts only the lines it recomputes; failures are qualified
  by package and parents reset per package; the oracle catches invented error lines; a large JSON
  sample is dropped rather than failing the summary. Terminal summaries keeping more middle lines
  (#619, part 2) need a new selection rule, not a fix, and were left; #462 and #464 wait for a
  maintainer's decision.
- 2026-10-30: codex reads every form of its config's MCP servers and follows codex's own trust
  (#495; PR #693, `docs/backends.md`): project configs from the run directory up to the root count
  only when the run directory, the root or a worktree's main root is trusted; a `--mcp` name that
  collides with a config server is refused, one from `~/.hrn/mcp.json` skipped; `Authorization:
  Bearer ${VAR}` becomes `bearer_token_env_var`; a `.cmd` shim refuses cmd.exe's special
  characters.
- 2026-10-30: smaller leftovers closed (#180, #455, #474, #475, #480, #489, #498, #542, #564, #684,
  #704; PRs #692, #695-#699, #703, #705). Originals are hard-linked across a continuation and a
  refused save is reported; eval route and diff compare lean, shaping, retrieval and a task's
  effective run options; the scripts hint catches more inline forms and `hrn runs prune` trims its
  log; comment-check precision; tool-shapes reports api cache writes; the knowledge tests the
  review named (the PostgreSQL ones run in CI); three Windows timing tests given room without
  losing what they test.
- 2026-10-30: batch gate temp folders sit outside every git work tree (#683; PR #687,
  `docs/batches.md`). Since PR #673 they were made under `<repo>/.hrn`, so tests walking up from
  `t.TempDir()` found the real checkout (a gc test listed its real worktrees). `.hrn-gate-tmp`
  now sits above the outermost repository on the same volume, used only if it is a real folder we
  own with no group or other permissions, else the system temp folder.
- 2026-10-30: review leftovers closed (#372, #425, #476, #496, #497, #499; PRs #685, #686, #688,
  #689, #690). runlog refuses records when no home can be resolved and `.`/`..` as run ids; the
  Backends card dedupes and shows unknown for a malformed file; worktree list/remove retry the
  commondir race in gc too, and a batch creates its task branch first so a failed add deletes only
  what it made; codex MCP servers expand every field from the guarded environment and may not set
  `GIT_CONFIG_*`; `skills` is reported as not enforced where it is not, and a continuation that
  keeps a non-lean session says it turned lean (a record without `lean` counts as lean only from
  2026-10-19 10:03 UTC, when PR #595 made lean the default).
- 2026-10-30: practice, slice 2 of the experience game (#306; PR #681, `docs/experience.md`).
  One exercise a day in `$HRN_HOME/game/reps/<date>.json`, picked by an FNV-1a seed of the UTC
  date and the attempt; the reveal is returned only once an answer is saved, and material and
  reveal are masked before they are written. The pick lists pull requests once per repository
  and state and fetches material for the chosen candidate only; every reference and repository
  name is validated before it reaches `gh`. Recall reads this file's dated entries as well as
  `###` headings. A closed, unmerged pull request settles its read, so a reopened one is not
  checked again; `hrn bot stats` needs no network once reads are credited or settled.
- 2026-10-30: plan-billed runs only, at both ends (#358, #364; PRs #679, #680). agy never
  receives `GEMINI_API_KEY`, `GOOGLE_API_KEY` or `GOOGLE_GENAI_USE_VERTEXAI`, whatever the
  switches, since its stream does not say which credential it used; it stays an automatic
  carry-on partner because those keys cannot reach it. Automatic carry-on picks only claude,
  codex or agy: gemini and opencode can bill per token on provider keys and run only when named.
  Open: whether agy's ADC sign-in (`GOOGLE_APPLICATION_CREDENTIALS`) can bill a Cloud project.
- 2026-10-30: `hrn exec` honours the worker's backend preference (#367; PR #678,
  `docs/exec-protocol.md`): disabled backends are unavailable in `hello`, a start for one is
  `unsupported`, an unreadable preference file is `storage`, and no message carries a local path.
  Server projects may name agy (#359; PR #677); job submission still accepts only `api`.
- 2026-10-30: PLAN.md keeps only open work and the log from 2026-10-16 on (PR #682); the rest
  moved to `docs/plan-history.md`, cutting the file every agent reads from 224 KB to 53 KB.
- 2026-10-26: a run given a GitHub issue leases it, so two runs never work on the same issue
  (#645; PR #657, `docs/workspaces.md#issue-leases`). `hrn run --issue`, the MCP `run` tool's
  `issue` and a batch task's `issue` take a slot-style lease under `$HRN_HOME/leases/issues`,
  shared by every checkout on the computer, and post the same `claimed` label and
  `Claim:`/`Released:` comments as the claim tool outside hrn, so Claude Code sessions and runs
  on any backend see each other. An open pull request that closes the issue, or another
  worker's live claim, refuses the run (exit 2 or a tool error naming the holder) unless
  `--take-over`. Only comments by the account gh is logged in as count, since anyone can comment
  on a public issue. Without gh the local lease still holds and the claim is noted as skipped.
- 2026-10-25: experience is two tracks, delivery and daily practice, with Ada's growth apart
  (#306; PR #642, `docs/experience.md`). The streak counts practice only, never merges, so the
  game never pushes towards merging every day. The ledger is an append-only file keyed by
  source record, with caps applied when it is summed, and `hrn runs prune` folds records into
  it before deleting them. Only a revert takes experience away; no model grades practice.
- 2026-10-25: records can be shared by project, and every read decision in public mode is
  logged (#575 toward #580; PR #650). `$HRN_HOME/grants.json` lists `{viewer, owner?, project}`;
  a grant opens that owner's records whose repository root is the project or inside it, matched
  on directory boundaries (case-insensitive on Windows), with the repository found by walking
  parents for `.git` and only when a grant is checked. hrn never writes the file; a malformed one
  fails closed. The decision stays one function used by every read path, so Ada and the MCP flow
  tool follow it, and batch child runs are checked on their own. In public mode each record read
  logs `record`, `decision` and `basis` in `watch-access.log`; a denied read still looks missing.
  Organisations, membership and revocation without a restart remain for the Team release.
- 2026-10-25: unstarted eval cells are rerun under a bounded policy, the flow page wakes on new
  steps between polls, and parallel lanes, per-run numbers and next actions show on the flow page
  (#567, #576, #577; PR #646). `rerun_unstarted` (default 1) reruns only cells whose every attempt
  did not start, under ids ending `.u<round>`; the first round's raw results are kept, `start_rate`
  reports started over scheduled cells, setup comparisons pair a rerun with its first-round cell,
  and a kept eval names every rerun batch. The flow watcher checks at a quarter of the poll but
  wakes at most once per poll, keeping a change made in the cooldown; `hrn runs prune` removes
  orphan flow files. Lanes are pure CSS; commands are text to copy, never links.
- 2026-10-20: gc also removes a batch task's worktree when merging its commit into the default
  branch changes nothing (#626, PR #627). This reopens the 2026-10-13 decision of #85: a change
  that landed split over several commits, or inside a larger one, matched no single patch id and
  was kept for ever. The merge proof is refused when any file either side changed has a `merge`
  attribute or `merge.default` is set, since a union or custom driver can keep the branch's side;
  attributes are read from the branch (`--attr-source`), so it needs Git 2.43.
- 2026-10-22: attempts whose backend never started are classified and left out of eval pass
  rates, Ada answers questions about a run or batch from its records, and the exec protocol
  routes an omitted backend (#567, #576, #577 toward #580; PR #636). `did_not_start` is set only
  from evidence that no model was reached (no turns, tool calls, final answer or tokens), a cell
  counts only when every attempt did not start, and the routing table skips such cells. Ada's
  answers (`/bot/answer`) use fixed keyword rules, the viewer's permissions, the flow page's
  masking and path shortening, and read a bounded set of records; today's count is bounded by
  its own two-day window so the number it states is exact. A continuation through the exec
  protocol is never routed: it stays on the earlier run's backend. The eval rerun policy for
  unstarted cells, live push of flow events and organisations with grants (#575) are left for
  later slices.
- 2026-10-21: the execution flow page updates while its batch or run is going, runs record why no
  route row applied, and `hrn mcp` serves a `flow` tool (#576, #577; PR #633 into #632). A record
  caught mid-write keeps the last good tree instead of ending the stream; a batch that fails to
  read is still a batch, never a missing run. `route_note` holds what was only a stderr warning
  before: a named backend, no row for the purpose, a disabled or unknown backend, the default
  backend used, or the limit that moved the run; a continuation keeps "continues run X" and adds
  the limit's reason when one was weighed. The shared renderer lives in `internal/flowview`;
  moving it there had double-encoded its `✓ ✗ ⊘ … —`, which a review caught and a test now pins.
- 2026-10-21: run and batch records carry a local owner, batches write a versioned execution-event
  file, and the dashboard, `hrn flow` and Ada show the recorded steps, reasons and next actions
  (#575, #576, #577 toward #580's Transparent Local; PR #632). `internal/flow` is the envelope
  (version, sequence, step and parent ids, status, reason, next); `hrn batch` appends to
  `batch-flows/<id>.jsonl` and older batches and every run are rebuilt from their records with the
  same step ids, checked both ways. `internal/owner` names the local user (`local:<OS user>` or
  `$HRN_OWNER`); records without the field belong to the local user, and every watch route answers
  "not found" for a record the viewer may not read. Reasons are only recorded causes, never model
  reasoning. Text is redacted before any cut: three review rounds found the same bug in different
  places. Organisations, grants and an admin flag, live push of events, Ada answering flow
  questions and #567's did-not-start class are left for later slices.
- 2026-10-21: the purpose classifier closes the gaps measured on real runs (#614, PR #621).
  `internal/purpose` gained an anchored review-brief rule, skips hand-over headings, lets a
  continuation inherit its parent run's resolved purpose (`purpose.Inherited`, saved in
  `task.purpose`), and keeps the CI, deploy and compare keywords to the lead's first sentence and
  off branch, file and path names. Re-measured over 2,450 local run records, undeclared runs
  classified unknown fell from 30.4% to 24.9% and agreement with declared purposes rose from 25.2%
  to 60.9%. Open: whether "Fix round for ..." briefs are implement or debug is still to be decided
  (`debug.fix` unchanged; 63 runs). The rules stay deterministic; hosted decision models were
  declined.
- 2026-10-20: a loaded run record takes the id it was loaded by, not the one stored in it (PR
  #622, no issue: harness is public). A record whose stored id was edited could otherwise point
  a continuation's originals copy outside `runs/`; the watch's fallback read of `task.json`
  follows the same rule.
- 2026-10-19: the gc and batch test packages build their git fixture once per test binary and copy
  it per test (#310, PR #598; #333, PR #599). Under memory pressure a process start on the
  development PC costs seconds or fails ("The paging file is too small"), and the old fixtures spent
  nine and five git processes per test; the gc fixture repoints its origin with one git call, the
  batch one needs none, and both builds drop GIT_* from the environment so the first test cannot
  bake its settings in. The check commands carry `-timeout 40m` so a slow package fails with a
  readable dump. What remains is the git work of the code under test, so the full suite still needs
  a quiet machine here: it passed in 17 minutes idle and was killed at 40 in gc beside other
  sessions' test binaries.
- 2026-10-19: an attempt records when its agent moved its branch or HEAD with git (#592, PR #597).
  hrn-evals cell `fbc00.fix-sum.luna-low.r1` ran `git commit` against its prompt and nothing said
  so. `runAttempt` reads the branch tip and HEAD just before and after the agent runs and sets
  `agent_committed` when either moved (a commit, reset, rename, orphan switch, or a commit on
  another branch or none); hrn's own commits fall outside that window. The eval table counts such
  cells ("N of M cells committed on their own") and `hrn batch show` marks the task. Recorded, not
  failed: failing a cell for it is a maintainer's choice, raised in #592.
- 2026-10-19: a task's own empty gate list is refused (#588, PR #594). `"gate": []` ran gateless,
  but records dropped it and diff and `Summarize` read it as the spec's gates. Option 1 of the
  issue: refusing it keeps the record format and the nil-means-inherit reading every consumer
  already had; a task that needs no gates under a gated spec moves the gates onto the tasks.
- 2026-10-19: hrn drops the git repository variables it inherited (#561, PR #586). Started from
  a git hook or a `!` alias, hrn carried `GIT_DIR`, `GIT_INDEX_FILE` and five more, and a batch
  made its branch and moved HEAD in that repository. `internal/gitenv` drops them for hrn's own
  git (batch, eval's judge copy, the follow-up handover, the MCP profile check), agent CLIs, the
  api backend's commands and MCP servers, and gates; an MCP server's own `env` still applies.
  ws and gc keep their own copies of the list. Running `GIT_DIR=... hrn run` no longer hands
  the agent that repository.
- 2026-10-19: a task records the commits its agent made itself (#560, PR #585). `commitChanges`
  found a clean tree and recorded nothing, so diff, merge and the eval saw no change. `agentCommit`
  now records the task branch's tip when it contains the base and moved past it: a detached
  commit, a renamed branch and a tip rewritten below the base are left out (the last with a note,
  since merging it would revert the base), and a continued attempt that resets to the base clears
  the commit. The eval judge's workaround from PR #566 went. Left: hrn's own commit on a detached
  `HEAD` is still recorded (#582).
- 2026-10-18: evals count cells that never acted apart from failures (#569, PR #583). A failed
  cell whose run ended `ok` in one turn with no tool call and no change did not act; four of
  luna-low's five failures in eval `f353a` were such cells, misled by the fixture's own wording.
  They stay out of the pass rate and medians, the table and footer say to re-run them, and an
  answer-only task (an `expect` and no gate) still counts a wrong answer. The review found routing
  from an eval rebuilt pass rates without the count, so `route.FromEval` carries it, and `hrn eval
  list` counts only the cells that acted.
- 2026-10-18: the retrieval saving gate reads passing pairs and the baseline's own noise (#570,
  PR #573). A flat 10% over every pair let a variant that failed cheaply look efficient, and over
  the 58 saved evals one variant's passing median moves between repeats by more than 10% in most
  rows. The saving must now reach the larger of 10% and that movement, over pairs where both cells
  passed, and with fewer than two passing repeats there is no saving verdict; variant rows also
  hold passing-cell medians. The same rule is planned for setup comparisons (#547).
- 2026-10-18: batches and evals protect the files their gates rely on (#552, PR #563). Gates run in
  the worktree the agent edited, so `protect` globs name the tests and scripts the gates read;
  an attempt that changed or removed one is flagged (`report`, the default) or fails before its
  gates (`fail`, the eval default), and a retry is told which files to restore. The review found
  the comparison ran plain git in the agent's worktree, where planted config could run a program
  or point git at a clean copy; it goes through `agentGit`.
- 2026-10-17: two tests from the long-gate change failed on Windows CI (#550, PR #551). One read the
  batch record with `os.ReadFile` while the beat renamed over it, the other set a batch's end
  microseconds after the watcher's look, which a coarse VM wall clock can reorder. Both were test
  defects; the product code was unchanged.
- 2026-10-18: the eval judge is blind to the variant (#554, PR #566). The judge's prompt never named
  the variant, but it ran in the cell's worktree, whose path, branch and commit message do. It now
  runs in a fresh repository under a neutral temporary name with commits `base` and `candidate`, so
  `git diff HEAD~1` also shows what the prompt clips past 60 KB, and verdicts are asked for in an
  order shuffled by the batch id. Beyond the issue: `batch.CheckoutTree` writes the copy under the
  task worktree guards and pins NTFS/HFS path protection, since the shared config could turn it off
  and plant `GIT~1/config` in the copy's `.git`; a cell whose agent committed its own work is judged
  on the worktree's HEAD; a cancel is recorded as canceled. Left for later: `batch.Diff` misses such
  self-commits (#560), batch git and agent CLIs inherit `GIT_DIR` (#561), and a read-only judge can
  still read run records (#562, a maintainer's call).
- 2026-10-16: four issues closed in the triage without a code PR. #388 (tests reading the
  developer's `backends.json`) was fixed by PR #386, its timing half by PRs #387 and #413. #190 (no
  `syscall.Flock` on solaris and aix) is not worth doing while neither is a release or CI target;
  reopen it with an fcntl lock if one becomes one. #360 (a model id starting with a dash reaches the
  CLI as a flag) is a duplicate of #397, an allow-list of model ids before any CLI argument. #37 (a
  budget across runs) is not needed: only plan backends are used (claude, codex, agy), `api` is
  disabled in `backends.json`, no API key is set, and none of 1,498 run records spent real money;
  reopen it if a metered backend is ever enabled.
- 2026-10-16: gemini and opencode cache semantics, and totals that carry their missing status
  (#320, #334, PR #506). Gemini counts cached tokens inside its input and opencode apart from it;
  `ModelTokens` now knows both, so their runs are no longer "unknown". Token totals are summed
  with their missing status: MCP rows, the report and eval's context median leave out runs whose
  usage is missing instead of adding zeros, and an adapter turn that would overflow marks the
  usage missing. For the api backend that turn's cost is left out too (`CostStatus: "missing"`),
  so the cost reads unknown rather than absurd. The review found the overflowing turn's cost still
  counted as known and eval counting cached tokens twice; both fixed.
- 2026-10-16: `hrn runs` masks prompts, and the `git` URL user stays (#311, PR #511). What #304's
  display masking left out, decided on the issue. `hrn runs` masks prompt excerpts as `hrn show`'s
  header does. Live `hrn run` output stays masked per fragment, and `docs/watch.md` says a secret
  split across two fragments can show in halves while it streams; `hrn show` and hrn watch mask the
  assembled text, and records keep the original. A URL's user part stays masked except the literal
  lower-case `git` with no password, a protocol name rather than a credential.
- 2026-10-16: stopped runs, Ada's watcher and one cache of cleaned batch records; all of #88 done
  (#96, #124, #88, #403, PRs #456, #486, #510). A record with no write for an hour shows as
  stopped instead of running for ever, worked out at render time and never written to records (PR
  #456). Ada forgets a batch she cannot read after three looks, announces no late-reading batch as
  new, hears "un mute" and drops the follower of a stopped run (PR #486). The watch index and Ada
  share one cache, so a batch record is cleaned again only when its file changes (PR #510). Left
  open in #403: a gate running over an hour shows its batch as stopped.
- 2026-10-16: a remote watch started after the job ended ends (#361, #169, PR #509). `WatchJob`
  reads the job's state first; for a finished job with the cursor at or past its last event it
  prints what remains and returns, instead of waiting for a terminal event that never comes. A 5xx
  or network error on that read falls through to the streaming watch, which retries as before (the
  review caught the first version dropping that retry), and `WatchJob` validates the job id
  itself. The client tests #169 named (TLS, 3xx and 4xx, malformed and oversized events, `--after
  -1`, the MCP `job_id` cases) landed with it.
- 2026-10-16: a remote job's cost ceiling is capped from the environment (#168, PR #508). Anyone
  with a client could queue remote jobs at any cost ceiling. Decided on the issue: caps read from
  the environment, unset keeping today's behaviour. The control service refuses `max_cost_usd` above
  `HRN_MAX_COST_USD_PER_JOB` with 422 `cost_limit` after the token and tenant checks, an invalid
  value stops it before it opens the database, and an idempotent retry of an existing job still
  returns it after the limit is lowered. `hrn remote submit` and the MCP `remote_submit` tool refuse
  a cost above `HRN_REMOTE_MAX_COST_USD` before any request. Tenant budgets are still not enforced,
  as `server/README.md` says.
- 2026-10-16: `hrn watch --public`: token, Host and Origin cases (#219, #229, PR #507). Tests pin
  what the code already handled: an absent, null or numeric `aud` is refused, an upper-case Host
  accepted and one with a trailing dot refused as `bad host`. A stream's access-log row is now
  written at its start (after the first flush) as well as its end, and the `*accessWriter`
  assertion checks `, ok` instead of panicking. The navigation exemption is narrowed to what the
  docs said: no Origin or `Origin: null`; any other Origin takes the normal check.
- 2026-10-16: the #44 security follow-ups, and hrn's own git in a task worktree runs nothing the
  agent configured (#56, PR #479). The guard refuses more deploy spellings (`npm -w`, `yarn
  workspace`, `wrangler publish`) and protects opencode's `auth.json`. Every git command hrn runs
  in a worktree an agent touched goes through one helper that neutralises hooks, fsmonitor, gpg
  signing and repository-scope filters through `GIT_CONFIG_COUNT`, skips submodules and diffs with
  `--no-ext-diff --no-textconv`; five review rounds, three adversarial, each closed an escape.
  Left as #467: an agent can set `core.worktree` so hrn's add commits files from outside the
  worktree, a read escape already on main.
- 2026-10-16: routing by the declared purpose, and routed choices dropped off their backend (#327,
  #332, PR #505). Routing uses `purpose.Resolve(declared, prompt)`, and `routed_by` says when the
  category was declared. A retry on another backend clears the routed model and effort, and a
  usage-limit switch also clears `routed_by`, in run, MCP and batch alike, so `hrn report --by
  routed` no longer credits a partner's results to a row. An invalid route row warns and is skipped
  instead of refusing every run of its purpose (decided on #327).
- 2026-10-16: a declared purpose follows continuations and exec (#322, #325, PR #504). A continued
  run inherits the earlier run's declared purpose unless given a new one, as mode is; an invalid
  stored purpose is not inherited (a review finding). `hrn exec`'s `start` accepts a `purpose`,
  checked with `purpose.Declarable()`, and a batch task's declared purpose shows on the batch page
  and in `hrn batch show`.
- 2026-10-16: `hrn gc` streams the default branch's log into patch-id (#225, PR #503). gc read the
  whole `git log -p`, binaries included, into memory and fed it to `git patch-id` again for every
  batch worktree. Now the log is piped straight in and the landed patch ids are kept per merge
  base for the run, with the same arguments and results; any failure still leaves gc without
  proof, so the worktree is kept. If patch-id fails, gc cancels before waiting on `git log`, so a
  full pipe cannot hang it.
- 2026-10-16: the review rule: fix every defect in the branch, drop the nits (PR #502, AGENTS.md).
  The rule since 2026-10-16, after a backlog run in which every merged PR spawned a follow-up issue
  of non-blocking findings (59 opened against 47 closed). A branch finishes everything its issue
  asks; every defect, missing test, broken house rule or security hole a review finds in the
  branch's code is fixed before the push, blocking or not, one commit per review round. Nits are
  dropped; only a defect elsewhere on `main` or a maintainer's decision becomes an issue, never a
  follow-up of a follow-up. The base branch is merged in once before the final checks, since CI
  tests the merge with `main`. The Definition of done says the same.
- 2026-10-16: the usage chart's review follow-ups (#243, PR #501). claude's per-turn output keeps
  the largest count across a message's lines instead of undercounting; a stream connect decodes
  only usage and compaction lines; the live chart is drawn once a poll and thinned past 500 turns,
  so a long run no longer sends about 1.5 MB per update. A mixed report row's median context takes
  one average per run, and the day-row test no longer flakes at midnight.
- 2026-10-16: a retry can recover from a failed required retrieval (#182, #262, PR #500). When the
  search itself failed and no pin was recorded, a retry retrieves afresh instead of failing every
  attempt with "pin cannot be resumed"; a recorded pin is honoured as before and kept after a
  refused replay, so no attempt falls back to an unpinned search. The spec check refuses retrieval
  with a retry on another backend, and the automatic switch at a usage limit is skipped when
  retrieval is on: a switch clears the model, which retrieval needs, so the task ends `limited` with
  the reason. Both decided on the issues.
- 2026-10-16: the api tool definitions measured per mode and held to a byte budget (Refs #438, PR
  #492). Steps 1 and 3 of #438: hrn's own definitions are 1,409 bytes read-only, 2,836 in auto and
  yolo and 3,295 with shaping, about 16%, 32% and 37% of the median api call (2,211 tokens over
  1,110 calls); those calls were local models without cache writes, so a call to Anthropic's API
  would be larger. A test holds each mode under its budget, and `scripts/tool-shapes.sh
  --definitions` reports the figures (`docs/tool-shapes.md`). Nothing is trimmed: a third less would
  save about 240 tokens a call, mostly at the cache-read rate, and step 2 waits for a maintainer's
  decision and an eval, so #438 stays open.
- 2026-10-16: a fresh continuation with a bounded hand-over (Refs #117, PR #494). `hrn run
  --continue <id> --fresh`, MCP `fresh: true` and exec `fresh` start a new session instead of
  forking the old one, in the earlier run's workspace and mode, carrying a hand-over built from
  the record with no model call (task, status, error, final answer, changed files, gate results),
  each part capped and saved as `handoff.md`. A later ordinary continue, the limit carry-on
  included, never reaches back past a fresh hand-over. The first slice of #117, which stays open
  for retrieval pins, adamap identity and the long-chain eval.
- 2026-10-16: the purpose card's costs summed exactly, and a purpose kept on one line (#122, #125,
  PR #490). Costs are summed as integer micro-dollars before the lower bound is floored to four
  decimals; floating-point sums printed an exact sum $0.0001 low past about $100. The runs table's
  purpose cell no longer breaks its word at medium widths; the phone layout is unchanged.
- 2026-10-16: JSON and table summaries (#336, PR #487). `internal/shape` gains the kinds `JSON`
  and `Table` (`Classify` tries JSON, Test, Table, Log, Terminal in that order), held to the same
  invariant: the summaries copy raw spans of the original rather than re-encoding it, which would
  add numbers the original never held, and `valid` rebuilds the one generated first line and
  compares it exactly. Error, `FAIL` and `panic` lines are kept verbatim. `go test -json` output
  stays line-based, and logs with comma timestamps stay logs.
- 2026-10-16: `hrn scripts hint`, an advisory hook that names a kept script (#439, PR #481). An
  opt-in Claude Code PreToolUse hook: when an inline script (a heredoc, `python -c`, `node -e`,
  `bash -c`, a PowerShell block) matches a kept script's `matches:` header, it adds one line of
  context naming the script. It never makes a permission decision and never exits 2; anything it
  does not understand is silent with exit 0, and the hint is limited to a plain file name and 200
  bytes of parameters, so a repository cannot put arbitrary text in an agent's context. It stands
  in until the gate does this (adanext-exec #2).
- 2026-10-16: the attempt id is left out of the durable task identity digest (#147, PR #478).
  `Task.AttemptID` was part of the digest that binds a checkpoint to its task, so a coordinator
  giving each retry a new id would fail every resume with "task identity does not match". Decided:
  the attempt id names an attempt, not the task. Existing checkpoints still resume, since
  `attempt_id` is `omitempty` and no stored checkpoint had one.
- 2026-10-16: stronger chart verifier assertions, with regression tests in CI (#173, PR #473).
  `server/deploy/verify_charts.py` makes the ten assertions #139's review listed (a pinned control
  image, Secret names, tenant and project ids, the control container's securityContext,
  NetworkPolicy egress contents, no repeated kinds, no `envFrom`), and `test_verify_charts.py`
  breaks each on purpose. The review found nothing ran those tests; the charts job in CI now does.
  No chart changed.
- 2026-10-16: adversarial probes and a fuzz target for the summary invariants (#436, PR #468). They
  found one defect: `valid` compared error lines with the raw original while summaries are built
  from colour-stripped text, so a colour code inside `error` or `FAIL` let a long terminal result
  drop that line and still pass; it now compares stripped lines. Left open, each needing a mechanism
  of its own: #461 (digits in escape codes satisfy the number check), #462 (an error word wrapped
  across two lines), #463 (the oracle does not catch invented error lines), #464 (a stack frame far
  after a log error; a maintainer's decision) and #465 (one test name in two packages always falls
  back).
- 2026-10-16: which `Usage` is which on `Result` and `Event` (#265, PR #471). `Result.Usage`
  (`*UsageMetadata`, how to read the run's totals) and `Event.Usage` (the per-call `*Usage`) now
  say what they are and name each other. The issue's smaller option: no rename, since
  `Result.Usage` is a stored field.
- 2026-10-16: live event streams bounded per tenant and closed at a terminal state (#156, PR
  #470). The control service's SSE route had no limits. Now each instance allows at most four open
  streams per tenant (a fifth gets 429 `stream_limit`), a slot is taken only after the token and
  job-scope checks and released on every exit, a stream closes once its job is terminal whichever
  event made it so, and a stream lasts at most 30 minutes before the client reconnects with its
  cursor. An event-ordering problem already on main is in #469.
- 2026-10-16: the #46 security follow-ups for the limit carry-on (#54, PR #466). Key variables
  count as set on any non-empty value (only the `CLAUDE_CODE_USE_*` switches keep "`0` or `false`
  is unset"), so `ANTHROPIC_API_KEY=0` refuses the carry-on with a reason. When a run stays on its
  backend, run, MCP and batch record why before it starts, and a batch no longer sends a `switch`
  event when nothing switched. A cancel stops the carry-on, but a real limit is still remembered
  in `limits.json`. `docs/backends.md` names what hrn cannot see (a Console login, a settings
  `env` block, codex profiles).
- 2026-10-16: hrn watch hides the backend start line and the shell wrapper around commands (Refs
  #435, PR #460). The display-only first step, asked for on 2026-10-16. A run's first info event,
  the backend's own command line, is hidden when its first word is the recorded backend; command
  calls lose one outer `powershell -Command`, `bash -lc`, `sh -c` or `cmd /c` layer and its quotes,
  in the event text, the tool input and Ada's "running" line, and a command that cannot be unquoted
  cleanly is shown as it was. Records and `hrn show` keep the full text. The gate itself is #435,
  re-scoped to run every agent command through adanext-exec.
- 2026-10-16: the comment rule, and a check with a baseline that only shrinks (Refs #441, PR
  #457). Part one of #441: AGENTS.md's rule, and `scripts/comment-check.sh` over the whole tree
  with a baseline of today's comments, so new code is held to the rule without a base ref. Today
  0.1859 of non-test bytes are whole-line comments; the target is under 0.10. A stale baseline
  entry fails the check and editing a baselined block changes its fingerprint, so a touched
  comment comes up to the rule. "Review" matches review narration, not the bare word, which hrn
  uses as a purpose. The cleanup, part two, comes after item 18's slices.
- 2026-10-16: quoted compact windows over MCP, and the lean carry-on and `--continue` covered (#208,
  PR #450). `compact_window`'s MCP schema is one type every host accepts, and a quoted `"200000"`
  passes the same check as a number; a test checks that a lean claude attempt carried on on codex
  records no `lean`; `hrn run --continue` of a lean batch run runs without lean unless asked, as
  `docs/backends.md` now says; `.claude/rules/records.md` forbids changing a stored field's JSON
  type. The issue's last item, measuring `compact_window` on a long real task, needs a maintainer's
  approval of the plan usage and moves to *Under consideration*.
- 2026-10-16: the incomplete-worktree test skips where git adds the worktree anyway (#451, PR
  #452). git 2.55.0 on GitHub's ubuntu runner adds a worktree beside a half-written
  `.git/worktrees/<n>` entry, so the test from PR #426 found no error and turned every PR's ubuntu
  job red. It now checks the kept error only when git fails, and skips with the reason otherwise.
  Test only.
- 2026-10-16: retrieval tests for a continuation without a packet, child MCP credentials and the
  durable refusal (#185, PR #448). Tests only, the ones #141's review named: a child MCP server
  cannot see retrieval credentials, `LoadConversation` keeps a failed optional retrieval's scope
  with no packet, and the durable refusal makes no request. Two of the cases were already on main.
  Each new one was seen failing against a temporary regression; no bug was found.
- 2026-10-16: UTF-16 gate output decoded, and a gate whose shell never started is not retried
  (#84, PR #446). WSL's launcher writes UTF-16, which was stored and handed to a retry as
  NUL-interleaved text; it is decoded to UTF-8 first. A gate whose process fails to start, or
  whose decoded output begins with a WSL or HCS launcher error, records `infrastructure_error` and
  fails the task instead of asking a backend to fix working code. The review narrowed the check to
  the start of the output, so a real failure that merely prints `HCS_E_` still retries.
- 2026-10-16: tests pin how a run's folder is spelt when recorded and continued (#75, PR #444).
  Windows-only where 8.3 names are needed: a session recorded under a short name is refused when
  continued from the long one; exec records the long spelling when sent the raw temp dir; a batch
  run reached through a short name records the long worktree path, for `fresh` and `slots`, and is
  continued (no test had continued a batch run). Tests only.
Older entries: [docs/plan-history.md](docs/plan-history.md#decisions-log-2026-09-21-to-2026-10-15).
