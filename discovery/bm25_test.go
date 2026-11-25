/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package discovery

import (
	"reflect"
	"testing"
)

type testDoc struct {
	ID   int
	Text string
}

func extractText(d testDoc) string { return d.Text }

func TestBM25Search_EdgeCases(t *testing.T) {
	engine := newBM25Engine([]testDoc{{1, "hello world"}, {2, "foo bar"}}, extractText)
	tests := []struct {
		name  string
		query string
		topK  int
	}{
		{"zero topK", "hello", 0},
		{"negative topK", "hello", -1},
		{"empty query", "", 5},
		{"only punctuation", "...,,,!!!", 5},
		{"no match", "golang", 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := engine.Search(tt.query, tt.topK)
			if results == nil || len(results) != 0 {
				t.Errorf("results = %#v, want an empty, non-nil slice", results)
			}
		})
	}
}

func TestBM25Search_EmptyCorpus(t *testing.T) {
	results := newBM25Engine([]testDoc{}, extractText).Search("hello", 5)
	if results == nil || len(results) != 0 {
		t.Errorf("results = %#v, want an empty, non-nil slice", results)
	}
}

func TestBM25Search_Ranking(t *testing.T) {
	engine := newBM25Engine([]testDoc{
		{1, "the quick brown fox jumps over the lazy dog"},
		{2, "quick fox"},
		{3, "quick quick quick fox"},
		{4, "completely irrelevant document here"},
	}, extractText)

	tests := []struct {
		name    string
		query   string
		topK    int
		wantLen int
		wantTop int
	}{
		{"term frequency boosts the score", "quick", 5, 3, 3},
		{"a shorter document wins at equal frequency", "fox", 5, 3, 2},
		{"topK limits the results", "quick", 2, 2, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := engine.Search(tt.query, tt.topK)
			if len(results) != tt.wantLen {
				t.Fatalf("got %d results, want %d", len(results), tt.wantLen)
			}
			if results[0].Document.ID != tt.wantTop {
				t.Errorf("top document = %d, want %d", results[0].Document.ID, tt.wantTop)
			}
		})
	}
}

func TestBM25Search_DescendingScores(t *testing.T) {
	engine := newBM25Engine([]testDoc{
		{1, "golang is good"},
		{2, "golang golang"},
		{3, "golang golang golang"},
		{4, "golang golang golang golang"},
	}, extractText)
	results := engine.Search("golang", 10)
	if len(results) != 4 {
		t.Fatalf("got %d results, want 4", len(results))
	}
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("result %d scores %v, above result %d's %v", i, results[i].Score, i-1, results[i-1].Score)
		}
	}
}

// Equal scores keep corpus order, at the cut-off as well as in the list.
func TestBM25Search_TiesKeepCorpusOrder(t *testing.T) {
	corpus := make([]testDoc, 0, 10)
	for i := range 10 {
		corpus = append(corpus, testDoc{i, "same text"})
	}
	engine := newBM25Engine(corpus, extractText)
	for range 20 {
		results := engine.Search("same", 4)
		got := make([]int, 0, len(results))
		for _, r := range results {
			got = append(got, r.Document.ID)
		}
		if want := []int{0, 1, 2, 3}; !reflect.DeepEqual(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestBM25Tokenize(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"Hello World", []string{"hello", "world"}},
		{"  spaces   everywhere  ", []string{"spaces", "everywhere"}},
		{"punctuation... test!!!", []string{"punctuation", "test"}},
		{"(parentheses) and-hyphens", []string{"parentheses", "and-hyphens"}},
		{"internal-hyphen is kept", []string{"internal-hyphen", "is", "kept"}},
		{".,;?!", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := bm25Tokenize(tt.input)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("bm25Tokenize(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestBM25Dedupe(t *testing.T) {
	got := bm25Dedupe([]string{"apple", "banana", "apple", "orange", "banana"})
	if want := []string{"apple", "banana", "orange"}; !reflect.DeepEqual(got, want) {
		t.Errorf("bm25Dedupe = %v, want %v", got, want)
	}
}
