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
- output at exactly 28,000 bytes, one byte over, and single lines longer than that.

`FuzzSummarize` checks the same oracle on random input in CI and in `/check`. Inputs holding an
escape byte are also placed in the dropped middle of a head-and-tail cut.

## Tool definitions

2026-10-16, runs from the 30 days before (2026-09-16 to 2026-10-16).

Command: `bash scripts/tool-shapes.sh --home "$HOME/.hrn" --days 30 --definitions`

```json
{"days":30,"definitions":{"auto_yolo":{"tools":6,"bytes":2836,"tokens_estimate":709,"median_api_call_share":0.3206452099193488},"readonly":{"tools":3,"bytes":1409,"tokens_estimate":352.25,"median_api_call_share":0.1593050425868697},"shaping":{"tools":7,"bytes":3295,"tokens_estimate":823.75,"median_api_call_share":0.3725408909323887}},"largest":[{"tool":"edit_file","bytes":554,"share":0.16813353566009104},{"tool":"grep","bytes":518,"share":0.15720789074355082},{"tool":"run_command","bytes":477,"share":0.14476479514415783},{"tool":"read_file","bytes":472,"share":0.14324734446130502},{"tool":"read_original","bytes":458,"share":0.13899848254931715}],"mcp_definitions":"not recorded","api_runs_with_mcp":0,"api":{"runs":176,"runs_from_usage":0,"calls":1110,"median_tokens":2211.1666666666665},"all":{"runs":1557,"runs_from_usage":173,"calls":13112,"median_tokens":77324}}
```

`--definitions` sizes the `tools` field the api loop sends (`APIBackend.ToolDefinitions`) for
each mode and compares it with the median input-plus-cache-read tokens per model call. A run
without usage events counts its totals spread evenly over its turns. Under the
four-bytes-per-token estimate, hrn's own definitions (709 tokens in auto and yolo, 352 read-only,
824 with shaping) are 32%, 16% and 37% of the median api call of 2,211 tokens. That median comes
from 1,110 calls in 176 api runs, none with usage events, so every figure is a run average, and
cache writes are not counted. The size is spread evenly: `edit_file`, `grep`, `run_command`,
`read_file` and `read_original` each hold 14% to 17%, so no one tool dominates. The 77,324-token
median over all runs is mostly CLI backends, which send their own tools. MCP servers'
definitions are not recorded (run records keep only the config file names), and no api run in
the window loaded one.

Trimming would pay by share: cutting a third of the text would save about 240 tokens per call,
about 11% of a median api call. The definitions sit in the cached prefix, though, so most of
those tokens are billed at the cache-read rate, and the money saved is smaller than the share.
`TestToolDefinitionsStayUnderTheirByteBudget` (`internal/agent/tooldefs_test.go`) holds each
mode under its size rounded up to 512 bytes: 1,536 read-only, 3,072 auto and yolo, 3,584 with shaping.
