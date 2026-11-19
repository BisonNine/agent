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
| `cost_usd` | The cost the call returned, or an estimate from the pricing table. |
| `cost_source` | `provided` (returned by the call), `estimated` (from the pricing table) or `-`. |
| `tool_calls` | Tool calls the model made: those in an API response, or those a CLI's output reports (only Codex's does). |
| `http_status` | The HTTP response status. |
| `duration_ms` | Wall-clock time of the call. |
| `success` | Whether the call succeeded. |
| `error` | The error; left out on success. |
| `audio_seconds` | Billed audio seconds, when reported. |
| `characters` | Characters of text sent for speech. |
| `images` | Images the call returned: an image generation, or a chat reply from `openai-chat` or `openai-responses`. |
| `notes` | A list of short remarks on how the record was filled in: the pricing row or fallback an estimate used, or why the cost could not be estimated. Left out when there is nothing to say. |

A number the provider did not report is written as `"-"`, never as 0; a
reported 0 is written as 0. `http_status` is `"-"` for a CLI call and for a
call that got no HTTP response.

```json
{"time":"2026-11-19T15:04:05.123456Z","agent":"alice","project":"default","kind":"text","provider":"openai-chat","transport":"api","model":"gpt-test","input_tokens":10,"output_tokens":3,"cache_read_tokens":2,"cache_creation_tokens":"-","cost_usd":"-","cost_source":"-","tool_calls":0,"http_status":200,"duration_ms":412,"success":true,"audio_seconds":"-","characters":"-","images":0}
```

## Pricing

An optional pricing table lets `Report` estimate the cost of a call that
returned none. There are no built-in prices; an estimate is only as good as
the table.

- `LoadPricing(path)` reads a JSON file and installs it; `SetPricing(rows)`
  installs rows from memory. Either replaces the whole table, and on error
  keeps the one in place. An empty table, or `ClearPricing()`, removes it.
- Prices are in US dollars: `input`, `cache_read`, `cache_write` and
  `output` per million tokens, `characters` per million characters,
  `audio_minute` per minute of audio, `image` per image. Each is optional,
  and 0 is a real, free price.
- A row needs `model` and at least one price. A negative price, an empty
  model, a duplicate model, a misplaced `*` or an unknown field is an error.

```json
{"prices": [
  {"model": "gpt-4o-2024-08-06", "input": 2.5, "cache_read": 1.25, "output": 10},
  {"model": "gpt-4o*", "input": 2.5, "output": 10},
  {"model": "whisper-1", "audio_minute": 0.006},
  {"model": "*", "image": 0.04}
]}
```

Models:

- `model` is an exact name, or a prefix ending in one `*` (`gpt-4o*`).
  `*` alone is the default row and matches any model.
- `*` is allowed only as the last character.
- An exact name wins; otherwise the longest matching prefix; `*` last. The
  order of rows does not matter.
- Matching is case-sensitive. A record with no model matches only `*`.

How a cost is settled:

- A cost the call returned is kept (`provided`); the table is not used.
- The row is chosen by model, as above.
- The estimate is the sum of every price the row sets times the record's
  quantity (`cache_write` prices `cache_creation_tokens`; `audio_minute`
  prices `audio_seconds` / 60).
- Cache tokens are priced at `input` when the row has no `cache_read` or
  `cache_write` price. Cached reads usually cost less than input, so set
  `cache_read` for a closer estimate.
- The cost stays `-` when there is no row, when a priced quantity is
  unknown, or when a token count is known and not zero but the row has no
  price for it.
- Failed calls are estimated too, since they may still be billed.
- `notes` names the row when it was a pattern, each cache price that fell
  back to `input`, and the reason when a table is loaded but the cost stays
  `-`.
