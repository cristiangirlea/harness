# Live dashboard and Ada

[← README](../README.md)

Pictures of each page: [hrn watch and Ada, in pictures](presentation.md).

## Watching runs live

```
hrn watch                  # http://127.0.0.1:8089, this computer only
hrn watch --lan            # also on this computer's private network addresses, for a phone
hrn watch --lan="Ethernet 3"   # only that adapter's network (a name or an address)
hrn show -f <run-id>       # the same timeline in the terminal
hrn bot                    # Ada, the bot, in a small window: what hrn is doing, out loud
```

`hrn watch` serves a page that lists the newest runs and batches and keeps itself current.
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
ones listed. A run's page is its
timeline as it happens: the assistant's text, each tool call with its input in one line
(the input itself folded under it, its first 6 KB and last 2 KB), each result folded and
capped (its first 12 KB and last 4 KB; all of it stays in `events.jsonl`), how long each
step waited, and, once the
run ends, its status, tokens, cost and final answer (and, for a `limited` run, when the
limit lifts). A batch's page shows each task with a link to each attempt's run, the
backend a usage limit moved it from, and its gates (command, exit code, folded output) as
they finish. Steps arrive over Server-Sent Events that check the run's `events.jsonl` twice a
second; a browser that loses the connection picks up where it left off. Cost is whatever
the backend reports: the api backend's estimate (`~`), the claude and opencode CLIs' own
figures, nothing for codex and gemini, and only once the run has ended.

Each run on the index also says what it was for (review, debug, implement, refactor, test,
research, plan, ops or unknown), from `internal/purpose`'s keyword rules on its prompt's
first paragraph; hover the word for the rule that decided it. Above the batches, a card,
"What the runs were for", sums up the newest 50 runs, the ones listed below, not all time:
one row per purpose that occurs, then "all", each with its runs, how many are running,
tokens (input, output and cache read) and known cost.

That cost is honest about what it does not know. codex and gemini report no cost; such a
run is left out of the sum, not counted as $0. A row whose finished runs all reported a
cost shows their sum; a row where some did not shows "≥ $X", a lower bound over those that
did, with how many runs report no cost under it; a row where none did shows "-". A sum
that includes the api backend's estimate keeps its `~`. Running runs count only as
running: they have no cost yet, known or unknown. A run killed before it saved its result
has no result and stays running on the page (#96), so it counts as running, not as no
cost.

A run's page names its workspace by the same name as its row (`ada-os-ipc`, the repository
for a batch task's worktree), and a batch's page its repository; a task's worktree shows
relative to the repository (`.\.hrn\worktrees\t1`). Hover the name for the whole path,
on this computer only: a phone on the network sees the name alone. In what a run's steps
show (step text, a tool call's one line and its input, results, info lines, the final
answer) and in a run's or batch's header, the workspace is `.` (`.\src\main.go`, in the
path's own slashes), the home folder is `~`, and a program named by its full path in
System32, Program Files, `/usr/bin`, `/bin` or `/usr/local/bin` is its file name: `codex
exec -C . --json`, `"powershell.exe" -Command ...`. A batch task's text is shortened by its
worktree. The index's rows (a run's prompt, a batch's task names) are shortened the same way,
for every viewer. A path only counts whole, so `ada-os-ipc-old` keeps its name. This is the page
only: `events.jsonl`, the records and `hrn show` keep the full paths.

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
turn's figures. While the run goes, each new turn redraws the chart over every turn so far.
The chart is inline SVG coloured by the page's stylesheet, so it needs no script, style or
font beyond the page's own. Usage events themselves are not listed in the timeline.

Everything from a record is escaped as text, since tool results hold whatever the agent
read. The page loads nothing from anywhere else (one stylesheet and a script or two, all
served by hrn, no fonts), follows the system's light or dark theme, and on a phone shows
each run and batch as a small card instead of a table. It cannot start, stop or merge
anything: a request other than GET or HEAD must carry hrn watch's key in
`X-Hrn-Watch-Key`, from this computer or the network alike (a custom header is also the
CSRF check), and as no action exists yet, hrn watch sets no key and refuses them all.

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
stops following and leaves the run alone. A run killed before it saved its result never
ends, on the page or in the terminal: hrn cannot tell it from a long step.

### Behind Cloudflare Access

To reach watch through a Cloudflare Tunnel, forward `https://harness.example.com` to
`http://127.0.0.1:8090` on the same computer. Create a Cloudflare Access application
for that hostname, require email one-time-code login, and copy its team domain and
audience tag into the command:

```
hrn watch --addr 127.0.0.1:8090 --public https://harness.example.com --access-team TEAM.cloudflareaccess.com --access-aud AUDIENCE_TAG
```

All three flags are required together; `--public` cannot be combined with `--lan`.
Watch checks the Cloudflare Access signing keys at start and refuses to serve if they cannot be fetched.
Watch still listens only on loopback. In this mode it accepts only the public Host
and verifies the Access JWT on every request, including static assets and event
streams. A top-level page load from elsewhere, such as the Access login redirect or an
email link, opens the page; cross-site embedded loads and other cross-site requests remain refused.
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
status, Cloudflare connecting IP, and a short reason when refused. Cloudflare's
own Access logs record logins; this file records requests that reached watch.

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
| task | Task two of four in tidedesk passed: fix the flaky test. | yes | no |
| done | Claude has finished in harness. It took four minutes. | yes | yes |
| failed | The batch in tidedesk is done: three of four tasks passed. It took twelve minutes. | yes | yes |
| status | Two things are going: Claude in harness, editing bot.go; and a batch of four tasks in tidedesk. | no | no |

Those are the defaults; the settings change each one, pick the voice and the speed, and
are kept in that browser. A done or failed notification stays until it is dismissed;
clicking one opens its run or batch. Skip (or Esc) stops talking; Snooze keeps it quiet for
15 minutes, an hour, or until unmuted; each run or batch going has a Mute this. "What an
agent is doing" comes from its latest tool call (reading, editing, searching, running a
command and the like), across the backends' different tool names. Ada opens quietly:
what ended before it was opened is not news, and the first thing it says is what is going.
A batch's own runs are told as the batch's, not twice.

Talk to me listens for one sentence each time it is pressed: "status" (or "what are you
doing?"), "skip", "quiet" ("for an hour", "until I unmute"), "unmute" and "repeat". These
steer the bot only; nothing it hears or shows acts on hrn, which waits for the
confirmations and audit log of #38.

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
  parts of five minutes and when it stops.

The page asks for the microphone as it is (echo cancellation, noise suppression and
automatic gain off, one channel) and says so if the browser filters it anyway, as it may
while another tab uses the microphone. Takes are 16-bit mono WAV at the browser's audio rate (usually 48 kHz).
They are saved by the browser into a folder the person picks (`0001.wav`, … with
`metadata.csv` as `id|text`, and `talk-<time>-<part>.wav`), never sent to hrn watch, which
only serves the page; picking the same folder again carries on at the first line not kept.
A browser without folder access (Firefox) downloads each take instead, named by its line.
A line's number never changes, so takes from different sessions line up.

## Secret masking

Human-facing text in watch rows, pages and streams (local and `--lan`), `hrn show`,
terminal progress lines and Ada's speech and notifications masks secret values as
`••••`: URL credentials; `-u`/`--user` user:password flags; JSON secret fields;
authorization, API-key and cookie headers; bare Bearer tokens; recognised `sk-`,
`ghp_`, `github_pat_`, `xox`, `AKIA` and JWT tokens; assignments whose names contain
KEY, TOKEN, SECRET, PASSWORD, AUTH, CREDENTIALS, COOKIE or SESSION; and secret flags
such as `--password`, `--token` and `--api-key`. Masking scans the whole text before
any display excerpt is shortened; it does not cut the text itself.
The existing environment-value redaction also runs. Records remain unchanged,
and JSON record output retains the original text. These rules recognise common
secret shapes; they cannot identify every possible secret.
