package helmx

import (
	"context"
	"io"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	helmtime "helm.sh/helm/v3/pkg/time"
)

// fakeConfig builds an action.Configuration backed by the in-memory storage
// driver and a fake kube client, following Helm's own actionConfigFixture.
func fakeConfig(t *testing.T, failOnUpdate bool) *action.Configuration {
	t.Helper()
	kc := &kubefake.FailingKubeClient{
		PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard},
	}
	if failOnUpdate {
		kc.UpdateError = errBoom
	}
	return &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   kc,
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...interface{}) {},
	}
}

var errBoom = errBoomType{}

type errBoomType struct{}

func (errBoomType) Error() string { return "boom" }

func testChart(name, version string) *chart.Chart {
	return &chart.Chart{
		Metadata: &chart.Metadata{
			APIVersion: chart.APIVersionV2,
			Name:       name,
			Version:    version,
		},
		Templates: []*chart.File{
			{Name: "templates/cm.yaml", Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n")},
		},
	}
}

func seedRelease(t *testing.T, cfg *action.Configuration, name, ns, chartName, version string, vals map[string]interface{}) {
	t.Helper()
	rel := &release.Release{
		Name:      name,
		Namespace: ns,
		Version:   1,
		Info:      &release.Info{Status: release.StatusDeployed, LastDeployed: helmtime.Now()},
		Chart:     testChart(chartName, version),
		Config:    vals,
	}
	if err := cfg.Releases.Create(rel); err != nil {
		t.Fatalf("seeding release %s: %v", name, err)
	}
}

func newTestClient(t *testing.T, cfgs map[string]*action.Configuration) *Client {
	t.Helper()
	c := &Client{configs: map[string]*action.Configuration{}}
	for ns, cfg := range cfgs {
		c.SetActionConfig(ns, cfg)
	}
	return c
}

func TestUpgradeAllSuccess(t *testing.T) {
	cfg := fakeConfig(t, false)
	seedRelease(t, cfg, "web-a", "prod", "nginx", "1.0.0", map[string]interface{}{"replicas": 1})
	seedRelease(t, cfg, "web-b", "prod", "nginx", "1.0.0", nil)
	c := newTestClient(t, map[string]*action.Configuration{"prod": cfg})

	refs := []ReleaseRef{
		{Name: "web-a", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0", Revision: 1},
		{Name: "web-b", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0", Revision: 1},
	}
	ch := testChart("nginx", "2.0.0")

	results, err := c.UpgradeAll(context.Background(), refs, ch, UpgradeOptions{
		Chart:  ChartSource{Ref: "nginx", Version: "2.0.0"},
		Values: map[string]interface{}{"foo": "bar"},
	})
	if err != nil {
		t.Fatalf("UpgradeAll: %v", err)
	}
	for _, r := range results {
		if r.Status != StatusUpgraded {
			t.Fatalf("%s: status %s (%s)", r.Release.Name, r.Status, r.Error)
		}
		if r.NewRevision != 2 || r.NewVersion != "2.0.0" {
			t.Fatalf("%s: revision %d version %q", r.Release.Name, r.NewRevision, r.NewVersion)
		}
	}

	// ReuseValues must preserve the stored values and merge the overrides on top.
	got, err := cfg.Releases.Get("web-a", 2)
	if err != nil {
		t.Fatalf("reading upgraded release: %v", err)
	}
	if got.Config["replicas"] == nil {
		t.Fatalf("stored values were not reused: %#v", got.Config)
	}
	if got.Config["foo"] != "bar" {
		t.Fatalf("override was not merged: %#v", got.Config)
	}
}

func TestUpgradeAllDryRun(t *testing.T) {
	cfg := fakeConfig(t, false)
	seedRelease(t, cfg, "web", "prod", "nginx", "1.0.0", nil)
	c := newTestClient(t, map[string]*action.Configuration{"prod": cfg})

	results, err := c.UpgradeAll(context.Background(),
		[]ReleaseRef{{Name: "web", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0"}},
		testChart("nginx", "2.0.0"),
		UpgradeOptions{DryRun: true},
	)
	if err != nil {
		t.Fatalf("UpgradeAll: %v", err)
	}
	if results[0].Status != StatusDryRun {
		t.Fatalf("status = %s (%s)", results[0].Status, results[0].Error)
	}
	// A dry run must not persist a new revision.
	if _, err := cfg.Releases.Get("web", 2); err == nil {
		t.Fatal("dry run persisted a revision")
	}
}

func TestUpgradeAllStopsOnError(t *testing.T) {
	cfg := fakeConfig(t, true)
	seedRelease(t, cfg, "web-a", "prod", "nginx", "1.0.0", nil)
	seedRelease(t, cfg, "web-b", "prod", "nginx", "1.0.0", nil)
	c := newTestClient(t, map[string]*action.Configuration{"prod": cfg})

	refs := []ReleaseRef{
		{Name: "web-a", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0"},
		{Name: "web-b", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0"},
	}
	ch := testChart("nginx", "2.0.0")

	results, err := c.UpgradeAll(context.Background(), refs, ch, UpgradeOptions{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if results[0].Status != StatusFailed {
		t.Fatalf("first result = %s, want failed", results[0].Status)
	}
	if results[1].Status != StatusSkipped {
		t.Fatalf("second result = %s, want skipped (run must halt)", results[1].Status)
	}
}

func TestUpgradeAllContinueOnError(t *testing.T) {
	cfg := fakeConfig(t, true)
	seedRelease(t, cfg, "web-a", "prod", "nginx", "1.0.0", nil)
	seedRelease(t, cfg, "web-b", "prod", "nginx", "1.0.0", nil)
	c := newTestClient(t, map[string]*action.Configuration{"prod": cfg})

	refs := []ReleaseRef{
		{Name: "web-a", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0"},
		{Name: "web-b", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0"},
	}

	results, err := c.UpgradeAll(context.Background(), refs, testChart("nginx", "2.0.0"),
		UpgradeOptions{ContinueOnError: true})
	if err == nil {
		t.Fatal("expected the first failure to be reported")
	}
	for _, r := range results {
		if r.Status != StatusFailed {
			t.Fatalf("%s: status = %s, want failed", r.Release.Name, r.Status)
		}
	}
}

func TestUpgradeAllCancelledContext(t *testing.T) {
	cfg := fakeConfig(t, false)
	seedRelease(t, cfg, "web", "prod", "nginx", "1.0.0", nil)
	c := newTestClient(t, map[string]*action.Configuration{"prod": cfg})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results, err := c.UpgradeAll(ctx,
		[]ReleaseRef{{Name: "web", Namespace: "prod", ChartName: "nginx"}},
		testChart("nginx", "2.0.0"), UpgradeOptions{})
	if err == nil {
		t.Fatal("expected a context error")
	}
	if results[0].Status != StatusSkipped {
		t.Fatalf("status = %s, want skipped", results[0].Status)
	}
}

// TestUpgradeAllIsolatesReleaseValues is the regression test for the bug where a
// single shared *chart.Chart and a single shared values map were handed to every
// release: Helm writes the previous release's coalesced values into chart.Values
// (action/upgrade.go reuseValues) and coalesces the old config into the values
// map in place, so the first release's values leaked into all the others.
func TestUpgradeAllIsolatesReleaseValues(t *testing.T) {
	cfg := fakeConfig(t, false)
	seedRelease(t, cfg, "web-a", "prod", "nginx", "1.0.0",
		map[string]interface{}{"onlyA": "a", "shared": "from-a"})
	seedRelease(t, cfg, "web-b", "prod", "nginx", "1.0.0",
		map[string]interface{}{"onlyB": "b", "shared": "from-b"})
	c := newTestClient(t, map[string]*action.Configuration{"prod": cfg})

	refs := []ReleaseRef{
		{Name: "web-a", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0", Revision: 1},
		{Name: "web-b", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0", Revision: 1},
	}
	ch := testChart("nginx", "2.0.0")
	ch.Values = map[string]interface{}{"shared": "chart-default"}
	sharedVals := map[string]interface{}{"cliFlag": "set"}

	if _, err := c.UpgradeAll(context.Background(), refs, ch, UpgradeOptions{
		Chart:  ChartSource{Ref: "nginx", Version: "2.0.0"},
		Values: sharedVals,
	}); err != nil {
		t.Fatalf("UpgradeAll: %v", err)
	}

	b, err := cfg.Releases.Get("web-b", 2)
	if err != nil {
		t.Fatalf("reading web-b: %v", err)
	}
	if _, leaked := b.Config["onlyA"]; leaked {
		t.Fatalf("web-a values leaked into web-b: %#v", b.Config)
	}
	if b.Config["shared"] != "from-b" {
		t.Fatalf("web-b kept the wrong value for shared: %#v", b.Config)
	}
	if b.Config["cliFlag"] != "set" {
		t.Fatalf("override missing on web-b: %#v", b.Config)
	}

	// The caller's chart and values map must come back untouched.
	if ch.Values["shared"] != "chart-default" {
		t.Fatalf("shared chart values were mutated: %#v", ch.Values)
	}
	if len(sharedVals) != 1 {
		t.Fatalf("shared values map was mutated: %#v", sharedVals)
	}
}

func TestUpgradeAllBaseRevisionOffset(t *testing.T) {
	cfg := fakeConfig(t, false)
	seedRelease(t, cfg, "web", "prod", "nginx", "1.0.0", map[string]interface{}{"good": "yes"})
	// Revision 2 carries the values we want to discard.
	rel2 := &release.Release{
		Name:      "web",
		Namespace: "prod",
		Version:   2,
		Info:      &release.Info{Status: release.StatusDeployed, LastDeployed: helmtime.Now()},
		Chart:     testChart("nginx", "1.0.0"),
		Config:    map[string]interface{}{"bad": "yes"},
	}
	if err := cfg.Releases.Create(rel2); err != nil {
		t.Fatalf("seeding revision 2: %v", err)
	}
	c := newTestClient(t, map[string]*action.Configuration{"prod": cfg})

	results, err := c.UpgradeAll(context.Background(),
		[]ReleaseRef{{Name: "web", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0", Revision: 2}},
		testChart("nginx", "2.0.0"),
		UpgradeOptions{BaseRevisionOffset: 1, Values: map[string]interface{}{"cli": "override"}},
	)
	if err != nil {
		t.Fatalf("UpgradeAll: %v", err)
	}
	if results[0].Status != StatusUpgraded {
		t.Fatalf("status = %s (%s)", results[0].Status, results[0].Error)
	}

	got, err := cfg.Releases.Get("web", 3)
	if err != nil {
		t.Fatalf("reading upgraded release: %v", err)
	}
	if got.Config["good"] != "yes" {
		t.Fatalf("revision 1 values were not used as base: %#v", got.Config)
	}
	if _, bad := got.Config["bad"]; bad {
		t.Fatalf("current revision values were reused: %#v", got.Config)
	}
	if got.Config["cli"] != "override" {
		t.Fatalf("override missing: %#v", got.Config)
	}
	if results[0].BaseRevision != 1 {
		t.Fatalf("base revision not reported: %#v", results[0])
	}
}

func TestUpgradeAllBaseRevisionOffsetTooLarge(t *testing.T) {
	cfg := fakeConfig(t, false)
	seedRelease(t, cfg, "web", "prod", "nginx", "1.0.0", nil)
	c := newTestClient(t, map[string]*action.Configuration{"prod": cfg})

	results, _ := c.UpgradeAll(context.Background(),
		[]ReleaseRef{{Name: "web", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0", Revision: 1}},
		testChart("nginx", "2.0.0"),
		UpgradeOptions{BaseRevisionOffset: 3},
	)
	if results[0].Status != StatusFailed {
		t.Fatalf("expected failure, got %s", results[0].Status)
	}
}

func TestResolveBaseRevisions(t *testing.T) {
	cfg := fakeConfig(t, false)
	seedRelease(t, cfg, "web", "prod", "nginx", "1.0.0", nil)
	rel2 := &release.Release{
		Name:      "web",
		Namespace: "prod",
		Version:   2,
		Info:      &release.Info{Status: release.StatusDeployed, LastDeployed: helmtime.Now()},
		Chart:     testChart("nginx", "1.1.0"),
	}
	if err := cfg.Releases.Create(rel2); err != nil {
		t.Fatalf("seeding revision 2: %v", err)
	}
	c := newTestClient(t, map[string]*action.Configuration{"prod": cfg})

	refs := []ReleaseRef{
		{Name: "web", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.1.0", Revision: 2},
		{Name: "missing", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.1.0", Revision: 1},
	}

	// Offset 0 must leave the refs untouched.
	if got := c.ResolveBaseRevisions(refs, 0); got[0].BaseRevision != 0 || got[0].BaseChartVersion != "" {
		t.Fatalf("offset 0 annotated the refs: %#v", got[0])
	}

	got := c.ResolveBaseRevisions(refs, 1)
	if got[0].BaseRevision != 1 || got[0].BaseChartVersion != "1.0.0" {
		t.Fatalf("base revision not resolved: %#v", got[0])
	}
	if got[1].BaseRevision != -1 {
		t.Fatalf("unresolvable release not marked: %#v", got[1])
	}
	// The caller's slice must not be modified in place.
	if refs[0].BaseRevision != 0 {
		t.Fatalf("input refs were mutated: %#v", refs[0])
	}
}
