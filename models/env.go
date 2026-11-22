package models

import (
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
)

// The default allowlist of a CLI's environment. A variable is kept when its
// name is listed or begins with a listed prefix.
//
//   - PATH, HOME, USER, LOGNAME, SHELL, LANG, LC_*, TERM, TMPDIR, TZ: what a
//     program needs to find binaries, its dotfiles and its locale.
//   - XDG_*: where many CLIs keep their configuration and state.
//   - SSL_CERT_FILE, SSL_CERT_DIR: a custom CA bundle.
//   - the proxy variables, in both cases.
//   - the login and key variables of the CLIs themselves (ANTHROPIC_,
//     OPENAI_, GOOGLE_, GEMINI_, CLAUDE_, CODEX_, CURSOR_, AGY_,
//     ANTIGRAVITY_).
//   - on macOS, what launchd sets for Foundation-based tools; on Windows, the
//     system variables any executable relies on.
var (
	envNames = []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TERM", "TMPDIR", "TZ",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
		"http_proxy", "https_proxy", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	}
	envPrefixes = []string{
		"LC_", "XDG_",
		"ANTHROPIC_", "OPENAI_", "GOOGLE_", "GEMINI_", "CLAUDE_", "CODEX_", "CURSOR_", "AGY_", "ANTIGRAVITY_",
	}
	envDarwin  = []string{"__CF_USER_TEXT_ENCODING", "XPC_FLAGS", "XPC_SERVICE_NAME"}
	envWindows = []string{
		"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP",
		"USERPROFILE", "USERNAME", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
		"PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)",
	}
)

// DefaultBaseEnv returns the allowlisted part of the process environment, in
// "K=V" form, sorted: the environment a CLI client starts from unless
// WithBaseEnv says otherwise.
func DefaultBaseEnv() []string {
	return filterEnv(os.Environ(), runtime.GOOS)
}

func filterEnv(environ []string, goos string) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if keepEnv(k, goos) {
			out = append(out, kv)
		}
	}
	sort.Strings(out)
	return out
}

func keepEnv(k, goos string) bool {
	switch goos {
	case "darwin":
		if slices.Contains(envDarwin, k) {
			return true
		}
	case "windows":
		k = strings.ToUpper(k) // Windows names are case-insensitive ("Path")
		if slices.Contains(envWindows, k) {
			return true
		}
	}
	if slices.Contains(envNames, k) {
		return true
	}
	for _, p := range envPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}
