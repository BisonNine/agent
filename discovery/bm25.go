package discovery

import (
	"cmp"
	"math"
	"slices"
	"strings"
)

// BM25 tuning.
const (
	// bm25K1 is the term-frequency saturation factor (typical range 1.2–2.0):
	// higher values give more weight to repeated terms.
	bm25K1 = 1.2
	// bm25B is the document-length normalisation factor (0 none, 1 full).
	bm25B = 0.75
)

// bm25Engine ranks a fixed corpus against queries with BM25. The corpus is
// indexed once, when the engine is built; Search only scores. It never
// changes after it is built and is safe for concurrent use.
type bm25Engine[T any] struct {
	corpus     []T
	tf         []map[string]uint32 // per document: term → frequency
	idf        map[string]float32
	docLenNorm []float32          // per document: k1 * (1 - b + b*len/avgLen)
	posting    map[string][]int32 // term → documents containing it, ascending
}

// bm25Result is one ranked document.
type bm25Result[T any] struct {
	Document T
	Score    float32
}

// newBM25Engine indexes corpus, reading each document's searchable text with
// textFunc. corpus is referenced, not copied; the caller does not change it.
func newBM25Engine[T any](corpus []T, textFunc func(T) string) *bm25Engine[T] {
	n := len(corpus)
	e := &bm25Engine[T]{
		corpus:     corpus,
		tf:         make([]map[string]uint32, n),
		idf:        map[string]float32{},
		docLenNorm: make([]float32, n),
		posting:    map[string][]int32{},
	}
	if n == 0 {
		return e
	}

	lengths := make([]int, n)
	df := make(map[string]int, 64)
	totalLen := 0
	for i, doc := range corpus {
		tokens := bm25Tokenize(textFunc(doc))
		totalLen += len(tokens)
		tf := make(map[string]uint32, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		// Each term counts once per document.
		for t := range tf {
			df[t]++
		}
		e.tf[i] = tf
		lengths[i] = len(tokens)
	}
	avgDocLen := float64(totalLen) / float64(n)

	// IDF with Robertson smoothing: log((N - df + 0.5) / (df + 0.5) + 1).
	for term, freq := range df {
		e.idf[term] = float32(math.Log(
			(float64(n)-float64(freq)+0.5)/(float64(freq)+0.5) + 1,
		))
	}
	for i, l := range lengths {
		e.docLenNorm[i] = float32(bm25K1 * (1 - bm25B + bm25B*float64(l)/avgDocLen))
	}
	// Posting lists in document order.
	for i, tf := range e.tf {
		for term := range tf {
			e.posting[term] = append(e.posting[term], int32(i))
		}
	}
	return e
}

// Search ranks the corpus against query and returns at most topK results,
// highest score first; equal scores keep corpus order. It returns an empty
// slice, never nil, when nothing matches.
func (e *bm25Engine[T]) Search(query string, topK int) []bm25Result[T] {
	if topK <= 0 || len(e.corpus) == 0 {
		return []bm25Result[T]{}
	}
	terms := bm25Dedupe(bm25Tokenize(query))
	if len(terms) == 0 {
		return []bm25Result[T]{}
	}

	scores := make(map[int32]float32)
	for _, term := range terms {
		termIDF, ok := e.idf[term]
		if !ok {
			continue // not in the vocabulary: no contribution
		}
		for _, docID := range e.posting[term] {
			freq := float32(e.tf[docID][term])
			// TF_norm = freq * (k1+1) / (freq + docLenNorm)
			tfNorm := freq * float32(bm25K1+1) / (freq + e.docLenNorm[docID])
			scores[docID] += termIDF * tfNorm
		}
	}
	if len(scores) == 0 {
		return []bm25Result[T]{}
	}

	type scored struct {
		docID int32
		score float32
	}
	ranked := make([]scored, 0, len(scores))
	for docID, sc := range scores {
		ranked = append(ranked, scored{docID: docID, score: sc})
	}
	slices.SortFunc(ranked, func(a, b scored) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(a.docID, b.docID)
	})
	if len(ranked) > topK {
		ranked = ranked[:topK]
	}
	out := make([]bm25Result[T], len(ranked))
	for i, r := range ranked {
		out[i] = bm25Result[T]{Document: e.corpus[r.docID], Score: r.score}
	}
	return out
}

// bm25Tokenize splits s into lowercase tokens, stripping edge punctuation.
func bm25Tokenize(s string) []string {
	raw := strings.Fields(strings.ToLower(s))
	out := raw[:0]
	for _, t := range raw {
		t = strings.Trim(t, ".,;:!?\"'()/\\-_")
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// bm25Dedupe returns tokens without repeats, in first-occurrence order.
func bm25Dedupe(tokens []string) []string {
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}
