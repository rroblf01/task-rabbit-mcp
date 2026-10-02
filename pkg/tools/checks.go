package tools

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/rroblf01/task-rabbit-mcp/pkg/checks"
	"github.com/rroblf01/task-rabbit-mcp/pkg/schema"
	"github.com/rroblf01/task-rabbit-mcp/pkg/toolchain"
)

func registerCheckTools(s *registrar, d Deps) {
	s.AddTool(mcp.NewTool("listTaskChecks",
		mcp.WithDescription(
			"List the checks this project declares, discovered from the repository itself: Makefile targets, "+
				"package.json scripts, ruff/mypy/pytest configuration, go.mod, Cargo.toml and the JVM wrappers.\n\n"+
				"Call this after loadTaskContext and before editing anything. It answers two questions at once: "+
				"how this project is expected to be validated, and whether it can be validated at all. A "+
				"repository with no checks is worth knowing about before the work starts rather than after.\n\n"+
				"Roles are fixed across every project — format, lint, typecheck, test, build, check — so "+
				"\"the lint\" is the same request whatever language the project is in, and runTaskChecks takes "+
				"the same names. Commands are what differ, and each result names the file that declared it.\n\n"+
				"readOnly in the sense that matters: nothing is executed. The repository's files are read and "+
				"nothing is run, so this cannot invoke a project's own tooling by accident."),
		mcp.WithTitleAnnotation("List task checks"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithInputSchema[schema.ListChecksArgs](),
	), listTaskChecks(d))

	s.AddTool(mcp.NewTool("runTaskChecks",
		mcp.WithDescription(
			"Run this project's declared checks and report each verdict. Takes roles (\"lint\", \"test\", \"check\") "+
				"rather than commands: there is no way to hand this server a command to run, because it only "+
				"ever runs what the project declares.\n\n"+
				"A check that passes says so, one that fails reports its exit code and its output, one whose "+
				"tool is not installed says that instead — which is a different problem with a different fix — "+
				"and one that runs past the timeout says so rather than hanging.\n\n"+
				"A check marked writeMode is a formatter that rewrites files. It is skipped unless fix is true, "+
				"because you are usually mid-edit and an unasked formatter changes work in progress. Output is "+
				"capped, keeping the beginning and the end, because a failure is at the end of a test log.\n\n"+
				"There is no shell: the command is an argument vector, so nothing in the repository's text can "+
				"change what runs. This is also why \"check\" — the project's own aggregate target — may do more "+
				"than the other roles; prefer the individual roles when you need to know what failed."),
		mcp.WithTitleAnnotation("Run task checks"),
		mcp.WithReadOnlyHintAnnotation(false), // it executes the project's tooling
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithInputSchema[schema.RunChecksArgs](),
	), runTaskChecks(d))
}

func listTaskChecks(d Deps) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args schema.ListChecksArgs
		if err := req.BindArguments(&args); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		repo, err := d.resolveRepo(args.ProjectKey, args.RepoPath)
		if err != nil {
			return toolErrorf("%v", err)
		}
		tc, err := detectFor(repo)
		if err != nil {
			return toolErrorf("%v", err)
		}

		report := checkReport{
			Repository:     repo,
			Checks:         tc.Ordered(),
			AlsoAvailable:  tc.AlsoAvailable,
			Signals:        tc.Signals,
			Roles:          roleNames(),
			RepositoryKind: kindOf(repo.Path),
		}
		if !hasCheck(tc) {
			report.Note = noChecksNote()
		}
		return jsonResult(report)
	}
}

func runTaskChecks(d Deps) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args schema.RunChecksArgs
		if err := req.BindArguments(&args); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		repo, err := d.resolveRepo(args.ProjectKey, args.RepoPath)
		if err != nil {
			return toolErrorf("%v", err)
		}
		if len(args.Roles) == 0 {
			return toolErrorf(
				"roles is required, for example [\"lint\"] or [\"lint\",\"test\"]. Pass \"check\" to run the " +
					"project's aggregate target. Call listTaskChecks to see what this repository declares")
		}
		tc, err := detectFor(repo)
		if err != nil {
			return toolErrorf("%v", err)
		}
		if !hasCheck(tc) {
			return toolErrorf("%s", noChecksNote())
		}

		report := checkRunReport{
			Repository: repo,
			Fix:        args.Fix,
			Results:    []checks.Result{},
			Skipped:    []skippedCheck{},
		}

		for _, requested := range args.Roles {
			role := toolchain.Role(strings.ToLower(strings.TrimSpace(requested)))
			check, known := tc.Checks[role]
			if !known {
				report.Unknown = append(report.Unknown, unknownRole(role, tc))
				continue
			}
			if check.WriteMode && !args.Fix {
				// Skipped rather than refused: the caller asked for a set of roles and
				// this is what happened to one of them. Silently dropping it would
				// read as a pass.
				report.Skipped = append(report.Skipped, skippedCheck{
					Role:    string(role),
					Command: check.CommandLine(),
					Reason: "this check rewrites files, so it only runs when fix is true. Pass fix: true if " +
						"you want it to rewrite the working tree",
				})
				continue
			}
			logCall("runTaskChecks", fmt.Sprintf("%s in %s", role, repo.Path))
			report.Results = append(report.Results, checks.Run(ctx, check, checks.Options{
				Dir:         repo.Path,
				Timeout:     checkTimeout(),
				OutputLimit: outputLimit(),
			}))
		}

		report.Summary = checks.Summarize(report.Results)
		report.AllPassed = len(report.Results) > 0 && allPassed(report.Results)
		if len(report.Results) == 0 && len(report.Skipped) == 0 {
			report.Summary = "nothing ran: every requested role was skipped or unknown"
		}
		return jsonResult(report)
	}
}

type checkRunReport struct {
	Repository resolvedRepo    `json:"repository"`
	Summary    string          `json:"summary"`
	AllPassed  bool            `json:"allPassed"`
	Results    []checks.Result `json:"results"`
	Skipped    []skippedCheck  `json:"skipped,omitempty"`
	Unknown    []string        `json:"unknownRoles,omitempty"`
	Fix        bool            `json:"fix"`
}

type skippedCheck struct {
	Role    string `json:"role"`
	Command string `json:"command"`
	Reason  string `json:"reason"`
}

// unknownRole names what is available, because "unknown role: lint" sends an agent
// back to the same call.
func unknownRole(role toolchain.Role, tc *toolchain.Toolchain) string {
	available := make([]string, 0, len(tc.Checks))
	for _, check := range tc.Ordered() {
		available = append(available, string(check.Role))
	}
	if len(available) == 0 {
		return fmt.Sprintf("%q is not a role, and this repository declares no checks at all", role)
	}
	return fmt.Sprintf("%q is not a role, or this project declares none for it. This repository has: %s",
		role, strings.Join(available, ", "))
}

func allPassed(results []checks.Result) bool {
	for _, result := range results {
		if !result.Passed {
			return false
		}
	}
	return len(results) > 0
}

func roleNames() []string {
	names := make([]string, 0, len(toolchain.AllRoles))
	for _, role := range toolchain.AllRoles {
		names = append(names, string(role))
	}
	return names
}

// kindOf says what is at the path, so a caller that passed a wrong repoPath finds
// out from this rather than from a confusing failure later.
func kindOf(path string) string {
	if _, err := os.Stat(path); err != nil {
		return "missing"
	}
	if _, err := os.Stat(path + "/.git"); err == nil {
		return "git repository"
	}
	return "directory"
}

func checkTimeout() time.Duration {
	return durationFromEnv("TASK_RABBIT_CHECK_TIMEOUT", checks.DefaultTimeout)
}

func outputLimit() int {
	raw := strings.TrimSpace(os.Getenv("TASK_RABBIT_OUTPUT_LIMIT"))
	if raw == "" {
		return checks.DefaultOutputLimit
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return checks.DefaultOutputLimit
	}
	return limit
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	// Seconds, because that is what every other limit in this server uses and a
	// duration string would be one more format to remember.
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
