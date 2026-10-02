package repos

import "testing"

func TestParseEmptyIsNotAnError(t *testing.T) {
	// No mapping is the default case, and it is not a configuration error.
	for _, raw := range []string{"", "   ", "\t"} {
		index, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if len(index.Projects()) != 0 {
			t.Errorf("Parse(%q) produced %v", raw, index.Projects())
		}
	}
	if got := (&Index{}).Describe(); got == "" {
		t.Error("Describe must say something when there is no mapping; that line is in the start-up log")
	}
}

func TestParseMapsProjectsToPaths(t *testing.T) {
	index, err := Parse(`{"WEB":"/home/you/code/web", "OPS":"/home/you/code/ops"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for project, want := range map[string]string{"WEB": "/home/you/code/web", "OPS": "/home/you/code/ops"} {
		got, ok := index.Lookup(project)
		if !ok || got != want {
			t.Errorf("Lookup(%s) = %q, %v", project, got, ok)
		}
	}
	// Case-insensitive, because jirrabit issue keys are upper case and nobody types
	// them that way in a path.
	if got, ok := index.Lookup("web"); !ok || got != "/home/you/code/web" {
		t.Errorf("Lookup(web) = %q, %v", got, ok)
	}
	if _, ok := index.Lookup("NOPE"); ok {
		t.Error("an unmapped project resolved")
	}
}

// JSON rather than KEY=path,KEY2=path2: a malformed value must be one clear error
// rather than a silently empty index that then sends every call to the current
// directory — which looks like a bug in something else entirely.
func TestParseRejectsMalformedValues(t *testing.T) {
	for _, raw := range []string{
		`WEB=/home/you/code/web`, // the flat format somebody will try
		`{`,                      // truncated
		`{"WEB": }`,              // not an object of strings
		`["WEB", "/x"]`,          // an array
		`{"": "/x"}`,             // no project key
		`{"WEB": ""}`,            // no path
		`{"WEB": "   "}`,         // whitespace is not a path
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) was accepted", raw)
		}
	}
}

func TestParseErrorNamesTheFormat(t *testing.T) {
	_, err := Parse(`WEB=/x`)
	if err == nil {
		t.Fatal("expected an error")
	}
	// An operator who typed the wrong format needs to see what it should have been.
	for _, want := range []string{"TASK_RABBIT_REPOS", "project key", "local path"} {
		if !contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
}

func TestPathsAreCleaned(t *testing.T) {
	index, err := Parse(`{"WEB":"/home/you/code/../code/web/"}`)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := index.Lookup("WEB")
	if got != "/home/you/code/web" {
		t.Errorf("path = %q, want it cleaned", got)
	}
}

func TestDescribeIsSortableAndComplete(t *testing.T) {
	// The line goes into the start-up log, and its whole purpose is letting an
	// operator see at a glance that the mapping took effect. "2 projects mapped"
	// would not catch a typo in one of them, and map iteration order would make the
	// log differ between runs.
	index, err := Parse(`{"OPS":"/o","WEB":"/w","API":"/a"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := index.Describe(), "API=/a, OPS=/o, WEB=/w"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
	if got := index.Projects(); len(got) != 3 || got[0] != "API" || got[2] != "WEB" {
		t.Errorf("Projects() = %v", got)
	}
}

func TestLookupOnANilIndex(t *testing.T) {
	// A Deps built without an index must not panic; "no mapping" is a valid setup.
	var index *Index
	if _, ok := index.Lookup("WEB"); ok {
		t.Error("a nil index resolved a project")
	}
	if got := index.Describe(); got == "" {
		t.Error("a nil index must still describe itself")
	}
	if got := index.Projects(); got != nil {
		t.Errorf("Projects() on nil = %v", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
