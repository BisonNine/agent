/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package accounting

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

// ErrInvalidPricing reports a pricing table that cannot be installed.
var ErrInvalidPricing = errors.New("invalid pricing")

// Price is one row of the pricing table: what one model costs. Every price
// is optional; nil is not set, and an explicit 0 is a real, free price. A row
// sets at least one price.
type Price struct {
	// Model is an exact model name ("gpt-4o-2024-08-06"), or a prefix
	// ending in a single "*" ("gpt-4o*"); "*" alone matches any model. A
	// record uses the exact row for its model, else the matching pattern
	// with the longest prefix. Matching is case-sensitive.
	Model string `json:"model"`
	// Input, CacheRead, CacheWrite and Output are USD per million tokens.
	// Cache tokens are priced at Input when their own price is not set.
	Input      *float64 `json:"input,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	// Characters is USD per million characters (speech synthesis).
	Characters *float64 `json:"characters,omitempty"`
	// AudioMinute is USD per minute of audio (transcription).
	AudioMinute *float64 `json:"audio_minute,omitempty"`
	// Image is USD per image.
	Image *float64 `json:"image,omitempty"`
}

// PricingFile is the format LoadPricing reads: {"prices": [Price, ...]}.
type PricingFile struct {
	Prices []Price `json:"prices"`
}

// priceTable is an installed pricing table.
type priceTable struct {
	exact map[string]Price
	// patterns are the rows whose model ends in "*", longest prefix first.
	patterns []pricePattern
}

type pricePattern struct {
	prefix string
	price  Price
}

// pricing is the installed table; nil when there is none.
var pricing atomic.Pointer[priceTable]

// LoadPricing reads a pricing file (see PricingFile) and installs it in
// place of the current table, as SetPricing does (a file with no prices
// removes the table). Unknown fields are an error, so a misspelt price is
// not silently ignored. On any error the current table is kept.
func LoadPricing(path string) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the path is the host's choice
	if err != nil {
		return fmt.Errorf("accounting: read pricing: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f PricingFile
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("accounting: %w: %s: %w", ErrInvalidPricing, path, err)
	}
	return SetPricing(f.Prices)
}

// SetPricing validates rows and installs them in place of the current table.
// Empty or nil rows remove the table, as ClearPricing does. On error the
// current table is kept.
func SetPricing(rows []Price) error {
	if len(rows) == 0 {
		ClearPricing()
		return nil
	}
	table := &priceTable{exact: make(map[string]Price, len(rows))}
	seen := make(map[string]bool, len(rows))
	for i, p := range rows {
		if err := p.validate(); err != nil {
			return fmt.Errorf("accounting: %w: row %d: %w", ErrInvalidPricing, i+1, err)
		}
		if seen[p.Model] {
			return fmt.Errorf("accounting: %w: row %d: duplicate model %q", ErrInvalidPricing, i+1, p.Model)
		}
		seen[p.Model] = true
		if prefix, ok := strings.CutSuffix(p.Model, "*"); ok {
			table.patterns = append(table.patterns, pricePattern{prefix: prefix, price: p})
		} else {
			table.exact[p.Model] = p
		}
	}
	// Longest prefix first, so the first pattern that matches is the most
	// specific. Two prefixes of the same length cannot both match one model.
	sort.SliceStable(table.patterns, func(a, b int) bool {
		return len(table.patterns[a].prefix) > len(table.patterns[b].prefix)
	})
	pricing.Store(table)
	return nil
}

// ClearPricing removes the pricing table: no cost is estimated.
func ClearPricing() { pricing.Store(nil) }

func (p Price) validate() error {
	if strings.TrimSpace(p.Model) == "" {
		return errors.New("model is required")
	}
	if i := strings.Index(p.Model, "*"); i >= 0 && i != len(p.Model)-1 {
		return fmt.Errorf("model %q: \"*\" is allowed only once, as the last character", p.Model)
	}
	set := 0
	for name, v := range p.prices() {
		if v == nil {
			continue
		}
		set++
		if math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
			return fmt.Errorf("price %s for model %q is %v, want a number of at least 0", name, p.Model, *v)
		}
	}
	if set == 0 {
		return fmt.Errorf("model %q sets no price", p.Model)
	}
	return nil
}

func (p Price) prices() map[string]*float64 {
	return map[string]*float64{
		"input": p.Input, "cache_read": p.CacheRead, "cache_write": p.CacheWrite, "output": p.Output,
		"characters": p.Characters, "audio_minute": p.AudioMinute, "image": p.Image,
	}
}

// priceFor returns the row for a record's model: the exact row, else the
// pattern with the longest matching prefix. A record without a model matches
// only "*".
func priceFor(r *Record) (Price, bool) {
	t := pricing.Load()
	if t == nil {
		return Price{}, false
	}
	if p, ok := t.exact[r.Model]; ok {
		return p, true
	}
	for _, pat := range t.patterns {
		if strings.HasPrefix(r.Model, pat.prefix) {
			return pat.price, true
		}
	}
	return Price{}, false
}

// settleCost sets the record's cost source, and estimates the cost from the
// pricing table when the call returned none. It notes how an estimate was
// made when that is not plain, and why a cost could not be estimated.
func settleCost(r *Record) {
	if _, ok := r.CostUSD.Value(); ok {
		r.CostSource = CostProvided
		return
	}
	r.CostSource = CostUnknown
	if pricing.Load() == nil {
		return
	}
	p, ok := priceFor(r)
	if !ok {
		r.Notes = append(r.Notes, fmt.Sprintf("no pricing row for model %q", r.Model))
		return
	}
	cost, notes, ok := estimate(p, r)
	if ok {
		r.CostUSD = KnownFloat(cost)
		r.CostSource = CostEstimated
		if p.Model != r.Model {
			r.Notes = append(r.Notes, fmt.Sprintf("priced by row %q", p.Model))
		}
	}
	r.Notes = append(r.Notes, notes...)
}

// estimate prices the record's quantities by row p. Cache tokens with no
// cache price of their own are priced at the input price. It fails when a
// price the row sets has an unknown quantity, or when a token class with a
// known, non-zero count has no price (the cost would be understated). The
// notes name each fallback used, or the reason it failed.
func estimate(p Price, r *Record) (float64, []string, bool) {
	type term struct {
		name     string // the price's name in the table
		qtyName  string // the quantity's name in the record
		price    *float64
		fallback *float64 // used for a known quantity when price is not set
		qty      float64  // in the price's unit
		known    bool
		token    bool
	}
	per := func(name, qtyName string, price, fallback *float64, v Int, unit float64, token bool) term {
		n, ok := v.Value()
		return term{name, qtyName, price, fallback, float64(n) / unit, ok, token}
	}
	seconds, secondsOK := r.AudioSeconds.Value()
	terms := []term{
		per("input", "input_tokens", p.Input, nil, r.InputTokens, 1e6, true),
		per("cache_read", "cache_read_tokens", p.CacheRead, p.Input, r.CacheReadTokens, 1e6, true),
		per("cache_write", "cache_creation_tokens", p.CacheWrite, p.Input, r.CacheCreationTokens, 1e6, true),
		per("output", "output_tokens", p.Output, nil, r.OutputTokens, 1e6, true),
		per("characters", "characters", p.Characters, nil, r.Characters, 1e6, false),
		{"audio_minute", "audio_seconds", p.AudioMinute, nil, seconds / 60, secondsOK, false},
		per("image", "images", p.Image, nil, r.Images, 1, false),
	}
	var (
		sum   float64
		notes []string
	)
	for _, t := range terms {
		if t.price == nil {
			switch {
			case !t.known || t.qty == 0:
			case t.fallback != nil:
				sum += *t.fallback * t.qty
				notes = append(notes, fmt.Sprintf("no %s price in row %q, used input price", t.name, p.Model))
			case t.token:
				return 0, []string{fmt.Sprintf("not priced: no %s price in row %q", t.name, p.Model)}, false
			}
			continue
		}
		if !t.known {
			return 0, []string{fmt.Sprintf("not priced: %s unknown", t.qtyName)}, false
		}
		sum += *t.price * t.qty
	}
	return sum, notes, true
}
