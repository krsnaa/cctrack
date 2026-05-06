//go:build discovery

package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// allowlistedExactPaths is the SINGLE source of truth for what the probe
// is willing to acknowledge from the live response. Each entry maps the
// dotted JSON path to the JSON type the production usageprovider expects
// at that path. Any path not in this map is silently dropped from output —
// neither its name nor its value reaches stdout.
//
// Per F2 S2.1.5/T2.1.5.1 verifier bars: this map mirrors the parser's
// decode contract. There is no second list of "things to suppress" that
// can drift from this list; conservative-by-default by construction.
var allowlistedExactPaths = map[string]string{
	"five_hour.utilization": "number",
	"five_hour.resets_at":   "string",
	"seven_day.utilization": "number",
	"seven_day.resets_at":   "string",
}

// pathStatus reports the role of path relative to allowlistedExactPaths:
//   - exact: path is itself an allowlisted leaf; report and stop recursion.
//   - prefix: path is "" (root) or a strict ancestor of an allowlisted leaf;
//     traversal must continue to find the leaves.
//   - neither: path is unknown; drop silently.
func pathStatus(path string) (exact bool, prefix bool, expectedType string) {
	if t, ok := allowlistedExactPaths[path]; ok {
		return true, false, t
	}
	if path == "" {
		return false, true, ""
	}
	for k := range allowlistedExactPaths {
		if strings.HasPrefix(k, path+".") {
			return false, true, ""
		}
	}
	return false, false, ""
}

// walkResponse traverses raw and writes one line per allowlisted exact
// path. Lines indicate "ok" (correct type, present), "TYPE_MISMATCH"
// (present with wrong type), or "MISSING" (not encountered at all).
// Non-allowlisted paths are silently dropped: their names, types, and
// values do not appear in any output line.
//
// Returns true iff every allowlisted path was encountered with its
// expected type. Callers (the binary) propagate this as an exit code so
// the run is observably failing under schema drift.
func walkResponse(w io.Writer, raw map[string]any) bool {
	seen := map[string]string{} // path -> actual JSON kind

	var walk func(path string, value any)
	walk = func(path string, value any) {
		exact, prefix, _ := pathStatus(path)
		switch {
		case exact:
			seen[path] = jsonType(value)
			// A leaf — do not descend even if it's an object (the allowlist
			// would have separate entries for nested leaves if we cared).
			return
		case prefix:
			obj, ok := value.(map[string]any)
			if !ok {
				return
			}
			keys := make([]string, 0, len(obj))
			for k := range obj {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				child := k
				if path != "" {
					child = path + "." + k
				}
				walk(child, obj[k])
			}
		default:
			return
		}
	}

	walk("", raw)

	paths := make([]string, 0, len(allowlistedExactPaths))
	for p := range allowlistedExactPaths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	allOk := true
	for _, p := range paths {
		expected := allowlistedExactPaths[p]
		actual, ok := seen[p]
		switch {
		case !ok:
			fmt.Fprintf(w, "%-30s  MISSING (expected %s)\n", p, expected)
			allOk = false
		case actual != expected:
			fmt.Fprintf(w, "%-30s  TYPE_MISMATCH (expected %s, got %s)\n", p, expected, actual)
			allOk = false
		default:
			fmt.Fprintf(w, "%-30s  type=%-8s  present=true\n", p, actual)
		}
	}

	return allOk
}

// jsonType returns the canonical name for a Go value's JSON kind, matching
// the kinds expected in allowlistedExactPaths.
func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "unknown"
	}
}
