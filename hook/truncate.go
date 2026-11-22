package hook

// Truncator shortens text the engine logs (the content of the request it
// logs when content may be logged) to at most limit runes; the engine
// chooses limit. A host that wants the full text passes one that returns s.
// It has the shape of toolkit.WithTruncate's function, whose default rule
// differs (it appends "…" past the limit). nil means Truncate.
type Truncator func(s string, limit int) string

// Truncate is the default Truncator: s when it has at most limit runes,
// otherwise its first limit-3 runes and "..." (its first limit runes when
// limit is 3 or less); "" when limit is not positive.
func Truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}
