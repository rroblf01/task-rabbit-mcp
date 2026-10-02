package branch

import (
	"strings"
	"unicode"
)

// Branch naming.
//
// The server proposes and the user decides. The proposals exist so the question
// put to the user is concrete — "demo-679-fix-login-timeout, or
// demo-679-login-timeout?" — rather than an open "what should I call it?" that
// the user has to invent from nothing.
//
// Everything here is deterministic and derived only from the issue, so two runs of
// the same issue propose the same names. A suggestion that changed between calls
// would be worse than none.

// maxSlugWords caps how much of the summary reaches a branch name.
//
// Git hosts and CI systems have their own limits on branch name length, and the
// ones that exist are unpredictable: a 100-character name is fine locally and
// truncated in a web UI. Forty characters of slug leaves room for the key prefix,
// the separator and a host's own suffix.
const maxSlugWords = 6

// SuggestBranchNames proposes branch names for an issue, best first.
//
// key is the issue key as jirrabit stores it ("DEMO-679"), summary is its
// summary. Every proposal carries the key as its prefix, because a branch that
// cannot be traced back to an issue from its name alone is a branch nobody will
// find again.
func SuggestBranchNames(key, summary string) []string {
	key = strings.TrimSpace(key)
	slug := slugify(summary)
	if slug == "" {
		if key == "" {
			return nil
		}
		return []string{slugify(key)}
	}

	prefix := slugify(key)
	if prefix == "" {
		return []string{slug}
	}

	candidates := []string{prefix + "-" + slug}
	// A shorter second option, for a summary that reads as a sentence rather than
	// a title. Offered alongside the full one rather than instead of it, because
	// "pick one of these" is a question a person answers and "here is the only
	// name" is a decision imposed.
	if words := strings.Split(slug, "-"); len(words) > 2 {
		if short := strings.Join(words[:2], "-"); short != "" && short != slug {
			candidates = append(candidates, prefix+"-"+short)
		}
	}
	// Without the key, for a repository whose branch naming already carries the
	// issue number, or whose host shows the branch name in a narrow column.
	candidates = append(candidates, slug)

	return dedupe(candidates)
}

// slugify turns a sentence into a branch-name fragment.
//
// Lowercase, ASCII, hyphen-separated, no leading or trailing hyphen, no runs of
// hyphens. Accented characters are folded rather than dropped, so "migración"
// becomes "migracion" rather than "migracin".
func slugify(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	lastHyphen := true // true, so a leading hyphen is never written

	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		switch {
		case r > unicode.MaxASCII:
			if folded := foldAccent(r); folded != 0 {
				r = folded
			} else {
				// A character with no ASCII equivalent is a separator, not a
				// letter: "añadir — prueba" must not become "adirprueba".
				if !lastHyphen && b.Len() > 0 {
					b.WriteByte('-')
					lastHyphen = true
				}
				continue
			}
		}
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			// Every separator collapses into one hyphen.
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}

	out := strings.Trim(b.String(), "-")
	if words := strings.Split(out, "-"); len(words) > maxSlugWords {
		out = strings.Join(words[:maxSlugWords], "-")
	}
	return out
}

// foldAccent maps the accented letters that appear in Spanish and Portuguese issue
// summaries onto ASCII, and returns 0 for anything it does not know.
//
// Hand-written rather than pulled from golang.org/x/text: this repository takes
// one dependency, and a table of sixteen letters is not worth a second module.
// The upper-case forms are absent on purpose, because slugify lowercases before it
// gets here, and a table that listed both would look like it had been tested.
func foldAccent(r rune) rune {
	switch r {
	case 'á', 'à', 'â', 'ä':
		return 'a'
	case 'é', 'è', 'ê':
		return 'e'
	case 'í', 'ì', 'î':
		return 'i'
	case 'ó', 'ò', 'ô':
		return 'o'
	case 'ú', 'ù', 'û':
		return 'u'
	case 'ü':
		return 'u'
	case 'ñ':
		return 'n'
	case 'ç':
		return 'c'
	}
	return 0
}

func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
