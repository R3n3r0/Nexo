package storage

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/r3n3r0/nexo/internal/domain"
)

// healingPath is the file system path of the healing tracker.
func (s *xlStorage) healingPath() string {
	return filepath.Join(s.root, domain.SystemBucket, HealingFile)
}

// loadHealing refreshes the cached healing tracker from healing.bin: a
// missing file means the drive is not healing; an unreadable one is
// reported and treated as missing.
func (s *xlStorage) loadHealing() {
	data, err := readFile(s.healingPath())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.deps.Logger.Warn("storage: cannot read healing tracker", "path", s.root, "err", err)
		}
		s.healing.Store(nil)
		return
	}
	var h HealingTracker
	if err := json.Unmarshal(data, &h); err != nil {
		s.deps.Logger.Warn("storage: invalid healing tracker", "path", s.root, "err", err)
		s.healing.Store(nil)
		return
	}
	s.healing.Store(&h)
}

// Healing implements StorageAPI. It is a pure accessor over the tracker
// cached at startup and refreshed by DiskInfo and by writes of HealingFile.
func (s *xlStorage) Healing() *HealingTracker {
	h := s.healing.Load()
	if h == nil {
		return nil
	}
	cp := *h
	return &cp
}

// isHealingFile reports whether volume/path designates the healing tracker.
func isHealingFile(volume, path string) bool {
	return volume == domain.SystemBucket && path == HealingFile
}
