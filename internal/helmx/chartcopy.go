package helmx

import (
	"helm.sh/helm/v3/pkg/chart"
)

// cloneChart returns a private copy of ch that can safely be handed to a single
// action.Upgrade run.
//
// Helm mutates the chart it is given: action.Upgrade.reuseValues() assigns the
// previous release's coalesced values to chart.Values, and
// chartutil.ProcessDependenciesWithMerge() rewrites chart.Values, the dependency
// list and each Dependency.ImportValues. Sharing one *chart.Chart across several
// releases therefore leaks the first release's values into all the later ones.
//
// Only the parts Helm writes to are deep-copied; template/file payloads are
// treated as read-only and shared.
func cloneChart(ch *chart.Chart) *chart.Chart {
	if ch == nil {
		return nil
	}

	out := new(chart.Chart)
	*out = *ch

	if ch.Metadata != nil {
		md := *ch.Metadata
		if ch.Metadata.Dependencies != nil {
			deps := make([]*chart.Dependency, len(ch.Metadata.Dependencies))
			for i, d := range ch.Metadata.Dependencies {
				if d == nil {
					continue
				}
				cp := *d
				cp.Tags = append([]string(nil), d.Tags...)
				cp.ImportValues = append([]interface{}(nil), d.ImportValues...)
				deps[i] = &cp
			}
			md.Dependencies = deps
		}
		if ch.Metadata.Annotations != nil {
			ann := make(map[string]string, len(ch.Metadata.Annotations))
			for k, v := range ch.Metadata.Annotations {
				ann[k] = v
			}
			md.Annotations = ann
		}
		md.Maintainers = append([]*chart.Maintainer(nil), ch.Metadata.Maintainers...)
		md.Keywords = append([]string(nil), ch.Metadata.Keywords...)
		md.Sources = append([]string(nil), ch.Metadata.Sources...)
		out.Metadata = &md
	}

	out.Values = deepCopyValues(ch.Values)

	// Subcharts are coalesced and pruned as well, so each one needs its own copy.
	if deps := ch.Dependencies(); len(deps) > 0 {
		clones := make([]*chart.Chart, 0, len(deps))
		for _, d := range deps {
			clones = append(clones, cloneChart(d))
		}
		out.SetDependencies(clones...)
	}

	return out
}

// deepCopyValues copies a Helm values tree. Maps and slices are rebuilt; scalars
// are shared, which is safe because Helm never mutates them in place.
func deepCopyValues(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return deepCopyValues(t)
	case map[interface{}]interface{}:
		out := make(map[interface{}]interface{}, len(t))
		for k, vv := range t {
			out[k] = deepCopyValue(vv)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, vv := range t {
			out[i] = deepCopyValue(vv)
		}
		return out
	default:
		return v
	}
}
