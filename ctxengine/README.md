# ctxengine — context engine for LLM agents

This modile is the part of an AI assistant, or agent, that decides what the
model sees. Given a stream of messages and a token budget, it keeps the transcript,
assembles each request, evicts stale tool results, and summarises older history
into a rolling summary when the window fills. 

It also keeps the whole conversation on disk and searchable.

This engine was originally written for a host application
and subsquently separated into it's own clearly-defined module.

## What it owns

- **The transcript.** Every message is ingested once and given a monotonic
  sequence number. That number is what everything else cites: summaries,
  the session tools, and any memory system that derives evidence from the
  conversation.
- **The live window and the archive.** A per-session SQLite archive with
  full-text search holds everything; a live window holds what the model is
  currently shown.
- **Assembly.** One `Assemble` call per model dispatch: the host's prompt
  layers, the rendered summary, the recent history, and any injections the
  host gathered (a memory system's blocks, say), each placed where the host
  says — stable content in the system prefix, per-turn content on the latest
  user message — so prompt caching keeps working.
- **Eviction.** An LLM-free per-turn sweep that collapses superseded or
  oversized re-retrievable tool results (file reads, web fetches) to short
  placeholders.
- **Compaction.** Threshold-, count- and age-triggered summarisation with
  evidence-cited, validated summaries, a safety net when the built request is
  still too large, per-model refusal memory, a failure circuit breaker, and
  durable summary checkpoints.
- **Tools.** `session_messages`, `session_search`, `session_summary_list`,
  `session_summary_get`, `session_info`, `session_compact`, `session_clear`,
  as [toolspec](../toolspec) definitions over a `Host`
  struct.

## Storage

Every session is one SQLite file, `<sessions dir>/<key>.archive.db`, where the
key has `:`, `/` and `\` replaced by `_` (`memory.SanitizeSessionKey`). It
holds four things:

- `messages` and its FTS5 index — the all-time archive, one row per ingested
  message keyed by seq;
- `summaries` — the summary checkpoints compaction has produced;
- `window` — the live history window the model is shown, in the same seq
  space as `messages`;
- `session_state` — the one-row session record: key, current summary, next
  seq, created/updated timestamps, the compaction counters and the
  pending-turn flag.

`session.NewSQLiteStore(dir)` is the `SessionStore` the engine and the host
use; it opens each session's database lazily and closes it on `ForgetSession`
or `Close`, after the operation in flight on that session; a session has one
open handle at a time, and each write reads the next sequence number inside
its own transaction, so no two messages can get the same one. Read-only access by path (a web UI listing sessions, say) goes
through `memory.OpenReadOnly` and its `Window()` / `State()` methods;
`memory.ListSessions` and `memory.DeleteSession` enumerate and remove whole
sessions. Truncation deletes window rows; nothing is logically skipped.

Directories ctxengine creates are `0700` and its files (databases with their
`-wal`/`-shm` files, `compact.jsonl`) are `0600`; existing files with another
mode are changed to it, existing directories are left alone. To change the
modes, pass an `fsmode.Modes{Dir, File}` (a zero field keeps the default):

- `ctxengine.WithFileModes(modes)` — Manager option.
- `session.NewSQLiteStore(dir, session.WithFileModes(modes))` — the store.
- `memory.Open(path, memory.WithFileModes(modes))` — a single archive.

### Migrating from the JSONL layout

Earlier host versions kept the window in `<key>.jsonl` and the state in
`<key>.meta.json` beside the archive. `session.MigrateJSONL(dir, logger)` folds every
such pair into its `<key>.archive.db` and renames the sources to
`*.jsonl.migrated` / `*.meta.json.migrated`, which can be deleted once the
result is verified. Run it once with the service stopped; it is idempotent (a
session whose database already holds a window is reported as skipped) and it
never guesses a key from a file name — a `.jsonl` without its `.meta.json` is
reported as an error. A legacy `<key>.summaries.jsonl` needs no step of its
own: the archive imports it into `summaries` the first time it is opened.

## Stores

The manager takes any `session.SessionStore`. Two ship with the engine:

- `session.NewSQLiteStore(dir)` — the durable store described above.
- `session.NewMemory()` — everything in process memory, nothing on disk,
  nothing kept after the process ends. For tests and small or ephemeral
  hosts. Its size is bounded only by what is stored; `DeleteSession` frees a
  session. It has no archive, so `GetArchiveBounds` is always `(0, 0)` and
  compaction keeps the current summary but no checkpoint log.

Both pass the same conformance suite, `sessiontest.Run`; a new store should
run it too.

A host with no disk at all uses `session.NewMemory()` and leaves out the
options that name a directory. Without them:

- no `WithArchiveDir`: there is no all-time archive or summary log, so
  nothing beyond the live window can be recalled, and summary references are
  not checked against archive bounds;
- the archive-backed session tools (`messages`, `search`, `summary_list`,
  `summary_get`) need `Host.SessionsDir` and refuse without it; `compact`,
  `info` and `clear` work through their closures;
- no `WithCompressionProfileDir`: no compression profile and no
  `compact.jsonl` debug log.

Eviction placeholders and the rendered summary still name
`session_messages` as the way back to older messages; without an archive the
model's only way back to an evicted result is to re-read the resource.

`NewMemory()` with `WithArchiveDir` archives messages but no summary
checkpoints: the manager hands checkpoints to the store, and the in-memory
store does not keep them.

## What it does not own

- **Models.** It never calls a provider. Compaction goes through a
  `ModelCaller` the host supplies:

  ```go
  type ModelRequest struct {
      System, User string
      JSONObject   bool     // ask for a JSON-object response where supported
      Exclude      []string // models the engine has learned to avoid
  }
  type ModelReply struct{ Content, FinishReason, Model string }
  type ModelCaller interface {
      Complete(ctx context.Context, req ModelRequest) (ModelReply, error)
  }
  ```

  The host owns model selection, credentials, transport, retries on
  transport errors and cooldowns, and honours `Exclude`. The engine owns its
  output: it classifies refusals, remembers refusing models per session, and
  calls again with a longer `Exclude`. The same shape serves
  [cogmem](../cogmem), so one host client and one
  test fake serve both.
- **The system prompt.** The host passes it as ordered layers; the engine
  only inserts its summary between the layers marked before and after it.
- **Memory.** Nothing here knows what a memory system is. The host asks its
  memory for blocks and hands them to `Assemble` as injections with a
  placement.
- **Logging, failure dumps, refusal rules, message envelopes.** All injected:
  `WithLogger`, `WithFailureDump`, `WithRefusalClassifier`,
  `WithNoiseKey` (how repeated scheduled messages are recognised and
  collapsed without the engine knowing what a scheduler is).

## Host obligations

- Pass a `*slog.Logger` with `WithLogger` (silent otherwise), and the same
  logger to `session.WithLogger` and the session tools' `Host.Logger`.
- Provide a `ModelCaller` for compaction.
- Call `AddUserMessage`/`AddAssistantMessage`/`AddToolCallMessage`/
  `AddToolResult` as the conversation happens, and `Assemble` before every
  model call.
- Call `CompactIfDue` once a turn's reply has been sent: the age trigger
  never makes an add wait on the summary model, it defers the pass to this.
- Call `SetContextWindow` when the session moves to a model with another
  context window; the next add and `Assemble` measure against it.
- Give the tools a `Host` (sessions directory and the compact, clear and
  info closures) if the model should be able to reach the archive.


## Building and testing

Run `make test` at the root of the repository. It is the one gate for the
whole module; see the root README.

The suite covers each part on its own and the engine as a whole: `e2e_test.go`
drives conversations through the real SQLite store and archive, compacts them
with a scripted model caller, reopens the session and reads it back through
the session tools.

## Copyright and license

Copyright (c) 2026 Tenebris Technologies Inc.

This software is licensed under the MIT License. Please see LICENSE for details.

## Trademarks

Any trademarks referenced are the property of their respective owners, used for identification only, and do not imply sponsorship, endorsement, or affiliation.

## No Warranty

**(zilch, none, void, nil, null, "", {}, 0x00, 0b00000000, EOF)**

THIS SOFTWARE IS PROVIDED “AS IS,” WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, AND NON-INFRINGEMENT. IN NO EVENT SHALL THE COPYRIGHT HOLDERS OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

Made in Canada with internationally sourced components.
