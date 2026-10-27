package siterepl

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestMerge: the three-way merge of the entries of two sites (S-STO-11):
// what changed on one side wins, a change beats a deletion, the most
// recent of two changes wins, and nothing is deleted without a base.
//
// covers: S-STO-11
func TestMerge(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	it := func(h string, mod time.Time) item { return item{value: []byte(h), Entry: Entry{Hash: h, ModTime: mod}} }
	en := func(h string, mod time.Time) Entry { return Entry{Hash: h, ModTime: mod} }
	local := map[string]item{
		"same":         it("x", t0),
		"localNew":     it("n", t0),
		"localChanged": it("l2", t0),
		"bothNewer":    it("l", t0.Add(time.Hour)),
		"bothOlder":    it("l", t0),
		"remoteDel":    it("v", t0),
		"tie":          it("l", t0),
	}
	remote := map[string]Entry{
		"same":         en("x", t0),
		"remoteNew":    en("r", t0),
		"localChanged": en("l1", t0),
		"bothNewer":    en("r", t0),
		"bothOlder":    en("r", t0.Add(time.Hour)),
		"localDel":     en("d", t0),
		"tie":          en("r", t0),
		"delVsChange":  en("c2", t0),
	}
	base := map[string]string{
		"same": "x", "localChanged": "l1", "bothNewer": "b", "bothOlder": "b", "remoteDel": "v", "localDel": "d",
		"tie": "b", "delVsChange": "c1", "goneBoth": "g",
	}
	newBase, changes := merge(local, remote, base, "aaa", "bbb")
	got := map[string]change{}
	for _, c := range changes {
		got[c.key] = c
	}
	assert.Equal(t, change{key: "localNew", push: true, hash: "n"}, got["localNew"])
	assert.Equal(t, change{key: "remoteNew", hash: "r"}, got["remoteNew"])
	assert.Equal(t, change{key: "localChanged", push: true, hash: "l2"}, got["localChanged"])
	assert.Equal(t, change{key: "bothNewer", push: true, hash: "l"}, got["bothNewer"])
	assert.Equal(t, change{key: "bothOlder", hash: "r"}, got["bothOlder"])
	// Deleted there, unchanged here: deleted here.
	assert.Equal(t, change{key: "remoteDel", hash: ""}, got["remoteDel"])
	// Deleted here, unchanged there: deleted there.
	assert.Equal(t, change{key: "localDel", push: true, hash: ""}, got["localDel"])
	// Same time: the smallest deployment ID wins.
	assert.Equal(t, change{key: "tie", push: true, hash: "l"}, got["tie"])
	// Deleted here, changed there: the change comes back.
	assert.Equal(t, change{key: "delVsChange", hash: "c2"}, got["delVsChange"])
	assert.NotContains(t, got, "same")
	assert.Equal(t, "x", newBase["same"])
	assert.NotContains(t, newBase, "goneBoth")
	assert.Len(t, changes, 9)

	// Without a base, entries missing on one side are added, never
	// deleted.
	_, changes = merge(map[string]item{"a": it("1", t0)}, map[string]Entry{"b": en("2", t0)}, nil, "aaa", "bbb")
	assert.Equal(t, []change{{key: "a", push: true, hash: "1"}, {key: "b", hash: "2"}}, changes)
}
