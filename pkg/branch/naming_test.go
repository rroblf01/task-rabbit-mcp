package branch

import "testing"

func TestSuggestBranchNamesLeadsWithTheKey(t *testing.T) {
	// A branch that cannot be traced back to an issue from its name alone is one
	// nobody finds again, so the key-prefixed name is the FIRST proposal rather
	// than one option among several. The keyless variant exists for a repository
	// that already carries the number, and is deliberately last.
	names := SuggestBranchNames("DEMO-679", "Login times out on slow networks")
	if len(names) == 0 {
		t.Fatal("no proposals for a normal issue")
	}
	if got, want := names[0], "demo-679-login-times-out-on-slow-networks"; got != want {
		t.Errorf("first proposal = %q, want %q", got, want)
	}
}

func TestSuggestBranchNamesIsDeterministic(t *testing.T) {
	// A suggestion that changed between two calls for the same issue would be
	// worse than none: the user would pick a name that then turned out not to be
	// the one they were shown.
	first := SuggestBranchNames("WEB-1", "Add a login form")
	second := SuggestBranchNames("WEB-1", "Add a login form")
	if len(first) != len(second) {
		t.Fatalf("proposal count changed between calls: %v vs %v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("proposal %d changed between calls: %q vs %q", i, first[i], second[i])
		}
	}
}

func TestSuggestBranchNames(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		summary string
		want    []string
	}{
		{
			name:    "accents are folded rather than dropped",
			key:     "DEMO-1",
			summary: "Migración de la base de datos",
			// "migracin" would be what a naive filter produces, and it is not a word.
			want: []string{
				"demo-1-migracion-de-la-base-de-datos",
				"demo-1-migracion-de",
				"migracion-de-la-base-de-datos",
			},
		},
		{
			name:    "punctuation collapses into single hyphens",
			key:     "DEMO-2",
			summary: "Fix: the /api/v1 endpoint (again!)",
			want: []string{
				"demo-2-fix-the-api-v1-endpoint-again",
				"demo-2-fix-the",
				"fix-the-api-v1-endpoint-again",
			},
		},
		{
			name:    "a short summary gets one proposal",
			key:     "OPS-9",
			summary: "Typo",
			want:    []string{"ops-9-typo", "typo"},
		},
		{
			name:    "no key falls back to the slug alone",
			key:     "",
			summary: "Some work",
			want:    []string{"some-work"},
		},
		{
			name:    "a summary with nothing sluggable still gets the key",
			key:     "DEMO-3",
			summary: "¿¿¿",
			want:    []string{"demo-3"},
		},
		{
			name:    "both empty yields nothing rather than a bare hyphen",
			key:     "",
			summary: "",
			want:    nil,
		},
		{
			name:    "a very long summary is capped",
			key:     "DEMO-4",
			summary: "one two three four five six seven eight nine ten eleven twelve",
			want: []string{
				"demo-4-one-two-three-four-five-six",
				"demo-4-one-two",
				"one-two-three-four-five-six",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SuggestBranchNames(test.key, test.summary)
			if len(got) != len(test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Errorf("proposal %d = %q, want %q (all: %v)", i, got[i], test.want[i], got)
				}
			}
		})
	}
}

func TestSlugifyNeverEmitsWhatGitWouldRefuse(t *testing.T) {
	// Every one of these is rejected by git check-ref-format, which is the
	// authority the branch tool defers to. Catching them here means the
	// suggestion is not a name the user picks and then cannot use.
	forbidden := []string{"~", "^", ":", "?", "*", "[", "\\", "..", "@{"}

	for _, summary := range []string{
		"Fix ~broken^ thing",
		"weird: name?",
		"glob* and [brackets]",
		"back\\slash",
		"double..dots",
		"@{ref}",
		"ends with a dot.",
		"  leading and trailing  ",
	} {
		slug := slugify(summary)
		for _, bad := range forbidden {
			if containsAny(slug, bad) {
				t.Errorf("slug %q from %q contains the git-forbidden %q", slug, summary, bad)
			}
		}
		if slug == "" {
			continue
		}
		if slug[0] == '-' || slug[0] == '.' || slug[len(slug)-1] == '-' || slug[len(slug)-1] == '.' {
			t.Errorf("slug %q from %q starts or ends with a separator", slug, summary)
		}
		if containsAny(slug, "--") {
			t.Errorf("slug %q from %q has a run of hyphens", slug, summary)
		}
	}
}

func containsAny(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
