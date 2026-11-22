package models_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/models"
)

// Enable and disable: off in the catalogue is off for every agent, off in a
// set is off for that agent, and a model is used only when on in both.
func TestEnableDisable(t *testing.T) {
	f := newFixture(t, three)
	bob := f.cat.Set("bob")
	for i, n := range []string{"sonnet", "gpt", "haiku"} {
		if err := bob.Assign(models.Chat, n, i); err != nil {
			t.Fatal(err)
		}
	}
	if !f.cat.Enabled("sonnet") || !bob.Enabled("sonnet") {
		t.Fatal("a new model is not enabled")
	}

	if err := bob.Disable("sonnet"); err != nil {
		t.Fatal(err)
	}
	if bob.Enabled("sonnet") || !f.cat.Enabled("sonnet") || !f.cat.Default().Enabled("sonnet") {
		t.Fatal("a set switch reached beyond the set")
	}
	res, callErr := bob.Begin(models.Chat).Chat(t.Context(), alice, models.Request{})
	if callErr != nil || res.Candidate.Name != "gpt" {
		t.Fatalf("bob: %v %+v", callErr, res)
	}
	if got := describeAll(res.Attempts); !slices.Equal(got, []string{"sonnet disabled"}) {
		t.Fatalf("attempts = %q", got)
	}
	if cur := bob.Current(models.Chat); cur.Reason != "disabled" || cur.Serving != "gpt" {
		t.Fatalf("current = %+v", cur)
	}
	if got := answeredBy(t, f.cat.Begin(models.Chat)); got != "sonnet" {
		t.Fatalf("answered by %q: the default set skipped a model only bob disabled", got)
	}

	if err := f.cat.Disable("gpt"); err != nil {
		t.Fatal(err)
	}
	if bob.Enabled("gpt") || f.cat.Default().Enabled("gpt") {
		t.Fatal("a catalogue switch did not reach the sets")
	}
	if e := bob.List(models.Chat)[1]; e.Enabled {
		t.Fatalf("list entry = %+v", e)
	}
	if err := bob.Enable("gpt"); err != nil {
		t.Fatal(err)
	}
	if bob.Enabled("gpt") {
		t.Fatal("on in the set made it usable while off in the catalogue")
	}
	if got := answeredBy(t, bob.Begin(models.Chat)); got != "haiku" {
		t.Fatalf("bob: answered by %q", got)
	}

	if err := f.cat.Disable("haiku", "nope"); err == nil || f.cat.Enabled("haiku") == false {
		t.Fatal("an unknown name was accepted, or half the change was made")
	}
	if err := bob.Disable("nope"); err == nil {
		t.Fatal("a set accepted an unknown name")
	}
}

// Every model off fails the call with ErrNoModelAvailable, naming the purpose.
func TestAllDisabled(t *testing.T) {
	f := newFixture(t, three)
	if err := f.cat.Disable("sonnet", "gpt", "haiku"); err != nil {
		t.Fatal(err)
	}
	_, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{})
	if !errors.Is(err, models.ErrNoModelAvailable) || !strings.Contains(err.Error(), `"chat"`) {
		t.Fatalf("err = %v", err)
	}
	if len(f.fake.Calls()) != 0 {
		t.Fatal("a disabled model was called")
	}
}

// A selection of a disabled model is kept, and is back when it is enabled.
func TestDisabledSelectionKept(t *testing.T) {
	f := newFixture(t, three)
	if err := f.cat.Select(models.Chat, "haiku"); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.Disable("haiku"); err != nil {
		t.Fatal(err)
	}
	if got := f.cat.Selected(models.Chat); got.Name != "haiku" || got.Enabled {
		t.Fatalf("selected = %+v", got)
	}
	res, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{})
	if err != nil || res.Candidate.Name != "sonnet" {
		t.Fatalf("%v %+v", err, res)
	}
	if err := f.cat.Enable("haiku"); err != nil {
		t.Fatal(err)
	}
	if got := answeredBy(t, f.cat.Begin(models.Chat)); got != "haiku" {
		t.Fatalf("after enabling: answered by %q", got)
	}
}

// The one change a pinned chain sees: the switch is re-read before each call.
func TestPinnedChainRereadsSwitch(t *testing.T) {
	f := newFixture(t, three)
	ch := f.cat.Begin(models.Chat)
	if err := f.cat.Disable("sonnet"); err != nil {
		t.Fatal(err)
	}
	res, err := ch.Chat(t.Context(), alice, models.Request{})
	if err != nil || res.Candidate.Name != "gpt" {
		t.Fatalf("%v %+v", err, res)
	}
	if err := f.cat.Enable("sonnet"); err != nil {
		t.Fatal(err)
	}
	if got := answeredBy(t, ch); got != "sonnet" {
		t.Fatalf("after enabling: answered by %q", got)
	}
}

func TestAssignmentSwitch(t *testing.T) {
	f := newFixture(t, three)
	bob := f.cat.Set("bob")
	if err := bob.Assign(models.Chat, "sonnet", 0, models.WithEnabled(false)); err != nil {
		t.Fatal(err)
	}
	if err := bob.Assign(models.Summarize, "sonnet", 0); err != nil {
		t.Fatal(err)
	}
	if bob.List(models.Chat)[0].Enabled || !bob.List(models.Summarize)[0].Enabled {
		t.Fatal("the assignment switch is not per purpose")
	}
	if _, err := bob.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}); !errors.Is(err, models.ErrNoModelAvailable) {
		t.Fatalf("err = %v", err)
	}
}
