# accounting

`go.bison9.com/agent/accounting` records every LLM or API call launched
through spawnllm: one `Record` per launch, refused launches included.

## Use

```go
if err := accounting.Start("/var/lib/example/accounting"); err != nil {
	return err
}
defer accounting.Close()
```

- `Start(dir)` creates `dir` (mode 0700) and appends to `dir/accounting.log`
  (mode 0600). It fails while a sink is already running; after `Close` it can
  be called again.
- `Report(r)` queues one record. It never drops a record: it blocks while the
  queue is full, so a log file on a stalled device blocks the callers.
  spawnllm calls it; a host rarely needs to.
- `Close()` writes what is queued, closes the file and returns. A second
  `Close`, or a `Start`, waits until that is done.

Without a running sink (before `Start`, after `Close`), or when a write to the
file fails, the record goes to the shared logger instead: an info event of
component `accounting`, message `accounting record`, whose field `accounting`
holds the record's JSON. A failed write is also logged as an error.

## Identity

`Identity{Agent, Project}` names who a call is for. `Normalize` trims spaces,
requires an agent (`ErrAgentRequired`), refuses a name longer than 16
characters (`ErrNameTooLong`) and sets an empty project to `default`.

## Record

One JSON object per line, in this order:

| Field | Meaning |
| --- | --- |
| `time` | Launch start, UTC, RFC 3339 with microseconds. |
| `agent`, `project` | The identity. |
| `kind` | `text`, `image`, `speech` or `transcription`. |
| `provider` | The provider, e.g. `claude-cli`, `openai-chat`, `anthropic`, `azure`, `elevenlabs`. |
| `transport` | `cli` or `api`; empty when nothing could be launched (spawnllm's `UnconfiguredProvider`). |
| `model` | The model that served the call when the response names it, else the one requested. |
| `input_tokens` | Input tokens not read from or written to the cache; the whole prompt when the provider does not break cache tokens out. |
| `output_tokens` | Output tokens. |
| `cache_read_tokens`, `cache_creation_tokens` | Prompt cache tokens. |
| `cost_usd` | The cost the provider reported. |
| `tool_calls` | Tool calls the model made: those in an API response, or those a CLI's output reports (only Codex's does). |
| `http_status` | The HTTP response status. |
| `duration_ms` | Wall-clock time of the call. |
| `success` | Whether the call succeeded. |
| `error` | The error; left out on success. |
| `audio_seconds` | Billed audio seconds, when reported. |
| `characters` | Characters of text sent for speech. |

A number the provider did not report is written as `"-"`, never as 0; a
reported 0 is written as 0. `http_status` is `"-"` for a CLI call and for a
call that got no HTTP response.

```json
{"time":"2026-11-19T15:04:05.123456Z","agent":"alice","project":"default","kind":"text","provider":"openai-chat","transport":"api","model":"gpt-test","input_tokens":10,"output_tokens":3,"cache_read_tokens":2,"cache_creation_tokens":"-","cost_usd":"-","tool_calls":0,"http_status":200,"duration_ms":412,"success":true,"audio_seconds":"-","characters":"-"}
```
