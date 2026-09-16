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
