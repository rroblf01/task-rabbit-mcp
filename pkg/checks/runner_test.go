package checks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rroblf01/task-rabbit-mcp/pkg/toolchain"
)

func check(name string, argv ...string) toolchain.Check {
	return toolchain.Check{Role: toolchain.Role(name), Command: argv, Source: "test"}
}

func TestRunReportsAPass(t *testing.T) {
	result := Run(context.Background(), check("test", "true"), Options{Dir: t.TempDir()})
	if !result.Passed {
		t.Errorf("`true` did not pass: %+v", result)
	}
	if !result.Ran || result.ExitCode != 0 {
		t.Errorf("ran/exit = %v/%d", result.Ran, result.ExitCode)
	}
}

func TestRunReportsAFailureWithItsOutput(t *testing.T) {
	result := Run(context.Background(),
		check("test", "sh", "-c", "echo 'FAILED: 3 assertions' >&2; exit 3"),
		Options{Dir: t.TempDir()})

	if result.Passed {
		t.Error("a non-zero exit was reported as a pass")
	}
	if result.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", result.ExitCode)
	}
	// The output is the whole point: a failing check that does not say why sends
	// the agent back to running it by hand.
	if !strings.Contains(result.Output, "FAILED: 3 assertions") {
		t.Errorf("output = %q", result.Output)
	}
}

// A missing tool is not a failing check. They call for opposite responses: one is
// "fix the code", the other is "install the tool or find another one".
func TestRunDistinguishesAMissingToolFromAFailure(t *testing.T) {
	result := Run(context.Background(),
		check("lint", "definitely-not-installed-ruff"), Options{Dir: t.TempDir()})

	if result.Passed {
		t.Error("a command that never ran was reported as a pass")
	}
	if result.Ran {
		t.Error("ran = true for a command that could not start")
	}
	if result.Error == "" {
		t.Error("no explanation of why the check could not run")
	}
}

func TestRunUsesTheGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := Run(context.Background(), check("test", "ls"), Options{Dir: dir})
	if !result.Passed {
		t.Fatalf("ls in a temp dir failed: %+v", result)
	}
	if !strings.Contains(result.Output, "marker.txt") {
		t.Errorf("the check did not run in %s; output was %q", dir, result.Output)
	}
}

// No shell anywhere: the argv is the command, so a repository's own text cannot
// become part of what runs.
func TestRunDoesNotUseAShell(t *testing.T) {
	result := Run(context.Background(),
		check("test", "sh", "-c", "echo hello; echo world"), Options{Dir: t.TempDir()})
	// The two echoes ran inside sh because the caller asked for sh explicitly.
	// What must not happen is the argv being re-split or joined into one string
	// that some shell then interprets.
	if !strings.Contains(result.Output, "hello") || !strings.Contains(result.Output, "world") {
		t.Errorf("output = %q", result.Output)
	}

	// A single argument with a semicolon in it must stay one argument.
	result = Run(context.Background(),
		check("test", "echo", "a; rm -rf /"), Options{Dir: t.TempDir()})
	if !result.Passed || !strings.Contains(result.Output, "a; rm -rf /") {
		t.Errorf("an argument was re-interpreted: %q", result.Output)
	}
}

func TestRunTimesOutAndSaysSo(t *testing.T) {
	result := Run(context.Background(), check("test", "sleep", "30"),
		Options{Dir: t.TempDir(), Timeout: 150 * time.Millisecond})

	if result.TimedOut {
		// The important part: a timeout is not a verdict on the code.
		if result.Passed {
			t.Error("a killed check was reported as a pass")
		}
		if !strings.Contains(result.Error, "TASK_RABBIT_CHECK_TIMEOUT") {
			t.Errorf("the timeout error should name the knob: %q", result.Error)
		}
		return
	}
	t.Skip("sleep 30 finished before the timeout on a very slow machine; nothing to assert")
}

func TestRunCapsOutputKeepingBothEnds(t *testing.T) {
	// A long head, a marker in the middle, and a marker at the end.
	middle := strings.Repeat("h", 3000)

	result := Run(context.Background(),
		check("test", "sh", "-c", "echo "+middle+" MIDDLEMARKER PANIC at the end"),
		Options{Dir: t.TempDir(), OutputLimit: 500})

	if !result.Truncated {
		t.Fatal("a long output was not truncated")
	}
	if len(result.Output) > 700 {
		t.Errorf("capped output is %d bytes, want roughly the cap", len(result.Output))
	}
	// Both ends survive: the head explains what was running, the tail is where a
	// failure actually is.
	if !strings.Contains(result.Output, "PANIC at the end") {
		t.Errorf("the end of the output was dropped, which is where failures are:\n%s", result.Output)
	}
	if !strings.Contains(result.Output, "omitted") {
		t.Errorf("the cap was silent; a reader cannot tell output was dropped:\n%s", result.Output)
	}
}

func TestTruncate(t *testing.T) {
	text := strings.Repeat("é", 500) // two bytes per rune
	got, truncated := Truncate(text, 100)
	if !truncated {
		t.Fatal("no truncation reported")
	}
	// Cutting mid-rune puts a replacement character in the middle of the text,
	// which reads as corruption rather than as a cap.
	for _, r := range got {
		if r == '�' {
			t.Fatalf("the cut split a rune:\n%q", got)
		}
	}
}

func TestRunDropsThisServersCredentialsFromTheEnvironment(t *testing.T) {
	// When this server is configured with a default instance its API key is in the
	// environment, and a test that prints its environment writes it into a CI log.
	t.Setenv("TASK_RABBIT_API_KEY", "super-secret-key")
	t.Setenv("TASK_RABBIT_URL", "https://jirrabit.example.com")
	t.Setenv("JIRRABIT_API_KEY", "another-secret")

	result := Run(context.Background(),
		check("test", "sh", "-c", "env"), Options{Dir: t.TempDir()})

	for _, secret := range []string{"super-secret-key", "another-secret"} {
		if strings.Contains(result.Output, secret) {
			t.Errorf("a check inherited %q, which a printing test would put in a log", secret)
		}
	}
	// But the environment is inherited, not curated: a real build needs PATH, HOME
	// and whatever else the project depends on.
	if !strings.Contains(result.Output, "PATH=") {
		t.Error("PATH was filtered out of the check's environment")
	}
}

func TestRunHandlesAnEmptyCommand(t *testing.T) {
	result := Run(context.Background(),
		toolchain.Check{Role: toolchain.RoleTest}, Options{Dir: t.TempDir()})
	if result.Ran || result.Passed || result.Error == "" {
		t.Errorf("an empty command was not reported as unrunnable: %+v", result)
	}
}

func TestSummarizeNamesEachVerdict(t *testing.T) {
	results := []Result{
		{Command: "make lint", Passed: true, Duration: "1s"},
		{Command: "make test", ExitCode: 1, Ran: true, Duration: "12s"},
		{Command: "make build", TimedOut: true, Duration: "10m"},
		{Command: "ty check", Error: "not installed", Duration: "1ms"},
	}
	summary := Summarize(results)

	for _, want := range []string{"PASS", "FAIL", "TIMEOUT", "NOTRUN", "make lint", "ty check"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary is missing %q:\n%s", want, summary)
		}
	}
}

// `gofmt -l` prints the files it would change and exits 0 either way. Judged on
// exit status, a repository with nine unformatted files reports "passed" — which
// is the failure this test exists to prevent.
func TestRunTreatsOutputAsTheVerdictWhenTheCheckSaysSo(t *testing.T) {
	t.Run("empty output passes", func(t *testing.T) {
		result := Run(context.Background(),
			toolchain.Check{Role: toolchain.RoleFormat, Command: []string{"true"}, NonEmptyOutputFails: true},
			Options{Dir: t.TempDir()})
		if !result.Passed {
			t.Errorf("no output should pass: %+v", result)
		}
		if result.Verdict == "" {
			t.Error("the verdict should say it came from the output, not the exit code")
		}
	})

	t.Run("any output fails", func(t *testing.T) {
		result := Run(context.Background(),
			toolchain.Check{Role: toolchain.RoleFormat, Command: []string{"echo", "main.go"},
				NonEmptyOutputFails: true},
			Options{Dir: t.TempDir()})
		if result.Passed {
			t.Error("a formatter check that printed a filename reported a pass")
		}
		if !result.Ran || result.ExitCode != 0 {
			t.Errorf("ran/exit = %v/%d; the command itself did succeed", result.Ran, result.ExitCode)
		}
		if !strings.Contains(result.Verdict, "is the finding") {
			t.Errorf("the verdict should explain itself: %q", result.Verdict)
		}
	})

	t.Run("the cap is not mistaken for output", func(t *testing.T) {
		// A truncated output begins with the ellipsis line. That must not count as
		// the command having printed something.
		result := Run(context.Background(),
			toolchain.Check{Role: toolchain.RoleFormat, Command: []string{"true"}, NonEmptyOutputFails: true},
			Options{Dir: t.TempDir(), OutputLimit: 10})
		if !result.Passed {
			t.Errorf("the truncation marker was read as a finding: %+v", result)
		}
	})
}
