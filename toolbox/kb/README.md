# kb — knowledge bases as OKF bundles

`go.bison9.com/agent/toolbox/kb` reads, searches and writes knowledge bases
stored as [Open Knowledge Format](https://github.com/open-knowledge-format/open-knowledge-format)
(OKF) v0.3 bundles: directories of Markdown concepts, each a file with a YAML
frontmatter block and a Markdown body. A concept's ID is its path in the
bundle without `.md` (`guidance/crypto-review`, `SFR/FCS_COP.1`).

`toolbox/kb/tools` publishes it to a model as three tools, mounted by the
host under the namespace `kb`:

| Tool | Purpose |
| --- | --- |
| `kb_get` | one concept by exact ID, with the concepts it includes |
| `kb_search` | ranked sections of one knowledge base: ID, title, heading, passage |
| `kb_put` | create or replace one concept |

The tools use only the package's exported API, the same one a host's own
code (inserting records into a worker's prompt before dispatch, say) uses.

## The API

```go
store, err := kb.New(kb.Options{
    Modes:  fsmode.Modes{Dir: 0o700, File: 0o600}, // what Put creates
    Limits: kb.Limits{},                            // zero fields: defaults
    Logger: logger,
})
defer store.Close()

rec, err := store.Get(ctx, resolver, "cc2022", "SFR/FCS_COP.1")
// rec.Frontmatter, rec.Body, rec.Includes (one level), rec.Missing

hits, err := store.Search(ctx, resolver, "audit-methods", "key sizes", 5)
// []kb.Hit{ID, Title, Section, Passage, Score}; none is a success

res, err := store.Put(ctx, resolver, "audit-methods", "guidance/crypto-review",
    kb.Frontmatter{"type": "Evaluation Guidance", "title": "Review cryptographic claims"},
    "Record the evidence for each claimed algorithm and key size.\n")
// res.Created: false when an existing concept was replaced

problems, err := store.Verify(ctx, resolver, "cc2022")
// every rule a bundle breaks, before activating a revision edited in Git
```

Every method takes the `Resolver` for the caller, so one `Store` serves every
agent. `kb.ValidateID` checks a concept ID; `kb.FrontmatterSchema` is the
JSON Schema every written frontmatter satisfies.

Errors a caller can act on match one of five sentinels with `errors.Is`:
`ErrUnknownKB`, `ErrNotFound`, `ErrReadOnly`, `ErrInvalid` (a
`*ValidationError` listing each `FieldError`: concept, field path such as
`frontmatter.sources[1].resource`, and reason) and `ErrTooLarge`. Any other
error is an unexpected failure: I/O, the index, a broken rule schema.

## What the host provides

### A resolver

The library knows no naming scheme and no folder layout. The host implements
`kb.Resolver` per agent:

```go
type Resolver interface {
    Resolve(ctx context.Context, name string) (kb.Bundle, error) // kb.ErrUnknownKB if absent
    Bundles(ctx context.Context) ([]kb.Info, error)              // for the system prompt
}

type Bundle struct {
    Name     string       // as results report it; "" = the name resolved
    Root     string       // absolute bundle root
    Confine  string       // optional absolute ancestor; Root is opened through it
    Writable bool         // Put allowed
    Create   bool         // Put may create a missing Root
    Rules    []kb.Rule    // dataset rules
    Modes    fsmode.Modes // overrides Options.Modes for this bundle
}
```

- **`Confine`** is for bundles the agent names itself: with the agent's
  bundle folder as `Confine`, a link under it cannot lead the bundle out of
  it. A shared bundle the host configured needs none.
- **`Create`**: a missing root reads as an empty bundle (`Get` is
  `ErrNotFound`, `Search` empty) when set, and as `ErrUnknownKB` when not.
- **`Rules`**: `kb.Rule{PathPrefix, Type, Schema}` applies a JSON Schema
  (a bundle-relative file, draft 2020-12 by default) to the frontmatter of
  every concept whose ID starts with `PathPrefix` *or* whose type is `Type`,
  so another type cannot slip past a path's schema; a rule with neither
  applies to all. A schema may not reference other files or URLs. It is
  recompiled when its file changes.

`example_test.go` has a complete resolver for the first host's rule: own
bundles are the directories directly under a confined folder, named
without `/`, writable and created by a put; shared bundles are an explicit
list addressed as `shared/<name>`, never created; `Bundles` lists both.

### Store lifetime

Create one `Store` per process and `Close` it at shutdown. It holds the
search index (in memory) and the per-concept write locks, so two stores over
the same bundles do not serialise their writes against each other.

### Mounting the tools

```go
tools, err := kbtools.New(kbtools.Config{
    Store:    store,
    Resolver: func(c *toolspec.ToolCall) kb.Resolver { return resolverFor(c.AgentID) },
})
// Register tools (a toolspec.ToolProvider and HostMeta) under "kb":
// kb_get, kb_search, kb_put.
```

- `Resolver` is called per call; a nil function or a nil result means the
  caller has no knowledge bases (every call: unknown knowledge base).
  `RegisterTools` ignores its `Deps`: identity comes from the call.
- `get` and `search` are read-only; `put` is not read-only, idempotent and
  closed-world. None is session-scoped or async, and none is allowed by
  default (`DefaultAllow` nil): grant `kb_get` and `kb_search` to evaluators
  and `kb_put` to authors.
- Results are compact JSON in `ForLLM`. The five sentinels give an error
  result (`IsError`, `Err` set) with `{"error","kb","fields":[...]}`; any
  other failure is the handler's Go error.

### Listing knowledge bases for the prompt

The tools do not say which knowledge bases exist. Call the agent's
`Resolver.Bundles` when building its system prompt and list each name, with
whether it is writable.

## The write path

`Put` replaces the whole record: keys missing from the frontmatter are
removed, so a model gets, changes and puts. In order:

1. **Resolve.** The ID is checked, and a read-only bundle refused (the error
   names it) before anything is created.
2. **Validate.** The frontmatter schema; every `sources` entry has a
   `resource`; OKF timestamps are RFC 3339 date-times with an offset or
   dates; `x-tenebris-include` lists at most `Limits.Includes` canonical IDs,
   not the concept itself. Other OKF families pass through.
3. **Dataset rules,** then each include must be a concept in the bundle.
4. **Publish.** Under a lock per concept, the record is written to a
   dot-prefixed temporary file beside the target (outside the bundle and the
   index), synced, read back and compared (metadata as canonical JSON, body
   byte for byte), renamed over the target and the directory synced. On any
   failure the temporary file is removed and the old file is untouched.

Guarantees:

- A reader sees the old file or the new one, never a mix; concurrent puts to
  one concept are serialised and the last wins.
- On an update the text of every unchanged top-level key is kept byte for
  byte, comments included (OKF §4); changed keys are written in place and new
  ones appended. Extension keys at any depth survive a get then put.
- An update keeps the file's mode; new files and directories get the
  bundle's (or the store's) modes, whatever the umask.
- IDs are case-sensitive on every file system: a put that differs only in
  case from an existing entry is refused, and a get must match exactly.
  `index` and `log` (any case) are reserved names; dots inside an ID are
  kept (`SFR/FCS_COP.1` is `SFR/FCS_COP.1.md`).
- Every path goes through an `os.Root`; symbolic links inside a bundle are
  not followed (a linked concept reads as not found, search skips it, a put
  refuses it).

Reading is lenient, as OKF asks of consumers: unknown keys are kept, one item
given for a list field (`tags: oncall`) is returned as a list of one (never
split at commas), timestamps keep their source text. Duplicate keys, custom
YAML tags, merge keys and non-string keys make a concept invalid.

## Search

The index is SQLite FTS5 in memory, one row per Markdown section, ranked by
bm25 with the ID weighing most, then the title, the heading and the text.
Every query term may match, and each is quoted so nothing a model types is
FTS syntax. Before each search the bundle's files are stat-walked and only
those whose size or modification time changed are read again, so Git
checkouts and edits made outside the store are seen; a put updates the index
at once. Files that are not concepts or do not parse are skipped and logged.

## Limits (`kb.Limits`)

| Field | Default | Meaning |
| --- | --- | --- |
| `SearchDefault` | 5 | hits when no limit is given |
| `SearchMax` | 20 | most hits a search returns |
| `Body` | 128 KiB | body bytes a put accepts |
| `Frontmatter` | 32 KiB | frontmatter bytes as written |
| `Includes` | 16 | `x-tenebris-include` entries per concept |
| `IDBytes` | 512 | concept ID bytes |
| `SegmentBytes` | 200 | bytes per ID segment |
| `QueryRunes` | 256 | search query characters |
| `PassageRunes` | 240 | characters of a hit's passage |
| `GetResult` | 512 KiB | bytes of a `kb_get` result; includes that do not fit are listed in `omitted_includes` |

Inclusion is one level deep, each ID once. A file larger than
`Body + Frontmatter` is not read. `GetResult` applies to the tool only: Go
callers get the whole `Record`.
