// A Dagger module for the helm-upgrade repository.
//
// It composes the disaster37 golang module (build/test/lint/vuln) and the
// goreleaser module (release) behind a single local entrypoint used by the
// GitHub Actions workflows.

package main

import (
	"context"
	"runtime"

	"dagger/helm-upgrade/internal/dagger"
)

// mainPackage is the path to the CLI entrypoint compiled by the golang module.
const mainPackage = "./cmd/helm-upgrade"

// HelmUpgrade is the repository CI/CD entrypoint. It composes the disaster37
// golang module (build/test/lint/vuln) and the goreleaser module (release).
type HelmUpgrade struct{}

// Ci runs the complete Go CI pipeline: lint, vulncheck, test, build.
// It delegates to the disaster37 golang module's Ci function.
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
	// Sync forces evaluation so lint/vulncheck/test failures surface here.
	return dag.Golang(src).Ci(dagger.GolangCiOpts{
		Main:    mainPackage,
		Out:     out,
		Ldflags: ldflags,
	}).Sync(ctx)
}

// Lint runs golangci-lint using the repo's .golangci.yml.
func (m *HelmUpgrade) Lint(ctx context.Context, src *dagger.Directory) (string, error) {
	return dag.Golang(src).Lint(ctx)
}

// Test runs `go test ./...` and returns the coverage.out file.
func (m *HelmUpgrade) Test(ctx context.Context, src *dagger.Directory) *dagger.File {
	return dag.Golang(src).Test()
}

// Vulncheck runs `govulncheck ./...`; non-zero exit on any finding.
func (m *HelmUpgrade) Vulncheck(ctx context.Context, src *dagger.Directory) (string, error) {
	return dag.Golang(src).Vulncheck(ctx)
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
	// GitHub token used to create the release/upload assets. Required to publish;
	// optional for snapshot/no-publish runs.
	// +optional
	githubToken *dagger.Secret,
	// GoReleaser args; defaults to ["release"].
	// +optional
	args []string,
) (string, error) {
	if len(args) == 0 {
		args = []string{"release"}
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
