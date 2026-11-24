package accounting

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIntJSON(t *testing.T) {
	tests := []struct {
		name string
		in   Int
		want string
	}{
		{name: "unknown", in: Int{}, want: `"-"`},
		{name: "known zero", in: KnownInt(0), want: `0`},
		{name: "known value", in: KnownInt(1234), want: `1234`},
		{name: "negative", in: KnownInt(-5), want: `-5`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.want {
				t.Fatalf("marshal = %s, want %s", b, tt.want)
			}
			var back Int
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			if back != tt.in {
				t.Errorf("round trip = %+v, want %+v", back, tt.in)
			}
		})
	}
}

func TestFloatJSON(t *testing.T) {
	tests := []struct {
		name string
		in   Float
		want string
	}{
		{name: "unknown", in: Float{}, want: `"-"`},
		{name: "known zero", in: KnownFloat(0), want: `0`},
		{name: "known value", in: KnownFloat(0.0125), want: `0.0125`},
		{name: "NaN is unknown", in: KnownFloat(math.NaN()), want: `"-"`},
		{name: "infinity is unknown", in: KnownFloat(math.Inf(1)), want: `"-"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.want {
				t.Fatalf("marshal = %s, want %s", b, tt.want)
			}
			var back Float
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			if back != tt.in {
				t.Errorf("round trip = %+v, want %+v", back, tt.in)
			}
		})
	}
}

func TestUnmarshalRejectsOtherStrings(t *testing.T) {
	var i Int
	if err := json.Unmarshal([]byte(`"12"`), &i); err == nil {
		t.Error("Int accepted a quoted number")
	}
	var f Float
	if err := json.Unmarshal([]byte(`"x"`), &f); err == nil {
		t.Error("Float accepted a string other than -")
	}
}

func TestRecordJSON(t *testing.T) {
	start := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("EST", -5*3600))
	rec := Record{
		Time:            start,
		Agent:           "alice",
		Project:         "default",
		Kind:            KindText,
		Provider:        "openai-chat",
		Transport:       TransportAPI,
		Model:           "gpt-test",
		InputTokens:     KnownInt(10),
		OutputTokens:    KnownInt(0),
		CacheReadTokens: KnownInt(4),
		CostSource:      CostUnknown,
		HTTPStatus:      KnownInt(200),
		Images:          KnownInt(0),
		DurationMs:      42,
		Success:         true,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"time":"2026-03-04T10:06:07.000000Z","agent":"alice","agent_name":"-","project":"default","kind":"text",` +
		`"provider":"openai-chat","transport":"api","model":"gpt-test","input_tokens":10,"output_tokens":0,` +
		`"cache_read_tokens":4,"cache_creation_tokens":"-","cost_usd":"-","cost_source":"-","tool_calls":"-","http_status":200,"duration_ms":42,` +
		`"success":true,"audio_seconds":"-","characters":"-","images":0}`
	if string(b) != want {
		t.Fatalf("marshal =\n%s\nwant\n%s", b, want)
	}

	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Time.Equal(start) {
		t.Errorf("time = %v, want %v", back.Time, start)
	}
	back.Time = rec.Time
	if !reflect.DeepEqual(back, rec) {
		t.Errorf("round trip =\n%+v\nwant\n%+v", back, rec)
	}
}

func TestRecordJSONKeepsSubSecondAndError(t *testing.T) {
	rec := Record{
		Time:    time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC),
		Agent:   "bob",
		Success: false,
		Error:   "boom",
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"time":"2026-01-02T03:04:05.123456Z"`) {
		t.Errorf("time not written with microseconds in UTC: %s", b)
	}
	if !strings.Contains(string(b), `"error":"boom"`) {
		t.Errorf("error missing: %s", b)
	}
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Error != "boom" || back.Agent != "bob" {
		t.Errorf("round trip = %+v", back)
	}
}

func TestRecordJSONNotes(t *testing.T) {
	rec := Record{Time: time.Date(2026, 3, 4, 10, 6, 7, 0, time.UTC), Agent: "alice", Project: "default"}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "notes") {
		t.Errorf("a record without notes wrote the field: %s", b)
	}

	rec.Notes = []string{`priced by row "gpt-4o*"`, "second note"}
	b, err = json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"notes":["priced by row \"gpt-4o*\"","second note"]}`; !strings.HasSuffix(string(b), want) {
		t.Errorf("marshal = %s, want it to end with %s", b, want)
	}
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Notes, rec.Notes) {
		t.Errorf("notes = %q, want %q", back.Notes, rec.Notes)
	}
}

func TestRecordJSONAgentName(t *testing.T) {
	rec := Record{Time: time.Date(2026, 3, 4, 10, 6, 7, 0, time.UTC), Agent: "3f2b8c1e-9d4a-4e7b-a1c6-5f0e2d9b7a43", AgentName: "Bob\nthe \"builder\"\x00", Project: "default"}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"agent_name":"Bob\nthe \"builder\"\u0000",`; !strings.Contains(string(b), want) {
		t.Errorf("marshal = %s, want it to contain %s", b, want)
	}
	if bytes.ContainsAny(b, "\n\r\x00") {
		t.Errorf("a name with control characters broke the record line: %q", b)
	}
	var back Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.AgentName != rec.AgentName {
		t.Errorf("agent_name = %q, want %q", back.AgentName, rec.AgentName)
	}
}
