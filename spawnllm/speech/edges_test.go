/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package speech

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

// errReader fails on its first read, as a broken upload source would.
type errReader struct{}

var errAudioSource = errors.New("audio source broke")

func (errReader) Read([]byte) (int, error) { return 0, errAudioSource }

// badBaseURL cannot be parsed as a URL (unterminated IPv6 literal).
const badBaseURL = "http://[::1"

func TestWithLogger_DroppedFieldsAreLogged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "speech-to-text") {
			writeJSON(t, w, http.StatusOK, `{"text":"ok"}`)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		writeRaw(t, w, []byte("ID3-audio"))
	}))
	defer srv.Close()

	var logs launchtest.Logs
	e := NewElevenLabs(srv.URL, "xi", WithLogger(logs.Logger()))
	ctx := context.Background()
	if _, err := e.Transcribe(ctx, testID, &TranscribeRequest{Model: "scribe", Audio: audio(), Filename: "a.ogg", Prompt: "Alice"}); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if _, err := e.Synthesize(ctx, testID, &SynthesizeRequest{Model: "tts", Text: "Hi", Voice: "v", Instructions: "slowly"}); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	for _, msg := range []string{
		"ElevenLabs transcription has no prompt field; prompt not sent",
		"ElevenLabs speech has no instructions field; instructions not sent",
	} {
		entry, ok := logs.Find(msg)
		if !ok {
			t.Errorf("no log %q; got %+v", msg, logs.Entries())
			continue
		}
		if entry.Attrs["component"] != component {
			t.Errorf("%q: component = %v, want %q", msg, entry.Attrs["component"], component)
		}
	}
}

// TestBackends_InvalidRequests checks that each backend refuses an invalid
// request before sending anything: the base URL points nowhere.
func TestBackends_InvalidRequests(t *testing.T) {
	ctx := context.Background()
	backends := map[string]interface {
		Transcriber
		Synthesizer
	}{
		"elevenlabs": NewElevenLabs("http://127.0.0.1:1", "k"),
		"gemini":     NewGemini("http://127.0.0.1:1", "k"),
		"openai":     NewOpenAI("http://127.0.0.1:1", "k"),
	}
	for name, b := range backends {
		t.Run(name, func(t *testing.T) {
			if _, err := b.Transcribe(ctx, testID, &TranscribeRequest{Audio: audio()}); err == nil || err.Error() != "transcribe: model is required" {
				t.Errorf("Transcribe err = %v, want model is required", err)
			}
			if _, err := b.Synthesize(ctx, testID, &SynthesizeRequest{Model: "m", Voice: "v"}); err == nil || err.Error() != "synthesize: text is required" {
				t.Errorf("Synthesize err = %v, want text is required", err)
			}
			if _, err := b.Synthesize(ctx, testID, nil); err == nil || err.Error() != "synthesize request is nil" {
				t.Errorf("Synthesize(nil) err = %v, want request is nil", err)
			}
		})
	}
}

func TestBackends_AudioReadError(t *testing.T) {
	tests := []struct {
		name string
		tr   Transcriber
	}{
		{"elevenlabs", NewElevenLabs("http://127.0.0.1:1", "k")},
		{"gemini", NewGemini("http://127.0.0.1:1", "k")},
		{"openai multipart", NewOpenAI("http://127.0.0.1:1", "k", WithTranscriptionAPI(TranscriptionMultipart))},
		{"openai input audio", NewOpenAI("http://127.0.0.1:1", "k", WithTranscriptionAPI(TranscriptionInputAudio))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.tr.Transcribe(context.Background(), testID,
				&TranscribeRequest{Model: "m", Audio: errReader{}, Filename: "a.wav", ContentType: "audio/wav"})
			if !errors.Is(err, errAudioSource) {
				t.Fatalf("err = %v, want the audio source's error", err)
			}
		})
	}
}

func TestBackends_InvalidBaseURL(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func() error
	}{
		{"elevenlabs transcribe", func() error {
			_, err := NewElevenLabs(badBaseURL, "k").Transcribe(ctx, testID, &TranscribeRequest{Model: "m", Audio: audio(), Filename: "a.ogg"})
			return err
		}},
		{"elevenlabs synthesize", func() error {
			_, err := NewElevenLabs(badBaseURL, "k").Synthesize(ctx, testID, &SynthesizeRequest{Model: "m", Text: "t", Voice: "v"})
			return err
		}},
		{"gemini transcribe", func() error {
			_, err := NewGemini(badBaseURL, "k").Transcribe(ctx, testID, &TranscribeRequest{Model: "m", Audio: audio(), Filename: "a.ogg"})
			return err
		}},
		{"openai synthesize", func() error {
			_, err := NewOpenAI(badBaseURL, "k").Synthesize(ctx, testID, &SynthesizeRequest{Model: "m", Text: "t", Voice: "v"})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); err == nil || !strings.Contains(err.Error(), "failed to create request") {
				t.Fatalf("err = %v, want failed to create request", err)
			}
		})
	}
}

func TestBackends_MalformedResponses(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		call    func(url string) error
		wantErr string
	}{
		{
			name: "elevenlabs transcription not JSON",
			body: `{"text":`,
			call: func(url string) error {
				_, err := NewElevenLabs(url, "k").Transcribe(context.Background(), testID, &TranscribeRequest{Model: "m", Audio: audio(), Filename: "a.ogg"})
				return err
			},
			wantErr: "failed to decode response",
		},
		{
			name: "gemini response not JSON",
			body: `{"candidates":[`,
			call: func(url string) error {
				_, err := NewGemini(url, "k").Transcribe(context.Background(), testID, &TranscribeRequest{Model: "m", Audio: audio(), Filename: "a.ogg"})
				return err
			},
			wantErr: "failed to decode response",
		},
		{
			name: "gemini audio not base64",
			body: `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16","data":"!!not base64!!"}}]},"finishReason":"STOP"}]}`,
			call: func(url string) error {
				_, err := NewGemini(url, "k").Synthesize(context.Background(), testID, &SynthesizeRequest{Model: "m", Text: "Hi"})
				return err
			},
			wantErr: "failed to decode response audio",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, tt.body)
			}))
			defer srv.Close()
			if err := tt.call(srv.URL); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestGemini_NoUsageMetadata checks that a response without usageMetadata
// leaves the token counts unreported rather than reported as zero.
func TestGemini_NoUsageMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(readBody(t, r), "inlineData") {
			writeJSON(t, w, http.StatusOK, `{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}]}`)
			return
		}
		writeJSON(t, w, http.StatusOK, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16","data":"AAEC"}}]}}]}`)
	}))
	defer srv.Close()
	g := NewGemini(srv.URL, "k")

	tr, err := g.Transcribe(context.Background(), testID, &TranscribeRequest{Model: "m", Audio: audio(), Filename: "a.ogg"})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if tr.Text != "hello" || tr.Usage != nil {
		t.Errorf("transcription = %q usage %+v, want hello and no usage", tr.Text, tr.Usage)
	}

	sp, err := g.Synthesize(context.Background(), testID, &SynthesizeRequest{Model: "m", Text: "Hi"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if string(sp.Audio) != "\x00\x01\x02" || sp.ContentType != "audio/L16" {
		t.Errorf("speech = %q %q, want the decoded audio", sp.Audio, sp.ContentType)
	}
	u := sp.Usage
	if u == nil || u.Characters != 2 || !u.Reported.Characters || u.Reported.InputTokens || u.Reported.OutputTokens {
		t.Errorf("usage = %+v, want 2 characters and no tokens reported", u)
	}
}

func TestOpenAIFormatType(t *testing.T) {
	tests := map[string]string{
		"":     "audio/mpeg",
		"mp3":  "audio/mpeg",
		"MP3":  "audio/mpeg",
		"opus": "audio/ogg",
		"aac":  "audio/aac",
		"flac": "audio/flac",
		"wav":  "audio/wav",
		"pcm":  "audio/pcm",
		"xyz":  "application/octet-stream",
	}
	for format, want := range tests {
		if got := openAIFormatType(format); got != want {
			t.Errorf("openAIFormatType(%q) = %q, want %q", format, got, want)
		}
	}
}
