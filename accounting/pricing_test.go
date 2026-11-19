package accounting

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
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
	otherProvider := chatRecord(1e6, 0, 0, 0)
	otherProvider.Provider = "azure"
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
		{
			name: "provider row beats generic", rec: chatRecord(1e6, 0, 0, 0),
			rows: []Price{{Model: "m", Input: new(1.0)}, {Provider: "openai-chat", Model: "m", Input: new(2.0)}},
			want: 2, wantCost: true, source: CostEstimated,
		},
		{
			name: "generic row for another provider", rec: otherProvider,
			rows: []Price{{Model: "m", Input: new(1.0)}, {Provider: "openai-chat", Model: "m", Input: new(2.0)}},
			want: 1, wantCost: true, source: CostEstimated,
		},
		{
			name: "provider row does not match another provider", rec: otherProvider,
			rows: []Price{{Provider: "openai-chat", Model: "m", Input: new(2.0)}}, source: CostUnknown,
		},
		{name: "dated model does not match undated row", rows: []Price{{Model: "m", Input: new(1.0)}}, rec: dated, source: CostUnknown},
		{name: "match is case-sensitive", rows: []Price{{Model: "m", Input: new(1.0)}}, rec: upper, source: CostUnknown},
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
		{"provider":"anthropic","model":"claude","input":3,"cache_read":0.3,"cache_write":3.75,"output":15}
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
		{name: "empty model", body: `{"prices":[{"model":" ","input":1}]}`},
		{name: "no prices in row", body: `{"prices":[{"provider":"azure","model":"m"}]}`},
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

func TestSetPricingDistinguishesProviders(t *testing.T) {
	// The same model under two providers is not a duplicate.
	usePricing(t, []Price{{Model: "m", Input: new(1.0)}, {Provider: "azure", Model: "m", Input: new(2.0)}})
	if err := SetPricing([]Price{{Provider: "azure", Model: "m", Input: new(1.0)}, {Provider: "azure", Model: "m", Image: new(1.0)}}); !errors.Is(err, ErrInvalidPricing) {
		t.Errorf("duplicate provider row: err = %v", err)
	}
	if err := SetPricing(nil); err != nil {
		t.Errorf("SetPricing(nil) = %v", err)
	}
	rec := Record{Model: "m", InputTokens: KnownInt(1)}
	settleCost(&rec)
	if rec.CostSource != CostUnknown {
		t.Error("SetPricing(nil) did not remove the table")
	}
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
