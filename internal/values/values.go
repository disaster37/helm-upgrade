// Package values parses the -f/--set family of flags into a single merged map.
package values

import (
	"fmt"
	"sort"

	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/getter"
)

// Options mirrors the value-related CLI flags.
type Options struct {
	ValueFiles   []string
	Values       []string
	StringValues []string
	JSONValues   []string
	FileValues   []string
}

// Merge resolves all value sources into one map. Precedence follows Helm's own
// rules: -f files first (in order), then --set, --set-string, --set-file and
// --set-json on top.
func Merge(o Options, settings *cli.EnvSettings) (map[string]interface{}, error) {
	opts := values.Options{
		ValueFiles:   o.ValueFiles,
		Values:       o.Values,
		StringValues: o.StringValues,
		JSONValues:   o.JSONValues,
		FileValues:   o.FileValues,
	}
	merged, err := opts.MergeValues(getter.All(settings))
	if err != nil {
		return nil, fmt.Errorf("merging values: %w", err)
	}
	return merged, nil
}

// TopLevelKeys returns the sorted top-level keys of a values map. It is used for
// debug logging, so that override *keys* can be logged without ever printing the
// values themselves (they routinely contain secrets).
func TopLevelKeys(vals map[string]interface{}) []string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
