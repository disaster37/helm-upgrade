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

// Plan renders the releases that a run would change.
func Plan(w io.Writer, f Format, refs []helmx.ReleaseRef, newVersion string) error {
	if f != FormatTable {
		return encode(w, f, refs)
	}
	t := newTable(w, []string{"NAME", "NAMESPACE", "CHART", "CURRENT_VERSION", "TARGET_VERSION"})
	for _, r := range refs {
		t.Append([]string{r.Name, r.Namespace, r.ChartName, r.ChartVersion, newVersion})
	}
	t.Render()
	return nil
}

// Results renders the summary of an upgrade run.
func Results(w io.Writer, f Format, results []helmx.Result) error {
	if f != FormatTable {
		return encode(w, f, results)
	}
	t := newTable(w, []string{"NAME", "NAMESPACE", "STATUS", "NEW_VERSION", "REVISION", "ERROR"})
	for _, r := range results {
		rev := ""
		if r.NewRevision > 0 {
			rev = fmt.Sprintf("%d", r.NewRevision)
		}
		t.Append([]string{r.Release.Name, r.Release.Namespace, r.Status, r.NewVersion, rev, r.Error})
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
