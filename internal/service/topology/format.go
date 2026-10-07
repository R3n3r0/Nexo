package topology

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// FormatVersion is the current version of the format.json document.
const FormatVersion = 1

// Values of FormatV1.Mode as recorded on disk (docs/08 §8.1).
const (
	// FormatModeSNSD marks a drive used by a single-node single-drive deployment.
	FormatModeSNSD = "snsd"
	// FormatModeErasure marks a drive used by an erasure coded deployment (SNMD or distributed).
	FormatModeErasure = "erasure"
)

// DistributionAlgoV1 is the object placement algorithm recorded in format.json:
// SipHash-2-4(objectKey, deploymentID) mod numSets, with the shard permutation
// derived from the key (docs/03 §3.2).
const DistributionAlgoV1 = "SIPMOD+PARITY"

// FormatV1 is the content of <drive>/.nexo.sys/format.json. Every drive of a
// pool carries the same document except for Erasure.This.
type FormatV1 struct {
	// Version is the document version (FormatVersion).
	Version int `json:"version"`
	// ID is the deployment identifier shared by every drive of the deployment.
	ID string `json:"id"`
	// Mode is FormatModeSNSD or FormatModeErasure.
	Mode string `json:"mode"`
	// Erasure describes the position of the drive in the pool.
	Erasure FormatErasure `json:"erasure"`
}

// FormatErasure describes the erasure sets of a pool and the drive that holds the document.
type FormatErasure struct {
	// This is the identifier of the drive holding the document.
	This string `json:"this"`
	// Sets lists, for every erasure set, the identifiers of its drives in disk order.
	Sets [][]string `json:"sets"`
	// DistributionAlgo is the object placement algorithm (DistributionAlgoV1).
	DistributionAlgo string `json:"distributionAlgo"`
}

// FormatModeFor returns the format.json mode recorded for a deployment mode.
func FormatModeFor(mode Mode) string {
	if mode == ModeSNSD {
		return FormatModeSNSD
	}
	return FormatModeErasure
}

// Marshal encodes the document as indented JSON suitable for format.json.
func (f *FormatV1) Marshal() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFormat, err)
	}
	return append(data, '\n'), nil
}

// UnmarshalFormat decodes and validates a format.json document.
func UnmarshalFormat(data []byte) (*FormatV1, error) {
	var f FormatV1
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFormat, err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate checks the structure of the document: supported version, non-empty
// deployment id, known mode, rectangular non-empty sets of unique drive ids,
// This present in the sets and a known distribution algorithm.
func (f *FormatV1) Validate() error {
	if f == nil {
		return fmt.Errorf("%w: nil document", ErrInvalidFormat)
	}
	if f.Version != FormatVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrFormatVersion, f.Version, FormatVersion)
	}
	if f.ID == "" {
		return fmt.Errorf("%w: empty deployment id", ErrInvalidFormat)
	}
	if f.Mode != FormatModeSNSD && f.Mode != FormatModeErasure {
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidFormat, f.Mode)
	}
	if f.Erasure.DistributionAlgo != DistributionAlgoV1 {
		return fmt.Errorf("%w: unknown distribution algorithm %q", ErrInvalidFormat, f.Erasure.DistributionAlgo)
	}
	if f.Erasure.This == "" {
		return fmt.Errorf("%w: empty drive id", ErrInvalidFormat)
	}
	if len(f.Erasure.Sets) == 0 {
		return fmt.Errorf("%w: no erasure sets", ErrInvalidFormat)
	}
	width := len(f.Erasure.Sets[0])
	if width == 0 {
		return fmt.Errorf("%w: empty erasure set", ErrInvalidFormat)
	}
	if f.Mode == FormatModeSNSD && (len(f.Erasure.Sets) != 1 || width != 1) {
		return fmt.Errorf("%w: snsd requires exactly one set with one drive", ErrInvalidFormat)
	}
	if f.Mode == FormatModeErasure && (width < MinSetDriveCount || width > MaxSetDriveCount) {
		return fmt.Errorf("%w: set size %d outside [%d..%d]", ErrInvalidFormat, width, MinSetDriveCount, MaxSetDriveCount)
	}
	ids := make(map[string]struct{}, len(f.Erasure.Sets)*width)
	for s, set := range f.Erasure.Sets {
		if len(set) != width {
			return fmt.Errorf("%w: set %d has %d drives, set 0 has %d", ErrInvalidFormat, s, len(set), width)
		}
		for d, id := range set {
			if id == "" {
				return fmt.Errorf("%w: empty drive id at set %d disk %d", ErrInvalidFormat, s, d)
			}
			if _, dup := ids[id]; dup {
				return fmt.Errorf("%w: duplicate drive id %q", ErrInvalidFormat, id)
			}
			ids[id] = struct{}{}
		}
	}
	if _, ok := ids[f.Erasure.This]; !ok {
		return fmt.Errorf("%w: drive id %q is not part of any set", ErrInvalidFormat, f.Erasure.This)
	}
	return nil
}

// SetCount returns the number of erasure sets.
func (f *FormatV1) SetCount() int { return len(f.Erasure.Sets) }

// SetDriveCount returns the number of drives per erasure set (0 for an empty document).
func (f *FormatV1) SetDriveCount() int {
	if len(f.Erasure.Sets) == 0 {
		return 0
	}
	return len(f.Erasure.Sets[0])
}

// Position returns the set and disk index of the drive holding the document.
func (f *FormatV1) Position() (set, disk int, err error) {
	for s, ids := range f.Erasure.Sets {
		for d, id := range ids {
			if id == f.Erasure.This {
				return s, d, nil
			}
		}
	}
	return -1, -1, fmt.Errorf("%w: drive id %q is not part of any set", ErrInvalidFormat, f.Erasure.This)
}

// Clone returns a deep copy of the document.
func (f *FormatV1) Clone() *FormatV1 {
	if f == nil {
		return nil
	}
	c := *f
	c.Erasure.Sets = make([][]string, len(f.Erasure.Sets))
	for i, set := range f.Erasure.Sets {
		c.Erasure.Sets[i] = append([]string(nil), set...)
	}
	return &c
}

// SameTopology reports whether two documents describe the same deployment and
// the same erasure sets, regardless of the drive holding them.
func (f *FormatV1) SameTopology(o *FormatV1) bool {
	if f == nil || o == nil {
		return false
	}
	if f.Version != o.Version || f.ID != o.ID || f.Mode != o.Mode || f.Erasure.DistributionAlgo != o.Erasure.DistributionAlgo {
		return false
	}
	if len(f.Erasure.Sets) != len(o.Erasure.Sets) {
		return false
	}
	for i := range f.Erasure.Sets {
		if len(f.Erasure.Sets[i]) != len(o.Erasure.Sets[i]) {
			return false
		}
		for j := range f.Erasure.Sets[i] {
			if f.Erasure.Sets[i][j] != o.Erasure.Sets[i][j] {
				return false
			}
		}
	}
	return true
}

// MatchesPool checks that the document is compatible with a pool of a
// deployment running in mode: same mode family (snsd vs erasure), same set
// count and same set drive count.
func (f *FormatV1) MatchesPool(mode Mode, p Pool) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if want := FormatModeFor(mode); f.Mode != want {
		return fmt.Errorf("%w: drive formatted as %q, deployment runs as %q (%s)", ErrFormatModeMismatch, f.Mode, want, mode)
	}
	if f.SetCount() != p.SetCount || f.SetDriveCount() != p.SetDriveCount {
		return fmt.Errorf("%w: drive has %d set(s) x %d, pool %d has %d set(s) x %d",
			ErrFormatLayoutMismatch, f.SetCount(), f.SetDriveCount(), p.Index+1, p.SetCount, p.SetDriveCount)
	}
	return nil
}

// CheckFormats verifies that the documents read from the drives of a pool are
// consistent: same deployment id, mode, algorithm and sets, every drive id
// unique, every document valid. Nil entries (unreadable drives) are skipped.
// It returns the first non-nil document as the reference.
func CheckFormats(formats []*FormatV1) (*FormatV1, error) {
	var ref *FormatV1
	refIdx := -1
	seen := make(map[string]int)
	for i, f := range formats {
		if f == nil {
			continue
		}
		if err := f.Validate(); err != nil {
			return nil, fmt.Errorf("drive %d: %w", i, err)
		}
		if ref == nil {
			ref, refIdx = f, i
		} else if !ref.SameTopology(f) {
			return nil, fmt.Errorf("%w: drive %d (deployment %s, %d set(s) x %d) differs from drive %d (deployment %s, %d set(s) x %d)",
				ErrFormatMismatch, i, f.ID, f.SetCount(), f.SetDriveCount(), refIdx, ref.ID, ref.SetCount(), ref.SetDriveCount())
		}
		if prev, dup := seen[f.Erasure.This]; dup {
			return nil, fmt.Errorf("%w: drives %d and %d share drive id %q", ErrFormatMismatch, prev, i, f.Erasure.This)
		}
		seen[f.Erasure.This] = i
	}
	if ref == nil {
		return nil, fmt.Errorf("%w: no readable format.json", ErrInvalidFormat)
	}
	return ref, nil
}

// NewFormats builds one document per drive of pool, in Pool.Endpoints order,
// sharing deploymentID and a set map filled with identifiers from ids.
func NewFormats(mode Mode, p Pool, deploymentID string, ids IDGenerator) ([]*FormatV1, error) {
	if deploymentID == "" {
		return nil, fmt.Errorf("%w: empty deployment id", ErrInvalidFormat)
	}
	if p.SetCount < 1 || p.SetDriveCount < 1 || p.SetCount*p.SetDriveCount != len(p.Endpoints) {
		return nil, fmt.Errorf("%w: pool %d has %d drives but %d set(s) x %d", ErrInvalidLayout, p.Index, len(p.Endpoints), p.SetCount, p.SetDriveCount)
	}
	sets := make([][]string, p.SetCount)
	for s := range sets {
		sets[s] = make([]string, p.SetDriveCount)
		for d := range sets[s] {
			sets[s][d] = ids.NewID()
		}
	}
	out := make([]*FormatV1, 0, len(p.Endpoints))
	for i := range p.Endpoints {
		f := &FormatV1{
			Version: FormatVersion,
			ID:      deploymentID,
			Mode:    FormatModeFor(mode),
			Erasure: FormatErasure{
				This:             sets[i/p.SetDriveCount][i%p.SetDriveCount],
				Sets:             sets,
				DistributionAlgo: DistributionAlgoV1,
			},
		}
		out = append(out, f.Clone())
	}
	return out, nil
}
