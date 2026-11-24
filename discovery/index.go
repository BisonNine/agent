package discovery

import (
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"go.bison9.com/agent/toolkit"
)

// DefaultMaxResults is how many hits a search returns unless MaxResults
// says otherwise.
const DefaultMaxResults = 10

// MaxRegexPatternLength is the longest regular expression search_tools
// accepts.
const MaxRegexPatternLength = 200

// Option configures an Index.
type Option func(*Index)

// MaxResults sets how many hits a search returns. A value below 1 keeps
// DefaultMaxResults.
func MaxResults(n int) Option {
	return func(ix *Index) {
		if n > 0 {
			ix.maxResults = n
		}
	}
}

// WithLogger sets the logger. Records carry component=discovery. The
// default discards everything.
func WithLogger(l *slog.Logger) Option {
	return func(ix *Index) {
		if l != nil {
			ix.logger = l.With("component", "discovery")
		}
	}
}

// LogQueries sets whether the "Tool search completed" record carries the
// search query (field query). The default, false, leaves it out.
func LogQueries(on bool) Option {
	return func(ix *Index) { ix.logQueries = on }
}

// Hit is one search result: a hidden tool by the name it is offered and
// called under, with its description.
type Hit struct {
	Name        string
	Description string
}

// Details is what get_tool_details shows for one tool.
type Details struct {
	// Schema is the tool's definition in function form (toolkit.ToolToSchema).
	Schema map[string]any
	// Published is the name to call it by.
	Published string
	// References are the tools the lookup reveals: a hidden tool, followed
	// by the other hidden tools of its group (sorted) when the group is
	// reveal-together; none for a listed tool, which is already loaded.
	References []string
}

// Index searches one registry's hidden tools. Every query names the
// snapshot it answers for, and is answered from an index built from exactly
// that snapshot: never an older or newer one. The index is built when a
// query first asks about a snapshot whose tools differ from the last one
// built, so a change in the registry is picked up by the next query, however
// late its change notice is delivered. Use one Index per Registry (per
// agent set). It is safe for concurrent use.
type Index struct {
	reg        toolkit.Registry
	maxResults int
	logger     *slog.Logger
	logQueries bool

	mu  sync.Mutex
	cur *corpus // the latest snapshot's corpus, built on demand
}

// NewIndex returns an index of reg's hidden tools. reg is what MetaTools
// answers from; queries name their snapshot.
func NewIndex(reg toolkit.Registry, opts ...Option) *Index {
	ix := &Index{
		reg:        reg,
		maxResults: DefaultMaxResults,
		logger:     slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(ix)
	}
	return ix
}

// doc is one hidden tool in a corpus.
type doc struct {
	name        string // internal
	published   string // the name it is offered and referenced under
	advertised  string // its ExternalName when it has one, else its name
	description string
	group       string
	reveal      bool
}

// corpus is the index of one snapshot's hidden tools. It never changes.
type corpus struct {
	version uint64
	names   []string // every tool the snapshot offered, for the cache key
	docs    []doc    // hidden tools, sorted by internal name
	bm25    *bm25Engine[doc]
}

// matches reports whether c was built from a snapshot offering the same
// tools as snap. A set's Version moves with every change to the catalogue
// and to the set's own settings, so an equal Version means the same
// registered tools; a grant policy that changed its answers without a new
// Grant leaves the Version alone, which the offered names catch.
func (c *corpus) matches(snap *toolkit.Snapshot, names []string) bool {
	return c.version == snap.Version() && slices.Equal(c.names, names)
}

// corpusFor returns the corpus for snap, building it when the cached one
// is not snap's. The cache only moves forward: a query about an older
// snapshot gets a corpus of its own and leaves the newer one cached.
//
// It is built outside the lock, because a tool's Description may itself
// read the registry; two queries racing on a new snapshot may both build.
func (ix *Index) corpusFor(snap *toolkit.Snapshot) *corpus {
	names := snap.Names()
	ix.mu.Lock()
	cur := ix.cur
	ix.mu.Unlock()
	if cur != nil && cur.matches(snap, names) {
		return cur
	}
	c := buildCorpus(snap, names)
	ix.mu.Lock()
	cached := ix.cur == nil || c.version >= ix.cur.version
	if cached {
		ix.cur = c
	}
	ix.mu.Unlock()
	if cached {
		ix.logger.Debug("Discovery index rebuilt", "tools", len(c.docs), "version", c.version)
	}
	return c
}

func buildCorpus(snap *toolkit.Snapshot, names []string) *corpus {
	hidden := snap.Hidden()
	docs := make([]doc, 0, len(hidden))
	for _, in := range hidden {
		adv := externalName(in.Tool)
		if adv == "" {
			adv = in.Name
		}
		docs = append(docs, doc{
			name:        in.Name,
			published:   in.Published,
			advertised:  adv,
			description: toolDescription(in.Tool),
			group:       in.Group,
			reveal:      in.RevealTogether,
		})
	}
	return &corpus{
		version: snap.Version(),
		names:   names,
		docs:    docs,
		bm25:    newBM25Engine(docs, func(d doc) string { return d.published + " " + d.description }),
	}
}

// toolDescription is t's description; search_tools' plain one, since its
// full one is built from the index being built.
func toolDescription(t toolkit.Tool) string {
	if _, ok := t.(*searchTool); ok {
		return searchDescription
	}
	return t.Description()
}

// Search ranks snap's hidden tools against a natural-language query (BM25
// over name and description) and returns at most MaxResults hits, best
// first; equal scores keep internal-name order.
func (ix *Index) Search(snap *toolkit.Snapshot, query string) []Hit {
	ranked := ix.corpusFor(snap).bm25.Search(query, ix.maxResults)
	out := make([]Hit, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, Hit{Name: r.Document.published, Description: r.Document.description})
	}
	return out
}

// SearchRegex returns snap's hidden tools whose published name or
// description matches pattern, case-insensitively, in internal-name order,
// at most MaxResults of them (an empty slice when none). It fails when
// pattern does not compile.
func (ix *Index) SearchRegex(snap *toolkit.Snapshot, pattern string) ([]Hit, error) {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to compile regex pattern %q: %w", pattern, err)
	}
	out := []Hit{}
	for _, d := range ix.corpusFor(snap).docs {
		if re.MatchString(d.published) || re.MatchString(d.description) {
			out = append(out, Hit{Name: d.published, Description: d.description})
			if len(out) >= ix.maxResults {
				break
			}
		}
	}
	return out, nil
}

// Details describes the tool snap offers under name (internal or external).
// ok is false when snap does not offer it: unknown, not granted or disabled.
func (ix *Index) Details(snap *toolkit.Snapshot, name string) (Details, bool) {
	in, ok := snap.Lookup(name)
	if !ok {
		return Details{}, false
	}
	d := Details{Schema: toolkit.ToolToSchema(in.Tool), Published: in.Published}
	if _, ok := in.Tool.(*searchTool); ok {
		// As ToolDefs lists it: with this snapshot's hidden namespaces.
		if fn, ok := d.Schema["function"].(map[string]any); ok {
			fn["description"] = searchDescriptionFor(ix.Namespaces(snap))
		}
	}
	if !in.Hidden {
		return d, true
	}
	d.References = []string{in.Published}
	if in.RevealTogether && in.Group != "" {
		var group []string
		for _, sib := range snap.Hidden() {
			if sib.Name != in.Name && sib.Group == in.Group {
				group = append(group, sib.Published)
			}
		}
		sort.Strings(group)
		d.References = append(d.References, group...)
	}
	return d, true
}

// Namespaces returns the distinct leading namespaces of snap's hidden tools
// (the part of the ExternalName, or else the internal name, before its
// first underscore), sorted: the groups a model can discover.
func (ix *Index) Namespaces(snap *toolkit.Snapshot) []string {
	return namespaces(ix.corpusFor(snap).docs)
}

func namespaces(docs []doc) []string {
	set := make(map[string]struct{}, len(docs))
	for _, d := range docs {
		ns := d.advertised
		if i := strings.IndexByte(ns, '_'); i > 0 {
			ns = ns[:i]
		}
		set[ns] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for ns := range set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}
