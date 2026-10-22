package object

import (
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// versionKey identifies a version for quorum agreement: the fields every
// drive shares (not the per-drive erasure index, nor the positional
// fields that depend on the other versions a drive holds).
func versionKey(fi storage.FileInfo) string {
	var b strings.Builder
	b.WriteString(fi.VersionID)
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(fi.ModTime.UnixNano(), 10))
	b.WriteByte('|')
	if tiered(fi) {
		// A transitioned version is its object in the tier, whether a
		// drive released its local data already or not (tier.go).
		b.WriteString("tier:" + fi.Metadata[metaTierObject])
	} else {
		b.WriteString(fi.DataDir)
	}
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(fi.Size, 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatBool(fi.Deleted))
	b.WriteByte('|')
	b.WriteString(fi.Metadata[metaETag])
	e := fi.Erasure
	fmt.Fprintf(&b, "|%d+%d/%d|", e.DataBlocks, e.ParityBlocks, e.BlockSize)
	for _, d := range e.Distribution {
		b.WriteString(strconv.Itoa(d))
		b.WriteByte(',')
	}
	for _, p := range fi.Parts {
		fmt.Fprintf(&b, "|%d:%d:%s", p.Number, p.Size, p.ETag)
	}
	return b.String()
}

// metadataKey identifies the user-visible metadata of a version (tags,
// headers), which an in-place update may have changed on some drives only.
func metadataKey(fi storage.FileInfo) uint64 {
	keys := make([]string, 0, len(fi.Metadata))
	for k := range fi.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := fnv.New64a()
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(fi.Metadata[k]))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// readQuorumFor is the number of agreeing drives a version needs to be
// read: its data shard count, the default for versions without a layout
// (delete markers).
func readQuorumFor(fi storage.FileInfo, defaultQuorum int) int {
	if fi.Erasure.DataBlocks > 0 && !fi.Deleted {
		return fi.Erasure.DataBlocks
	}
	return defaultQuorum
}

// pickValidFileInfo chooses, among the metadata read from the drives of a
// set (infos[i] valid when errs[i] is nil), the version a read quorum of
// drives agree on (TS-U-06): same version id, modification time, data
// directory, size and layout. With several candidates the most supported
// one wins, then the newest. Within the agreeing drives the most common
// metadata wins. agreeing marks the drives of the chosen version. When no
// version has its quorum, the error the drives agree on (not found) is
// returned when it has the default quorum, domain.ErrReadQuorum otherwise.
func pickValidFileInfo(infos []storage.FileInfo, errs []error, defaultQuorum int) (storage.FileInfo, []bool, error) {
	type group struct {
		first int
		count int
		mod   time.Time
	}
	groups := map[string]*group{}
	keys := make([]string, len(infos))
	for i, err := range errs {
		if err != nil {
			continue
		}
		k := versionKey(infos[i])
		keys[i] = k
		g, ok := groups[k]
		if !ok {
			g = &group{first: i, mod: infos[i].ModTime}
			groups[k] = g
		}
		g.count++
	}
	var best *group
	bestKey := ""
	for k, g := range groups {
		if g.count < readQuorumFor(infos[g.first], defaultQuorum) {
			continue
		}
		if best == nil || g.count > best.count || (g.count == best.count && g.mod.After(best.mod)) ||
			(g.count == best.count && g.mod.Equal(best.mod) && g.first < best.first) {
			best, bestKey = g, k
		}
	}
	agreeing := make([]bool, len(infos))
	if best == nil {
		var notFound, noVersion error
		absentCount := 0
		for _, err := range errs {
			switch {
			case err == nil:
			case errors.Is(err, domain.ErrNoSuchVersion):
				absentCount++
				if noVersion == nil {
					noVersion = err
				}
			case isNotFound(err):
				absentCount++
				if notFound == nil {
					notFound = err
				}
			}
		}
		switch {
		case absentCount >= defaultQuorum && noVersion != nil:
			return storage.FileInfo{}, agreeing, noVersion
		case absentCount >= defaultQuorum:
			return storage.FileInfo{}, agreeing, notFound
		case len(groups) > 0:
			return storage.FileInfo{}, agreeing, fmt.Errorf("%w: drives disagree on the version", domain.ErrReadQuorum)
		}
		return storage.FileInfo{}, agreeing, reduceErrs(errs, defaultQuorum, domain.ErrReadQuorum)
	}
	metaCounts := map[uint64]int{}
	chosen, chosenCount := best.first, 0
	for i, k := range keys {
		if errs[i] != nil || k != bestKey {
			continue
		}
		agreeing[i] = true
		mk := metadataKey(infos[i])
		metaCounts[mk]++
		if betterMeta(metaCounts[mk], infos[i], chosenCount, infos[chosen]) {
			chosen, chosenCount = i, metaCounts[mk]
		}
	}
	if tiered(infos[chosen]) && infos[chosen].DataDir != "" {
		// A transitioned version some drives released the local data of:
		// their copy is the one to heal the others with.
		mk := metadataKey(infos[chosen])
		for i, ok := range agreeing {
			if ok && infos[i].DataDir == "" && metadataKey(infos[i]) == mk {
				chosen = i
				break
			}
		}
	}
	return infos[chosen], agreeing, nil
}

// betterMeta reports whether metadata seen count times (fi) beats the
// current choice: the most common one wins, then, among equally common
// ones (an in-place update that reached part of the drives, some of which
// are now offline), the most recently updated.
func betterMeta(count int, fi storage.FileInfo, bestCount int, best storage.FileInfo) bool {
	if count != bestCount {
		return count > bestCount
	}
	return metaStamp(fi.Metadata) > metaStamp(best.Metadata)
}

// mergeVersions returns the versions of an object that a read quorum of
// drives agree on, latest first: perDrive holds the versions each drive
// reported (nil for the drives that do not hold the object). The order is
// the one the drives recorded (descending modification time, the last
// write first among equal times).
func mergeVersions(perDrive [][]storage.FileInfo, defaultQuorum int) []storage.FileInfo {
	if len(perDrive) == 1 && defaultQuorum <= 1 {
		return perDrive[0]
	}
	type candidate struct {
		fi    storage.FileInfo
		count int
		// rank is the first position the version was seen at.
		rank  int
		metas map[uint64]int
		best  int
	}
	byKey := map[string]*candidate{}
	var order []*candidate
	rank := 0
	for _, versions := range perDrive {
		for _, fi := range versions {
			k := versionKey(fi)
			c, ok := byKey[k]
			if !ok {
				c = &candidate{fi: fi, rank: rank, metas: map[uint64]int{}}
				byKey[k] = c
				order = append(order, c)
			}
			rank++
			c.count++
			mk := metadataKey(fi)
			c.metas[mk]++
			if betterMeta(c.metas[mk], fi, c.best, c.fi) {
				c.best, c.fi = c.metas[mk], fi
			}
		}
	}
	out := make([]storage.FileInfo, 0, len(order))
	kept := make([]*candidate, 0, len(order))
	// One entry per version id: while a commit replacing a version (the
	// null version) is reaching the drives, both states may have a quorum;
	// the most supported, then the newest, is listed.
	byID := map[string]int{}
	for _, c := range order {
		if c.count < readQuorumFor(c.fi, defaultQuorum) {
			continue
		}
		if j, dup := byID[c.fi.VersionID]; dup {
			prev := kept[j]
			if c.count > prev.count || (c.count == prev.count && c.fi.ModTime.After(prev.fi.ModTime)) {
				kept[j] = c
			}
			continue
		}
		byID[c.fi.VersionID] = len(kept)
		kept = append(kept, c)
	}
	slices.SortStableFunc(kept, func(a, b *candidate) int {
		if c := b.fi.ModTime.Compare(a.fi.ModTime); c != 0 {
			return c
		}
		return a.rank - b.rank
	})
	for i, c := range kept {
		fi := c.fi
		fi.IsLatest = i == 0
		fi.NumVersions = len(kept)
		fi.Idx = i
		fi.SuccessorModTime = time.Time{}
		if i > 0 {
			fi.SuccessorModTime = kept[i-1].fi.ModTime
		}
		out = append(out, fi)
	}
	return out
}
