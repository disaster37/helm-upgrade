// Command helm-upgrade bulk-upgrades Helm releases of a given chart across one
// or more namespaces, reusing each release's existing values.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"golang.org/x/term"
	helmcli "helm.sh/helm/v3/pkg/cli"

	"github.com/example/helm-upgrade/internal/helmx"
	"github.com/example/helm-upgrade/internal/kube"
	"github.com/example/helm-upgrade/internal/output"
	"github.com/example/helm-upgrade/internal/values"
)

// Build information, injected via -ldflags at build time.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
	helmSDK   = "v3.16.4"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitSignal  = 130
)

// usageError marks an error as a configuration/usage problem (exit code 2).
type usageError struct{ err error }

func (u usageError) Error() string { return u.err.Error() }
func (u usageError) Unwrap() error { return u.err }

func usagef(format string, a ...interface{}) error {
	return usageError{fmt.Errorf(format, a...)}
}

// appState is built once in Before and shared by the commands.
type appState struct {
	client     *helmx.Client
	namespaces []string
	allNS      bool
	format     output.Format
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	state := &appState{}
	app := newApp(state)

	err := app.RunContext(ctx, os.Args)
	if err == nil {
		os.Exit(exitOK)
	}

	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		logrus.Warn("interrupted")
		os.Exit(exitSignal)
	}
	logrus.Error(err)
	var ue usageError
	if errors.As(err, &ue) {
		os.Exit(exitUsage)
	}
	os.Exit(exitFailure)
}

func newApp(state *appState) *cli.App {
	app := &cli.App{
		Name:  "helm-upgrade",
		Usage: "bulk-upgrade Helm releases of a chart across namespaces",
		Description: "helm-upgrade lists the Helm releases of a given chart in one or more\n" +
			"namespaces and upgrades them to a target chart version. Existing release\n" +
			"values are reused (helm's --reuse-values semantics) and any -f/--set\n" +
			"overrides are merged on top.",
		Version:              version,
		HideVersion:          true,
		HideHelpCommand:      true,
		EnableBashCompletion: true,
		Flags:                globalFlags(),
		Before:               func(c *cli.Context) error { return setup(c, state) },
		Commands: []*cli.Command{
			listCommand(state),
			upgradeCommand(state),
			versionCommand(),
		},
	}
	// Errors are reported by main so that exit codes stay under our control.
	app.ExitErrHandler = func(*cli.Context, error) {}
	return app
}

func globalFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "kubeconfig",
			Usage:   "path to the kubeconfig `FILE` (defaults to $KUBECONFIG, then ~/.kube/config)",
			EnvVars: []string{"KUBECONFIG"},
		},
		&cli.StringFlag{
			Name:  "context",
			Usage: "kubeconfig context `NAME` to use",
		},
		&cli.StringSliceFlag{
			Name:    "namespace",
			Aliases: []string{"n"},
			Usage:   "`NAMESPACE` to operate on; repeatable (default: current kubeconfig namespace)",
		},
		&cli.BoolFlag{
			Name:    "all-namespaces",
			Aliases: []string{"A"},
			Usage:   "operate on releases in all namespaces (needs cluster-wide read access)",
		},
		&cli.StringFlag{
			Name:  "log-level",
			Value: "info",
			Usage: "log verbosity: debug, info, warn or error",
		},
		&cli.StringFlag{
			Name:  "log-format",
			Value: "text",
			Usage: "log format: text or json",
		},
		&cli.StringFlag{
			Name:    "output",
			Aliases: []string{"o"},
			Value:   "table",
			Usage:   "output format: table, json or yaml",
		},
	}
}

func setup(c *cli.Context, state *appState) error {
	if err := configureLogging(c.String("log-level"), c.String("log-format")); err != nil {
		return err
	}

	format, err := output.ParseFormat(c.String("output"))
	if err != nil {
		return usageError{err}
	}
	state.format = format

	// `version` and help output need no cluster connection.
	if c.Args().First() == "version" || wantsHelp(os.Args) {
		return nil
	}

	namespaces := c.StringSlice("namespace")
	allNS := c.Bool("all-namespaces")
	if allNS && len(namespaces) > 0 {
		return usagef("--all-namespaces cannot be combined with --namespace")
	}

	getter := kube.NewGetter(kube.Options{
		KubeConfig: c.String("kubeconfig"),
		Context:    c.String("context"),
		Namespace:  firstOrEmpty(namespaces),
	})
	if err := getter.CheckConnectivity(); err != nil {
		return err
	}
	if !allNS && len(namespaces) == 0 {
		namespaces = []string{getter.DefaultNamespace()}
		logrus.WithField("namespace", namespaces[0]).Debug("defaulting to kubeconfig namespace")
	}

	settings := helmcli.New()
	settings.Debug = logrus.GetLevel() == logrus.DebugLevel

	client, err := helmx.NewClient(getter, settings)
	if err != nil {
		return err
	}

	state.client = client
	state.namespaces = namespaces
	state.allNS = allNS
	return nil
}

func configureLogging(level, format string) error {
	lvl, err := logrus.ParseLevel(strings.ToLower(strings.TrimSpace(level)))
	if err != nil {
		return usagef("invalid --log-level %q (want debug, info, warn or error)", level)
	}
	logrus.SetLevel(lvl)
	// Logs go to stderr so that -o json on stdout stays machine-parseable.
	logrus.SetOutput(os.Stderr)

	switch strings.ToLower(strings.TrimSpace(format)) {
	case "text", "":
		logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	case "json":
		logrus.SetFormatter(&logrus.JSONFormatter{})
	default:
		return usagef("invalid --log-format %q (want text or json)", format)
	}
	return nil
}

func listCommand(state *appState) *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list Helm releases, optionally filtered by chart and version",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "chart", Usage: "only releases of this chart `NAME`"},
			&cli.StringFlag{Name: "chart-version", Usage: "semver `CONSTRAINT` on the installed chart version, e.g. '<2.0.0'"},
			&cli.StringSliceFlag{Name: "status", Usage: "release `STATUS` filter; repeatable (default: deployed, failed)"},
			&cli.StringSliceFlag{Name: "release", Usage: "restrict to these release `NAME`s; repeatable"},
		},
		Action: func(c *cli.Context) error {
			refs, err := state.client.List(helmx.ListOptions{
				Namespaces:        state.namespaces,
				AllNamespaces:     state.allNS,
				ChartName:         c.String("chart"),
				VersionConstraint: c.String("chart-version"),
				Statuses:          c.StringSlice("status"),
				ReleaseNames:      c.StringSlice("release"),
			})
			if err != nil {
				return err
			}
			if len(refs) == 0 {
				logrus.Warn("no releases matched the given filters")
			}
			return output.Releases(os.Stdout, state.format, refs)
		},
	}
}

func upgradeCommand(state *appState) *cli.Command {
	return &cli.Command{
		Name:  "upgrade",
		Usage: "upgrade every matching release to a target chart version",
		Description: "Values handling: the stored values of each release's last revision are reused\n" +
			"(equivalent to `helm upgrade --reuse-values`) and -f/--set overrides are merged\n" +
			"on top. Values injected by a previous --set therefore persist; this differs from\n" +
			"helm's --reset-then-reuse-values, which is not exposed by this CLI.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "chart", Required: true, Usage: "chart `NAME` or oci:// reference to upgrade to"},
			&cli.StringFlag{Name: "version", Required: true, Usage: "target chart `VERSION`"},
			&cli.StringFlag{Name: "repo", Usage: "chart repository `URL` for HTTPS repos"},
			&cli.StringFlag{
				Name:    "username",
				Usage:   "chart repository / OCI registry username",
				EnvVars: []string{"HELM_REPO_USERNAME"},
			},
			&cli.StringFlag{
				Name:    "password",
				Usage:   "chart repository / OCI registry password (prefer $HELM_REPO_PASSWORD or --password-stdin: argv is world-readable)",
				EnvVars: []string{"HELM_REPO_PASSWORD"},
			},
			&cli.BoolFlag{
				Name:  "password-stdin",
				Usage: "read the repository / registry password from stdin",
			},
			&cli.StringFlag{Name: "ca-file", Usage: "verify the repository certificate with this CA bundle `FILE`"},
			&cli.StringFlag{Name: "cert-file", Usage: "TLS client certificate `FILE`"},
			&cli.StringFlag{Name: "key-file", Usage: "TLS client key `FILE`"},
			&cli.BoolFlag{Name: "insecure-skip-tls-verify", Usage: "skip TLS verification when fetching the chart"},
			&cli.BoolFlag{Name: "plain-http", Usage: "use plain HTTP for the OCI registry"},

			&cli.StringSliceFlag{Name: "release", Usage: "restrict to these release `NAME`s; repeatable"},
			&cli.StringFlag{Name: "current-version", Usage: "only upgrade releases whose current chart version matches this semver `CONSTRAINT`"},
			&cli.StringSliceFlag{Name: "status", Usage: "release `STATUS` filter; repeatable (default: deployed, failed)"},

			&cli.StringSliceFlag{Name: "values", Aliases: []string{"f"}, Usage: "values `FILE` merged on top of the reused values; repeatable"},
			&cli.StringSliceFlag{Name: "set", Usage: "set values on the command line (`key=value`); repeatable"},
			&cli.StringSliceFlag{Name: "set-string", Usage: "set STRING values on the command line (`key=value`); repeatable"},
			&cli.StringSliceFlag{Name: "set-json", Usage: "set JSON values on the command line (`key=json`); repeatable"},
			&cli.StringSliceFlag{Name: "set-file", Usage: "set values from files (`key=path`); repeatable"},

			&cli.BoolFlag{Name: "dry-run", Usage: "simulate the upgrades without changing anything"},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the interactive confirmation prompt"},
			&cli.BoolFlag{Name: "continue-on-error", Usage: "keep upgrading remaining releases after a failure"},

			&cli.BoolFlag{Name: "atomic", Usage: "roll back the release if the upgrade fails (implies --wait)"},
			&cli.BoolFlag{Name: "wait", Usage: "wait until all resources are ready"},
			&cli.BoolFlag{Name: "wait-for-jobs", Usage: "with --wait, also wait for completed jobs"},
			&cli.DurationFlag{Name: "timeout", Value: 5 * time.Minute, Usage: "time to wait for each individual upgrade"},
			&cli.IntFlag{Name: "max-history", Value: 10, Usage: "maximum number of release revisions to keep"},
			&cli.IntFlag{Name: "parallelism", Value: 1, Usage: "number of concurrent upgrades"},
			&cli.BoolFlag{Name: "force", Usage: "force resource updates through a replacement strategy"},
			&cli.BoolFlag{Name: "disable-openapi-validation", Usage: "skip OpenAPI schema validation of rendered manifests"},
		},
		Action: func(c *cli.Context) error { return runUpgrade(c, state) },
	}
}

func runUpgrade(c *cli.Context, state *appState) error {
	if c.Int("parallelism") < 1 {
		return usagef("--parallelism must be at least 1")
	}

	chartRef := c.String("chart")
	chartName := helmx.ChartNameFromRef(chartRef)

	password, err := resolvePassword(c)
	if err != nil {
		return err
	}

	src := helmx.ChartSource{
		Ref:                   chartRef,
		Version:               c.String("version"),
		RepoURL:               c.String("repo"),
		Username:              c.String("username"),
		Password:              password,
		CaFile:                c.String("ca-file"),
		CertFile:              c.String("cert-file"),
		KeyFile:               c.String("key-file"),
		InsecureSkipTLSVerify: c.Bool("insecure-skip-tls-verify"),
		PlainHTTP:             c.Bool("plain-http"),
	}

	// 1. Find the releases to upgrade.
	refs, err := state.client.List(helmx.ListOptions{
		Namespaces:        state.namespaces,
		AllNamespaces:     state.allNS,
		ChartName:         chartName,
		VersionConstraint: c.String("current-version"),
		Statuses:          c.StringSlice("status"),
		ReleaseNames:      c.StringSlice("release"),
	})
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		logrus.Warn("no releases matched the given filters; nothing to upgrade")
		return nil
	}

	// 2. Merge the value overrides.
	vals, err := values.Merge(values.Options{
		ValueFiles:   c.StringSlice("values"),
		Values:       c.StringSlice("set"),
		StringValues: c.StringSlice("set-string"),
		JSONValues:   c.StringSlice("set-json"),
		FileValues:   c.StringSlice("set-file"),
	}, state.client.Settings())
	if err != nil {
		return usageError{err}
	}
	if len(vals) > 0 {
		// Keys only: values routinely contain secrets.
		logrus.WithField("keys", values.TopLevelKeys(vals)).Debug("value overrides merged on top of reused values")
	}

	// 3. Resolve and load the chart once for the whole run.
	ch, err := state.client.LoadChart(src)
	if err != nil {
		return err
	}
	if err := helmx.VerifyChartName(ch, chartName); err != nil {
		return usageError{err}
	}

	// 4. Show the plan and confirm.
	if err := output.Plan(os.Stdout, state.format, refs, ch.Metadata.Version); err != nil {
		return err
	}
	dryRun := c.Bool("dry-run")
	if !dryRun && !c.Bool("yes") {
		ok, err := confirm(fmt.Sprintf("Upgrade %d release(s) to %s %s?", len(refs), ch.Metadata.Name, ch.Metadata.Version))
		if err != nil {
			return err
		}
		if !ok {
			logrus.Info("aborted by user")
			return nil
		}
	}

	// 5. Run the upgrades.
	results, upErr := state.client.UpgradeAll(c.Context, refs, ch, helmx.UpgradeOptions{
		Chart:                    src,
		Values:                   vals,
		DryRun:                   dryRun,
		Atomic:                   c.Bool("atomic"),
		Wait:                     c.Bool("wait"),
		WaitForJobs:              c.Bool("wait-for-jobs"),
		Timeout:                  c.Duration("timeout"),
		MaxHistory:               c.Int("max-history"),
		Force:                    c.Bool("force"),
		DisableOpenAPIValidation: c.Bool("disable-openapi-validation"),
		ContinueOnError:          c.Bool("continue-on-error"),
		Parallelism:              c.Int("parallelism"),
	})

	// 6. Report.
	if err := output.Results(os.Stdout, state.format, results); err != nil {
		return err
	}
	summarize(results)
	return upErr
}

func summarize(results []helmx.Result) {
	var upgraded, dryRun, failed, skipped int
	for _, r := range results {
		switch r.Status {
		case helmx.StatusUpgraded:
			upgraded++
		case helmx.StatusDryRun:
			dryRun++
		case helmx.StatusFailed:
			failed++
		default:
			skipped++
		}
	}
	logrus.WithFields(logrus.Fields{
		"upgraded": upgraded,
		"dryRun":   dryRun,
		"failed":   failed,
		"skipped":  skipped,
	}).Info("run complete")
}

// resolvePassword returns the repository password, reading it from stdin when
// --password-stdin is set. Consuming stdin rules out the interactive
// confirmation prompt, so --yes or --dry-run must be supplied as well.
func resolvePassword(c *cli.Context) (string, error) {
	if !c.Bool("password-stdin") {
		return c.String("password"), nil
	}
	if c.String("password") != "" {
		return "", usagef("--password and --password-stdin are mutually exclusive")
	}
	if !c.Bool("yes") && !c.Bool("dry-run") {
		return "", usagef("--password-stdin consumes stdin, so the confirmation prompt cannot be shown: add --yes or --dry-run")
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("reading password from stdin: %w", err)
	}
	pw := strings.TrimRight(string(b), "\r\n")
	if pw == "" {
		return "", usagef("--password-stdin was given but stdin was empty")
	}
	return pw, nil
}

// confirm prompts on stdin. When stdin is not a terminal the run is aborted,
// because an unattended caller must pass --yes explicitly.
func confirm(prompt string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, usagef("stdin is not a terminal: pass --yes to confirm non-interactively (or --dry-run to preview)")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print version information",
		Action: func(c *cli.Context) error {
			fmt.Printf("helm-upgrade %s (commit %s, built %s)\n", version, commit, buildDate)
			fmt.Printf("helm SDK %s\n", helmSDK)
			fmt.Printf("go %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

// wantsHelp reports whether the invocation only asks for help or usage text,
// in which case no cluster connection should be attempted.
func wantsHelp(args []string) bool {
	if len(args) <= 1 {
		return true
	}
	if args[1] == "help" {
		return true
	}
	for _, a := range args[1:] {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
