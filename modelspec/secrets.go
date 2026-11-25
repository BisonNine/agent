/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package modelspec

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// Secret references.
//
// A secret-bearing string in a Document may be written as a reference instead
// of a literal: "env:NAME" names the environment variable NAME, and
// "file:/abs/path" names a file whose contents, trimmed of surrounding
// whitespace, are the value. ResolveSecrets replaces references with values in
// a copy of the document, so the document the application saves keeps the
// references.
//
// The secret positions are a provider's api_key and proxy, the values of a
// model's env, and, inside a model's extra_body, any field named like a
// credential ("api_key", "…_api_key", "token", "…_token", "secret",
// "…_secret", "password", "…_password", "password_hash"), a proxy ("proxy",
// "…_proxy"), a credential list ("api_keys", "…_api_keys") or a credential
// map ("env", "headers"). A reference anywhere else is a literal.

// SecretRefEnvPrefix and SecretRefFilePrefix introduce the two reference forms.
const (
	SecretRefEnvPrefix  = "env:"
	SecretRefFilePrefix = "file:"
)

// IsSecretRef reports whether s is a secret reference rather than a literal.
func IsSecretRef(s string) bool {
	return strings.HasPrefix(s, SecretRefEnvPrefix) || strings.HasPrefix(s, SecretRefFilePrefix)
}

// Resolver returns the value a secret reference stands for. ResolveSecrets
// calls it only with strings for which IsSecretRef is true.
type Resolver func(ref string) (string, error)

// DefaultResolver returns the resolver for "env:NAME" and "file:/abs/path":
// the first reads the environment variable NAME, which must be set; the
// second reads the file, which must be named by an absolute path, must not be
// a directory and, except on Windows, must grant no permission bit beyond
// maxFileMode (0600 is the usual choice). The file's contents are trimmed of
// surrounding whitespace.
func DefaultResolver(maxFileMode fs.FileMode) Resolver {
	allowed := maxFileMode.Perm()
	return func(ref string) (string, error) {
		return resolveDefault(ref, allowed)
	}
}

func resolveDefault(ref string, allowed fs.FileMode) (string, error) {
	if name, ok := strings.CutPrefix(ref, SecretRefEnvPrefix); ok {
		name = strings.TrimSpace(name)
		if name == "" {
			return "", fmt.Errorf("secret reference %q names no environment variable", ref)
		}
		v, set := os.LookupEnv(name)
		if !set {
			return "", fmt.Errorf("secret reference %q: environment variable %s is not set", ref, name)
		}
		return v, nil
	}
	path, ok := strings.CutPrefix(ref, SecretRefFilePrefix)
	if !ok {
		return "", fmt.Errorf("%q is not a secret reference", ref)
	}
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("secret reference %q: the path must be absolute", ref)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("secret reference %q: %w", ref, err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("secret reference %q: %s is a directory", ref, path)
	}
	if runtime.GOOS != "windows" {
		if mode := fi.Mode().Perm(); mode&^allowed != 0 {
			return "", fmt.Errorf("secret reference %q: %s has mode %04o, more than the %04o allowed; run: chmod %03o %s",
				ref, path, mode, allowed, allowed, path)
		}
	}
	data, err := os.ReadFile(path) //nolint:gosec // the document names this file
	if err != nil {
		return "", fmt.Errorf("secret reference %q: %w", ref, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ResolveSecrets returns a copy of doc with every secret reference replaced by
// the value resolve returns for it. doc itself is not changed, and the copy
// shares no slice, map or pointer with it. An error names the location of the
// reference that failed ("providers[0].api_key").
func ResolveSecrets(doc Document, resolve Resolver) (Document, error) {
	if resolve == nil {
		return Document{}, errors.New("resolve secrets: no resolver")
	}
	out := doc.clone()
	r := secretWalker{resolve: resolve}
	for i := range out.Providers {
		p := &out.Providers[i]
		base := "providers[" + strconv.Itoa(i) + "]"
		p.APIKey = r.visit(base+".api_key", p.APIKey)
		p.Proxy = r.visit(base+".proxy", p.Proxy)
	}
	for i := range out.Models {
		m := &out.Models[i]
		base := "models[" + strconv.Itoa(i) + "]"
		for _, k := range slices.Sorted(maps.Keys(m.Env)) {
			m.Env[k] = r.visit(base+".env."+k, m.Env[k])
		}
		r.walk(m.ExtraBody, base+".extra_body")
	}
	if r.err != nil {
		return Document{}, r.err
	}
	return out, nil
}

// secretWalker resolves the references it visits and keeps the first error.
type secretWalker struct {
	resolve Resolver
	err     error
}

func (r *secretWalker) visit(path, s string) string {
	if r.err != nil || !IsSecretRef(s) {
		return s
	}
	v, err := r.resolve(s)
	if err != nil {
		r.err = fmt.Errorf("%s: %w", path, err)
		return s
	}
	return v
}

// walk visits every string in a secret position below a decoded JSON value,
// in key order so the first error is the same every time.
func (r *secretWalker) walk(doc any, path string) {
	switch t := doc.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			val := t[k]
			p := path + "." + k
			switch {
			case isSecretKey(k) || isProxyKey(k):
				if s, ok := val.(string); ok {
					t[k] = r.visit(p, s)
					continue
				}
			case isSecretListKey(k):
				if list, ok := val.([]any); ok {
					for i, item := range list {
						if s, ok := item.(string); ok {
							list[i] = r.visit(p+"["+strconv.Itoa(i)+"]", s)
						}
					}
					continue
				}
			case isSecretMapKey(k):
				if m, ok := val.(map[string]any); ok {
					for _, mk := range slices.Sorted(maps.Keys(m)) {
						if s, ok := m[mk].(string); ok {
							m[mk] = r.visit(p+"."+mk, s)
						}
					}
					continue
				}
			}
			r.walk(val, p)
		}
	case []any:
		for i, item := range t {
			r.walk(item, path+"["+strconv.Itoa(i)+"]")
		}
	}
}

// isSecretKey reports whether a JSON field name denotes a credential.
func isSecretKey(key string) bool {
	k := strings.ToLower(key)
	switch {
	case k == "api_key" || strings.HasSuffix(k, "_api_key"):
		return true
	case k == "token" || strings.HasSuffix(k, "_token"):
		return true
	case k == "secret" || strings.HasSuffix(k, "_secret"):
		return true
	case k == "password" || strings.HasSuffix(k, "_password") || k == "password_hash":
		return true
	default:
		return false
	}
}

// isSecretListKey reports whether a JSON field holds a list of credentials.
func isSecretListKey(key string) bool {
	k := strings.ToLower(key)
	return k == "api_keys" || strings.HasSuffix(k, "_api_keys")
}

// isSecretMapKey reports whether every value of a JSON object field is a
// credential.
func isSecretMapKey(key string) bool {
	k := strings.ToLower(key)
	return k == "env" || k == "headers"
}

// isProxyKey reports whether a JSON field is a proxy URL, which may embed
// credentials as userinfo.
func isProxyKey(key string) bool {
	k := strings.ToLower(key)
	return k == "proxy" || strings.HasSuffix(k, "_proxy")
}
