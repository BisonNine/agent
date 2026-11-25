/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package imagegen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

func TestWithLogger_TagsComponent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"data":[{"b64_json":"`+pngB64+`"}]}`)
	}))
	defer srv.Close()

	var logs launchtest.Logs
	c := NewOpenAI(srv.URL, "sk", WithLogger(logs.Logger()))
	if _, err := c.Generate(context.Background(), testID, generateReq()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	e, ok := logs.Find("Sending request")
	if !ok {
		t.Fatalf("no request log; got %+v", logs.Entries())
	}
	if e.Attrs["component"] != component || e.Attrs["model"] != "img-test" {
		t.Errorf("attrs = %+v, want component %q and model img-test", e.Attrs, component)
	}
}

func TestOpenAI_EditValidation(t *testing.T) {
	img := []InputImage{{Data: strings.NewReader("a")}}
	tests := []struct {
		name    string
		req     *EditRequest
		wantErr string
	}{
		{"missing model", &EditRequest{Prompt: "p", Images: img}, "edit: model is required"},
		{"missing prompt", &EditRequest{Model: "m", Images: img}, "edit: prompt is required"},
		{"mask without data", &EditRequest{Model: "m", Prompt: "p", Images: img, Mask: &InputImage{}}, "mask: no data"},
		{"image without data names it", &EditRequest{Model: "m", Prompt: "p", Images: []InputImage{img[0], {}}}, "image 2: no data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No server: validation must fail before any request is sent.
			_, err := NewOpenAI("http://127.0.0.1:1", "k").Edit(context.Background(), testID, tt.req)
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestOpenAI_InvalidBaseURL(t *testing.T) {
	const bad = "http://[::1" // unterminated IPv6 literal: not a URL
	c := NewOpenAI(bad, "k")
	if c.openRouter {
		t.Error("an unparsable base URL was taken for OpenRouter")
	}
	ctx := context.Background()
	if _, err := c.Generate(ctx, testID, generateReq()); err == nil || !strings.Contains(err.Error(), "failed to create request") {
		t.Errorf("Generate err = %v, want failed to create request", err)
	}
	if _, err := c.Edit(ctx, testID, editReq()); err == nil || !strings.Contains(err.Error(), "failed to create request") {
		t.Errorf("Edit err = %v, want failed to create request", err)
	}
}
