package blocklist

import "sync/atomic"

// Source holds the active blocklist trie and publishes replacements
// atomically. The read side is lock-free and safe for unbounded
// concurrent callers; the write side is a single atomic store.
//
// The published trie must be immutable after the Swap that publishes
// it — callers hand over a fully-constructed *Trie and never mutate it
// again. That invariant is what makes the lock-free read correct: a
// reader holding an older *Trie keeps reading a consistent snapshot
// while the next one is published. See ADR 0001.
type Source struct {
	current atomic.Pointer[Trie]
}

// NewSource returns a Source with no trie loaded. Current returns nil
// until the first Swap. The daemon is in its fail-closed cold-start
// posture while the Source is empty.
func NewSource() *Source {
	return &Source{}
}

// Current returns the active trie, or nil if none has been published.
// It is a single atomic pointer load: it never blocks, never errors,
// and never allocates, so it is safe on the /check hot path under
// unbounded concurrent callers. A nil return is a valid, expected
// cold-start state, not a failure.
func (s *Source) Current() *Trie {
	return s.current.Load()
}

// Swap publishes t as the active trie via one atomic pointer store.
// t may be nil (not expected in practice, but defined: it returns the
// Source to the empty state). Callers must treat t as immutable after
// this call — never mutate a trie after handing it to Swap.
func (s *Source) Swap(t *Trie) {
	s.current.Store(t)
}
