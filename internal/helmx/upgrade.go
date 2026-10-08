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
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
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
	// BaseRevisionOffset selects which revision of each release supplies the
	// reused values. 0 (the default) means the current revision, 1 means the
	// revision before it (n-1), and so on. Anything greater than 0 makes the
	// run read that revision's stored values explicitly instead of letting
	// Helm reuse the current ones.
	BaseRevisionOffset int
}

// Result records the outcome of upgrading one release.
type Result struct {
	Release     ReleaseRef `json:"release" yaml:"release"`
	NewRevision int        `json:"newRevision,omitempty" yaml:"newRevision,omitempty"`
	NewVersion  string     `json:"newVersion,omitempty" yaml:"newVersion,omitempty"`
	// BaseRevision is the revision whose values were reused, set only when
	// BaseRevisionOffset was greater than 0.
	BaseRevision int    `json:"baseRevision,omitempty" yaml:"baseRevision,omitempty"`
	Status       string `json:"status" yaml:"status"`
	Error        string `json:"error,omitempty" yaml:"error,omitempty"`
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

	// Helm mutates both the chart and the values map it is handed, so every
	// release gets its own copies; otherwise the first release's values leak
	// into all the following ones.
	relChart := cloneChart(ch)
	vals := deepCopyValues(opts.Values)
	if vals == nil {
		vals = map[string]interface{}{}
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

	baseRevision := 0
	if opts.BaseRevisionOffset > 0 {
		baseVals, rev, err := baseValues(cfg, ref, opts.BaseRevisionOffset)
		if err != nil {
			res.Error = err.Error()
			log.WithError(err).Error("resolving base revision failed")
			return res
		}
		// Take the chosen revision's values as the base and let the command
		// line overrides win on top of them.
		vals = chartutil.CoalesceTables(vals, baseVals)
		baseRevision = rev
		res.BaseRevision = rev
		// Stop Helm from re-applying the current revision's values over ours.
		up.ReuseValues = false
		up.ResetValues = true
	}

	fields := logrus.Fields{
		"from":   ref.ChartVersion,
		"to":     chartVersion(relChart),
		"dryRun": opts.DryRun,
	}
	if baseRevision > 0 {
		fields["baseRevision"] = baseRevision
	}
	log.WithFields(fields).Info("upgrading release")

	rel, err := up.RunWithContext(ctx, ref.Name, relChart, vals)
	if err != nil {
		res.Error = err.Error()
		log.WithError(err).Error("upgrade failed")
		return res
	}

	res.NewVersion = chartVersion(relChart)
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

// baseValues loads the user-supplied values stored on revision
// (current - offset) of a release.
func baseValues(cfg *action.Configuration, ref ReleaseRef, offset int) (map[string]interface{}, int, error) {
	old, rev, err := baseRelease(cfg, ref, offset)
	if err != nil {
		return nil, 0, err
	}
	return deepCopyValues(old.Config), rev, nil
}

// baseRelease returns revision (current - offset) of a release. The current
// revision is taken from the release storage rather than from ref, so the
// offset stays correct even if the listing is stale.
func baseRelease(cfg *action.Configuration, ref ReleaseRef, offset int) (*release.Release, int, error) {
	last, err := cfg.Releases.Last(ref.Name)
	if err != nil {
		return nil, 0, fmt.Errorf("reading history of %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	rev := last.Version - offset
	if rev < 1 {
		return nil, 0, fmt.Errorf("release %s/%s is at revision %d; revision %d (n-%d) does not exist",
			ref.Namespace, ref.Name, last.Version, rev, offset)
	}

	old, err := cfg.Releases.Get(ref.Name, rev)
	if err != nil {
		return nil, 0, fmt.Errorf("reading revision %d of %s/%s: %w", rev, ref.Namespace, ref.Name, err)
	}
	return old, rev, nil
}

// ResolveBaseRevisions annotates refs with the revision and chart version that
// an upgrade with this offset would reuse values from, so the plan can show
// them before anything is changed. Releases whose offset cannot be resolved are
// marked with BaseRevision -1 and are left to fail during the upgrade itself,
// where --continue-on-error applies.
func (c *Client) ResolveBaseRevisions(refs []ReleaseRef, offset int) []ReleaseRef {
	if offset <= 0 {
		return refs
	}

	out := make([]ReleaseRef, len(refs))
	copy(out, refs)
	for i := range out {
		log := logrus.WithFields(logrus.Fields{"release": out[i].Name, "namespace": out[i].Namespace})

		cfg, err := c.ActionConfig(out[i].Namespace)
		if err != nil {
			log.WithError(err).Warn("cannot resolve base revision")
			out[i].BaseRevision = -1
			continue
		}
		old, rev, err := baseRelease(cfg, out[i], offset)
		if err != nil {
			log.WithError(err).Warn("cannot resolve base revision")
			out[i].BaseRevision = -1
			continue
		}
		out[i].BaseRevision = rev
		if old.Chart != nil && old.Chart.Metadata != nil {
			out[i].BaseChartVersion = old.Chart.Metadata.Version
		}
	}
	return out
}

func chartVersion(ch *chart.Chart) string {
	if ch == nil || ch.Metadata == nil {
		return ""
	}
	return ch.Metadata.Version
}
