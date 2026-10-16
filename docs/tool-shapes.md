2026-10-14

Command: `bash scripts/tool-shapes.sh --home "$HOME/.hrn" --days 30 --json`

```json
{"shapes":[{"shape":"test","bytes":5051647,"tool_result_share":0.05829519214656731,"input_token_share_estimate":0.0008861343195603294,"input_token_share_replayed_estimate":0.002209496013204698},{"shape":"log","bytes":5626975,"tool_result_share":0.06493438453417877,"input_token_share_estimate":0.0010249843735109713,"input_token_share_replayed_estimate":0.001476474725123373},{"shape":"json","bytes":135698,"tool_result_share":0.0015659330479554274,"input_token_share_estimate":0.000024987378690170388,"input_token_share_replayed_estimate":0.00017734874636410956},{"shape":"table","bytes":161274,"tool_result_share":0.001861075965570337,"input_token_share_estimate":0.00002968481621519596,"input_token_share_replayed_estimate":0.000842728225603632},{"shape":"terminal","bytes":66857407,"tool_result_share":0.771523700584434,"input_token_share_estimate":0.01205544370086366,"input_token_share_replayed_estimate":0.05707727240152826},{"shape":"other","bytes":8823322,"tool_result_share":0.10181971372129417,"input_token_share_estimate":0.001594784467809834,"input_token_share_replayed_estimate":0.042582007694569456}],"inline_scripts":[{"shape":"heredoc","count":6,"runs":4,"command_bytes":1256},{"shape":"heredoc","count":3,"runs":1,"command_bytes":548},{"shape":"heredoc","count":3,"runs":1,"command_bytes":140},{"shape":"heredoc","count":3,"runs":1,"command_bytes":915},{"shape":"heredoc","count":2,"runs":2,"command_bytes":9757},{"shape":"heredoc","count":2,"runs":2,"command_bytes":92},{"shape":"heredoc","count":2,"runs":2,"command_bytes":72},{"shape":"heredoc","count":2,"runs":1,"command_bytes":387},{"shape":"python","count":2,"runs":2,"command_bytes":610},{"shape":"heredoc","count":1,"runs":1,"command_bytes":337},{"shape":"heredoc","count":1,"runs":1,"command_bytes":736},{"shape":"heredoc","count":1,"runs":1,"command_bytes":4529},{"shape":"heredoc","count":1,"runs":1,"command_bytes":3643},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1143},{"shape":"heredoc","count":1,"runs":1,"command_bytes":18370},{"shape":"heredoc","count":1,"runs":1,"command_bytes":833},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1195},{"shape":"heredoc","count":1,"runs":1,"command_bytes":385},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1367},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1502}],"tool_result_bytes":86656323,"input_plus_cache_read_tokens":1354994072,"inline_script_bytes":1447040,"inline_script_input_share_estimate":0.0002577658878495817,"inline_script_input_share_replayed_estimate":0.004257569179977933,"repeated_inline_script_bytes":13777,"repeated_inline_script_input_share_estimate":0.0000025418930393667433,"repeated_inline_script_input_share_replayed_estimate":0.000010029195168316573,"claude":{"shapes":[{"shape":"test","bytes":737513,"tool_result_share":0.027409209384049178,"input_token_share_estimate":0.00020065247615785254,"input_token_share_replayed_estimate":0.0032978219986000766},{"shape":"log","bytes":666151,"tool_result_share":0.024757085285810207,"input_token_share_estimate":0.00018649833355429215,"input_token_share_replayed_estimate":0.0022404005718496462},{"shape":"json","bytes":93335,"tool_result_share":0.00346873690071935,"input_token_share_estimate":0.000026130444842520478,"input_token_share_replayed_estimate":0.0002691087263534214},{"shape":"table","bytes":154467,"tool_result_share":0.005740669447082186,"input_token_share_estimate":0.00004324520730154402,"input_token_share_replayed_estimate":0.0012780353398533916},{"shape":"terminal","bytes":16655844,"tool_result_share":0.6190040252362455,"input_token_share_estimate":0.004635499760347411,"input_token_share_replayed_estimate":0.08660896902066288},{"shape":"other","bytes":8600179,"tool_result_share":0.31962027374609353,"input_token_share_estimate":0.0023574515824624272,"input_token_share_replayed_estimate":0.06431475988425014}],"inline_scripts":[{"shape":"heredoc","count":3,"runs":1,"command_bytes":140},{"shape":"heredoc","count":2,"runs":2,"command_bytes":92},{"shape":"heredoc","count":2,"runs":2,"command_bytes":72},{"shape":"heredoc","count":2,"runs":1,"command_bytes":387},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1143},{"shape":"heredoc","count":1,"runs":1,"command_bytes":18370},{"shape":"heredoc","count":1,"runs":1,"command_bytes":833},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1195},{"shape":"heredoc","count":1,"runs":1,"command_bytes":385},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1367},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1502},{"shape":"heredoc","count":1,"runs":1,"command_bytes":4493},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1114},{"shape":"heredoc","count":1,"runs":1,"command_bytes":807},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1255},{"shape":"heredoc","count":1,"runs":1,"command_bytes":572},{"shape":"heredoc","count":1,"runs":1,"command_bytes":858},{"shape":"heredoc","count":1,"runs":1,"command_bytes":1251},{"shape":"heredoc","count":1,"runs":1,"command_bytes":700},{"shape":"heredoc","count":1,"runs":1,"command_bytes":2980}],"tool_result_bytes":26907489,"input_plus_cache_read_tokens":892971786,"inline_script_bytes":1414242,"inline_script_input_share_estimate":0.000382971226371983,"inline_script_input_share_replayed_estimate":0.006441964729667282,"repeated_inline_script_bytes":691,"repeated_inline_script_input_share_estimate":1.934551602955124e-7,"repeated_inline_script_input_share_replayed_estimate":0.000012184595494039495},"runs":{"read":1196,"with_usage":9,"with_tokens":1158,"skipped_unreadable":0}}
```

Output shaping (#296) has a once-counted input-token share of 1.5616% and a replayed share of 10.4365% under the four-bytes-per-token estimate (0.7449% once-counted and 15.8009% replayed for Claude). The replayed share bounds the estimated context budget shaping could touch if every eligible result byte were removed from every later call. The window contains 86,656,323 tool-result bytes and 1,354,994,072 input tokens; 1158 of 1196 finished runs have token figures, including 9 with usage events, and 0 were skipped as unreadable. Token shares use bytes from those same runs. Terminal output dominates the all-runs result budget at 77.15%, while test output accounts for 5.83%. These are maximal removable byte estimates, not measured savings: classification is heuristic, replacements retain useful output, and replay assumes retained context; the ordinal fallback is approximate when model calls and tool results are not one to one.

Kept scripts (#298) have a once-counted input-token share of 0.025777% and a replayed share of 0.425757% before allowing for replacement invocations (0.038297% once-counted and 0.644196% replayed for Claude). The replayed share bounds the estimated context budget kept scripts could touch. The detector finds 1,447,040 inline command bytes across all runs; repeated normalised groups contain 13,777 bytes and account for 0.000254% once-counted and 0.001003% replayed when restricted to runs with token figures. Totals include groups beyond the top 20 printed. Repeated command text offers a much smaller replayed token budget than output shaping, so keeping scripts is supported chiefly by reuse and maintainability here. Normalisation can merge different scripts, the detector covers only the documented forms, and the call-ordinal fallback is approximate; scripts still cost their invocations and results. Result bytes belong to shaping and are not added again.

## Invariant probes

`internal/shape/adversarial_test.go` attacks the rule that `Summarize` returns true only when the
summary keeps every error, `FAIL` and `panic` line, every failing test and its diagnostics, and
invents no number, path or negation. An oracle written in the test checks each case; a false
return (head-and-tail cut) always passes. Attack classes:

- one failure buried in thousands of passing tests, and one test name failing in two packages;
- negations ("not found", "no such file", "only on Windows") on lines the summary drops;
- repeated lines whose numbers differ in one digit (`exit 1`/`exit 11`, ports `8081`/`8082`);
- error strings wrapped across lines, colour codes inside `error` and `FAIL`, CRLF, NUL, invalid UTF-8;
- panics with goroutine dumps, stack traces, and log lines repeated with changing timestamps;
- output at exactly 28,000 bytes, one byte over, and single lines longer than that;
- JSON: hand-made summaries with a wrong item or key count, a count of the other kind, a changed
  number, an invented path or negation in the sample, a dropped error line, or an error invented
  in the sample; values holding error lines (also in the first item), deep nesting (and nesting
  past the parser's limit), exponent and negative numbers, and a single huge string;
- tables: a wrong row or column count, a table first line over JSON or over text that is not a
  table, an error row in the omitted middle or the last row, a changed number in a kept row, and
  CSV with quoted commas;
- a summary of one kind's shape presented for the other kind, and NDJSON from `go test -json`.

The oracle recounts a `json:` or `table:` first line itself (with `encoding/json`, and by counting
delimiters) and exempts only a true one from its digit check. It ends path chunks at `"` as
well as at whitespace, so a sample joined without spaces keeps the original's string chunks.

`FuzzSummarize` checks the same oracle on random input in CI and in `/check`. It covers all five
kinds. Inputs holding an escape byte are also placed in the dropped middle of a head-and-tail cut.

## JSON and table summaries

`Classify` checks JSON first, then test, table, log and terminal. Text is JSON when, trimmed, it
is one valid object or array. It is a table when at least five non-blank lines all have the same
count (at least one) of tabs, or else of commas, unless most of them start like log lines
(log4j and Python timestamps hold a comma): those stay logs, whose summary keeps warnings. Its
summaries:

```
json: array of 500 items
keys: "id","name","tags","owner"
sample: {"id":0,"name":"item-0","tags":["a","b"],"owner":{"login":"dev","meta":{...}}}
```

```
table: 1000 rows, 4 columns
id,name,status,ms
1,job-1,ok,10
2,job-2,ok,20
3,job-3,ok,30
[... omitted ...]
600,job-600,error: timeout,6000
[... omitted ...]
999,job-999,ok,9990
1000,job-1000,ok,10000
```

The JSON sample is the first item (or the object itself), re-joined from the original's raw
bytes, never re-encoded. So every key, string and number keeps its spelling (`1e5`, `-0`,
escapes). Containers below the second level become `{...}` or `[...]`. Every original line
containing an error follows verbatim. A keys or sample line that would contain one is left out:
it is not an original line. Minified JSON is a single line, so if that line holds an error the
summary cannot be smaller and the cut is used. A table summary keeps the header, the first three
and last two rows, and every error row, in order. Either summary is used only when it is smaller
than the original and within 28,000 bytes. `valid` recomputes the first line from the original
for the kind it was given. Only that exact line may hold numbers the original lacks. A `json:` or
`table:` line anywhere else, or with any other count, fails, so an ordinary output line that
looks like one of these first lines makes the summary fall back to the cut.

NDJSON, such as `go test -json`, is neither: the text is not one JSON value, and lines that are
each a JSON object or array are refused as a table even when their comma counts agree. It falls
to the line-based kinds, usually terminal, because its test lines start with `{`. Turning it back
into test text would be a converter with its own invariants, not a summary, so it waits for a
measured need.

The measurement in `scripts/tool-shapes` is broader: it checks test and log first, counts any
valid JSON value, and counts a table when `encoding/csv` parses at least two rows of two or more
fields (quoted commas allowed). Its `json` and `table` bytes are an upper bound on what these
summaries can take.
