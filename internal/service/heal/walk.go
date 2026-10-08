package heal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// walkBuffer is the number of keys a drive walk produces ahead of the
// merge.
const walkBuffer = 128

// keyStream is the walk of one drive, consumed key by key.
type keyStream struct {
	ch   chan string
	errc chan error
	head string
	done bool
	err  error
}

func (s *keyStream) advance() {
	k, ok := <-s.ch
	if !ok {
		s.head, s.done = "", true
		s.err = <-s.errc
		return
	}
	s.head = k
}

// online returns the online drives among drives.
func online(drives []storage.StorageAPI) []storage.StorageAPI {
	out := make([]storage.StorageAPI, 0, len(drives))
	for _, d := range drives {
		if d != nil && d.IsOnline() {
			out = append(out, d)
		}
	}
	return out
}

// walkKeys calls fn once for every object key held by at least one of
// drives in bucket under prefix and after the key after (excluded), in
// key order: the union of the drives, so that the objects a quorum of
// drives miss are found too. A drive whose walk fails is left out; the
// walk fails when every drive failed. fn stops the walk by returning an
// error, which walkKeys returns.
func walkKeys(ctx context.Context, drives []storage.StorageAPI, bucket, prefix, after string, fn func(key string) error) error {
	drives = online(drives)
	if len(drives) == 0 {
		return domain.ErrDiskNotFound
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opts := storage.WalkDirOptions{Bucket: bucket, Recursive: true, FilterPrefix: prefix, ForwardTo: after}
	if i := strings.LastIndex(prefix, "/"); i > 0 {
		opts.BaseDir = prefix[:i]
	}
	streams := make([]*keyStream, len(drives))
	var wg sync.WaitGroup
	for i, d := range drives {
		s := &keyStream{ch: make(chan string, walkBuffer), errc: make(chan error, 1)}
		streams[i] = s
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := d.WalkDir(ctx, opts, func(we storage.WalkEntry) error {
				if we.IsDir || we.Name <= after || !strings.HasPrefix(we.Name, prefix) {
					return nil
				}
				select {
				case s.ch <- we.Name:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			s.errc <- err
			close(s.ch)
		}()
	}
	defer func() {
		cancel()
		for _, s := range streams {
			for !s.done {
				s.advance()
			}
		}
		wg.Wait()
	}()
	live := make([]*keyStream, 0, len(streams))
	var errs []error
	for _, s := range streams {
		s.advance()
		switch {
		case !s.done:
			live = append(live, s)
		case s.err != nil:
			errs = append(errs, s.err)
		}
	}
	if len(errs) == len(streams) {
		return walkError(errs)
	}
	for len(live) > 0 {
		next := live[0].head
		for _, s := range live[1:] {
			if s.head < next {
				next = s.head
			}
		}
		if err := fn(next); err != nil {
			return err
		}
		for _, s := range live {
			if s.head == next {
				s.advance()
			}
		}
		live = slices.DeleteFunc(live, func(s *keyStream) bool { return s.done })
	}
	return ctx.Err()
}

// walkError reduces the errors of drive walks that all failed: the first
// one that is not a missing bucket, else the missing bucket.
func walkError(errs []error) error {
	for _, err := range errs {
		if !errors.Is(err, domain.ErrBucketNotFound) {
			return err
		}
	}
	return errs[0]
}

// listBuckets returns the union of the buckets held by the online drives,
// sorted by name.
func listBuckets(ctx context.Context, drives []storage.StorageAPI) ([]string, error) {
	seen := map[string]bool{}
	var errs []error
	ok := 0
	for _, d := range online(drives) {
		vols, err := d.ListVols(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ok++
		for _, v := range vols {
			if v.Name != "" && !strings.HasPrefix(v.Name, ".") {
				seen[v.Name] = true
			}
		}
	}
	if ok == 0 {
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		return nil, domain.ErrDiskNotFound
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}
