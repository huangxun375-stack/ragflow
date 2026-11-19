package codearchive

import "encoding/json"

const (
	maxFilteredEntries = 512
	maxFilteredBytes   = 128 << 10
)

type filteredRecorder struct {
	stats   *Stats
	policy  Policy
	ignore  *ignoreMatcher
	indices map[string]int
	bytes   int
}

func (r *filteredRecorder) add(name, reason string) {
	item := FilteredEntry{Path: name, Type: "file", Reason: reason, FileCount: 1}
	// Only a rule that excludes a directory can collapse its descendants.
	// A suffix match on one child must never hide accepted siblings.
	for i := 0; i < len(name); i++ {
		if name[i] != '/' {
			continue
		}
		prefix := name[:i]
		why := fixedSkip(prefix, true, r.policy)
		if why == "" && r.ignore.match(prefix, true) {
			why = "gitignore"
		}
		if why == "" && hidden(prefix) {
			why = "hidden"
		}
		if why != "" {
			item.Path, item.Type, item.Reason = prefix, "directory", why
			break
		}
	}
	if index, ok := r.indices[item.Path]; ok {
		r.stats.FilteredEntries[index].FileCount++
		return
	}
	// Reserve the largest count representation so later aggregation cannot
	// exceed the byte cap. json.Marshal also accounts for escaped path bytes.
	reserved := item
	reserved.FileCount = int(^uint(0) >> 1)
	raw, _ := json.Marshal(reserved)
	cost := len(raw) + 1
	if len(r.stats.FilteredEntries) >= maxFilteredEntries || r.bytes+cost+2 > maxFilteredBytes {
		r.stats.FilteredEntriesTruncated = true
		return
	}
	if r.indices == nil {
		r.indices = make(map[string]int)
	}
	r.indices[item.Path] = len(r.stats.FilteredEntries)
	r.stats.FilteredEntries = append(r.stats.FilteredEntries, item)
	r.bytes += cost
}
