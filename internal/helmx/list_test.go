package helmx

import (
	"testing"

	"helm.sh/helm/v3/pkg/action"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	helmtime "helm.sh/helm/v3/pkg/time"
)

func mkRelease(name, ns, chartName, version string, revision int, status release.Status) *release.Release {
	return &release.Release{
		Name:      name,
		Namespace: ns,
		Version:   revision,
		Info: &release.Info{
			Status:       status,
			LastDeployed: helmtime.Now(),
		},
		Chart: &chart.Chart{
			Metadata: &chart.Metadata{
				Name:       chartName,
				Version:    version,
				AppVersion: "1.0",
			},
		},
	}
}

func TestToRef(t *testing.T) {
	r := mkRelease("web", "prod", "nginx", "1.2.3", 4, release.StatusDeployed)
	ref, ok := toRef(r)
	if !ok {
		t.Fatal("expected conversion to succeed")
	}
	if ref.Name != "web" || ref.Namespace != "prod" || ref.ChartName != "nginx" ||
		ref.ChartVersion != "1.2.3" || ref.Revision != 4 || ref.Status != "deployed" {
		t.Fatalf("unexpected ref: %+v", ref)
	}

	if _, ok := toRef(nil); ok {
		t.Fatal("nil release must not convert")
	}
	if _, ok := toRef(&release.Release{}); ok {
		t.Fatal("release without chart metadata must not convert")
	}
}

func TestFilter(t *testing.T) {
	refs := []ReleaseRef{
		{Name: "a", Namespace: "ns1", ChartName: "nginx", ChartVersion: "1.0.0"},
		{Name: "b", Namespace: "ns1", ChartName: "nginx", ChartVersion: "2.1.0"},
		{Name: "c", Namespace: "ns2", ChartName: "redis", ChartVersion: "1.5.0"},
		{Name: "d", Namespace: "ns2", ChartName: "nginx", ChartVersion: "not-semver"},
	}

	tests := []struct {
		name       string
		names      []string
		chart      string
		constraint string
		want       []string
	}{
		{name: "no filters", want: []string{"a", "b", "c", "d"}},
		{name: "chart name", chart: "nginx", want: []string{"a", "b", "d"}},
		{name: "chart name and constraint", chart: "nginx", constraint: "<2.0.0", want: []string{"a"}},
		{name: "constraint drops invalid semver", constraint: ">=0.0.1", want: []string{"a", "b", "c"}},
		{name: "explicit names", names: []string{"b", "c"}, want: []string{"b", "c"}},
		{name: "names and chart", names: []string{"b", "c"}, chart: "redis", want: []string{"c"}},
		{name: "no match", chart: "postgres", want: []string{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseConstraint(tc.constraint)
			if err != nil {
				t.Fatalf("parseConstraint: %v", err)
			}
			nameSet := map[string]struct{}{}
			for _, n := range tc.names {
				nameSet[n] = struct{}{}
			}
			got := Filter(refs, nameSet, tc.chart, c)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d results %v, want %v", len(got), names(got), tc.want)
			}
			for i, n := range tc.want {
				if got[i].Name != n {
					t.Fatalf("got %v, want %v", names(got), tc.want)
				}
			}
		})
	}
}

func TestParseConstraintInvalid(t *testing.T) {
	if _, err := parseConstraint("not a constraint"); err == nil {
		t.Fatal("expected an error for an invalid constraint")
	}
	c, err := parseConstraint("  ")
	if err != nil || c != nil {
		t.Fatalf("blank constraint should be a no-op, got %v, %v", c, err)
	}
}

func TestStatusMask(t *testing.T) {
	if _, err := statusMask([]string{"deployed", "failed"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m, err := statusMask([]string{"all"}); err != nil || m == 0 {
		t.Fatalf("all should expand to every state, got %v, %v", m, err)
	}
	if _, err := statusMask([]string{"bogus"}); err == nil {
		t.Fatal("expected an error for an unknown status")
	}
	if _, err := statusMask([]string{" "}); err == nil {
		t.Fatal("expected an error when no status remains")
	}
}

func TestSortRefs(t *testing.T) {
	refs := []ReleaseRef{
		{Name: "z", Namespace: "ns2"},
		{Name: "a", Namespace: "ns2"},
		{Name: "m", Namespace: "ns1"},
	}
	sortRefs(refs)
	want := []string{"m", "a", "z"}
	for i, n := range want {
		if refs[i].Name != n {
			t.Fatalf("got %v, want %v", names(refs), want)
		}
	}
}

func TestVerifyChartName(t *testing.T) {
	ch := &chart.Chart{Metadata: &chart.Metadata{Name: "nginx", Version: "2.0.0"}}
	if err := VerifyChartName(ch, "nginx"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := VerifyChartName(ch, "oci://registry.example.com/charts/nginx"); err != nil {
		t.Fatalf("oci reference should match on the last path element: %v", err)
	}
	if err := VerifyChartName(ch, "redis"); err == nil {
		t.Fatal("expected a mismatch error")
	}
	if err := VerifyChartName(ch, ""); err != nil {
		t.Fatalf("empty expectation should be skipped: %v", err)
	}
}

func TestChartVersion(t *testing.T) {
	if got := chartVersion(nil); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if got := chartVersion(&chart.Chart{}); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if got := chartVersion(&chart.Chart{Metadata: &chart.Metadata{Version: "3.1.4"}}); got != "3.1.4" {
		t.Fatalf("got %q, want 3.1.4", got)
	}
}

func TestDisplayNS(t *testing.T) {
	if displayNS("") != "<all>" {
		t.Fatal("empty namespace should render as <all>")
	}
	if displayNS("prod") != "prod" {
		t.Fatal("named namespace should render verbatim")
	}
}

func names(refs []ReleaseRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

func TestNewListerKeepsStatusMask(t *testing.T) {
	// Regression: calling SetStateMask() after assigning StateMask silently
	// resets the mask to the deployed+failed default, discarding --status.
	mask, err := statusMask([]string{"superseded", "pending-upgrade"})
	if err != nil {
		t.Fatalf("statusMask: %v", err)
	}
	lister := newLister(&action.Configuration{}, true, mask)
	if lister.StateMask != mask {
		t.Fatalf("StateMask = %d, want %d", lister.StateMask, mask)
	}
	if !lister.AllNamespaces || lister.All {
		t.Fatalf("unexpected lister flags: AllNamespaces=%v All=%v", lister.AllNamespaces, lister.All)
	}

	all, err := statusMask([]string{"all"})
	if err != nil {
		t.Fatalf("statusMask(all): %v", err)
	}
	if got := newLister(&action.Configuration{}, false, all).StateMask; got != action.ListAll {
		t.Fatalf("StateMask = %d, want ListAll (%d)", got, action.ListAll)
	}
}

func TestChartNameFromRef(t *testing.T) {
	cases := map[string]string{
		"nginx": "nginx",
		"oci://registry.example.com/charts/redis": "redis",
		"https://charts.example.com/foo/bar":      "bar",
		"":                                        "",
	}
	for in, want := range cases {
		if got := ChartNameFromRef(in); got != want {
			t.Fatalf("ChartNameFromRef(%q) = %q, want %q", in, got, want)
		}
	}
}
