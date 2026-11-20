package watch

import (
	"crypto/sha256"
	"path/filepath"
	"slices"
	"time"
)

const initialLabel = "(initial run)"

type request struct {
	hash    [sha256.Size]byte
	retried bool
}

type batch struct {
	initial        bool
	initialRetried bool
	paths          map[string]request
	deleted        map[string]struct{}
}

func newBatch() batch {
	return batch{paths: make(map[string]request), deleted: make(map[string]struct{})}
}

func (b batch) changes(root string) []ChangedFile {
	if b.initial {
		return nil
	}
	files := make([]ChangedFile, 0, len(b.paths)+len(b.deleted))
	for path := range b.paths {
		rel, _ := filepath.Rel(root, path)
		files = append(files, ChangedFile{Path: filepath.ToSlash(rel)})
	}
	for path := range b.deleted {
		rel, _ := filepath.Rel(root, path)
		files = append(files, ChangedFile{Path: filepath.ToSlash(rel), Deleted: true})
	}
	slices.SortFunc(files, func(a, b ChangedFile) int {
		if a.Deleted != b.Deleted {
			if a.Deleted {
				return 1
			}
			return -1
		}
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})
	return files
}

// schedule owns dispatch decisions independently of filesystem events and timers.
type schedule struct {
	quietPeriod, retryDelay time.Duration
	quietUntil, retryAt     time.Time
	hashes                  map[string][sha256.Size]byte
	pending, retry          batch
}

func newSchedule(quietPeriod, retryDelay time.Duration) *schedule {
	s := &schedule{
		quietPeriod: quietPeriod, retryDelay: retryDelay,
		hashes:  make(map[string][sha256.Size]byte),
		pending: newBatch(), retry: newBatch(),
	}
	s.pending.initial = true
	return s
}

func (s *schedule) ready(path string, hash [sha256.Size]byte, now time.Time) bool {
	if old, ok := s.hashes[path]; ok && old == hash {
		return false
	}
	s.hashes[path] = hash
	delete(s.retry.paths, path)
	delete(s.pending.deleted, path)
	delete(s.retry.deleted, path)
	s.pending.paths[path] = request{hash: hash}
	s.quietUntil = now.Add(s.quietPeriod)
	return true
}

func (s *schedule) unready(path string) {
	s.suppress(path)
	delete(s.pending.deleted, path)
	delete(s.retry.deleted, path)
}

func (s *schedule) suppress(path string) {
	delete(s.hashes, path)
	delete(s.pending.paths, path)
	delete(s.retry.paths, path)
}

func (s *schedule) ignoreWrite(path string, work batch) {
	// When writes are only reported on completion, an earlier notification
	// may have queued a spurious request. Keep the dispatched request's hash
	// and retry budget; a write during a failed run does not skip its retry.
	if _, queued := s.pending.paths[path]; queued {
		delete(s.pending.paths, path)
		s.clearQuietIfIdle()
		if old, ok := work.paths[path]; ok {
			s.hashes[path] = old.hash
		} else {
			delete(s.hashes, path)
		}
	} else if old, ok := work.paths[path]; ok {
		s.hashes[path] = old.hash
	}
	delete(s.pending.deleted, path)
}

func (s *schedule) clearQuietIfIdle() {
	if len(s.pending.paths) == 0 {
		s.quietUntil = time.Time{}
	}
}

func (s *schedule) remove(path string) {
	s.suppress(path)
	s.pending.deleted[path] = struct{}{}
}

func (s *schedule) next() time.Time {
	if s.pending.initial {
		return time.Unix(0, 0)
	}
	if len(s.pending.paths) != 0 {
		return s.quietUntil
	}
	if s.retry.initial || len(s.retry.paths) != 0 {
		if s.quietUntil.After(s.retryAt) {
			return s.quietUntil
		}
		return s.retryAt
	}
	return time.Time{}
}

func (s *schedule) take(now time.Time, revalidate func(path string) bool) batch {
	if !due(s.next(), now) {
		return newBatch()
	}
	work := s.pending
	s.pending = newBatch()
	// A fresh submission can bring forward a queued retry, as in watch mode's
	// original coalescing rule.
	if s.retry.initial {
		work.initial, work.initialRetried = true, s.retry.initialRetried
	}
	for path, request := range s.retry.paths {
		if _, ok := work.paths[path]; !ok {
			work.paths[path] = request
		}
	}
	for path := range s.retry.deleted {
		work.deleted[path] = struct{}{}
	}
	s.retry = newBatch()
	s.retryAt = time.Time{}

	for path := range work.paths {
		if !revalidate(path) {
			delete(work.paths, path)
			delete(s.hashes, path)
		} else {
			delete(work.deleted, path)
		}
	}
	if !work.initial && len(work.paths) == 0 {
		s.pending.deleted = work.deleted
	}
	return work
}

func (s *schedule) failed(work batch, now time.Time) (gaveUp []string, retry bool) {
	if work.initial {
		if work.initialRetried {
			gaveUp = append(gaveUp, initialLabel)
		} else {
			s.retry.initial, s.retry.initialRetried = true, true
		}
	}
	for path, request := range work.paths {
		// An intervening edit/removal supersedes the failed submission.
		if s.hashes[path] != request.hash {
			continue
		}
		if request.retried {
			gaveUp = append(gaveUp, path)
		} else {
			request.retried = true
			s.retry.paths[path] = request
		}
	}
	slices.Sort(gaveUp)
	if s.retry.initial || len(s.retry.paths) != 0 {
		s.retry.deleted = work.deleted
		s.retryAt = now.Add(s.retryDelay)
		retry = true
	}
	return gaveUp, retry
}
