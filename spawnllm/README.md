# spawnllm

`go.bison9.com/agent/spawnllm` is an **LLM-dispatch core** for host applications.
It calls an LLM and drives it to completion:

- **CLI providers** (claude-cli, codex-cli, cursor-cli, antigravity-cli; gemini-cli is an alias for antigravity-cli): run the subprocess, return the result.
- **API providers** (OpenAI-chat/responses, Azure, Anthropic): run the LLM↔tool-call loop, giving the model access to host-injected `toolspec.Tool`s until it is done.

## Boundaries

spawnllm imports only [`toolspec`](../toolspec),
[`accounting`](../accounting), the shared logger and the standard library
(plus provider SDKs). It **never imports a host**. Policy —
model selection, fallback, cooldown, config, results handling — stays in the host
application. The host resolves a `ProviderSpec`, injects tools, and calls
the worker.

Logging is host-injectable via `go.bison9.com/agent/logger` (`logger.SetBackend`), so
provider/loop logs flow into the host's own logger; spawnllm is silent until a
backend is installed.

## Identity and accounting

Every call that can launch a model takes an `accounting.Identity` right
after the context: `LLMProvider.Chat`, `speech` `Transcribe` and
`Synthesize`, and `imagegen` `Generate` and `Edit`. A `Worker` takes it from
`WithAccounting`.

- The agent name is required. A missing agent, or an agent or project name
  longer than 16 characters, is refused before anything is launched; the
  error wraps `accounting.ErrAgentRequired` or `accounting.ErrNameTooLong`.
- An empty project is recorded as `default`.
- Every call, refused or not, reports exactly one `accounting.Record`: kind,
  provider, transport, model, the tokens and cost the provider reported, the
  number of tool calls the model made, the HTTP status (API providers),
  duration and outcome. A value the provider did
  not report is written as `"-"`, never as 0.
- `DispatchStatus.Reported` and `TaskUsage.Reported` say which counts a
  response actually carried. `DispatchStatus.ToolCalls` is the tool call
  count.

See [`accounting`](../accounting) for the record and where it is written.

## Generated images

Images a model returns come back in `LLMResponse.Images` (and, for a whole
run, `Result.Images`), passed through unfiltered and whatever their size:
spawnllm executes the call and leaves judging the data to the caller. Each
`GeneratedImage` has exactly one of `Data` (decoded base64, with its
`ContentType`) or `URL` (an image given as a URL, never fetched; also base64
that does not decode, returned as a data URL so nothing is lost). `Size` is
the decoded length in bytes, or -1 (unknown) for a `URL` image.

## Speech and image generation

Single-shot task calls live in their own packages. Each call is one request:
the caller supplies the base URL, API key and model and keeps every policy
decision (model choice, fallback, cooldown, limits). Results are passed
through: no size caps, no rewriting, a URL stays a URL. The timeout is the
context's deadline (the default HTTP client has none of its own; supply one
with `WithHTTPClient`). A non-2xx response is a `*common.HTTPStatusError`
(status, Retry-After, a body excerpt of at most 1 KiB), so `ClassifyError`
handles it as it does a chat provider's error.

- `speech` — `Transcriber` (speech to text) and `Synthesizer` (text to speech).
  - `NewOpenAI(baseURL, apiKey, ...)`: `POST {base}/audio/transcriptions` and
    `/audio/speech` (OpenAI, Groq, local servers, OpenRouter). Transcription
    is a multipart upload, or OpenRouter's JSON body with base64
    `input_audio` for an `openrouter.ai` base URL (or with
    `WithTranscriptionAPI`); OpenRouter's `usage.cost` is returned as
    `CostUSD`. `DefaultSTTModel(baseURL)` gives the usual model for Groq,
    OpenAI and OpenRouter.
  - `NewGemini(baseURL, apiKey, ...)`: `generateContent` with inline audio,
    and TTS models with `responseModalities: ["AUDIO"]`. The audio is returned
    as Gemini sends it: raw PCM (e.g. `audio/L16;codec=pcm;rate=24000`), not
    converted.
  - `NewElevenLabs(baseURL, apiKey, ...)`: Scribe (`/v1/speech-to-text`) and
    `/v1/text-to-speech/{voice_id}`.
- `imagegen` — `Generator` and `Editor`. `NewOpenAI(baseURL, apiKey, ...)`:
  `POST {base}/images/generations` (OpenRouter: `{base}/images`) and
  `/images/edits` (multipart). Images are `GeneratedImage` values, as for chat
  image output: base64 decoded into `Data`, a URL returned as `URL` with
  `Size` -1, never fetched.

## Building and testing

Run `make test` at the root of the repository. It is the one gate for the
whole module; see the root README.
