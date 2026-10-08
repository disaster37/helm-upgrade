// Package output renders results as a table, JSON or YAML.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/olekukonko/tablewriter"
	"sigs.k8s.io/yaml"

	"github.com/example/helm-upgrade/internal/helmx"
)

// Format is the requested rendering format.
type Format string

// Supported formats.
const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// ParseFormat validates a user-supplied format string.
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case FormatTable:
		return FormatTable, nil
	case FormatJSON:
		return FormatJSON, nil
	case FormatYAML:
		return FormatYAML, nil
	default:
		return "", fmt.Errorf("unknown output format %q (want table, json or yaml)", s)
	}
}

// Releases renders a release list.
func Releases(w io.Writer, f Format, refs []helmx.ReleaseRef) error {
	if f != FormatTable {
		return encode(w, f, refs)
	}
	t := newTable(w, []string{"NAME", "NAMESPACE", "CHART", "CHART_VERSION", "APP_VERSION", "REVISION", "STATUS", "UPDATED"})
	for _, r := range refs {
		t.Append([]string{
			r.Name, r.Namespace, r.ChartName, r.ChartVersion, r.AppVersion,
			fmt.Sprintf("%d", r.Revision), r.Status, formatTime(r.Updated),
		})
	}
	t.Render()
	return nil
}

// unknownValue marks a cell whose real value could not be determined.
const unknownValue = "?"

// Plan renders the releases that a run would change.
func Plan(w io.Writer, f Format, refs []helmx.ReleaseRef, newVersion string) error {
	if f != FormatTable {
		return encode(w, f, refs)
	}
	// With --base-revision-offset the run starts from an older revision, so
	// CURRENT_VERSION shows that revision's chart version instead of the one
	// deployed right now, and its number is spelled out next to it.
	showBase := false
	for _, r := range refs {
		if r.BaseRevision != 0 {
			showBase = true
			break
		}
	}

	header := []string{"NAME", "NAMESPACE", "CHART", "CURRENT_VERSION", "TARGET_VERSION"}
	if showBase {
		header = []string{"NAME", "NAMESPACE", "CHART", "CURRENT_VERSION", "BASE_REVISION", "TARGET_VERSION"}
	}
	t := newTable(w, header)

	for _, r := range refs {
		current, base := r.ChartVersion, ""
		switch {
		case r.BaseRevision > 0:
			current = r.BaseChartVersion
			if current == "" {
				current = unknownValue
			}
			base = fmt.Sprintf("%d", r.BaseRevision)
		case r.BaseRevision < 0:
			// The offset could not be resolved; do not imply a version.
			current, base = unknownValue, unknownValue
		}

		row := []string{r.Name, r.Namespace, r.ChartName, current}
		if showBase {
			row = append(row, base)
		}
		t.Append(append(row, newVersion))
	}
	t.Render()
	return nil
}

// Results renders the summary of an upgrade run.
func Results(w io.Writer, f Format, results []helmx.Result) error {
	if f != FormatTable {
		return encode(w, f, results)
	}
	// The base revision column only appears when values were taken from an
	// older revision, so ordinary runs keep their narrower table.
	showBase := false
	for _, r := range results {
		if r.BaseRevision > 0 {
			showBase = true
			break
		}
	}

	header := []string{"NAME", "NAMESPACE", "STATUS", "NEW_VERSION", "REVISION"}
	if showBase {
		header = append(header, "BASE_REVISION")
	}
	header = append(header, "ERROR")
	t := newTable(w, header)

	for _, r := range results {
		rev := ""
		if r.NewRevision > 0 {
			rev = fmt.Sprintf("%d", r.NewRevision)
		}
		row := []string{r.Release.Name, r.Release.Namespace, r.Status, r.NewVersion, rev}
		if showBase {
			base := ""
			if r.BaseRevision > 0 {
				base = fmt.Sprintf("%d", r.BaseRevision)
			}
			row = append(row, base)
		}
		t.Append(append(row, r.Error))
	}
	t.Render()
	return nil
}

func newTable(w io.Writer, headers []string) *tablewriter.Table {
	t := tablewriter.NewWriter(w)
	t.SetHeader(headers)
	t.SetAutoFormatHeaders(false)
	t.SetAutoWrapText(false)
	t.SetBorder(false)
	t.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	t.SetAlignment(tablewriter.ALIGN_LEFT)
	t.SetCenterSeparator("")
	t.SetColumnSeparator("")
	t.SetRowSeparator("")
	t.SetHeaderLine(false)
	t.SetTablePadding("   ")
	t.SetNoWhiteSpace(true)
	return t
}

func encode(w io.Writer, f Format, v interface{}) error {
	switch f {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case FormatYAML:
		b, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	default:
		return fmt.Errorf("unsupported format %q", f)
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
