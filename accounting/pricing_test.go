package accounting

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
)

// usePricing installs rows for the test and removes the table after it.
func usePricing(t *testing.T, rows []Price) {
	t.Helper()
	if err := SetPricing(rows); err != nil {
		t.Fatalf("SetPricing: %v", err)
	}
	t.Cleanup(ClearPricing)
}

// chatRecord is a text record with every token count known.
func chatRecord(in, cacheRead, cacheWrite, out int64) Record {
	return Record{
		Provider: "openai-chat", Model: "m", Kind: KindText, Success: true,
		InputTokens: KnownInt(in), CacheReadTokens: KnownInt(cacheRead),
		CacheCreationTokens: KnownInt(cacheWrite), OutputTokens: KnownInt(out),
	}
}

func TestSettleCost(t *testing.T) {
	speech := Record{Provider: "openai", Model: "tts", Kind: KindSpeech, Characters: KnownInt(2000)}
	transcription := Record{Provider: "openai", Model: "stt", Kind: KindTranscription, AudioSeconds: KnownFloat(90)}
	images := Record{Provider: "openai", Model: "img", Kind: KindImage, Images: KnownInt(3)}
	tokenSpeech := Record{
		Provider: "gemini", Model: "tts", Kind: KindSpeech,
		Characters: KnownInt(5), InputTokens: KnownInt(1000), OutputTokens: KnownInt(2000),
	}
	withCost := chatRecord(1000, 0, 0, 0)
	withCost.CostUSD = KnownFloat(0.5)
	cacheUnknown := chatRecord(1000, 0, 0, 0)
	cacheUnknown.CacheReadTokens = Int{}
	failed := chatRecord(1e6, 0, 0, 0)
	failed.Success = false
	dated := chatRecord(1e6, 0, 0, 0)
	dated.Model = "m-2026-01-01"
	upper := chatRecord(1e6, 0, 0, 0)
	upper.Model = "M"

	tokens := Price{Model: "m", Input: new(3.0), CacheRead: new(0.3), CacheWrite: new(3.75), Output: new(15.0)}
	tests := []struct {
		name     string
		rows     []Price
		rec      Record
		want     float64
		wantCost bool
		source   CostSource
	}{
		{
			name: "provided cost wins", rows: []Price{{Model: "m", Input: new(100.0)}}, rec: withCost,
			want: 0.5, wantCost: true, source: CostProvided,
		},
		{name: "no table", rec: chatRecord(1, 0, 0, 0), source: CostUnknown},
		{name: "no matching row", rows: []Price{{Model: "other", Input: new(1.0)}}, rec: chatRecord(1, 0, 0, 0), source: CostUnknown},
		{name: "dated model does not match undated exact row", rows: []Price{{Model: "m", Input: new(1.0)}}, rec: dated, source: CostUnknown},
		{
			name: "dated model matches pattern", rows: []Price{{Model: "m", Input: new(1.0)}, {Model: "m*", Input: new(2.0)}},
			rec: dated, want: 2, wantCost: true, source: CostEstimated,
		},
		{name: "match is case-sensitive", rows: []Price{{Model: "m", Input: new(1.0)}}, rec: upper, source: CostUnknown},
		{name: "pattern is case-sensitive", rows: []Price{{Model: "m*", Input: new(1.0)}}, rec: upper, source: CostUnknown},
		// 1000×3 + 2000×0.3 + 400×3.75 + 500×15, per million.
		{
			name: "tokens with cache read and write", rows: []Price{tokens}, rec: chatRecord(1000, 2000, 400, 500),
			want: 0.0126, wantCost: true, source: CostEstimated,
		},
		{
			name: "characters", rows: []Price{{Model: "tts", Characters: new(15.0)}}, rec: speech,
			want: 0.03, wantCost: true, source: CostEstimated,
		},
		// Characters without a per-character price do not block a token price.
		{
			name: "token-billed speech", rows: []Price{{Model: "tts", Input: new(0.5), Output: new(10.0)}}, rec: tokenSpeech,
			want: 0.0205, wantCost: true, source: CostEstimated,
		},
		{
			name: "audio minutes", rows: []Price{{Model: "stt", AudioMinute: new(0.006)}}, rec: transcription,
			want: 0.009, wantCost: true, source: CostEstimated,
		},
		{
			name: "images", rows: []Price{{Model: "img", Image: new(0.04)}}, rec: images,
			want: 0.12, wantCost: true, source: CostEstimated,
		},
		{
			name: "priced unit with unknown quantity", rows: []Price{{Model: "m", Input: new(1.0), CacheRead: new(0.1)}},
			rec: cacheUnknown, source: CostUnknown,
		},
		{
			name: "priced audio with unknown seconds", rows: []Price{{Model: "stt", AudioMinute: new(0.006)}},
			rec: Record{Provider: "openai", Model: "stt"}, source: CostUnknown,
		},
		{
			name: "known non-zero token class without price", rows: []Price{{Model: "m", Input: new(1.0)}},
			rec: chatRecord(1000, 0, 0, 500), source: CostUnknown,
		},
		// 1000×2 + 2000×2 + 400×2 + 500×10, per million.
		{
			name: "cache tokens without a cache price use the input price",
			rows: []Price{{Model: "m", Input: new(2.0), Output: new(10.0)}},
			rec:  chatRecord(1000, 2000, 400, 500), want: 0.0118, wantCost: true, source: CostEstimated,
		},
		// 1000×2 + 2000×0.2 + 400×2 + 500×10, per million.
		{
			name: "a set cache price is used, the other falls back to input",
			rows: []Price{{Model: "m", Input: new(2.0), CacheRead: new(0.2), Output: new(10.0)}},
			rec:  chatRecord(1000, 2000, 400, 500), want: 0.0082, wantCost: true, source: CostEstimated,
		},
		{
			name: "cache tokens with no cache price and no input price",
			rows: []Price{{Model: "m", Output: new(10.0)}},
			rec:  chatRecord(0, 2000, 0, 500), source: CostUnknown,
		},
		{
			name: "known zero token class without price", rows: []Price{{Model: "m", Input: new(1.0)}},
			rec: chatRecord(1e6, 0, 0, 0), want: 1, wantCost: true, source: CostEstimated,
		},
		{
			name: "explicit zero price", rows: []Price{{Model: "m", Input: new(0.0)}},
			rec: chatRecord(1000, 0, 0, 0), want: 0, wantCost: true, source: CostEstimated,
		},
		{
			name: "failed call is still estimated", rows: []Price{{Model: "m", Input: new(1.0)}},
			rec: failed, want: 1, wantCost: true, source: CostEstimated,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usePricing(t, tt.rows)
			rec := tt.rec
			settleCost(&rec)
			if rec.CostSource != tt.source {
				t.Errorf("cost_source = %q, want %q", rec.CostSource, tt.source)
			}
			got, ok := rec.CostUSD.Value()
			if ok != tt.wantCost {
				t.Fatalf("cost known = %v, want %v (cost %v)", ok, tt.wantCost, got)
			}
			if ok && math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("cost_usd = %v, want %v", got, tt.want)
			}
		})
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pricing.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPricing(t *testing.T) {
	t.Cleanup(ClearPricing)
	path := writeFile(t, `{"prices":[
		{"model":"m","input":2,"output":8},
		{"model":"claude*","input":3,"cache_read":0.3,"cache_write":3.75,"output":15}
	]}`)
	if err := LoadPricing(path); err != nil {
		t.Fatalf("LoadPricing: %v", err)
	}
	rec := chatRecord(1e6, 0, 0, 1e6)
	settleCost(&rec)
	if got, ok := rec.CostUSD.Value(); !ok || got != 10 || rec.CostSource != CostEstimated {
		t.Errorf("cost = %v (known %v, source %q), want 10 estimated", got, ok, rec.CostSource)
	}

	// An empty file removes the table.
	if err := LoadPricing(writeFile(t, `{"prices":[]}`)); err != nil {
		t.Fatalf("LoadPricing empty: %v", err)
	}
	rec = chatRecord(1e6, 0, 0, 1e6)
	settleCost(&rec)
	if rec.CostSource != CostUnknown {
		t.Errorf("cost_source = %q after an empty table, want -", rec.CostSource)
	}
}

func TestLoadPricingErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "bad JSON", body: `{"prices":[`},
		{name: "negative price", body: `{"prices":[{"model":"m","input":-1}]}`},
		{name: "duplicate row", body: `{"prices":[{"model":"m","input":1},{"model":"m","output":1}]}`},
		{name: "duplicate pattern", body: `{"prices":[{"model":"m*","input":1},{"model":"m*","output":1}]}`},
		{name: "duplicate default", body: `{"prices":[{"model":"*","input":1},{"model":"*","output":1}]}`},
		{name: "empty model", body: `{"prices":[{"model":" ","input":1}]}`},
		{name: "star in the middle", body: `{"prices":[{"model":"gpt-*-mini","input":1}]}`},
		{name: "double star", body: `{"prices":[{"model":"**","input":1}]}`},
		{name: "leading star", body: `{"prices":[{"model":"*foo","input":1}]}`},
		{name: "no prices in row", body: `{"prices":[{"model":"m"}]}`},
		{name: "provider is no longer a field", body: `{"prices":[{"provider":"azure","model":"m","input":1}]}`},
		{name: "unknown field", body: `{"prices":[{"model":"m","inputs":1}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usePricing(t, []Price{{Model: "keep", Input: new(1.0)}})
			if err := LoadPricing(writeFile(t, tt.body)); !errors.Is(err, ErrInvalidPricing) {
				t.Fatalf("err = %v, want ErrInvalidPricing", err)
			}
			// The table in place is kept.
			rec := Record{Model: "keep", InputTokens: KnownInt(1e6)}
			settleCost(&rec)
			if rec.CostSource != CostEstimated {
				t.Error("a failed load replaced the table")
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		err := LoadPricing(filepath.Join(t.TempDir(), "none.json"))
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("err = %v, want a not-exist error", err)
		}
	})
}

func TestSetPricingNilRemovesTable(t *testing.T) {
	usePricing(t, []Price{{Model: "m", Input: new(1.0)}})
	if err := SetPricing(nil); err != nil {
		t.Errorf("SetPricing(nil) = %v", err)
	}
	rec := Record{Model: "m", InputTokens: KnownInt(1)}
	settleCost(&rec)
	if rec.CostSource != CostUnknown {
		t.Error("SetPricing(nil) did not remove the table")
	}
}

func TestModelMatching(t *testing.T) {
	rows := []Price{
		{Model: "*", Input: new(1.0)},
		{Model: "gpt-4*", Input: new(2.0)},
		{Model: "gpt-4o-2*", Input: new(3.0)},
		{Model: "gpt-4o-2024-08-06", Input: new(4.0)},
	}
	tests := []struct {
		model string
		want  float64
	}{
		{model: "gpt-4o-2024-08-06", want: 4}, // exact beats every pattern
		{model: "gpt-4o-2024-11-20", want: 3}, // longest matching prefix
		{model: "gpt-4o-mini", want: 2},
		{model: "gpt-4", want: 2},       // a prefix matches itself
		{model: "claude-test", want: 1}, // only the default matches
		{model: "", want: 1},            // no model matches only the default
	}
	// The order of the rows does not matter.
	orders := map[string][]Price{
		"as written": rows,
		"reversed":   {rows[3], rows[2], rows[1], rows[0]},
		"shuffled":   {rows[1], rows[3], rows[0], rows[2]},
	}
	for name, order := range orders {
		for _, tt := range tests {
			t.Run(name+"/"+tt.model, func(t *testing.T) {
				usePricing(t, order)
				rec := Record{Model: tt.model, InputTokens: KnownInt(1e6)}
				settleCost(&rec)
				if got, ok := rec.CostUSD.Value(); !ok || got != tt.want {
					t.Errorf("cost = %v (known %v), want %v", got, ok, tt.want)
				}
			})
		}
	}

	t.Run("no default and no match", func(t *testing.T) {
		usePricing(t, rows[1:])
		for _, model := range []string{"claude-test", ""} {
			rec := Record{Model: model, InputTokens: KnownInt(1e6)}
			settleCost(&rec)
			if rec.CostSource != CostUnknown {
				t.Errorf("model %q: cost_source = %q, want -", model, rec.CostSource)
			}
		}
	})
}

func TestPricingConcurrentWithReport(t *testing.T) {
	t.Cleanup(ClearPricing)
	c := useCapture(t)
	rows := []Price{{Model: "m", Input: new(1.0)}}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				if err := SetPricing(rows); err != nil {
					t.Error(err)
				}
				ClearPricing()
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 200 {
				Report(chatRecord(1e6, 0, 0, 0))
			}
		})
	}
	wg.Wait()
	logged, _ := c.snapshot()
	if len(logged) != 800 {
		t.Fatalf("logger got %d records, want 800", len(logged))
	}
	for _, line := range logged {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		cost, ok := r.CostUSD.Value()
		switch {
		case r.CostSource == CostEstimated && ok && cost == 1:
		case r.CostSource == CostUnknown && !ok:
		default:
			t.Fatalf("record cost %v (known %v) with source %q", cost, ok, r.CostSource)
		}
	}
}

func TestRecordJSONCostSourceAndImages(t *testing.T) {
	rec := Record{CostUSD: KnownFloat(0.25), CostSource: CostEstimated, Images: KnownInt(2)}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.CostSource != CostEstimated || back.Images != KnownInt(2) || back.CostUSD != KnownFloat(0.25) {
		t.Errorf("round trip = %+v from %s", back, b)
	}
}

func TestSettleCostIgnoresAgentName(t *testing.T) {
	usePricing(t, []Price{{Model: "m", Input: new(2.0)}, {Model: "Alice", Input: new(100.0)}})
	plain := chatRecord(1e6, 0, 0, 0)
	named := plain
	named.AgentName = "Alice"
	settleCost(&plain)
	settleCost(&named)
	named.AgentName = ""
	if !reflect.DeepEqual(plain, named) {
		t.Errorf("with a name = %+v, without = %+v", named, plain)
	}
	if v, ok := plain.CostUSD.Value(); !ok || v != 2 {
		t.Errorf("cost = %v, %v, want 2", v, ok)
	}
}

func TestSettleCostNotes(t *testing.T) {
	plain := Price{Model: "m", Input: new(2.0), CacheRead: new(0.2), CacheWrite: new(2.5), Output: new(10.0)}
	withCost := chatRecord(1000, 2000, 400, 500)
	withCost.CostUSD = KnownFloat(0.5)
	cacheUnknown := chatRecord(1000, 0, 0, 500)
	cacheUnknown.CacheReadTokens = Int{}
	dated := chatRecord(1000, 0, 0, 500)
	dated.Model = "m-2026"

	tests := []struct {
		name string
		rows []Price
		rec  Record
		want []string
	}{
		{name: "no table says nothing", rec: chatRecord(1000, 0, 0, 500)},
		{name: "provided cost says nothing", rows: []Price{plain}, rec: withCost},
		{name: "exact row with every price says nothing", rows: []Price{plain}, rec: chatRecord(1000, 2000, 400, 500)},
		{
			name: "no matching row", rows: []Price{{Model: "other", Input: new(1.0)}}, rec: chatRecord(1000, 0, 0, 500),
			want: []string{`no pricing row for model "m"`},
		},
		{
			name: "pattern row is named", rows: []Price{{Model: "m*", Input: new(2.0), Output: new(10.0)}}, rec: dated,
			want: []string{`priced by row "m*"`},
		},
		{
			name: "default row is named", rows: []Price{{Model: "*", Input: new(2.0), Output: new(10.0)}}, rec: dated,
			want: []string{`priced by row "*"`},
		},
		{
			name: "cache fallbacks are named", rows: []Price{{Model: "m", Input: new(2.0), Output: new(10.0)}},
			rec: chatRecord(1000, 2000, 400, 500),
			want: []string{
				`no cache_read price in row "m", used input price`,
				`no cache_write price in row "m", used input price`,
			},
		},
		{
			name: "missing token price", rows: []Price{{Model: "m", Input: new(2.0)}}, rec: chatRecord(1000, 0, 0, 500),
			want: []string{`not priced: no output price in row "m"`},
		},
		{
			name: "unknown quantity", rows: []Price{plain}, rec: cacheUnknown,
			want: []string{"not priced: cache_read_tokens unknown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usePricing(t, tt.rows)
			rec := tt.rec
			settleCost(&rec)
			if !slices.Equal(rec.Notes, tt.want) {
				t.Errorf("notes = %q, want %q", rec.Notes, tt.want)
			}
		})
	}
}
