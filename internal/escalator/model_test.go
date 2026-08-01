package escalator

import "testing"

func TestSnapshotNormalizeOrdersByBlockedWorkThenAge(t *testing.T) {
	snapshot := Snapshot{Items: []Item{
		{ID: "old", BlockedWork: 1, AgeSeconds: 500},
		{ID: "wide-new", BlockedWork: 3, AgeSeconds: 10},
		{ID: "wide-old", BlockedWork: 3, AgeSeconds: 20},
	}}
	snapshot.Normalize()
	want := []string{"wide-old", "wide-new", "old"}
	for index, id := range want {
		if snapshot.Items[index].ID != id {
			t.Fatalf("items[%d].ID = %q, want %q", index, snapshot.Items[index].ID, id)
		}
	}
}

func TestCompareIgnoresAgeButReportsSourceChangesAndResolution(t *testing.T) {
	previous := Snapshot{Items: []Item{
		{ID: "same", AgeSeconds: 10, Fingerprint: "stable"},
		{ID: "changed", Fingerprint: "before"},
		{ID: "resolved", Fingerprint: "gone"},
	}}
	current := Snapshot{Items: []Item{
		{ID: "same", AgeSeconds: 500, Fingerprint: "stable"},
		{ID: "changed", Fingerprint: "after"},
		{ID: "added", Fingerprint: "new"},
	}}
	delta := Compare(&previous, current)
	if len(delta.Added) != 1 || delta.Added[0].ID != "added" {
		t.Fatalf("added = %#v", delta.Added)
	}
	if len(delta.Resolved) != 1 || delta.Resolved[0].ID != "resolved" {
		t.Fatalf("resolved = %#v", delta.Resolved)
	}
	if len(delta.Changed) != 1 || delta.Changed[0].After.ID != "changed" {
		t.Fatalf("changed = %#v", delta.Changed)
	}
}

func TestCompareSupportsEmptyDigestSuppression(t *testing.T) {
	previous := Snapshot{}
	if delta := Compare(&previous, Snapshot{}); !delta.Empty() {
		t.Fatalf("empty snapshots produced delta %#v", delta)
	}
}
