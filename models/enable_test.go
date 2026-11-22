package models_test

import (
	"errors"
	"strings"
	"testing"

	"go.bison9.com/agent/models"
)

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
