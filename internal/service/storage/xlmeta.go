package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// xlMetaVersion is the current version of the xl.meta document.
const xlMetaVersion = 1

// xlMeta is the on-disk metadata document of an object: every version in
// descending modification time order (the latest first). The document is
// encoded as JSON; small objects carry their content inline in Data.
type xlMeta struct {
	Version  int         `json:"version"`
	Versions []xlVersion `json:"versions"`
}

// xlVersion is one version of an object as persisted in xl.meta. It mirrors
// the durable fields of FileInfo.
type xlVersion struct {
	ID               string            `json:"id,omitempty"`
	Deleted          bool              `json:"deleted,omitempty"`
	DataDir          string            `json:"dataDir,omitempty"`
	ModTime          time.Time         `json:"modTime"`
	Size             int64             `json:"size"`
	Mode             uint32            `json:"mode,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Parts            []PartInfo        `json:"parts,omitempty"`
	Erasure          ErasureInfo       `json:"erasure"`
	Data             []byte            `json:"data,omitempty"`
	Checksum         []byte            `json:"checksum,omitempty"`
	Versioned        bool              `json:"versioned,omitempty"`
	ReplicationState map[string]string `json:"replicationState,omitempty"`
	TransitionStatus string            `json:"transitionStatus,omitempty"`
}

func versionFromFileInfo(fi FileInfo) xlVersion {
	return xlVersion{
		ID:               fi.VersionID,
		Deleted:          fi.Deleted,
		DataDir:          fi.DataDir,
		ModTime:          fi.ModTime,
		Size:             fi.Size,
		Mode:             fi.Mode,
		Metadata:         fi.Metadata,
		Parts:            fi.Parts,
		Erasure:          fi.Erasure,
		Data:             fi.Data,
		Checksum:         fi.Checksum,
		Versioned:        fi.Versioned,
		ReplicationState: fi.ReplicationState,
		TransitionStatus: fi.TransitionStatus,
	}
}

func (v xlVersion) fileInfo(volume, name string) FileInfo {
	return FileInfo{
		Volume:           volume,
		Name:             name,
		VersionID:        v.ID,
		Deleted:          v.Deleted,
		DataDir:          v.DataDir,
		ModTime:          v.ModTime,
		Size:             v.Size,
		Mode:             v.Mode,
		Metadata:         v.Metadata,
		Parts:            v.Parts,
		Erasure:          v.Erasure,
		Data:             v.Data,
		Checksum:         v.Checksum,
		Versioned:        v.Versioned,
		ReplicationState: v.ReplicationState,
		TransitionStatus: v.TransitionStatus,
	}
}

// decodeXLMeta parses an xl.meta document.
func decodeXLMeta(data []byte) (*xlMeta, error) {
	var m xlMeta
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: xl.meta: %v", domain.ErrInvalidFormat, err)
	}
	if m.Version != xlMetaVersion {
		return nil, fmt.Errorf("%w: xl.meta version %d, want %d", domain.ErrInvalidFormat, m.Version, xlMetaVersion)
	}
	return &m, nil
}

// encode serializes the document.
func (m *xlMeta) encode() ([]byte, error) {
	m.Version = xlMetaVersion
	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: encode xl.meta: %v", domain.ErrInvalidFormat, err)
	}
	return data, nil
}

// find returns the index of the version with the given id, or -1. An empty
// id selects the latest version.
func (m *xlMeta) find(versionID string) int {
	if versionID == "" {
		if len(m.Versions) == 0 {
			return -1
		}
		return 0
	}
	for i, v := range m.Versions {
		if v.ID == versionID {
			return i
		}
	}
	return -1
}

// upsert replaces the version with the same id or inserts a new one, then
// re-sorts the versions by descending modification time.
func (m *xlMeta) upsert(v xlVersion) (replaced *xlVersion) {
	if i := m.find(v.ID); i >= 0 && (v.ID != "" || len(m.Versions) > 0 && m.Versions[i].ID == "") {
		old := m.Versions[i]
		replaced = &old
		m.Versions[i] = v
	} else {
		m.Versions = append(m.Versions, v)
	}
	m.sort()
	return replaced
}

// remove deletes the version at index i.
func (m *xlMeta) remove(i int) xlVersion {
	v := m.Versions[i]
	m.Versions = append(m.Versions[:i], m.Versions[i+1:]...)
	return v
}

func (m *xlMeta) sort() {
	sort.SliceStable(m.Versions, func(i, j int) bool {
		return m.Versions[i].ModTime.After(m.Versions[j].ModTime)
	})
}

// fileInfoAt builds the FileInfo of version i with the positional fields
// (IsLatest, NumVersions, SuccessorModTime, Idx) filled in.
func (m *xlMeta) fileInfoAt(i int, volume, name string, readData bool) FileInfo {
	fi := m.Versions[i].fileInfo(volume, name)
	fi.IsLatest = i == 0
	fi.NumVersions = len(m.Versions)
	fi.Idx = i
	if i > 0 {
		fi.SuccessorModTime = m.Versions[i-1].ModTime
	}
	if !readData {
		fi.Data = nil
	}
	return fi
}
