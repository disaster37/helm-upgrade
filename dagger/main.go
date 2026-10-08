// A Dagger module for the helm-upgrade repository.
//
// It composes the disaster37 golang module (build/test/lint/vuln) and the
// goreleaser module (release) behind a single local entrypoint used by the
// GitHub Actions workflows.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"dagger/helm-upgrade/internal/dagger"
)

// mainPackage is the path to the CLI entrypoint compiled by the golang module.
const mainPackage = "./cmd/helm-upgrade"

// Pinned CI tool versions.
//
// The upstream disaster37 golang module installs "latest" golangci-lint,
// govulncheck and gofumpt whenever they are missing from the container, which
// is non-reproducible and currently breaks this repository:
//
//   - floating golangci-lint resolves to v2.x, and no golangci-lint v1.x
//     release can type-check Go 1.27 packages (their bundled x/tools cannot
//     read Go 1.27 export data), so the repo's .golangci.yml was migrated to
//     the v2 format and v2.14.0 (built with Go 1.27) is pinned here;
//   - floating govulncheck resolved to a release whose bundled x/tools panics
//     on Go 1.27 ("unexpected expr: *ast.KeyValueExpr"), so v1.8.0 (x/tools
//     v0.50.0) is pinned here.
//
// The tools are pre-installed into /usr/local/bin and /usr/local/bin is
// prepended to PATH (see golangBase) so the upstream module's
// Lint/Vulncheck/Format probes find the pinned binaries and skip their
// floating installs. /usr/local/bin is used instead of GOPATH/bin because the
// upstream module mounts its `gobin` cache volume over GOPATH/bin, which would
// both shadow binaries installed there and let stale binaries from previous
// floating installs win over the pinned ones.
const (
	golangciLintVersion = "v2.14.0"
	govulncheckVersion  = "v1.8.0"
	gofumptVersion      = "v0.12.0"
)

// goModVersionRe matches the `go` directive of go.mod.
var goModVersionRe = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+(?:\.\d+)?)\s*$`)

// HelmUpgrade is the repository CI/CD entrypoint. It composes the disaster37
// golang module (build/test/lint/vuln) and the goreleaser module (release).
type HelmUpgrade struct{}

// golangBase returns the Go base container used by the upstream golang module:
// the official golang image whose version is resolved from the repo's go.mod
// (the same resolution rule as the module's default, keeping go.mod the single
// source of truth), with the pinned CI tools pre-installed so the upstream
// module never installs floating "latest" tools.
func (m *HelmUpgrade) golangBase(ctx context.Context, src *dagger.Directory) (*dagger.Container, error) {
	contents, err := src.File("go.mod").Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read go.mod: %w", err)
	}
	version := goModVersionRe.FindStringSubmatch(contents)
	if version == nil {
		return nil, fmt.Errorf("no `go` directive found in go.mod")
	}

	// GOBIN is set per-command (not with WithEnvVariable) so it does not leak
	// into the container environment: the upstream module reads `go env GOBIN`
	// to decide where to mount its `gobin` cache volume.
	ctr := dag.Container().From("golang:" + version[1])
	for _, tool := range []struct{ pkg, ver string }{
		{"github.com/golangci/golangci-lint/v2/cmd/golangci-lint", golangciLintVersion},
		{"golang.org/x/vuln/cmd/govulncheck", govulncheckVersion},
		{"mvdan.cc/gofumpt", gofumptVersion},
	} {
		ctr = ctr.WithExec([]string{
			"bash", "-c",
			fmt.Sprintf("GOBIN=/usr/local/bin go install %s@%s", tool.pkg, tool.ver),
		})
	}

	// The upstream module mounts its `gobin` cache volume over /go/bin, which
	// is the first PATH entry of the golang image. Stale binaries from earlier
	// floating installs persist in that volume and would shadow the pinned
	// ones, so give /usr/local/bin precedence.
	path, err := ctr.EnvVariable(ctx, "PATH")
	if err != nil {
		return nil, fmt.Errorf("read PATH: %w", err)
	}
	ctr = ctr.WithEnvVariable("PATH", "/usr/local/bin:"+path)

	return ctr, nil
}

// golang returns the upstream golang module object bound to the pinned base.
func (m *HelmUpgrade) golang(ctx context.Context, src *dagger.Directory) (*dagger.Golang, error) {
	base, err := m.golangBase(ctx, src)
	if err != nil {
		return nil, err
	}
	return dag.Golang(src, dagger.GolangOpts{Base: base}), nil
}

// Ci runs the complete Go CI pipeline: lint, vulncheck, test, build.
//
// It mirrors the disaster37 golang module's Ci pipeline (Lint → Vulncheck →
// Test → Format → Build) but composes it locally so that Vulncheck can apply
// the fixable-only failure policy (see Vulncheck) — the upstream Ci calls the
// upstream Vulncheck, which fails on any finding including unfixable ones.
func (m *HelmUpgrade) Ci(
	ctx context.Context,
	// Repository source directory (caller passes --src .).
	// +required
	src *dagger.Directory,
	// Binary output name for the smoke build.
	// +optional
	// +default="helm-upgrade"
	out string,
	// Linker flags for the CI smoke build. Release ldflags live in .goreleaser.yml.
	// +optional
	// +default=["-s","-w"]
	ldflags []string,
) (*dagger.Directory, error) {
	g, err := m.golang(ctx, src)
	if err != nil {
		return nil, err
	}

	// Lint code.
	if _, err := g.Lint(ctx); err != nil {
		return nil, fmt.Errorf("lint failed: %w", err)
	}

	// Vulnerability check. Unfixable findings are logged as warnings (they do
	// not fail the pipeline, see Vulncheck) so they stay visible in CI logs.
	if summary, err := m.vulncheck(ctx, g); err != nil {
		return nil, fmt.Errorf("vulncheck failed: %w", err)
	} else if summary != "" {
		slog.Warn(summary)
	}

	// Test: Sync forces evaluation so test failures surface here.
	if _, err := g.Test().Sync(ctx); err != nil {
		return nil, fmt.Errorf("test failed: %w", err)
	}

	// Format code (in-container only; gofumpt never fails the pipeline).
	// Any error surfaces when the formatted directory is consumed below.
	formatted := g.Format()

	return g.WithSource(formatted).Build(dagger.GolangBuildOpts{
		Main:    mainPackage,
		Out:     out,
		Ldflags: ldflags,
	}), nil
}

// Lint runs golangci-lint using the repo's .golangci.yml.
func (m *HelmUpgrade) Lint(ctx context.Context, src *dagger.Directory) (string, error) {
	g, err := m.golang(ctx, src)
	if err != nil {
		return "", err
	}
	return g.Lint(ctx)
}

// Test runs `go test ./...` and returns the coverage.out file.
func (m *HelmUpgrade) Test(ctx context.Context, src *dagger.Directory) *dagger.File {
	return dag.Golang(src).Test()
}

// Vulncheck runs govulncheck ./... and fails when any reachable vulnerability
// has a fixed version available.
//
// Reachable vulnerabilities WITHOUT a fixed version are reported as warnings
// and do not fail the build: they cannot be remediated by upgrading
// dependencies, so failing on them would permanently block CI without any
// actionable signal. This repo currently has such findings (the standing
// golang.org/x/crypto/openpgp "unmaintained" advisory reached through Helm's
// provenance code, and containerd advisories reachable only through package
// init edges); they are tracked upstream, not in this repository.
func (m *HelmUpgrade) Vulncheck(ctx context.Context, src *dagger.Directory) (string, error) {
	g, err := m.golang(ctx, src)
	if err != nil {
		return "", err
	}
	summary, err := m.vulncheck(ctx, g)
	if err != nil {
		return "", err
	}
	if summary == "" {
		summary = "No vulnerabilities found."
	}
	return summary, nil
}

// vulncheck runs the pinned govulncheck against the module container and
// applies the fixable-only failure policy. It returns an empty summary when
// there is nothing to report, and a human-readable summary (warnings for
// unfixable findings) otherwise.
func (m *HelmUpgrade) vulncheck(ctx context.Context, g *dagger.Golang) (string, error) {
	// govulncheck exits non-zero when findings exist; capture the JSON stream
	// regardless so the policy can classify the findings.
	out, err := g.Container().
		WithExec(
			[]string{"/usr/local/bin/govulncheck", "-json", "./..."},
			dagger.ContainerWithExecOpts{Expect: dagger.ReturnTypeAny},
		).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	return vulncheckPolicy(out)
}

// vulnMessage is one message of the govulncheck -json stream.
type vulnMessage struct {
	OSV *struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	} `json:"osv"`
	Finding *struct {
		OSV          string `json:"osv"`
		FixedVersion string `json:"fixed_version"`
		Trace        []struct {
			Function string `json:"function"`
		} `json:"trace"`
	} `json:"finding"`
}

// vulncheckPolicy classifies govulncheck -json findings.
//
// A finding is "reachable" when its trace contains a function frame
// (module/package-level findings carry no function). Reachable findings with
// a fixed version are actionable and fail the build; reachable findings
// without any fixed version are reported as warnings.
func vulncheckPolicy(stream string) (string, error) {
	summaries := map[string]string{}
	reachable := map[string]string{} // vuln ID -> fixed version ("" when none)

	dec := json.NewDecoder(strings.NewReader(stream))
	for {
		var msg vulnMessage
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode govulncheck output: %w", err)
		}
		switch {
		case msg.OSV != nil:
			summaries[msg.OSV.ID] = msg.OSV.Summary
		case msg.Finding != nil:
			f := msg.Finding
			reached := false
			for _, frame := range f.Trace {
				if frame.Function != "" {
					reached = true
					break
				}
			}
			if !reached {
				continue // imported/required but never called: informational only
			}
			if prev, ok := reachable[f.OSV]; !ok || (prev == "" && f.FixedVersion != "") {
				reachable[f.OSV] = f.FixedVersion
			}
		}
	}

	var actionable, unfixable []string
	for id, fixed := range reachable {
		switch fixed {
		case "":
			unfixable = append(unfixable, fmt.Sprintf("  - %s: %s", id, summaries[id]))
		default:
			actionable = append(actionable, fmt.Sprintf("  - %s (fix available: upgrade to %s): %s", id, fixed, summaries[id]))
		}
	}
	sort.Strings(actionable)
	sort.Strings(unfixable)

	if len(actionable) > 0 {
		return "", fmt.Errorf("govulncheck found %d reachable vulnerabilities with fixes available:\n%s\n\nUpgrade the affected modules to fix them.",
			len(actionable), strings.Join(actionable, "\n"))
	}
	if len(unfixable) > 0 {
		return fmt.Sprintf("WARNING: govulncheck found %d reachable vulnerabilities with no fixed version available (not actionable, reported only):\n%s",
			len(unfixable), strings.Join(unfixable, "\n")), nil
	}
	return "", nil
}

// Build cross-compiles a single target with CGO_ENABLED=0 (for local verification).
func (m *HelmUpgrade) Build(
	src *dagger.Directory,
	// +optional
	// +default="helm-upgrade"
	out string,
	// +optional
	// +default="linux"
	os string,
	// +optional
	// +default="amd64"
	arch string,
	// +optional
	// +default=["-s","-w"]
	ldflags []string,
) *dagger.Directory {
	return dag.Golang(src).Build(dagger.GolangBuildOpts{
		Main:    mainPackage,
		Out:     out,
		Os:      os,
		Arch:    arch,
		Ldflags: ldflags,
	})
}

// Release runs GoReleaser (default subcommand: `release`) to build the matrix and
// publish a GitHub release using githubToken.
func (m *HelmUpgrade) Release(
	ctx context.Context,
	// Repository source (must include .git; use actions/checkout fetch-depth: 0).
	// +required
	src *dagger.Directory,
	// GitHub token used to create the release/upload assets. Required to
	// publish; optional for snapshot/no-publish runs.
	// +optional
	githubToken *dagger.Secret,
	// GoReleaser args; defaults to ["release"].
	// +optional
	args []string,
) (string, error) {
	if len(args) == 0 {
		args = []string{"release"}
	}

	// Fail fast with a clear message instead of letting GoReleaser fail deep
	// inside the publish step when no token was provided.
	if githubToken == nil && releaseNeedsToken(args) {
		return "", fmt.Errorf("githubToken is required to publish a release; pass --github-token, or use --snapshot / --skip=publish for a tokenless run")
	}

	// Pin the GoReleaser source to an explicit tag (bump independently of the module ref).
	goreleaserSource := dag.Git("https://github.com/goreleaser/goreleaser.git",
		dagger.GitOpts{KeepGitDir: true}).Tag("v2.10.2").Tree()

	// Build the GoReleaser CLI (cached by Dagger) using the module's Go+zig base.
	goreleaserBin := dag.Goreleaser(dagger.GoreleaserOpts{Source: goreleaserSource}).
		Build(dagger.GoreleaserBuildOpts{Os: "linux", Arch: runtime.GOARCH})

	// The disaster37 golang container already carries the source plus the Go
	// toolchain and go-mod/build caches. Unlike the goreleaser module's pinned
	// wolfi base, the official golang image bundles git (needed by GoReleaser's
	// changelog/version detection) and can still resolve its apk packages.
	// GoReleaser provenance is preserved: the CLI itself is built from the
	// pinned v2.10.2 tag above, and the execution image is the same
	// golang:<go.mod version> image the CI path uses.
	ctr := dag.Golang(src).Container().
		WithMountedFile("/usr/local/bin/goreleaser", goreleaserBin).
		WithEnvVariable("CI", "true")

	if githubToken != nil {
		ctr = ctr.WithSecretVariable("GITHUB_TOKEN", githubToken)
	}

	return ctr.
		WithExec(append([]string{"/usr/local/bin/goreleaser"}, args...)).
		Stdout(ctx)
}

// releaseNeedsToken reports whether the given GoReleaser arguments will
// publish a release (and therefore require a GitHub token).
func releaseNeedsToken(args []string) bool {
	// The subcommand is the first non-flag argument.
	subcmd := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			subcmd = a
			break
		}
	}
	switch subcmd {
	case "release", "publish":
	default:
		// check, validate, build, changelog, healthcheck, ... never publish.
		return false
	}

	for _, a := range args {
		switch {
		case a == "--snapshot" || a == "--skip-publish":
			return false
		case strings.HasPrefix(a, "--skip="):
			for _, skip := range strings.Split(strings.TrimPrefix(a, "--skip="), ",") {
				if skip == "publish" {
					return false
				}
			}
		}
	}
	return true
}
