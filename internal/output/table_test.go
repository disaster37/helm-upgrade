package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/example/helm-upgrade/internal/helmx"
)

var sample = []helmx.ReleaseRef{
	{
		Name: "web", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.0.0",
		AppVersion: "1.25", Revision: 3, Status: "deployed",
		Updated: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	},
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{
		"table": FormatTable, "JSON": FormatJSON, " yaml ": FormatYAML,
	} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Fatalf("ParseFormat(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Fatal("expected an error for an unknown format")
	}
}

func TestReleasesTable(t *testing.T) {
	var buf bytes.Buffer
	if err := Releases(&buf, FormatTable, sample); err != nil {
		t.Fatalf("Releases: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "CHART_VERSION", "web", "prod", "nginx", "1.0.0", "deployed", "2026-01-02T03:04:05Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestReleasesJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := Releases(&buf, FormatJSON, sample); err != nil {
		t.Fatalf("Releases: %v", err)
	}
	var got []helmx.ReleaseRef
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(got) != 1 || got[0].Name != "web" || got[0].ChartVersion != "1.0.0" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestReleasesYAML(t *testing.T) {
	var buf bytes.Buffer
	if err := Releases(&buf, FormatYAML, sample); err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if !strings.Contains(buf.String(), "chartVersion: 1.0.0") {
		t.Fatalf("unexpected yaml:\n%s", buf.String())
	}
}

func TestPlan(t *testing.T) {
	var buf bytes.Buffer
	if err := Plan(&buf, FormatTable, sample, "2.0.0"); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "TARGET_VERSION") || !strings.Contains(out, "2.0.0") {
		t.Fatalf("unexpected plan output:\n%s", out)
	}
}

func TestResults(t *testing.T) {
	results := []helmx.Result{
		{Release: sample[0], Status: helmx.StatusUpgraded, NewRevision: 4, NewVersion: "2.0.0"},
		{Release: sample[0], Status: helmx.StatusFailed, Error: "boom"},
	}

	var table bytes.Buffer
	if err := Results(&table, FormatTable, results); err != nil {
		t.Fatalf("Results: %v", err)
	}
	for _, want := range []string{"upgraded", "failed", "boom", "2.0.0", "4"} {
		if !strings.Contains(table.String(), want) {
			t.Fatalf("results table missing %q:\n%s", want, table.String())
		}
	}

	var js bytes.Buffer
	if err := Results(&js, FormatJSON, results); err != nil {
		t.Fatalf("Results json: %v", err)
	}
	var decoded []helmx.Result
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil {
		t.Fatalf("results json invalid: %v", err)
	}
	if decoded[1].Error != "boom" {
		t.Fatalf("unexpected decoded results: %+v", decoded)
	}
}

func TestEncodeUnsupported(t *testing.T) {
	if err := encode(&bytes.Buffer{}, Format("xml"), sample); err == nil {
		t.Fatal("expected an error for an unsupported format")
	}
}

func TestFormatTimeZero(t *testing.T) {
	if got := formatTime(time.Time{}); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestPlanShowsBaseRevisionVersion(t *testing.T) {
	refs := []helmx.ReleaseRef{
		{Name: "web-a", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.5.0",
			Revision: 3, BaseRevision: 2, BaseChartVersion: "1.4.0"},
		{Name: "web-b", Namespace: "prod", ChartName: "nginx", ChartVersion: "1.5.0",
			Revision: 1, BaseRevision: -1},
	}

	var buf bytes.Buffer
	if err := Plan(&buf, FormatTable, refs, "2.0.0"); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	out := buf.String()

	// CURRENT_VERSION must show the base revision's chart version, not the
	// version deployed right now.
	for _, want := range []string{"BASE_REVISION", "1.4.0", "?"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "1.5.0") {
		t.Fatalf("plan showed the current chart version instead of the base one:\n%s", out)
	}
}

func TestPlanWithoutBaseRevisionUnchanged(t *testing.T) {
	var buf bytes.Buffer
	if err := Plan(&buf, FormatTable, sample, "2.0.0"); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if strings.Contains(buf.String(), "BASE_REVISION") {
		t.Fatalf("base revision column shown without an offset:\n%s", buf.String())
	}
}
