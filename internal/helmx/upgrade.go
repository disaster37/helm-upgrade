package helmx

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// ChartSource identifies the chart to upgrade to. Ref is either a plain chart
// name (resolved against RepoURL or the local repo cache) or an `oci://` URL.
type ChartSource struct {
	Ref                   string
	Version               string
	RepoURL               string
	Username              string
	Password              string
	CaFile                string
	CertFile              string
	KeyFile               string
	InsecureSkipTLSVerify bool
	PlainHTTP             bool
}

// UpgradeOptions holds the knobs of a single bulk upgrade run.
type UpgradeOptions struct {
	Chart                    ChartSource
	Values                   map[string]interface{}
	DryRun                   bool
	Atomic                   bool
	Wait                     bool
	WaitForJobs              bool
	Timeout                  time.Duration
	MaxHistory               int
	Force                    bool
	DisableOpenAPIValidation bool
	ContinueOnError          bool
	Parallelism              int
}

// Result records the outcome of upgrading one release.
type Result struct {
	Release     ReleaseRef `json:"release" yaml:"release"`
	NewRevision int        `json:"newRevision,omitempty" yaml:"newRevision,omitempty"`
	NewVersion  string     `json:"newVersion,omitempty" yaml:"newVersion,omitempty"`
	Status      string     `json:"status" yaml:"status"`
	Error       string     `json:"error,omitempty" yaml:"error,omitempty"`
}

// Outcome values used in Result.Status.
const (
	StatusUpgraded = "upgraded"
	StatusDryRun   = "dry-run"
	StatusFailed   = "failed"
	StatusSkipped  = "skipped"
)

// LoadChart resolves and loads the target chart exactly once, so that the whole
// bulk run shares a single download and a single parsed chart.
func (c *Client) LoadChart(src ChartSource) (*chart.Chart, error) {
	cpo := action.ChartPathOptions{
		Version:               src.Version,
		RepoURL:               src.RepoURL,
		Username:              src.Username,
		Password:              src.Password,
		CaFile:                src.CaFile,
		CertFile:              src.CertFile,
		KeyFile:               src.KeyFile,
		InsecureSkipTLSverify: src.InsecureSkipTLSVerify,
		PlainHTTP:             src.PlainHTTP,
	}
	// LocateChart needs a registry client for oci:// refs; it is unexported, so
	// borrow the one wired into an Upgrade action.
	cfg, err := c.ActionConfig("")
	if err != nil {
		return nil, err
	}
	probe := action.NewUpgrade(cfg)
	probe.ChartPathOptions = cpo
	probe.SetRegistryClient(c.registry)

	path, err := probe.ChartPathOptions.LocateChart(src.Ref, c.settings)
	if err != nil {
		return nil, fmt.Errorf("locating chart %q version %q: %w", src.Ref, src.Version, err)
	}
	logrus.WithField("path", path).Debug("chart located")

	ch, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading chart from %s: %w", path, err)
	}
	if ch.Metadata == nil {
		return nil, fmt.Errorf("chart %s has no metadata", path)
	}
	if ch.Metadata.Deprecated {
		logrus.WithField("chart", ch.Metadata.Name).Warn("target chart is marked deprecated")
	}
	if len(ch.CRDObjects()) > 0 {
		logrus.WithField("chart", ch.Metadata.Name).
			Warn("chart contains crds/; Helm does not upgrade existing CRDs, apply them manually if they changed")
	}
	return ch, nil
}

// ChartNameFromRef extracts the chart name from a chart reference. A plain name
// is returned unchanged; an `oci://` or URL reference yields its last path
// element. This is the single source of truth for that parsing.
func ChartNameFromRef(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// VerifyChartName guards against typos by rejecting a chart whose metadata name
// differs from the name the releases were filtered on. The expectation may be a
// bare name or a full chart reference.
func VerifyChartName(ch *chart.Chart, expected string) error {
	if expected == "" || ch.Metadata == nil {
		return nil
	}
	want := ChartNameFromRef(expected)
	if ch.Metadata.Name != want {
		return fmt.Errorf("resolved chart is named %q but %q was requested; check --chart", ch.Metadata.Name, want)
	}
	return nil
}

// UpgradeAll upgrades every release in refs, honouring parallelism and the
// continue-on-error policy. Results are returned in the input order.
func (c *Client) UpgradeAll(ctx context.Context, refs []ReleaseRef, ch *chart.Chart, opts UpgradeOptions) ([]Result, error) {
	results := make([]Result, len(refs))
	for i, r := range refs {
		results[i] = Result{Release: r, Status: StatusSkipped}
	}

	parallelism := opts.Parallelism
	if parallelism < 1 {
		parallelism = 1
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		stop     bool
		sem      = make(chan struct{}, parallelism)
	)

	for i := range refs {
		mu.Lock()
		halted := stop
		mu.Unlock()
		if halted || ctx.Err() != nil {
			break
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			res := c.upgradeOne(ctx, refs[idx], ch, opts)

			mu.Lock()
			defer mu.Unlock()
			results[idx] = res
			if res.Status == StatusFailed {
				if firstErr == nil {
					firstErr = fmt.Errorf("upgrading %s/%s: %s", res.Release.Namespace, res.Release.Name, res.Error)
				}
				if !opts.ContinueOnError {
					stop = true
				}
			}
		}(i)

		if parallelism == 1 {
			wg.Wait()
		}
	}
	wg.Wait()

	if ctx.Err() != nil && firstErr == nil {
		return results, ctx.Err()
	}
	return results, firstErr
}

func (c *Client) upgradeOne(ctx context.Context, ref ReleaseRef, ch *chart.Chart, opts UpgradeOptions) Result {
	res := Result{Release: ref, Status: StatusFailed}
	log := logrus.WithFields(logrus.Fields{"release": ref.Name, "namespace": ref.Namespace})

	cfg, err := c.ActionConfig(ref.Namespace)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	up := action.NewUpgrade(cfg)
	up.Namespace = ref.Namespace
	up.Version = opts.Chart.Version
	up.RepoURL = opts.Chart.RepoURL
	up.ReuseValues = true
	up.Atomic = opts.Atomic
	up.Wait = opts.Wait
	up.WaitForJobs = opts.WaitForJobs
	up.Timeout = opts.Timeout
	up.MaxHistory = opts.MaxHistory
	up.Force = opts.Force
	up.DisableOpenAPIValidation = opts.DisableOpenAPIValidation
	up.DryRun = opts.DryRun
	up.SetRegistryClient(c.registry)

	log.WithFields(logrus.Fields{
		"from":   ref.ChartVersion,
		"to":     chartVersion(ch),
		"dryRun": opts.DryRun,
	}).Info("upgrading release")

	rel, err := up.RunWithContext(ctx, ref.Name, ch, opts.Values)
	if err != nil {
		res.Error = err.Error()
		log.WithError(err).Error("upgrade failed")
		return res
	}

	res.NewVersion = chartVersion(ch)
	if rel != nil {
		res.NewRevision = rel.Version
	}
	if opts.DryRun {
		res.Status = StatusDryRun
	} else {
		res.Status = StatusUpgraded
	}
	return res
}

func chartVersion(ch *chart.Chart) string {
	if ch == nil || ch.Metadata == nil {
		return ""
	}
	return ch.Metadata.Version
}
