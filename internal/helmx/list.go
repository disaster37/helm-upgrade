package helmx

import (
	"fmt"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ReleaseRef is the trimmed-down view of a Helm release this CLI works with.
type ReleaseRef struct {
	Name         string `json:"name" yaml:"name"`
	Namespace    string `json:"namespace" yaml:"namespace"`
	ChartName    string `json:"chart" yaml:"chart"`
	ChartVersion string `json:"chartVersion" yaml:"chartVersion"`
	AppVersion   string `json:"appVersion" yaml:"appVersion"`
	Revision     int    `json:"revision" yaml:"revision"`
	// BaseRevision and BaseChartVersion describe the revision whose values a
	// run will reuse when --base-revision-offset is in play. BaseRevision is
	// 0 when the current revision is used and -1 when the offset could not be
	// resolved; BaseChartVersion is the chart version of that revision.
	BaseRevision     int       `json:"baseRevision,omitempty" yaml:"baseRevision,omitempty"`
	BaseChartVersion string    `json:"baseChartVersion,omitempty" yaml:"baseChartVersion,omitempty"`
	Status           string    `json:"status" yaml:"status"`
	Updated          time.Time `json:"updated" yaml:"updated"`
}

// ListOptions controls which releases are returned.
type ListOptions struct {
	// Namespaces to search. Ignored when AllNamespaces is true.
	Namespaces    []string
	AllNamespaces bool
	// ChartName, when set, keeps only releases whose chart has this name.
	ChartName string
	// VersionConstraint is a semver constraint applied to the chart version.
	VersionConstraint string
	// Statuses are Helm release states to include; empty means deployed+failed.
	Statuses []string
	// ReleaseNames, when non-empty, restricts results to these release names.
	ReleaseNames []string
}

// DefaultStatuses is the status filter applied when the user gives none.
var DefaultStatuses = []string{"deployed", "failed"}

// List returns the releases matching opts, sorted by namespace then name.
func (c *Client) List(opts ListOptions) ([]ReleaseRef, error) {
	constraint, err := parseConstraint(opts.VersionConstraint)
	if err != nil {
		return nil, err
	}

	statuses := opts.Statuses
	if len(statuses) == 0 {
		statuses = DefaultStatuses
	}
	mask, err := statusMask(statuses)
	if err != nil {
		return nil, err
	}

	nameSet := map[string]struct{}{}
	for _, n := range opts.ReleaseNames {
		nameSet[n] = struct{}{}
	}

	searchNamespaces := opts.Namespaces
	if opts.AllNamespaces {
		searchNamespaces = []string{""}
	}

	var out []ReleaseRef
	for _, ns := range searchNamespaces {
		cfg, err := c.ActionConfig(ns)
		if err != nil {
			return nil, err
		}

		lister := newLister(cfg, opts.AllNamespaces, mask)

		rels, err := lister.Run()
		if err != nil {
			if opts.AllNamespaces && apierrors.IsForbidden(err) {
				return nil, fmt.Errorf("listing releases across all namespaces requires cluster-wide read access to helm storage; re-run with explicit -n/--namespace flags: %w", err)
			}
			return nil, fmt.Errorf("listing releases in namespace %q: %w", displayNS(ns), err)
		}

		refs := make([]ReleaseRef, 0, len(rels))
		for _, r := range rels {
			if ref, ok := toRef(r); ok {
				refs = append(refs, ref)
			}
		}
		out = append(out, Filter(refs, nameSet, opts.ChartName, constraint)...)
	}

	sortRefs(out)
	return out, nil
}

// newLister builds the Helm list action. StateMask is assigned last and
// SetStateMask is deliberately not called: it recomputes the mask from the
// boolean fields (defaulting to deployed+failed) and would discard the mask
// derived from --status.
func newLister(cfg *action.Configuration, allNamespaces bool, mask action.ListStates) *action.List {
	lister := action.NewList(cfg)
	lister.All = false
	lister.AllNamespaces = allNamespaces
	lister.StateMask = mask
	return lister
}

// Filter applies the release-name, chart-name and semver-constraint filters.
// It is pure so that the selection logic can be unit-tested without a cluster.
func Filter(refs []ReleaseRef, names map[string]struct{}, chartName string, constraint *semver.Constraints) []ReleaseRef {
	out := make([]ReleaseRef, 0, len(refs))
	for _, ref := range refs {
		if len(names) > 0 {
			if _, want := names[ref.Name]; !want {
				continue
			}
		}
		if chartName != "" && ref.ChartName != chartName {
			continue
		}
		if constraint != nil && !checkConstraint(constraint, ref.ChartVersion) {
			logrus.WithFields(logrus.Fields{
				"release": ref.Name,
				"version": ref.ChartVersion,
			}).Debug("skipping release: chart version outside constraint")
			continue
		}
		out = append(out, ref)
	}
	return out
}

func toRef(r *release.Release) (ReleaseRef, bool) {
	if r == nil || r.Chart == nil || r.Chart.Metadata == nil {
		return ReleaseRef{}, false
	}
	ref := ReleaseRef{
		Name:         r.Name,
		Namespace:    r.Namespace,
		ChartName:    r.Chart.Metadata.Name,
		ChartVersion: r.Chart.Metadata.Version,
		AppVersion:   r.Chart.Metadata.AppVersion,
		Revision:     r.Version,
	}
	if r.Info != nil {
		ref.Status = r.Info.Status.String()
		ref.Updated = r.Info.LastDeployed.Time
	}
	return ref, true
}

func parseConstraint(s string) (*semver.Constraints, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	c, err := semver.NewConstraint(s)
	if err != nil {
		return nil, fmt.Errorf("invalid version constraint %q: %w", s, err)
	}
	return c, nil
}

func checkConstraint(c *semver.Constraints, version string) bool {
	v, err := semver.NewVersion(version)
	if err != nil {
		return false
	}
	return c.Check(v)
}

// statusMask translates status names into Helm's list state bitmask.
func statusMask(statuses []string) (action.ListStates, error) {
	var mask action.ListStates
	for _, s := range statuses {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if s == "all" {
			return action.ListAll, nil
		}
		state := action.ListStates(0).FromName(s)
		if state == action.ListUnknown {
			return 0, fmt.Errorf("unknown release status %q", s)
		}
		mask |= state
	}
	if mask == 0 {
		return 0, fmt.Errorf("no valid release status given")
	}
	return mask, nil
}

func displayNS(ns string) string {
	if ns == "" {
		return "<all>"
	}
	return ns
}

func sortRefs(refs []ReleaseRef) {
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0; j-- {
			a, b := refs[j-1], refs[j]
			if a.Namespace < b.Namespace || (a.Namespace == b.Namespace && a.Name <= b.Name) {
				break
			}
			refs[j-1], refs[j] = b, a
		}
	}
}
