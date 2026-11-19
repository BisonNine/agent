/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package accounting

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Kind is what a launch produces.
type Kind string

const (
	KindText          Kind = "text"
	KindImage         Kind = "image"
	KindSpeech        Kind = "speech"
	KindTranscription Kind = "transcription"
)

// Transport is how a launch reaches the model.
type Transport string

const (
	// TransportCLI is a subprocess (a vendor CLI).
	TransportCLI Transport = "cli"
	// TransportAPI is an HTTP API.
	TransportAPI Transport = "api"
)

// Unknown is how a value the provider did not report is written in JSON.
const Unknown = "-"

// timeLayout is RFC 3339 with a fixed number of sub-second digits, so every
// record's time has the same width.
const timeLayout = "2006-01-02T15:04:05.000000Z07:00"

// Record is one launch of an LLM or API call. A numeric field the provider
// did not report is unknown (its zero value) and is written as "-", never as
// 0; a reported 0 is a known 0.
type Record struct {
	// Time is when the launch started. MarshalJSON writes it first, in UTC.
	Time      time.Time `json:"-"`
	Agent     string    `json:"agent"`
	Project   string    `json:"project"`
	Kind      Kind      `json:"kind"`
	Provider  string    `json:"provider"`
	Transport Transport `json:"transport"`
	Model     string    `json:"model"`
	// InputTokens counts the input tokens neither read from nor written to
	// the prompt cache (when the provider breaks cache tokens out).
	InputTokens         Int   `json:"input_tokens"`
	OutputTokens        Int   `json:"output_tokens"`
	CacheReadTokens     Int   `json:"cache_read_tokens"`
	CacheCreationTokens Int   `json:"cache_creation_tokens"`
	CostUSD             Float `json:"cost_usd"`
	// ToolCalls is the number of tool calls the model made in the launch.
	ToolCalls Int `json:"tool_calls"`
	// HTTPStatus is the status of the HTTP response; unknown for a CLI
	// launch and for a call that got no response.
	HTTPStatus Int `json:"http_status"`
	// DurationMs is the wall-clock time of the launch.
	DurationMs int64 `json:"duration_ms"`
	Success    bool  `json:"success"`
	// Error is the launch's error; empty on success.
	Error string `json:"error,omitempty"`
	// AudioSeconds is the audio duration the provider reported as billed.
	AudioSeconds Float `json:"audio_seconds"`
	// Characters is the length of the text sent for speech synthesis, in
	// code points.
	Characters Int `json:"characters"`
}

// recordJSON is Record without its methods, so the JSON methods can reuse
// the field tags.
type recordJSON Record

// MarshalJSON writes the record with Time first, in UTC, as RFC 3339 with
// microseconds.
func (r Record) MarshalJSON() ([]byte, error) {
	fields, err := json.Marshal(recordJSON(r))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(fields)+48)
	out = append(out, `{"time":"`...)
	out = r.Time.UTC().AppendFormat(out, timeLayout)
	out = append(out, `",`...)
	return append(out, fields[1:]...), nil
}

// UnmarshalJSON reads a record written by MarshalJSON.
func (r *Record) UnmarshalJSON(b []byte) error {
	aux := struct {
		*recordJSON

		Time string `json:"time"`
	}{recordJSON: (*recordJSON)(r)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	t, err := time.Parse(time.RFC3339Nano, aux.Time)
	if err != nil {
		return fmt.Errorf("accounting: record time: %w", err)
	}
	r.Time = t
	return nil
}

// Int is an integer that may be unknown. The zero value is unknown.
type Int struct {
	value int64
	known bool
}

// KnownInt returns a known integer.
func KnownInt(v int64) Int { return Int{value: v, known: true} }

// Value returns the integer and whether it is known.
func (i Int) Value() (int64, bool) { return i.value, i.known }

// MarshalJSON writes the integer as a JSON number, or "-" when unknown.
func (i Int) MarshalJSON() ([]byte, error) {
	if !i.known {
		return []byte(`"` + Unknown + `"`), nil
	}
	return strconv.AppendInt(nil, i.value, 10), nil
}

// UnmarshalJSON reads a JSON integer, or "-" as unknown.
func (i *Int) UnmarshalJSON(b []byte) error {
	if isUnknown(b) {
		*i = Int{}
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("accounting: integer: %w", err)
	}
	*i = KnownInt(v)
	return nil
}

// Float is a number that may be unknown. The zero value is unknown.
type Float struct {
	value float64
	known bool
}

// KnownFloat returns a known number. NaN and infinities are not numbers a
// provider reports, and JSON cannot hold them, so they give an unknown value.
func KnownFloat(v float64) Float {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return Float{}
	}
	return Float{value: v, known: true}
}

// Value returns the number and whether it is known.
func (f Float) Value() (float64, bool) { return f.value, f.known }

// MarshalJSON writes the number as a JSON number, or "-" when unknown.
func (f Float) MarshalJSON() ([]byte, error) {
	if !f.known {
		return []byte(`"` + Unknown + `"`), nil
	}
	return strconv.AppendFloat(nil, f.value, 'g', -1, 64), nil
}

// UnmarshalJSON reads a JSON number, or "-" as unknown.
func (f *Float) UnmarshalJSON(b []byte) error {
	if isUnknown(b) {
		*f = Float{}
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("accounting: number: %w", err)
	}
	*f = KnownFloat(v)
	return nil
}

func isUnknown(b []byte) bool {
	return bytes.Equal(bytes.TrimSpace(b), []byte(`"`+Unknown+`"`))
}
