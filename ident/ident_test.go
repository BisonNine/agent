/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package ident_test

import (
	"strings"
	"testing"

	"go.bison9.com/agent/ident"
)

// idCase is one row of the shared table: the input, its identity and whether
// it is a valid id. The rows are those a host's configuration and its web
// front end are checked against, with one difference: an input of which
// nothing survives the repair normalizes to "" here, where the host
// substitutes its default agent.
type idCase struct {
	input, normalized string
	valid             bool
}

var idCases = []idCase{
	{"alice", "alice", true},
	{"bob-2", "bob-2", true},
	{"a_b", "a_b", true},
	{"main", "main", true},
	{"9lives", "9lives", true},
	{"alice_", "alice_", true},
	{"alice-", "alice-", true},
	{"a", "a", true},
	{"0", "0", true},
	{strings.Repeat("b", 64), strings.Repeat("b", 64), true},
	{"Bob", "bob", true},
	{"bOB", "bob", true},
	{"Alice_Smith-2", "alice_smith-2", true},
	{"Alice", "alice", true},
	{"MAIN", "main", true},
	{"Alice.Smith", "alice-smith", false},
	{" alice", "alice", false},
	{"alice smith", "alice-smith", false},
	{strings.Repeat("b", 63) + " ", strings.Repeat("b", 63), false},
	{"  Bob.Smith  ", "bob-smith", false},
	{"-alice", "alice", false},
	{"_alice", "alice", false},
	{"__alice__", "alice", false},
	{"-_alice_-", "alice", false},
	{"alice_!", "alice", false},
	{strings.Repeat("a", 65), strings.Repeat("a", 64), false},
	{strings.Repeat("a", 63) + "_b", strings.Repeat("a", 63), false},
	{strings.Repeat("a", 63) + "!b", strings.Repeat("a", 63), false},
	{strings.Repeat("a", 63) + "-bcd", strings.Repeat("a", 63), false},
	{"!!!", "", false},
	{"", "", false},
	{"   ", "", false},
	{"-", "", false},
	{"_", "", false},
	{"İstanbul", "stanbul", false},
	{"café", "caf", false},
	{"Ünïcødé", "n-c-d", false},
	{"\u212aelvin", "elvin", false}, // KELVIN SIGN, not ASCII K
	{"a\tb", "a-b", false},
	{"x/../y", "x-y", false},
	{"Alice:Bob", "alice-bob", false},
	{strings.Repeat("ab-", 30), strings.Repeat("ab-", 21) + "a", false},
	{"a\x7fb", "a-b", false},
	{"a\vb", "a-b", false},
	{"a\u200bb", "a-b", false},
	{"a\u00a0b", "a-b", false},
	{`a"b\c`, "a-b-c", false},
}

func TestSharedCases(t *testing.T) {
	for _, tc := range idCases {
		if got := ident.ValidID(tc.input); got != tc.valid {
			t.Errorf("ValidID(%q) = %v, want %v", tc.input, got, tc.valid)
		}
		if got := ident.NormalizeID(tc.input); got != tc.normalized {
			t.Errorf("NormalizeID(%q) = %q, want %q", tc.input, got, tc.normalized)
		}
	}
}

// NormalizeID always gives a valid lower-case id, or "", and is idempotent.
func TestNormalizeIDIsAnIdentity(t *testing.T) {
	for _, tc := range idCases {
		n := ident.NormalizeID(tc.input)
		if n == "" {
			continue
		}
		if !ident.ValidID(n) || n != strings.ToLower(n) {
			t.Errorf("NormalizeID(%q) = %q, not a valid lower-case id", tc.input, n)
		}
		if again := ident.NormalizeID(n); again != n {
			t.Errorf("NormalizeID(%q) = %q, not idempotent (%q)", tc.input, n, again)
		}
	}
}

func TestRepairIDKeepsCase(t *testing.T) {
	for in, want := range map[string]string{
		"Alice.Smith": "Alice-Smith",
		"  Bob  ":     "Bob",
		"!!!":         "",
		"alice":       "alice",
		"alice-":      "alice", // valid, but the suggestion trims it
		"Bob_":        "Bob",
	} {
		if got := ident.RepairID(in); got != want {
			t.Errorf("RepairID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameID(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Bob", "bob", true},
		{"alice", "ALICE", true},
		{"Alice.Smith", "alice-smith", true},
		{"alice", "bob", false},
		{"", "", false},
		{"  ", "alice", false},
		{"!!!", "???", false}, // nothing survives: never the same agent
		{"!!!", "main", false},
	}
	for _, tc := range cases {
		if got := ident.SameID(tc.a, tc.b); got != tc.want {
			t.Errorf("SameID(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := ident.SameID(tc.b, tc.a); got != tc.want {
			t.Errorf("SameID(%q, %q) = %v, want %v (not symmetric)", tc.b, tc.a, got, tc.want)
		}
	}
}
