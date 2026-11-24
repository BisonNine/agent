# kb — knowledge bases as OKF bundles

**Knowledge bases are stored as [Open Knowledge Format](https://github.com/open-knowledge-format/open-knowledge-format/blob/527e81aff55992be21507e97004286789126f88c/SPEC.md) (OKF) v0.3
bundles and are OKF compatible: a bundle written by these tools can be read by
any OKF tool, and an OKF bundle can be served by these tools.** What that
covers exactly is under [OKF compatibility](#okf-compatibility).

`go.bison9.com/agent/toolbox/kb` reads, searches and writes those bundles:
directories of Markdown concepts, each a file with a YAML frontmatter block
and a Markdown body. A concept's ID is its path in the bundle without `.md`
(`guidance/crypto-review`, `SFR/FCS_COP.1`).

`toolbox/kb/tools` publishes it to a model as three tools, mounted by the
host under the namespace `kb`:

| Tool | Purpose |
| --- | --- |
| `kb_get` | one concept by exact ID, with the concepts it includes |
| `kb_search` | ranked sections of one knowledge base: ID, title, heading, passage |
| `kb_put` | create or replace one concept |

The tools use only the package's exported API, the same one a host's own
code (inserting records into a worker's prompt before dispatch, say) uses.

## OKF compatibility

Against the [OKF v0.3 specification](https://github.com/open-knowledge-format/open-knowledge-format/blob/527e81aff55992be21507e97004286789126f88c/SPEC.md):

**Every file a put writes is a conformant OKF concept.** It starts with a
`---` line, its frontmatter is one YAML mapping closed by a `---` line, with a
non-empty `type`, no duplicate keys and no custom tags; strings that look like
numbers, booleans or dates are quoted (§4). On an update, keys the caller
keeps are kept, unknown and extension keys at any depth included, and the
text of every unchanged top-level key is not re-serialised (§4, §4.1). The
write checks what OKF defines for the fields it validates: `type` (§4.1),
`title`, `description`, `resource` and `tags` (their types, §4.1), a
`resource` in every `sources` entry (§5.1), and RFC 3339 date-times with an
offset or dates for the timestamp keys (§5). The other §5 and §10 fields
(actors, `status`, `superseded_by`, `parameters`, `executor`, `attester`, …)
are stored as given and not checked. The package never writes `index.md` or
`log.md` (§8, §9) and never changes files it was not asked to write.

**Reading follows OKF's consumer rules.** A concept is a `.md` file, not
`index.md` or `log.md`, whose frontmatter has a non-empty `type`; other `.md`
files and all other files are skipped; entries whose names start with `.` are
not part of the bundle (§3.1, §3.2). Unknown types and keys are accepted
(§11.2), one item given for a list field is a list of one and a scalar tag is
never split at commas (§11.2), and YAML-native timestamps are accepted, kept
as their source text (§5). Links in bodies are text: the package does not
resolve links or path fields, and computes no trust tier, staleness or
attestation (§5.3, §5.5, §10); a caller reading the frontmatter does that.

**Where this package is stricter than OKF**, so a valid OKF bundle can hold
files these tools do not serve:

- Symbolic links inside a bundle are never followed, even ones that stay in
  it (OKF forbids only those leading out, §3.2): a linked concept reads as
  not found, search skips it, a put refuses it. With `Bundle.Confine`, the
  bundle directory itself may not be a link either.
- Reserved names: besides `index.md` and `log.md`, any case of `index` or
  `log` as the last segment of an ID is refused.
- IDs: a put that differs only in case from an existing entry is refused, and
  a get must match the name exactly on every file system (OKF says producers
  SHOULD NOT make such paths, §6). IDs with segments starting with `.`,
  starting or ending with whitespace, or containing `\`, control, format
  (Cf) or line and paragraph separator characters cannot be addressed.
- Frontmatter that OKF consumers MAY reject is rejected: duplicate keys (§4);
  likewise custom YAML tags, merge keys, non-string keys and an alias to a
  value that contains it. Such a concept is reported invalid, not served.
- Limits (see [Limits](#limits-kblimits)): a file over `Body + Frontmatter`
  bytes is not read; IDs over `IDBytes` or with a segment over
  `SegmentBytes` cannot be addressed; a put refuses a body, frontmatter or
  include list over its limit; one frontmatter expands to at most 65,536
  values.

**One extension**, `x-tenebris-include`: a list of concept IDs in the same
bundle whose concepts a get returns with this one, one level deep. It is an
`x-` key (§4.1), so other OKF tools ignore it; a put requires each entry to be
a canonical concept ID of an existing concept.

## The API

```go
store, err := kb.New(kb.Options{
    Modes:  fsmode.Modes{Dir: 0o700, File: 0o600}, // what Put creates
    Limits: kb.Limits{},                            // zero fields: defaults
    Logger: logger,
})
defer store.Close()

rec, err := store.Get(ctx, resolver, "cc2022", "SFR/FCS_COP.1")
// rec.Frontmatter, rec.Body, rec.Includes (one level), rec.Missing, rec.Omitted

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
Results and errors name a knowledge base as the caller asked for it, and no
error holds a host path: a bundle directory that cannot be used (missing
parent, a link, a file, bad paths) is `ErrUnknownKB` naming the knowledge
base, with the detail logged. Errors are bounded: at most 20 problems per
concept, each field path and reason clipped.

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
    Root     string       // absolute bundle root
    Confine  string       // optional absolute ancestor; Root is opened through it
    Writable bool         // Put allowed
    Create   bool         // Put may create a missing Root
    Rules    []kb.Rule    // dataset rules
    Modes    fsmode.Modes // overrides Options.Modes for this bundle
}
```

- **`Confine`** is for bundles the agent names itself. Root and every
  directory between `Confine` and Root are opened one at a time, each
  checked to be a directory and not a symbolic link, so no link, absolute
  or relative, makes a bundle another bundle under `Confine` (Alice's
  `notes` linked to `../bob/notes`) or anything outside it. `Confine` itself
  is opened as given. A shared bundle the host configured needs none.
- **`Create`**: a missing root reads as an empty bundle (`Get` is
  `ErrNotFound`, `Search` empty) when set, and as `ErrUnknownKB` when not.
- **`Rules`**: `kb.Rule{PathPrefix, Type, Schema}` applies a JSON Schema
  (a bundle-relative file, draft 2020-12 by default) to the frontmatter of
  every concept whose ID is under `PathPrefix` (whole segments: `SFR`
  covers `SFR/x`, not `SFRX/x`) *or* whose type is `Type`,
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

Nothing is created before steps 1 to 3 pass (a missing bundle that may be
created is checked as empty), and a write that fails after creating the
bundle root or a directory for the concept removes them again when empty.

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
YAML tags, merge keys, non-string keys and an alias to a value containing it
make a concept invalid. Reading one file is bounded whatever its text: at most
65,536 values are converted (aliases counted each time), and conversion
stops after 20 problems.

## Search

The index is SQLite FTS5 in memory, one row per Markdown section, ranked by
bm25 with the ID weighing most, then the title, the heading and the text.
Every query term may match, and each is quoted so nothing a model types is
FTS syntax. Before each search the bundle's files are stat-walked and only
those whose size or modification time changed are read again, so Git
checkouts and edits made outside the store are seen; a put updates the index
at once. Files that are not concepts or do not parse are skipped and logged.
Each bundle has its own index lock, so work on one does not wait for another.
The index keeps the `Limits.IndexedBundles` most recently searched or written
bundles; the least recently used one is dropped and rebuilt when next
searched.

## Limits (`kb.Limits`)

| Field | Default | Meaning |
| --- | --- | --- |
| `SearchDefault` | 5 | hits when no limit is given |
| `SearchMax` | 20 | most hits a search returns |
| `Body` | 128 KiB | body bytes a put accepts |
| `Frontmatter` | 32 KiB | frontmatter bytes as written |
| `Includes` | 16 | `x-tenebris-include` entries per concept a put accepts; a get resolves the first this many and lists the rest in `Omitted` |
| `IDBytes` | 512 | concept ID bytes |
| `SegmentBytes` | 200 | bytes per ID segment |
| `QueryRunes` | 256 | search query characters |
| `PassageRunes` | 240 | characters of a hit's passage |
| `GetResult` | 512 KiB | bytes of a `kb_get` result; includes that do not fit are listed in `omitted_includes` |
| `IndexedBundles` | 32 | bundles the search index keeps |

Inclusion is one level deep, each ID once. A put checks the body, ID,
include count and a lower bound of the frontmatter's size on its arguments
before any other work. A file larger than `Body + Frontmatter` is not read. `GetResult` applies to the tool only: Go
callers get the whole `Record`.
