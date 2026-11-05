# Text feeds

[Command reference](commands.md#hrn-feed)

`hrn feed` applies an instruction to a stream of small text items: classify an
observation, extract fields or summarise a message. It runs up to four items at
once by default and emits one JSONL result per input line as work completes.
Every valid item is an ordinary run, so its events, answer and token usage appear
in `hrn runs` and `hrn watch`, using the same guard as other runs. Invalid input
lines produce a result without starting a model or creating a run record.

```bash
hrn feed -b claude --task 'Classify the item as question, update or other. Reply with one label.' --in items.jsonl --out results.jsonl
```

Example input:

```jsonl
{"id":"message-1","text":"When does the build finish?"}
"The build passed."
```

The task is followed by a blank line and an `<item>` block. Item text is HTML
escaped, including angle brackets and ampersands, so it cannot close that block.
This delimiter preserves the input boundary; it is not a guarantee that a model
will ignore instructions embedded in untrusted text.

Each feed creates an empty temporary workspace outside the current repository.
Items are read-only, single-turn runs with purpose `none`. The API loop offers
no tools and loads no project instructions. Claude runs lean and read-only,
with all built-in tools disabled through `--tools ""` and no MCP servers under
strict MCP configuration. Feed refuses codex, gemini, opencode and agy because
they cannot run without tools yet. Item text is untrusted, so no item may use tools.
There is no fallback backend or automatic carry-on to another backend. Ctrl-C
stops admitting items while already running items retain their per-item timeout.

## Local API inference

The backend must always be explicit. `api` is refused unless
`ANTHROPIC_BASE_URL` is an `http` URL whose host is exactly `127.0.0.1`,
`localhost` or `::1`, so an omitted endpoint cannot accidentally select billed
per-token inference. URL credentials, queries and fragments are refused.
Feed uses a placeholder local credential, disables proxies and refuses redirects;
it does not need or forward the operator's API credentials. Claude uses its
usual plan login.

Point feed at an already running llama.cpp server with a Messages-compatible
endpoint, and supply the name of its loaded model:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8081
hrn feed -b api --model local-model --task 'Summarise the item in one sentence.' --max-tokens 128 --in items.jsonl
```

In PowerShell set the endpoint with
`$env:ANTHROPIC_BASE_URL = 'http://127.0.0.1:8081'` before the same command.
The server is managed separately; feed neither starts nor downloads a model.
The request path is covered by fake SSE tests, not a live llama.cpp run.
`--max-tokens` accepts 1 through 64000. A response cut at that ceiling returns
`status: error`, `error: "response hit max_tokens"` and no output.

`--thinking off|on` defaults to `off` and affects only the API backend; CLI
backends ignore it. Off omits the `thinking` field and, for loopback HTTP
endpoints, sends `chat_template_kwargs: {"enable_thinking": false}` for llama.cpp.
On preserves the API's adaptive thinking request. In a local llama.cpp measurement
with Gemma 4 26B-A4B, disabling thinking reduced a one-word answer from 160-200
output tokens and 6.8 s to 2 tokens and 1.1 s, with the same answer.

See the [command reference](commands.md#hrn-feed) for flags, limits, result
fields, failure handling and the stderr summary.

## Later stages (not built)

This is the text-stream slice of [the staged pipeline design](https://github.com/cristiangirlea/harness/issues/224).
Front models per sense would turn their respective streams into small structured
observations. Fusion would combine them, and events would carry the combined
observations to later consumers. Those stages, multimodal inputs and continuous
event routing are not built here. The local-model eval belongs in `hrn-evals`
separately; the feeder supplies the recorded per-item execution underneath it.
