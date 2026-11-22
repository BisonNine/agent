package modelspec

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsSecretRef(t *testing.T) {
	for s, want := range map[string]bool{
		"env:ALICE_KEY":   true,
		"file:/etc/key":   true,
		"sk-plain":        false,
		"":                false,
		"ENV:ALICE_KEY":   false,
		"https://env:x@y": false,
	} {
		if got := IsSecretRef(s); got != want {
			t.Errorf("IsSecretRef(%q) = %v, want %v", s, got, want)
		}
	}
}

func writeSecret(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultResolver(t *testing.T) {
	t.Setenv("MODELSPEC_TEST_ALICE", "alice-value")
	t.Setenv("MODELSPEC_TEST_EMPTY", "")
	good := writeSecret(t, "  file-value\n", 0o600)
	loose := writeSecret(t, "x", 0o644)
	resolve := DefaultResolver(0o600)

	tests := []struct {
		name    string
		ref     string
		want    string
		wantErr string
	}{
		{name: "env", ref: "env:MODELSPEC_TEST_ALICE", want: "alice-value"},
		{name: "env name is trimmed", ref: "env: MODELSPEC_TEST_ALICE ", want: "alice-value"},
		{name: "env set but empty", ref: "env:MODELSPEC_TEST_EMPTY", want: ""},
		{name: "env not set", ref: "env:MODELSPEC_TEST_BOB", wantErr: "environment variable MODELSPEC_TEST_BOB is not set"},
		{name: "env names nothing", ref: "env:", wantErr: "names no environment variable"},
		{name: "file is trimmed", ref: "file:" + good, want: "file-value"},
		{name: "relative path", ref: "file:secret.txt", wantErr: "the path must be absolute"},
		{name: "empty path", ref: "file:", wantErr: "the path must be absolute"},
		{name: "missing file", ref: "file:" + filepath.Join(t.TempDir(), "none"), wantErr: "no such file"},
		{name: "directory", ref: "file:" + t.TempDir(), wantErr: "is a directory"},
		{name: "not a reference", ref: "sk-plain", wantErr: "is not a secret reference"},
	}
	if runtime.GOOS != "windows" {
		tests = append(tests, struct {
			name    string
			ref     string
			want    string
			wantErr string
		}{name: "file too open", ref: "file:" + loose, wantErr: "has mode 0644, more than the 0600 allowed; run: chmod 600 " + loose})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolve(tc.ref)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("resolve(%q) = %q, %v; want %q", tc.ref, got, err, tc.want)
			}
		})
	}
}

// The resolver may grant more than 0600.
func TestDefaultResolver_FollowsTheModeItIsGiven(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	path := writeSecret(t, "v", 0o640)
	if _, err := DefaultResolver(0o600)("file:" + path); err == nil {
		t.Error("0640 accepted under 0600")
	}
	if got, err := DefaultResolver(0o640)("file:" + path); err != nil || got != "v" {
		t.Errorf("0640 under 0640 = %q, %v", got, err)
	}
}

// as asserts v's type.
func as[T any](t *testing.T, v any) T {
	t.Helper()
	out, ok := v.(T)
	if !ok {
		t.Fatalf("%v is a %T", v, v)
	}
	return out
}

// fakeResolver resolves "env:X" to "value-of-X" and records every call.
func fakeResolver(calls *[]string) Resolver {
	return func(ref string) (string, error) {
		*calls = append(*calls, ref)
		name, ok := strings.CutPrefix(ref, SecretRefEnvPrefix)
		if !ok {
			return "", errors.New("unsupported")
		}
		return "value-of-" + name, nil
	}
}

func secretsDocument() Document {
	on := true
	return Document{
		Version: 1,
		Providers: []Provider{
			{Name: "gw", Protocol: "openai-chat", BaseURL: "env:NOT_SECRET", APIKey: "env:GW_KEY", Proxy: "env:GW_PROXY"},
			{Name: "plain", Protocol: "openai-chat", BaseURL: "https://api.example.com", APIKey: "sk-literal"},
		},
		Models: []Model{{
			ModelName:  "m",
			Model:      "env:NOT_SECRET",
			Provider:   "gw",
			Workspace:  "env:NOT_SECRET",
			ExtraArgs:  []string{"env:NOT_SECRET"},
			Env:        map[string]string{"ALICE_TOKEN": "env:ALICE_TOKEN", "LANG": "C"},
			DropParams: []string{"temperature"},
			ExtraBody: map[string]any{
				"mode":    "env:NOT_SECRET",
				"api_key": "env:BODY_KEY",
				"nested": map[string]any{
					"auth_token": "env:NESTED_TOKEN",
					"api_keys":   []any{"env:LIST_A", "literal", 3.0},
					"headers":    map[string]any{"Authorization": "env:HEADER"},
					"list":       []any{map[string]any{"password": "env:PW"}, "env:NOT_SECRET"},
				},
			},
			Enabled: true,
		}},
		Assignments: map[string]map[string][]Assignment{"alice": {"chat": {{Model: "m", Enabled: &on}}}},
	}
}

func TestResolveSecrets_ResolvesSecretPositionsOnly(t *testing.T) {
	var calls []string
	got, err := ResolveSecrets(secretsDocument(), fakeResolver(&calls))
	if err != nil {
		t.Fatal(err)
	}
	p, m := got.Providers[0], got.Models[0]
	nested := as[map[string]any](t, m.ExtraBody["nested"])
	list := as[[]any](t, nested["api_keys"])
	headers := as[map[string]any](t, nested["headers"])
	inner := as[[]any](t, nested["list"])
	innerMap := as[map[string]any](t, inner[0])
	checks := []struct {
		where string
		got   any
		want  any
	}{
		{"providers[0].api_key", p.APIKey, "value-of-GW_KEY"},
		{"providers[0].proxy", p.Proxy, "value-of-GW_PROXY"},
		{"providers[0].base_url", p.BaseURL, "env:NOT_SECRET"},
		{"providers[1].api_key", got.Providers[1].APIKey, "sk-literal"},
		{"models[0].model", m.Model, "env:NOT_SECRET"},
		{"models[0].workspace", m.Workspace, "env:NOT_SECRET"},
		{"models[0].extra_args", m.ExtraArgs[0], "env:NOT_SECRET"},
		{"models[0].env.ALICE_TOKEN", m.Env["ALICE_TOKEN"], "value-of-ALICE_TOKEN"},
		{"models[0].env.LANG", m.Env["LANG"], "C"},
		{"extra_body.mode", m.ExtraBody["mode"], "env:NOT_SECRET"},
		{"extra_body.api_key", m.ExtraBody["api_key"], "value-of-BODY_KEY"},
		{"extra_body.nested.auth_token", nested["auth_token"], "value-of-NESTED_TOKEN"},
		{"extra_body.nested.api_keys[0]", list[0], "value-of-LIST_A"},
		{"extra_body.nested.api_keys[1]", list[1], "literal"},
		{"extra_body.nested.api_keys[2]", list[2], 3.0},
		{"extra_body.nested.headers.Authorization", headers["Authorization"], "value-of-HEADER"},
		{"extra_body.nested.list[0].password", innerMap["password"], "value-of-PW"},
		{"extra_body.nested.list[1]", inner[1], "env:NOT_SECRET"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.where, c.got, c.want)
		}
	}
	for _, ref := range calls {
		if ref == "env:NOT_SECRET" || !IsSecretRef(ref) {
			t.Errorf("resolver called with %q", ref)
		}
	}
}

// Mutating anything reachable from the resolved copy never changes the input.
func TestResolveSecrets_ReturnsADeepCopy(t *testing.T) {
	in := secretsDocument()
	before, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	out, err := ResolveSecrets(in, fakeResolver(&calls))
	if err != nil {
		t.Fatal(err)
	}

	out.Providers[0].Name = "changed"
	out.Models[0].ExtraArgs[0] = "changed"
	out.Models[0].DropParams[0] = "changed"
	out.Models[0].Env["LANG"] = "changed"
	out.Models[0].Env["NEW"] = "added"
	nested := as[map[string]any](t, out.Models[0].ExtraBody["nested"])
	as[map[string]any](t, nested["headers"])["Authorization"] = "changed"
	as[map[string]any](t, as[[]any](t, nested["list"])[0])["password"] = "changed"
	as[[]any](t, nested["api_keys"])[1] = "changed"
	nested["added"] = true
	out.Models[0].ExtraBody["added"] = true
	*out.Assignments["alice"]["chat"][0].Enabled = false
	out.Assignments["alice"]["chat"][0].Model = "changed"
	out.Assignments["alice"]["new"] = nil
	out.Assignments["bob"] = nil

	after, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the input changed:\nbefore %s\nafter  %s", before, after)
	}
	if in.Providers[0].APIKey != "env:GW_KEY" {
		t.Errorf("the input's reference was replaced: %q", in.Providers[0].APIKey)
	}
}

func TestResolveSecrets_ErrorNamesTheLocation(t *testing.T) {
	doc := secretsDocument()
	doc.Models[0].Env["ALICE_TOKEN"] = "file:relative"
	var calls []string
	_, err := ResolveSecrets(doc, fakeResolver(&calls))
	if err == nil || !strings.HasPrefix(err.Error(), "models[0].env.ALICE_TOKEN: ") {
		t.Errorf("error = %v, want it to name models[0].env.ALICE_TOKEN", err)
	}
}

func TestResolveSecrets_DefaultResolver(t *testing.T) {
	t.Setenv("MODELSPEC_TEST_KEY", "alice-key")
	doc := Document{Version: 1, Providers: []Provider{{Name: "a", Protocol: "openai-chat", APIKey: "env:MODELSPEC_TEST_KEY"}}}
	got, err := ResolveSecrets(doc, DefaultResolver(0o600))
	if err != nil || got.Providers[0].APIKey != "alice-key" {
		t.Errorf("api_key = %q, %v; want alice-key", got.Providers[0].APIKey, err)
	}
	doc.Providers[0].APIKey = "env:MODELSPEC_TEST_UNSET"
	if _, err := ResolveSecrets(doc, DefaultResolver(0o600)); err == nil ||
		!strings.HasPrefix(err.Error(), "providers[0].api_key: ") {
		t.Errorf("error = %v, want it to name providers[0].api_key", err)
	}
}

func TestResolveSecrets_NilResolver(t *testing.T) {
	if _, err := ResolveSecrets(Document{Version: 1}, nil); err == nil {
		t.Error("a nil resolver was accepted")
	}
}
