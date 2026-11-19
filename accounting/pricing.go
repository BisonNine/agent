/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
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
	"strings"
	"sync/atomic"
)

// ErrInvalidPricing reports a pricing table that cannot be installed.
var ErrInvalidPricing = errors.New("invalid pricing")

// Price is one row of the pricing table: what one model costs. Every price
// is optional; nil is not set, and an explicit 0 is a real, free price. A row
// sets at least one price.
type Price struct {
	// Provider limits the row to one provider (a record's provider, e.g.
	// "openai-chat"); empty matches any provider.
	Provider string `json:"provider,omitempty"`
	// Model is matched exactly against the record's model.
	Model string `json:"model"`
	// Input, CacheRead, CacheWrite and Output are USD per million tokens.
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

type priceKey struct{ provider, model string }

// pricing is the installed table; nil when there is none.
var pricing atomic.Pointer[map[priceKey]Price]

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
	table := make(map[priceKey]Price, len(rows))
	for i, p := range rows {
		if err := p.validate(); err != nil {
			return fmt.Errorf("accounting: %w: row %d: %w", ErrInvalidPricing, i+1, err)
		}
		k := priceKey{p.Provider, p.Model}
		if _, dup := table[k]; dup {
			return fmt.Errorf("accounting: %w: row %d: duplicate provider %q, model %q",
				ErrInvalidPricing, i+1, p.Provider, p.Model)
		}
		table[k] = p
	}
	pricing.Store(&table)
	return nil
}

// ClearPricing removes the pricing table: no cost is estimated.
func ClearPricing() { pricing.Store(nil) }

func (p Price) validate() error {
	if strings.TrimSpace(p.Model) == "" {
		return errors.New("model is required")
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

// priceFor returns the row for a record: the one for its provider and model,
// else the one for its model and any provider.
func priceFor(r *Record) (Price, bool) {
	t := pricing.Load()
	if t == nil {
		return Price{}, false
	}
	if p, ok := (*t)[priceKey{r.Provider, r.Model}]; ok {
		return p, true
	}
	p, ok := (*t)[priceKey{"", r.Model}]
	return p, ok
}

// settleCost sets the record's cost source, and estimates the cost from the
// pricing table when the call returned none.
func settleCost(r *Record) {
	if _, ok := r.CostUSD.Value(); ok {
		r.CostSource = CostProvided
		return
	}
	r.CostSource = CostUnknown
	p, ok := priceFor(r)
	if !ok {
		return
	}
	if cost, ok := estimate(p, r); ok {
		r.CostUSD = KnownFloat(cost)
		r.CostSource = CostEstimated
	}
}

// estimate prices the record's quantities by row p. It fails when a price
// the row sets has an unknown quantity, or when a token class with a known,
// non-zero count has no price (the cost would be understated).
func estimate(p Price, r *Record) (float64, bool) {
	type term struct {
		price *float64
		qty   float64 // in the price's unit
		known bool
		token bool
	}
	per := func(price *float64, v Int, unit float64, token bool) term {
		n, ok := v.Value()
		return term{price, float64(n) / unit, ok, token}
	}
	seconds, secondsOK := r.AudioSeconds.Value()
	terms := []term{
		per(p.Input, r.InputTokens, 1e6, true),
		per(p.CacheRead, r.CacheReadTokens, 1e6, true),
		per(p.CacheWrite, r.CacheCreationTokens, 1e6, true),
		per(p.Output, r.OutputTokens, 1e6, true),
		per(p.Characters, r.Characters, 1e6, false),
		{p.AudioMinute, seconds / 60, secondsOK, false},
		per(p.Image, r.Images, 1, false),
	}
	var sum float64
	for _, t := range terms {
		if t.price == nil {
			if t.token && t.known && t.qty != 0 {
				return 0, false
			}
			continue
		}
		if !t.known {
			return 0, false
		}
		sum += *t.price * t.qty
	}
	return sum, true
}
