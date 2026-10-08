package object

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// storageClasses is the parity of each storage class in an erasure set.
type storageClasses struct {
	standard int
	rrs      int
}

// parity returns the parity of a storage class: REDUCED_REDUNDANCY has its
// own, every other class (STANDARD, the empty class and the classes Nexo
// stores like STANDARD) the standard one.
func (c storageClasses) parity(class string) int {
	if strings.EqualFold(class, domain.StorageClassReducedRedundancy) {
		return c.rrs
	}
	return c.standard
}

// ParseStorageClass parses a storage class parity setting of the form
// "EC:n" (NEXO_STORAGE_CLASS_STANDARD, NEXO_STORAGE_CLASS_RRS) and returns
// n. Errors wrap domain.ErrInvalidArgument.
func ParseStorageClass(v string) (int, error) {
	scheme, num, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok || !strings.EqualFold(scheme, "EC") {
		return 0, fmt.Errorf("%w: storage class %q: want EC:<parity>", domain.ErrInvalidArgument, v)
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: storage class %q: invalid parity", domain.ErrInvalidArgument, v)
	}
	return n, nil
}

// resolveStorageClasses computes the parity of the storage classes of the
// sets of a pool of setSize drives: STANDARD is the configured class
// (WithStorageClass), else the parity of the pool, else the default of
// docs/03 §3.3 (topology.DefaultParity); REDUCED_REDUNDANCY is the
// configured class, else 1 (at most STANDARD). STANDARD must leave at
// least as many data shards as parity shards.
func resolveStorageClasses(setSize, poolParity int, o options) (storageClasses, error) {
	standard := poolParity
	if standard <= 0 {
		standard = topology.DefaultParity(setSize)
	}
	if v := o.storageClasses[domain.StorageClassStandard]; v != "" {
		p, err := ParseStorageClass(v)
		if err != nil {
			return storageClasses{}, err
		}
		standard = p
	}
	if standard < 1 || standard > setSize/2 {
		return storageClasses{}, fmt.Errorf("%w: parity %d is invalid for erasure sets of %d drives (1..%d)", domain.ErrInvalidArgument, standard, setSize, setSize/2)
	}
	rrs := min(1, standard)
	if v := o.storageClasses[domain.StorageClassReducedRedundancy]; v != "" {
		p, err := ParseStorageClass(v)
		if err != nil {
			return storageClasses{}, err
		}
		rrs = p
	}
	if rrs < 1 || rrs > standard {
		return storageClasses{}, fmt.Errorf("%w: %s parity %d must be within 1..%d (the %s parity)",
			domain.ErrInvalidArgument, domain.StorageClassReducedRedundancy, rrs, standard, domain.StorageClassStandard)
	}
	return storageClasses{standard: standard, rrs: rrs}, nil
}
