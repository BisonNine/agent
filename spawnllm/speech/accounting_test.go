/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package speech

import (
	"context"
	"errors"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

// testID is the accounting identity the tests launch calls with.
var testID = accounting.Identity{Agent: "alice"}

func transcribeReq() *TranscribeRequest {
	return &TranscribeRequest{Model: "stt-test", Audio: strings.NewReader("audio"), Filename: "a.ogg", ContentType: "audio/ogg"}
}

func synthesizeReq() *SynthesizeRequest {
	// Five code points, six bytes.
	return &SynthesizeRequest{Model: "tts-test", Text: "héllo", Voice: "v"}
}

// usage is the usage part of a record a test expects.
type usage struct {
	input, output, characters accounting.Int
	seconds                   accounting.Float
}

func checkUsage(t *testing.T, rec accounting.Record, want usage) {
	t.Helper()
	got := usage{rec.InputTokens, rec.OutputTokens, rec.Characters, rec.AudioSeconds}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

const geminiAudio = `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000",` +
	`"data":"AAAA"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":20,"totalTokenCount":28}}`

var backends = []struct {
	provider string
	build    func(url string) interface {
		Transcriber
		Synthesizer
	}
	transcribeType, transcribeBody string
	transcribeWant                 usage
	synthesizeType, synthesizeBody string
	synthesizeWant                 usage
}{
	{
		provider: "openai",
		build: func(url string) interface {
			Transcriber
			Synthesizer
		} {
			return NewOpenAI(url, "k")
		},
		// whisper-1 reports the billed duration and no tokens.
		transcribeType: "application/json",
		transcribeBody: `{"text":"hi","usage":{"type":"duration","seconds":3}}`,
		transcribeWant: usage{seconds: accounting.KnownFloat(3)},
		synthesizeType: "audio/mpeg",
		synthesizeBody: "ID3audio",
		synthesizeWant: usage{characters: accounting.KnownInt(5)},
	},
	{
		provider: "gemini",
		build: func(url string) interface {
			Transcriber
			Synthesizer
		} {
			return NewGemini(url, "k")
		},
		transcribeType: "application/json",
		transcribeBody: `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0,"totalTokenCount":100}}`,
		transcribeWant: usage{input: accounting.KnownInt(100), output: accounting.KnownInt(0)},
		synthesizeType: "application/json",
		synthesizeBody: geminiAudio,
		synthesizeWant: usage{input: accounting.KnownInt(8), output: accounting.KnownInt(20), characters: accounting.KnownInt(5)},
	},
	{
		provider: "elevenlabs",
		build: func(url string) interface {
			Transcriber
			Synthesizer
		} {
			return NewElevenLabs(url, "k")
		},
		// Scribe reports no usage at all.
		transcribeType: "application/json",
		transcribeBody: `{"text":"hi","language_code":"en","audio_duration_secs":2.5}`,
		transcribeWant: usage{},
		synthesizeType: "audio/mpeg",
		synthesizeBody: "ID3audio",
		synthesizeWant: usage{characters: accounting.KnownInt(5)},
	},
}

func TestRefusesInvalidIdentity(t *testing.T) {
	for _, b := range backends {
		for _, r := range launchtest.Rejected {
			t.Run(b.provider+"/transcribe/"+r.Name, func(t *testing.T) {
				rec := launchtest.Capture(t)
				url, hits := launchtest.Server(t, http.StatusOK, b.transcribeType, b.transcribeBody)
				if _, err := b.build(url).Transcribe(context.Background(), r.ID, transcribeReq()); !errors.Is(err, r.Want) {
					t.Fatalf("err = %v, want %v", err, r.Want)
				}
				if hits.Load() != 0 {
					t.Errorf("requests = %d, want 0", hits.Load())
				}
				if got := rec.Refused(b.provider); got.Kind != accounting.KindTranscription {
					t.Errorf("kind = %s", got.Kind)
				}
			})
			t.Run(b.provider+"/synthesize/"+r.Name, func(t *testing.T) {
				rec := launchtest.Capture(t)
				url, hits := launchtest.Server(t, http.StatusOK, b.synthesizeType, b.synthesizeBody)
				if _, err := b.build(url).Synthesize(context.Background(), r.ID, synthesizeReq()); !errors.Is(err, r.Want) {
					t.Fatalf("err = %v, want %v", err, r.Want)
				}
				if hits.Load() != 0 {
					t.Errorf("requests = %d, want 0", hits.Load())
				}
				if got := rec.Refused(b.provider); got.Kind != accounting.KindSpeech {
					t.Errorf("kind = %s", got.Kind)
				}
			})
		}
	}
}

func checkSuccess(t *testing.T, got accounting.Record, provider string, kind accounting.Kind, model string) {
	t.Helper()
	if !got.Success || got.Error != "" {
		t.Errorf("success=%v error=%q, want success", got.Success, got.Error)
	}
	if got.Provider != provider || got.Kind != kind || got.Transport != accounting.TransportAPI || got.Model != model {
		t.Errorf("record = %s/%s/%s/%s", got.Provider, got.Kind, got.Transport, got.Model)
	}
	if got.Agent != "alice" || got.Project != accounting.DefaultProject {
		t.Errorf("identity = %q/%q", got.Agent, got.Project)
	}
	if n := launchtest.Known(t, "http_status", got.HTTPStatus); n != http.StatusOK {
		t.Errorf("http_status = %d", n)
	}
	launchtest.Unknown(t, "cache_read_tokens", got.CacheReadTokens)
	launchtest.Unknown(t, "tool_calls", got.ToolCalls)
	launchtest.Unknown(t, "images", got.Images)
}

func TestAccountsSuccess(t *testing.T) {
	for _, b := range backends {
		t.Run(b.provider+"/transcribe", func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, _ := launchtest.Server(t, http.StatusOK, b.transcribeType, b.transcribeBody)
			if _, err := b.build(url).Transcribe(context.Background(), testID, transcribeReq()); err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			got := rec.One()
			checkSuccess(t, got, b.provider, accounting.KindTranscription, "stt-test")
			checkUsage(t, got, b.transcribeWant)
		})
		t.Run(b.provider+"/synthesize", func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, _ := launchtest.Server(t, http.StatusOK, b.synthesizeType, b.synthesizeBody)
			if _, err := b.build(url).Synthesize(context.Background(), testID, synthesizeReq()); err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
			got := rec.One()
			checkSuccess(t, got, b.provider, accounting.KindSpeech, "tts-test")
			checkUsage(t, got, b.synthesizeWant)
		})
	}
}

func TestAccountsHTTPError(t *testing.T) {
	for _, b := range backends {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
			t.Run(b.provider+"/"+http.StatusText(status), func(t *testing.T) {
				rec := launchtest.Capture(t)
				url, _ := launchtest.Server(t, status, "application/json", `{"error":"no"}`)
				if _, err := b.build(url).Transcribe(context.Background(), testID, transcribeReq()); err == nil {
					t.Fatal("Transcribe succeeded")
				}
				checkUsage(t, rec.Failed(status), usage{})
			})
		}
	}
}

func TestAccountsConnectionError(t *testing.T) {
	rec := launchtest.Capture(t)
	if _, err := NewOpenAI(launchtest.ClosedURL(t), "k").Synthesize(context.Background(), testID, synthesizeReq()); err == nil {
		t.Fatal("Synthesize succeeded")
	}
	rec.Failed(0)
}

func TestAccountsOpenRouterTranscriptionCost(t *testing.T) {
	rec := launchtest.Capture(t)
	url, _ := launchtest.Server(t, http.StatusOK, "application/json",
		`{"text":"hi","usage":{"seconds":4,"input_tokens":20,"output_tokens":3,"total_tokens":23,"cost":0.002}}`)
	target, err := neturl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: redirectTransport{target: target}}
	c := NewOpenAI("https://openrouter.ai/api/v1", "k", WithHTTPClient(client))
	if _, err := c.Transcribe(context.Background(), testID, transcribeReq()); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	got := rec.One()
	checkUsage(t, got, usage{
		input: accounting.KnownInt(20), output: accounting.KnownInt(3), seconds: accounting.KnownFloat(4),
	})
	if cost, ok := got.CostUSD.Value(); !ok || cost != 0.002 {
		t.Errorf("cost_usd = %v (known %v), want 0.002", cost, ok)
	}
}
