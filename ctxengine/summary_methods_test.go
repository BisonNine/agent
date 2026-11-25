/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package ctxengine

import (
	"reflect"
	"strings"
	"testing"
)

// TestSummary_NilReceiver confirms every method is safe on a nil summary and
// reports the empty answer, since a session that was never compacted has none.
func TestSummary_NilReceiver(t *testing.T) {
	var s *Summary
	if err := s.Validate(); err == nil {
		t.Error("Validate(nil) = nil, want an error")
	}
	s.NormalizeRefs()
	s.StripOutOfRangeSeqRefs(1, 10)
	if s.HasMaterial() {
		t.Error("HasMaterial(nil) = true")
	}
	if s.HasEvidence() {
		t.Error("HasEvidence(nil) = true")
	}
	if n := s.DropUnsourcedExact([]string{"x"}); n != 0 {
		t.Errorf("DropUnsourcedExact(nil) = %d, want 0", n)
	}
	if v := s.exactValues(); v != nil {
		t.Errorf("exactValues(nil) = %v, want nil", v)
	}
	if r := s.LastSummarizedSeqRange(); r != (SeqRange{}) {
		t.Errorf("LastSummarizedSeqRange(nil) = %+v, want zero", r)
	}
	if s.TruncateToFit(10) {
		t.Error("TruncateToFit(nil) = true")
	}
}

// TestSummary_Validate_EndBeforeStart rejects an inverted covered range.
func TestSummary_Validate_EndBeforeStart(t *testing.T) {
	s := validSummary()
	s.CoveredSeqStart, s.CoveredSeqEnd = 20, 10
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "end (10) < start (20)") {
		t.Fatalf("Validate = %v, want the inverted-range error", err)
	}
}

// TestSummary_NormalizeRefs fills omitted range ends in every section and
// turns a legacy single-seq key moment into a ref.
func TestSummary_NormalizeRefs(t *testing.T) {
	s := &Summary{
		State: SummaryState{
			Goals:       []SummaryItem{{Refs: []SeqRange{{SeqStart: 1}}}},
			Progress:    []SummaryItem{{Refs: []SeqRange{{SeqStart: 2}}}},
			Pending:     []SummaryItem{{Refs: []SeqRange{{SeqStart: 3}}}},
			Constraints: []SummaryItem{{Refs: []SeqRange{{SeqStart: 4, SeqEnd: 6}}}},
		},
		CarryForward:  []SummaryItem{{Refs: []SeqRange{{SeqStart: 5}}}},
		KeyMoments:    []KeyMoment{{Seq: 7}, {Refs: []SeqRange{{SeqStart: 8}}}},
		CoveredRanges: []SeqRange{{SeqStart: 9}},
	}
	s.NormalizeRefs()
	want := map[string]SeqRange{
		"goal":       {1, 1},
		"progress":   {2, 2},
		"pending":    {3, 3},
		"constraint": {4, 6},
		"carry":      {5, 5},
		"legacy km":  {7, 7},
		"km":         {8, 8},
		"covered":    {9, 9},
	}
	got := map[string]SeqRange{
		"goal":       s.State.Goals[0].Refs[0],
		"progress":   s.State.Progress[0].Refs[0],
		"pending":    s.State.Pending[0].Refs[0],
		"constraint": s.State.Constraints[0].Refs[0],
		"carry":      s.CarryForward[0].Refs[0],
		"legacy km":  s.KeyMoments[0].Refs[0],
		"km":         s.KeyMoments[1].Refs[0],
		"covered":    s.CoveredRanges[0],
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalized refs = %v, want %v", got, want)
	}
}

// TestSummary_HasEvidence covers the citation rule: blank items need no refs,
// a cited item passes, and an uncited key moment fails the summary.
func TestSummary_HasEvidence(t *testing.T) {
	cited := []SeqRange{{SeqStart: 1, SeqEnd: 1}}
	tests := []struct {
		name string
		s    Summary
		want bool
	}{
		{"blank item needs no refs", Summary{State: SummaryState{Goals: []SummaryItem{{Text: "  "}}}}, true},
		{"cited item", Summary{State: SummaryState{Goals: []SummaryItem{{Text: "ship", Refs: cited}}}}, true},
		{"uncited pending", Summary{State: SummaryState{Pending: []SummaryItem{{Text: "ship"}}}}, false},
		{"uncited key moment", Summary{KeyMoments: []KeyMoment{{Summary: "Alice decided"}}}, false},
		{"uncited exact key moment", Summary{KeyMoments: []KeyMoment{{Exact: "use v2"}}}, false},
		{"cited key moment", Summary{KeyMoments: []KeyMoment{{Summary: "Alice decided", Refs: cited}}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.HasEvidence(); got != tc.want {
				t.Errorf("HasEvidence = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSummary_DropUnsourcedExact_BlankExact drops an Exact that is only
// whitespace: it quotes nothing.
func TestSummary_DropUnsourcedExact_BlankExact(t *testing.T) {
	s := &Summary{State: SummaryState{Goals: []SummaryItem{{Text: "goal", Exact: "   "}}}}
	if n := s.DropUnsourcedExact([]string{"Alice said hello"}); n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	if s.State.Goals[0].Exact != "" || s.State.Goals[0].Text != "goal" {
		t.Errorf("item = %+v, want Exact cleared and Text kept", s.State.Goals[0])
	}
}

// TestSummary_ExactValues collects every Exact, key moments included.
func TestSummary_ExactValues(t *testing.T) {
	s := &Summary{
		State:        SummaryState{Constraints: []SummaryItem{{Exact: "no force push"}, {Text: "no exact"}}},
		CarryForward: []SummaryItem{{Exact: "tell Bob"}},
		KeyMoments:   []KeyMoment{{Exact: "deploy friday"}, {Summary: "none"}},
	}
	want := []string{"no force push", "tell Bob", "deploy friday"}
	if got := s.exactValues(); !reflect.DeepEqual(got, want) {
		t.Errorf("exactValues = %v, want %v", got, want)
	}
}

// TestSummary_LastSummarizedSeqRange prefers the recorded last range, falls
// back to the last covered range, and is zero when neither exists.
func TestSummary_LastSummarizedSeqRange(t *testing.T) {
	tests := []struct {
		name string
		s    Summary
		want SeqRange
	}{
		{"recorded", Summary{LastSummarizedRange: SeqRange{5, 9}, CoveredRanges: []SeqRange{{1, 3}}}, SeqRange{5, 9}},
		{"from covered", Summary{CoveredRanges: []SeqRange{{1, 3}, {7, 12}}}, SeqRange{7, 12}},
		{"none", Summary{}, SeqRange{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.LastSummarizedSeqRange(); got != tc.want {
				t.Errorf("LastSummarizedSeqRange = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSummary_StripOutOfRangeSeqRefs_Bounds drops items, key moments and
// index entries citing seqs outside the archive window, including invalid
// refs, and keeps those inside it.
func TestSummary_StripOutOfRangeSeqRefs_Bounds(t *testing.T) {
	s := &Summary{
		State: SummaryState{
			Goals: []SummaryItem{
				{Text: "below min", Refs: []SeqRange{{SeqStart: 2, SeqEnd: 3}}},
				{Text: "above max", Refs: []SeqRange{{SeqStart: 40, SeqEnd: 60}}},
				{Text: "invalid", Refs: []SeqRange{{SeqStart: 0}}},
				{Text: " ", Refs: []SeqRange{{SeqStart: 10}}},
				{Text: "kept", Refs: []SeqRange{{SeqStart: 10, SeqEnd: 12}}},
			},
		},
		KeyMoments: []KeyMoment{
			{Summary: "out of range", Refs: []SeqRange{{SeqStart: 99}}},
			{Summary: "  ", Refs: []SeqRange{{SeqStart: 11}}},
			{Summary: "uncited"},
			{Summary: "kept", Refs: []SeqRange{{SeqStart: 11}}},
		},
		MessageIndex: []IndexEntry{
			{SeqStart: 12, Label: "single, end omitted"},
			{SeqStart: 3, SeqEnd: 4, Label: "below min"},
		},
	}
	s.StripOutOfRangeSeqRefs(5, 50)

	if len(s.State.Goals) != 1 || s.State.Goals[0].Text != "kept" {
		t.Errorf("goals = %+v, want only the in-range one", s.State.Goals)
	}
	if len(s.KeyMoments) != 1 || s.KeyMoments[0].Summary != "kept" || s.KeyMoments[0].Seq != 11 {
		t.Errorf("key moments = %+v, want only the in-range one with Seq 11", s.KeyMoments)
	}
	if len(s.MessageIndex) != 1 || s.MessageIndex[0].Label != "single, end omitted" {
		t.Errorf("message index = %+v, want only seq 12", s.MessageIndex)
	}
}

// TestSummary_Render_ExactAndUncited renders Exact values quoted, items
// without refs without a ref prefix, and a legacy key moment by its Seq.
func TestSummary_Render_ExactAndUncited(t *testing.T) {
	s := &Summary{
		Version: summaryVersion,
		State: SummaryState{
			Goals:       []SummaryItem{{Text: "uncited goal"}},
			Constraints: []SummaryItem{{Text: "ignored", Exact: "never push to main", Refs: []SeqRange{{SeqStart: 3, SeqEnd: 3}}}},
		},
		CarryForward: []SummaryItem{
			{Text: "ignored", Exact: "tell Bob", Refs: []SeqRange{{SeqStart: 4, SeqEnd: 5}}},
			{Text: "uncited reminder"},
		},
		KeyMoments: []KeyMoment{{Seq: 6, Role: "user", Summary: "Alice asked"}},
	}
	out := s.Render(1, 10)
	for _, want := range []string{
		"- uncited goal\n",
		`- [#3] exact: "never push to main"`,
		`- [#4-#5] exact: "tell Bob"`,
		"- uncited reminder\n",
		"- [#6] user: Alice asked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ignored") {
		t.Errorf("Text shown instead of Exact:\n%s", out)
	}
}

// TestRenderSummaryFromRaw renders stored summary JSON and passes legacy
// prose, invalid JSON and an invalid summary through unchanged.
func TestRenderSummaryFromRaw(t *testing.T) {
	valid := `{"version":2,"state":{"goals":[{"text":"finish the report","refs":[{"seq_start":1}]}]}}`
	if got := RenderSummaryFromRaw(valid, 1, 10); !strings.Contains(got, "- [#1] finish the report") {
		t.Errorf("valid summary not rendered:\n%s", got)
	}
	for _, raw := range []string{
		"Alice and Bob discussed the plan.",
		"{not json",
		`{"version":99}`,
		"",
	} {
		if got := RenderSummaryFromRaw(raw, 1, 10); got != raw {
			t.Errorf("RenderSummaryFromRaw(%q) = %q, want it unchanged", raw, got)
		}
	}
}

// TestBuildSummarizationPrompt_Profile appends the agent's compression
// profile after the base prompt.
func TestBuildSummarizationPrompt_Profile(t *testing.T) {
	p := buildSummarizationPrompt(nil, 1, 10, false, "Keep every ticket number.")
	if !strings.Contains(p, "## Agent compression profile") || !strings.HasSuffix(p, "Keep every ticket number.") {
		t.Errorf("profile not appended:\n%s", p[max(0, len(p)-300):])
	}
	if plain := buildSummarizationPrompt(nil, 1, 10, false, ""); strings.Contains(plain, "Agent compression profile") {
		t.Error("profile section present without a profile")
	}
}

// TestValidateAndUnmarshalLLMResponse_Failures rejects a response whose
// summary fails validation, directly or after recovery from prose, and keeps
// the original parse error when the recovered object is not a summary.
func TestValidateAndUnmarshalLLMResponse_Failures(t *testing.T) {
	tests := []struct {
		name    string
		resp    string
		wantErr string
	}{
		{"direct invalid version", `{"version":7}`, "validate: unsupported summary version"},
		{"wrapped invalid version", `Here it is: {"version":7} done`, "validate: unsupported summary version"},
		{"wrapped non-summary object", `Note {"version":"two"} end`, "unmarshal: invalid character"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := validateAndUnmarshalLLMResponse(tc.resp, (&logCapture{}).logger())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q (summary %+v)", err, tc.wantErr, s)
			}
		})
	}
}

// TestFindBalancedJSONObject_SkipsStrayClose ignores a closing brace that
// precedes any opening one.
func TestFindBalancedJSONObject_SkipsStrayClose(t *testing.T) {
	in := `} prose {"a":"}"} tail`
	start, end, ok := findBalancedJSONObject(in)
	if !ok || in[start:end+1] != `{"a":"}"}` {
		t.Errorf("found %d..%d ok=%v, want the object after the stray brace", start, end, ok)
	}
}
