# Live dashboard and Ada

[← README](../README.md)

Pictures of each page: [hrn watch and Ada, in pictures](presentation.md).

## Watching runs live

```
hrn watch                  # http://127.0.0.1:8089, this computer only
hrn watch --lan            # also on this computer's private network addresses, for a phone
hrn watch --lan="Ethernet"     # only that adapter's network (a name or an address)
hrn show -f <run-id>       # the same timeline in the terminal
hrn bot                    # Ada, the bot, in a small window: what hrn is doing, out loud
```

Run and batch pages link to **Execution flow**, a hierarchical view of recorded steps and their timing. It can also be opened at `/flow/<id>` or fetched as JSON at `/flow/<id>/json` (`version: 1`). The `?level=simple`, `?level=technical` and `?level=debug` links choose how much detail is shown; simple omits tool steps, while debug includes step identifiers and marks rebuilt steps. At every level, overlapping siblings occupy lanes in a CSS grid, with lanes beyond lane 4 folded into that last column; below 640px the grid becomes one column. The summary reports the peak number of sibling tasks running side by side, including those folded into the last column. Steps show their recorded duration, and run steps show turns, tool calls and any reported cost using the viewer's access to the run record. Failed tasks offer `hrn batch show <batch>` to inspect the batch: there is no retry subcommand, and show does not accept a task ID. Passed tasks with a commit offer `hrn batch merge <batch> <task>` after reviewing the diff; task IDs are positional, with no `--tasks` flag. Commands are copyable text only; the page cannot run them. An unfinished flow updates live at the selected level and ends when its run or batch finishes or stops. hrn shows recorded steps and reasons, not the model's private reasoning. Run steps link back to the run page, where tool calls and output provide the evidence.

A new flow step after a quiet spell appears within a quarter of the configured poll interval (at least 50ms); while a run keeps writing, the page refreshes at most once per poll interval, since each refresh rebuilds the whole flow.

`hrn watch` serves a page that lists the newest runs and batches and keeps itself current.
When [automatic gc](commands.md#automatic-gc) is enabled, watch runs the periodic pass and
its local index shows the last pass on page load.
`hrn update` restarts a running watch on the new binary with the same flags (see
[hrn update](commands.md#hrn-update)).
Each row leads with the repository the work is in (its workspace folder's name; for a batch
task's worktree, `<repo>/.hrn/worktrees/<task>` or a slot, `<repo>/.hrn/slots/<n>`, the folder
above `.hrn`) and, for a run a
batch started, the task: `harness · 82-repo-and-task`. A task's name is its id, so name tasks
after their issue (`82-repo-and-task`) to see the number there; when hrn made the id up
(`t1`, `t2`, ...), the name is the start of the task's prompt. The run's or batch's id is
under that label, small, still there to copy; a run or batch without a workspace keeps its
id as its label. Then come a run's backend, purpose, status, duration, cost and the prompt's
first line, and a batch's status, how many tasks passed, its backends, duration, cost and up
to three of its tasks' names, then "and N more". Every attempt of a batch's task is also a
run; its row links to the batch under the run's id, even when that batch is older than the
ones listed. Watch blanks a batch record's secrets once and reuses that copy, for the index and
for Ada, until the record's file changes time or size. A run's page is its
timeline as it happens: the assistant's text, each tool call with its input in one line
(the input itself folded under it, its first 6 KB and last 2 KB), each result folded and
capped (its first 12 KB and last 4 KB; all of it stays in `events.jsonl`), how long each
step waited, and, once the
run ends, its status, tokens, cost and final answer (and, for a `limited` run, when the
limit lifts). Watch hides the backend's startup command and shows tool commands without
one outer PowerShell, bash, sh, zsh or cmd invocation, in both summaries and folded
input. Commands with unbalanced quotes keep their wrapper. Workspace and home paths
still show as `.` and `~`. These display rules also apply to live streams, `--lan`
and `--public`; records on disk and `hrn show` keep the full commands.
A batch's page shows each task with a link to each attempt's run, the
backend a usage limit moved it from, and its gates (command, exit code, folded output) as
they finish. Steps arrive over Server-Sent Events that check the run's `events.jsonl` twice a
second; a browser that loses the connection picks up where it left off. Cost is whatever
the backend reports: the api backend's estimate (`~`), the claude and opencode CLIs' own
figures, nothing for codex and gemini, and only once the run has ended.

An unfinished run whose transcript has not been written to for an hour shows
`stopped · no word since 21:36` on the index and its page. This uses the same
file-modification-time rule as `runs prune`, not the timestamp of an event or
the run's age. With no transcript, the run directory's modification time is used.
An unfinished batch is stopped only when its record and all its attempts' runs
have been quiet for an hour. A first attempt can run for hours without rewriting
the batch record and still count as running while its transcript is written to;
a gate writes nothing itself, so `hrn batch` saves the record again every ten
minutes while one runs.
Stopped tasks and unfinished attempts show as stopped too; the running clock ends.
Open pages update to stopped through their streams. Reload a stopped page to see
later activity if the writer resumes.

This is a display inference, not a saved result: watch never rewrites or deletes
records, and a saved result still determines how a run ended. An unreadable
modification time alone is not evidence that a writer stopped. Ada excludes stopped
records from what is going and does not announce them as starting or finished; she stops
reading a stopped run's transcript and picks it up again, from where it stopped, if the
run writes again: what it did before is not said again as what it is doing.

Each run on the index also says what it was for (review, debug, implement, refactor, test,
research, plan, ops or unknown), from `internal/purpose`'s keyword rules on its prompt's
first paragraph, or its second when the first matches no rule, with leading `Label: value`
lines skipped for an anchored instruction before trying the paragraph as written
and then with labels skipped as a fallback, unless the run declared one;
hover the word for the rule that decided it
(or `declared` for an explicit purpose or valid leading header). Above the batches, a card,
"What the runs were for", sums up the newest 50 runs, the ones listed below, not all time:
one row per purpose that occurs, then "all", each with its runs, how many are running,
tokens (input, output, cache reads and writes, counted once) and known cost.

That cost is honest about what it does not know. codex and gemini report no cost; such a
run is left out of the sum, not counted as $0. A row whose finished runs all reported a
cost shows their sum; a row where some did not shows "≥ $X", a lower bound over those that
did, with how many runs report no cost under it; a row where none did shows "-". A known
cost too small to round still shows "$0.0000", and a cost above $1,000,000 (only a corrupted
record holds one) counts as no cost reported. A sum
that includes the api backend's estimate keeps its `~`. Running runs count only as
running: they have no cost yet, known or unknown. A stopped run still counts towards
the number of runs, but not the running count or the count of finished runs with no
reported cost.

A run's page names its workspace by the same name as its row (`demo-app`, the repository
for a batch task's worktree), and a batch's page its repository; a task's worktree shows
relative to the repository (`.\.hrn\worktrees\t1`). Hover the name for the whole path,
on this computer only: a phone on the network sees the name alone. In what a run's steps
show (step text, a tool call's one line and its input, results, info lines, the final
answer) and in a run's or batch's header, the workspace is `.` (`.\src\main.go`, in the
path's own slashes), the home folder is `~`, and a program named by its full path in
System32, Program Files, `/usr/bin`, `/bin` or `/usr/local/bin` is its file name: `codex
exec -C . --json`, `"powershell.exe" -Command ...`. A batch task's text is shortened by its
worktree. The index's rows (a run's prompt, a batch's task names) are shortened the same way,
for every viewer, and so are Ada's sentences, log and list of what is going (by each run's
workspace or task's worktree). A path only counts whole, so `demo-app-old` keeps its name.
This is the page only: `events.jsonl`, the records and `hrn show` keep the full paths.

When the backend reports the last turn's context size, the run page also shows
`Context: 285.0k tokens at the end`. This includes input served from or written to
the cache. It is absent when that size is unknown, including on older records.

When the run recorded `usage` events (the api, claude and opencode backends; see
[Usage per turn](run-records.md#usage-per-turn)), its page opens with "Tokens per turn": a
chart of the context each model call carried and, under it on a scale of its own, the cache
reads so far, with the turn count, peak and median context above. A compaction the backend
reported (Claude Code's `compact_boundary`) is a solid line between the turns it came between,
and also a step in the timeline; a turn whose context fell by more than half with nothing
reported is a dashed line, "probably compacted", a reading of the numbers that is never
recorded. Hovering a point or a line says what it is, and "Turns as a table" lists every
turn's figures. Past 500 turns the line still runs through every turn, but only one turn in
every few (enough to stay near 500) has a point and a row, with the last turn and each
compaction always kept; a line under the chart says so. While the run goes, the chart is
redrawn over every turn so far once a poll in which turns arrived.
The chart is inline SVG coloured by the page's stylesheet, so it needs no script, style or
font beyond the page's own. Usage events themselves are not listed in the timeline.

Everything from a record is escaped as text, since tool results hold whatever the agent
read. The page loads nothing from anywhere else (one stylesheet and a script or two, all
served by hrn, no fonts), follows the system's light or dark theme, and on a phone shows
each run and batch as a small card instead of a table. It cannot start, stop or merge
anything: a request other than GET or HEAD must carry hrn watch's key in
`X-Hrn-Watch-Key`, from this computer or the network alike (a custom header is also the
CSRF check). Each start issues a fresh key, shared only with viewers allowed to change
backend preferences, as described below.

It listens on a loopback address and refuses any other `--addr`, and it answers only
requests whose Host header names it, so a DNS name rebound to 127.0.0.1 cannot read it
through a browser. `--lan` also serves it on each private IPv4 address (192.168.x, 10.x,
172.16-31.x) of this computer's network adapters that are up, never on 0.0.0.0 or a public
address, and the terminal and the index list those links; on Wi-Fi, use the one on the
Wi-Fi adapter. `--lan=NAME` keeps one adapter, by its name in any case or by its address,
and a name that matches nothing is refused with the adapters there are. Point-to-point
links (most VPN tunnels) are left out, but a VPN that shows up as an ordinary adapter
with a private address is served too, and listed. Viewing needs no login: while `--lan`
runs, anyone on each listed network can read the runs. Windows Firewall asks once
whether hrn may listen; allowing Private networks is enough when the network is marked
Private.

For the phone, the index as seen from this computer shows each link with its QR code,
and with one network (`--lan=NAME`, or only one adapter) the terminal draws that code
too. The codes come from hrn's own encoder (`internal/qr`: byte mode, level M, versions
1 to 10), standard library only. A phone that came in over the network sees neither the
codes nor the hints meant for this computer.

`hrn show -f` prints the same steps in the terminal, with a line for each wait of a
second or more, and ends with the summary line once the run's result is saved. Ctrl+C
stops following and leaves the run alone. The terminal waits for a saved result even
when watch's one-hour quiet rule displays the run as stopped.

### Backends

The index's Backends card, above "What the runs were for", shows every registered
backend to every viewer: installed or not, enabled or disabled, the configured
default (or "chosen: X" when none is configured), and any remembered limit, highlighted
as `at its limit, resets in 2h 5m` (any backend, agy included, whose run ended at its plan
limit with a reset time). Unreadable or malformed preferences show a single notice without a path,
and the enabled column says unknown until the preferences are fixed. The edit controls start unchecked.
Reload the index to refresh the card.

The Quota left column shows what is left of claude's and codex's plans, per window, as the
CLI last reported it: `5h: 62% left, resets in 1h 12m / week: 80% left, resets in 3d 4h`.
A window under 10% left is highlighted; once its reset time has passed it reads
`... left before its reset 5m ago (stale)` until the CLI reports again. Codex's figures come
from the tails of its newest session files under `$CODEX_HOME/sessions` (default
`~/.codex/sessions`); claude's from the `quota` events of the 40 newest runs
([Run records](run-records.md#quota)), each window as last reported by any claude run among
them. A claude figure comes from the newest report found within each run's last 64 KiB of
events, so a run whose last report lies further back gives none. Only the numbers are read;
nothing else from those files is kept or shown. A plan that reports only one window (codex on some plans has only
the weekly one) shows only that window, and a backend with nothing reported shows nothing.
Answers are reused for 30 seconds.

Loopback viewers can change the enabled checkboxes and default radio buttons,
then click Apply. In public mode every Cloudflare Access-verified viewer can do
the same. LAN viewers see the state only, without controls or the write key, and so
does any viewer whose request carries a forwarding header (`Forwarded`, `X-Forwarded-For`,
`Cf-Connecting-Ip`): a proxy in front of watch belongs in public mode, where Access says
who the viewer is.
Apply uses a same-origin request carrying the startup key; it writes
`$HRN_HOME/backends.json` atomically and reloads the index. Runs started afterwards
follow those preferences. Choose "No configured default" to let hrn choose;
leaving all backends unchecked with no default restores all backends. A configured
default must be enabled, and unknown backend names are refused.

Running MCP servers keep their backend enum until their host restarts them;
the handler still refuses a disabled backend.

### Behind Cloudflare Access

To reach watch through a Cloudflare Tunnel, forward `https://harness.example.com` to
`http://127.0.0.1:8090` on the same computer. Create a Cloudflare Access application
for that hostname, require email one-time-code login, and copy its team domain and
audience tag into the command:

```
hrn watch --addr 127.0.0.1:8090 --public https://harness.example.com --access-team TEAM.cloudflareaccess.com --access-aud AUDIENCE_TAG
```

All three flags are required together; `--public` cannot be combined with `--lan`.
Use repeatable `--owner-email address` flags to identify which verified Access emails act as the local owner. Those emails (matched without case) see local records; other verified emails can see only records owned by their `email:` identity. With no `--owner-email`, every verified visitor acts as the local owner as before. Loopback and LAN visitors use the local owner identity.

To share selected projects, create `$HRN_HOME/grants.json` (default `~/.hrn/grants.json`):

```json
[
  {"viewer": "email:reader@example.com", "project": "/absolute/path/to/repo"},
  {"viewer": "user:teammate", "owner": "local:me", "project": "/absolute/path/to/another-repo"}
]
```

Each grant allows that viewer to read that owner's records in the project directory
or below it. `viewer` must be an `email:` or `user:` principal; use lowercase email
addresses. Omitting `owner` selects the local owner, including older records without
an owner. `project` must be an absolute repository directory (on Windows, for example,
`"E:\\projects\\repo"`). Paths are cleaned, compared without case on Windows, and
matched on directory boundaries, so `/repo` never includes `/repo2`. Runs use their
repository root when a `.git` entry can be found by walking parents, otherwise their
recorded task directory; batches use their recorded repository directory. A batch's
child run is checked on its own: a worktree inside the granted project is covered by
the same grant, while a run recorded elsewhere (such as a slot under another checkout)
is hidden on the flow page. A batch grant still shows the batch record itself,
including each attempt's summary, error line and gate output; only the run record
(its prompt, events and tool output) needs its own access.

Watch reads grants once at startup and never writes the file; restart watch after an
edit. A missing file grants nothing. A malformed file disables all project grants
and reports one error on stderr; ownership access still works. Use `--owner-email`
to distinguish the local owner's emails from visitors who need project grants.

Watch checks the Cloudflare Access signing keys at start and refuses to serve if they cannot be fetched.
Watch still listens only on loopback. In this mode it accepts only the public Host
and verifies the Access JWT on every request, including static assets and event
streams. A top-level page load from elsewhere, such as the Access login redirect or an
email link, opens the page when it carries no `Origin` header or `Origin: null`; any other
Origin goes through the normal check and must be the public origin. Cross-site embedded loads
and other cross-site requests remain refused.
It treats visitors as remote even though `cloudflared`
connects from the same computer: the page omits the hrn home path and phone hints,
and `/voice` is unavailable. You cannot open this public watch at its loopback URL
from this computer's browser: the loopback Host is refused by design so the Access
login cannot be skipped. For local use, run a second, plain `hrn watch` in another
terminal; its default `127.0.0.1:8089` port works alongside the public watch on `8090`.
`hrn bot` needs the plain watch too. If the public watch holds the bot's port, run
`hrn bot --addr 127.0.0.1:8091` instead. The terminal prints the public URL, the
loopback address for the tunnel, and the access log path. Each request appends one
JSON line to `$HRN_HOME/watch-access.log`
(or `~/.hrn/watch-access.log`), with the time, email when allowed, method, path,
status, Cloudflare connecting IP, and a short reason when refused. An event stream
writes two lines: one after its first bytes reach the browser, marked
`"stream":"start"`, and one when it ends, marked `"stream":"end"`, each with its
own time; a stream that ends before sending anything writes one plain line. Cloudflare's
own Access logs record logins; this file records requests that reached watch.

Requests for a run, batch or flow also include `record` (the requested id),
`decision` (`allowed` or `denied`), and `basis` (`owner`, `admin`, `grant:<project>`
or `none`). Both stream rows carry these fields. Missing records use `denied` and
`none`; denied reads answer just like missing records. A batch flow's decision names
the batch, rather than its child runs. Index and other requests without a record id
omit these fields. Loopback and LAN modes do not write an access log.

### Ada, the bot

`hrn bot` opens Ada, hrn watch's bot (`/bot`), in a small window of her own: Edge's or Chrome's
app window where one is installed, else the default browser. It serves hrn watch on
loopback for her until Ctrl+C, or, when an hrn watch already serves that address, just
opens her there. The index links to her too. Ada is a robot scarlet macaw: she blinks,
bobs her head and moves her beak while she talks, and keeps still for anyone whose system
asks for less motion.

Ada says what hrn is doing in plain UK English, in a line at the top, a log, out loud
and as desktop notifications:

| Kind | For example | Says it | Notifies |
|---|---|---|---|
| started | Claude has started in harness: add the budget guard. | yes | no |
| working | Claude is editing bot.go in harness. | no | no |
| task | Task two of four in demo-api passed: fix the flaky test. | yes | no |
| done | Claude has finished in harness. It took four minutes. | yes | yes |
| failed | The batch in demo-api is done: three of four tasks passed. It took twelve minutes. | yes | yes |
| status | Two things are going: Claude in harness, editing bot.go; and a batch of four tasks in demo-api. | no | no |

Those are the defaults; the settings change each one, pick the voice and the speed, and
are kept in that browser. A done or failed notification stays until it is dismissed;
clicking one opens its run or batch. Skip (or Esc) stops talking; Snooze keeps it quiet for
15 minutes, an hour, or until unmuted; each run or batch going has a Mute this. "What an
agent is doing" comes from its latest tool call (reading, editing, searching, running a
command and the like), across the backends' different tool names. Ada opens quietly:
what ended before it was opened is not news, and the first thing it says is what is going.
A batch whose record could not be read on that first look (caught mid-save) is not
announced as new when it reads, but its ending is if it ended after Ada opened. A batch
whose record cannot be read for a look is kept as it was while its file is on disk.
A batch's own runs are told as the batch's, not twice.

Talk to me listens for one sentence each time it is pressed: "status" (or "what are you
doing?"), "skip", "quiet" ("for an hour", "until I unmute"), "unmute" (also heard as "un
mute") and "repeat". These
steer the bot only; nothing it hears or shows acts on hrn, which waits for the
confirmations and audit log of #38.

Ask a question with Talk to me, or type it in the question box and press Enter:
"why did it fail?", "what is it doing?" (or "where is it?"), "what happens next?"
(or "what's next?"), "how long did it take?", and "what went wrong today?".
Use a run or batch ID, or part of a task name, instead of "it" to select a record.
Without a name, failure questions select the newest failed record, activity questions
the newest running record, and next-step and duration questions the newest record.
Today's failures are counted by their recorded ending time in the server's local day, among
the records started in the last two days (a longer batch that ended today is not counted).
Answers name the record and link to its execution flow in the log. They come only
from recorded steps, causes, next actions and durations, using fixed keyword rules;
Ada makes no model call and cannot explain anything hrn did not record. Answers
use the same viewer permissions, secret masking and path shortening as the flow
pages, and a question other than today's count reads at most the newest fifty records.

The sentences are made by hrn (`internal/watch/bot.go`, where they are tested) and pass
through the same blanking of secret values as every page; the page only chooses which to
say. She speaks with the browser's own speech synthesis, which needs one click on the page
first ("Let me talk"). Voices on this computer come before online ones, since an online
voice sends what it says (prompts included) to its maker; among them British ones, then
a woman's, come first. Windows has British voices only with the English (United Kingdom)
speech pack (Settings, Time and language, Speech); without it the page says so and speaks
British English in the voice there is. Edge's online Natural voices (Sonia, Libby and
Maisie are British women's) sound far better and can be chosen in the settings, knowing
that each sentence then goes to Microsoft. Talk to me uses the browser's speech
recogniser, which in Edge and Chrome sends what the microphone hears to Microsoft or Google
while it listens. The microphone is allowed on Ada's page and the voice page below only.
Opened over `--lan`, Ada speaks, but the browser allows neither notifications nor the
microphone on a plain network address.

#### Recording Ada's voice

`/voice` (linked from Ada's page on this computer) is where someone records the voice Ada
will speak in (#83). It works on this computer only, and is refused over `--lan`: it uses
the microphone, and the recordings are that person's voice.

- **Read the lines**: 523 lines, one at a time, mixing everyday British English (original
  sentences covering English's sounds, questions, numbers and dates) with Ada's own
  sentences, made by the functions she speaks with over many backends, places, tasks and
  durations. Record and stop with Space, play back with P, keep with Enter, retake with R,
  skip with S, go back with the left arrow. Each take is judged: too short, clipped, too
  quiet, or good.
- **Just record me**: one long recording for talking, to someone or to ChatGPT's voice mode,
  with headphones, so that only the speaker's voice reaches the microphone. It is saved in
  parts of five minutes and when it stops, even if releasing the audio fails. A failed save
  keeps the audio for another try; the warning clears once that part is saved.

The page asks for the microphone as it is (echo cancellation, noise suppression and
automatic gain off, one channel) and says so if the browser filters it anyway, as it may
while another tab uses the microphone. Takes are 16-bit mono WAV at the browser's audio rate (usually 48 kHz).
Record starts a fresh capture; Stop includes the last partial audio block, releases the
microphone and suspends the audio context, or leaves it to an earlier playback still running,
which suspends it when it ends. A block the old capture posts after Stop gave up waiting
never reaches the next take, and a microphone that fails to open leaves a playback playing.
Playback wakes the context without opening the microphone; the next Record opens it again. Switching modes stops the current recording
and cancels one still opening the microphone. Keep and Retake wait until opening finishes.
They are saved by the browser into a folder the person picks (`0001.wav`, … with
`metadata.csv` as `id|text`, and `talk-<time>-<part>.wav`), never sent to hrn watch, which
only serves the page; picking the same folder again carries on at the first line not kept.
Recording shortcuts wait while the folder loads. Unrecognised metadata rows (such as a
header or notes) are preserved when saving; unreadable metadata is reported rather than
overwritten.
A browser without folder access (Firefox) downloads each take instead, named by its line.
**Download metadata.csv** downloads the text for the lines kept in this page session;
save it alongside those WAV files. Download it before closing or reloading the page.
A line's number never changes, so takes from different sessions line up.

The recorder's browser-API unit tests run locally with
`node --test internal/watch/testdata/voice.test.cjs` (Node.js, no browser download).
They use mocked audio and folder APIs; they do not replace a real browser microphone check.

## Secret masking

Human-facing text in watch rows, pages and streams (local and `--lan`), `hrn show`,
`hrn runs` prompt excerpts, terminal progress lines and Ada's speech and notifications masks secret values as
`••••`: URL credentials; `-u`/`--user` user:password flags; JSON secret fields;
authorization, API-key and cookie headers; bare Bearer tokens; recognised `sk-`,
`ghp_`, `github_pat_`, `xox`, `AKIA` and JWT tokens; assignments whose names have KEY,
TOKEN, SECRET, PASSWORD, AUTH, CREDENTIALS, COOKIE or SESSION as a whole segment
(separated by `_`, `-` or the name's ends); and secret flags
such as `--password`, `--token` and `--api-key`. Masking scans the whole text before
any display excerpt is shortened; it does not cut the text itself.
A URL whose user is the literal lower-case `git` with no password (`ssh://git@host/...`, as on
git remotes) keeps it; any other user part, including `GIT` or a bare token, is masked.
Live `hrn run` output is masked per streamed fragment, so a secret split across two
fragments can show in halves while it streams; `hrn show` and `hrn watch` mask the
assembled text from the record.
The existing environment-value redaction also runs. Records remain unchanged,
and JSON record output retains the original text. These rules recognise common
secret shapes; they cannot identify every possible secret.
