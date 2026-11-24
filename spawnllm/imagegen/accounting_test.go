package imagegen

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

func generateReq() *GenerateRequest { return &GenerateRequest{Model: "img-test", Prompt: "Alice"} }

func editReq() *EditRequest {
	return &EditRequest{Model: "img-test", Prompt: "Bob", Images: []InputImage{{Data: strings.NewReader("x")}}}
}

func imagesReply(usage string) string {
	return `{"created":1,"data":[{"b64_json":"` + pngB64 + `"}]` + usage + `}`
}

func TestRefusesInvalidIdentity(t *testing.T) {
	for _, r := range launchtest.Rejected {
		t.Run("generate/"+r.Name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, hits := launchtest.Server(t, http.StatusOK, "application/json", imagesReply(""))
			if _, err := NewOpenAI(url, "k").Generate(context.Background(), r.ID, generateReq()); !errors.Is(err, r.Want) {
				t.Fatalf("err = %v, want %v", err, r.Want)
			}
			if hits.Load() != 0 {
				t.Errorf("requests = %d, want 0", hits.Load())
			}
			if got := rec.Refused("openai"); got.Kind != accounting.KindImage {
				t.Errorf("kind = %s", got.Kind)
			}
		})
		t.Run("edit/"+r.Name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, hits := launchtest.Server(t, http.StatusOK, "application/json", imagesReply(""))
			if _, err := NewOpenAI(url, "k").Edit(context.Background(), r.ID, editReq()); !errors.Is(err, r.Want) {
				t.Fatalf("err = %v, want %v", err, r.Want)
			}
			if hits.Load() != 0 {
				t.Errorf("requests = %d, want 0", hits.Load())
			}
			rec.Refused("openai")
		})
	}
}

func TestAccountsSuccess(t *testing.T) {
	calls := map[string]func(c *OpenAI) error{
		"generate": func(c *OpenAI) error { _, err := c.Generate(context.Background(), testID, generateReq()); return err },
		"edit":     func(c *OpenAI) error { _, err := c.Edit(context.Background(), testID, editReq()); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, hits := launchtest.Server(t, http.StatusOK, "application/json",
				imagesReply(`,"usage":{"input_tokens":5,"output_tokens":100,"total_tokens":105}`))
			if err := call(NewOpenAI(url, "k")); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if hits.Load() != 1 {
				t.Errorf("requests = %d, want 1", hits.Load())
			}
			got := rec.One()
			if !got.Success || got.Kind != accounting.KindImage || got.Provider != "openai" ||
				got.Transport != accounting.TransportAPI || got.Model != "img-test" || got.Project != accounting.DefaultProject {
				t.Errorf("record = %+v", got)
			}
			if n := launchtest.Known(t, "http_status", got.HTTPStatus); n != http.StatusOK {
				t.Errorf("http_status = %d", n)
			}
			if n := launchtest.Known(t, "input_tokens", got.InputTokens); n != 5 {
				t.Errorf("input_tokens = %d, want 5", n)
			}
			if n := launchtest.Known(t, "output_tokens", got.OutputTokens); n != 100 {
				t.Errorf("output_tokens = %d, want 100", n)
			}
			if _, ok := got.CostUSD.Value(); ok {
				t.Error("cost_usd known, want unknown")
			}
			launchtest.Unknown(t, "tool_calls", got.ToolCalls)
			if n := launchtest.Known(t, "images", got.Images); n != 1 {
				t.Errorf("images = %d, want 1", n)
			}
		})
	}
}

func TestAccountsWithoutUsage(t *testing.T) {
	rec := launchtest.Capture(t)
	url, _ := launchtest.Server(t, http.StatusOK, "application/json", imagesReply(""))
	if _, err := NewOpenAI(url, "k").Generate(context.Background(), testID, generateReq()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := rec.One()
	launchtest.Unknown(t, "input_tokens", got.InputTokens)
	launchtest.Unknown(t, "output_tokens", got.OutputTokens)
}

func TestAccountsOpenRouterCost(t *testing.T) {
	rec := launchtest.Capture(t)
	url, _ := launchtest.Server(t, http.StatusOK, "application/json",
		imagesReply(`,"usage":{"prompt_tokens":3,"completion_tokens":4175,"total_tokens":4178,"cost":0.04}`))
	target, err := neturl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	c := NewOpenAI("https://openrouter.ai/api/v1", "k", WithHTTPClient(&http.Client{Transport: redirectTransport{target: target}}))
	if _, err := c.Generate(context.Background(), testID, generateReq()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := rec.One()
	if n := launchtest.Known(t, "output_tokens", got.OutputTokens); n != 4175 {
		t.Errorf("output_tokens = %d, want 4175", n)
	}
	if cost, ok := got.CostUSD.Value(); !ok || cost != 0.04 {
		t.Errorf("cost_usd = %v (known %v), want 0.04", cost, ok)
	}
}

func TestAccountsHTTPError(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, _ := launchtest.Server(t, status, "application/json", `{"error":{"message":"no"}}`)
			if _, err := NewOpenAI(url, "k").Generate(context.Background(), testID, generateReq()); err == nil {
				t.Fatal("Generate succeeded")
			}
			launchtest.Unknown(t, "images", rec.Failed(status).Images)
		})
	}
}

func TestAccountsConnectionError(t *testing.T) {
	rec := launchtest.Capture(t)
	if _, err := NewOpenAI(launchtest.ClosedURL(t), "k").Generate(context.Background(), testID, generateReq()); err == nil {
		t.Fatal("Generate succeeded")
	}
	rec.Failed(0)
}

func TestCostEstimatedFromPricing(t *testing.T) {
	if err := accounting.SetPricing([]accounting.Price{{Model: "img-test", Image: new(0.04)}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(accounting.ClearPricing)
	rec := launchtest.Capture(t)
	// Two images and no usage: the row prices images only.
	url, _ := launchtest.Server(t, http.StatusOK, "application/json",
		`{"created":1,"data":[{"b64_json":"`+pngB64+`"},{"b64_json":"`+pngB64+`"}]}`)
	if _, err := NewOpenAI(url, "k").Generate(context.Background(), testID, generateReq()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := rec.One()
	if cost, ok := got.CostUSD.Value(); !ok || cost != 0.08 || got.CostSource != accounting.CostEstimated {
		t.Errorf("cost = %v (known %v, source %q), want 0.08 estimated", cost, ok, got.CostSource)
	}
}
