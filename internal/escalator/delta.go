package escalator

import "sort"

type Change struct {
	Before Item `json:"before"`
	After  Item `json:"after"`
}

type Delta struct {
	Added    []Item   `json:"added"`
	Resolved []Item   `json:"resolved"`
	Changed  []Change `json:"changed"`
}

func (d Delta) Empty() bool {
	return len(d.Added) == 0 && len(d.Resolved) == 0 && len(d.Changed) == 0
}

// Compare treats the previous digest only as a delta baseline. Durable source
// state remains authoritative; an absent previous snapshot means every current
// item is newly surfaced.
func Compare(previous *Snapshot, current Snapshot) Delta {
	before := map[string]Item{}
	if previous != nil {
		for _, item := range previous.Items {
			before[item.ID] = item
		}
	}
	after := make(map[string]Item, len(current.Items))
	for _, item := range current.Items {
		after[item.ID] = item
	}
	delta := Delta{}
	for id, item := range after {
		old, exists := before[id]
		if !exists {
			delta.Added = append(delta.Added, item)
			continue
		}
		if old.Fingerprint != item.Fingerprint {
			delta.Changed = append(delta.Changed, Change{Before: old, After: item})
		}
	}
	for id, item := range before {
		if _, exists := after[id]; !exists {
			delta.Resolved = append(delta.Resolved, item)
		}
	}
	sort.Slice(delta.Added, func(i, j int) bool { return delta.Added[i].ID < delta.Added[j].ID })
	sort.Slice(delta.Resolved, func(i, j int) bool { return delta.Resolved[i].ID < delta.Resolved[j].ID })
	sort.Slice(delta.Changed, func(i, j int) bool { return delta.Changed[i].After.ID < delta.Changed[j].After.ID })
	return delta
}
