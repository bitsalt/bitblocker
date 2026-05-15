package blocklist_test

import (
	"net/netip"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bitsalt/bitblocker/internal/blocklist"
)

// trieWith builds a populated trie from the given prefixes.
func trieWith(t *testing.T, prefixes ...string) *blocklist.Trie {
	t.Helper()
	tr := blocklist.New()
	for _, p := range prefixes {
		pref, err := netip.ParsePrefix(p)
		require.NoErrorf(t, err, "parse prefix %q", p)
		tr.Insert(pref)
	}
	return tr
}

func TestSource_CurrentBeforeSwapIsNil(t *testing.T) {
	src := blocklist.NewSource()
	require.Nil(t, src.Current(), "a fresh Source must report nil until the first Swap")
}

func TestSource_SwapPublishesTrie(t *testing.T) {
	src := blocklist.NewSource()
	tr := trieWith(t, "10.0.0.0/24")

	src.Swap(tr)

	got := src.Current()
	require.Same(t, tr, got, "Current must return the exact trie handed to Swap")
	require.True(t, got.Contains(netip.MustParseAddr("10.0.0.5")))
}

func TestSource_SwapReplacesPriorTrie(t *testing.T) {
	src := blocklist.NewSource()
	src.Swap(trieWith(t, "10.0.0.0/24"))
	src.Swap(trieWith(t, "192.0.2.0/24"))

	got := src.Current()
	require.False(t, got.Contains(netip.MustParseAddr("10.0.0.5")), "stale trie must not still be visible")
	require.True(t, got.Contains(netip.MustParseAddr("192.0.2.5")))
}

func TestSource_SwapNilReturnsToEmptyState(t *testing.T) {
	src := blocklist.NewSource()
	src.Swap(trieWith(t, "10.0.0.0/24"))
	src.Swap(nil)
	require.Nil(t, src.Current(), "Swap(nil) must return the Source to the empty state")
}

// TestSource_ConcurrentReadDuringSwap exercises the lock-free read side
// against a concurrent writer. Under `go test -race` this is the
// regression guard for the atomic-pointer publish/consume contract: a
// data race here would fail the build.
func TestSource_ConcurrentReadDuringSwap(t *testing.T) {
	src := blocklist.NewSource()
	src.Swap(trieWith(t, "10.0.0.0/24"))

	const readers = 16
	const iterations = 2000

	var readerWG sync.WaitGroup
	stop := make(chan struct{})

	// Readers: each call must observe a fully-consistent trie.
	for i := 0; i < readers; i++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for j := 0; j < iterations; j++ {
				if tr := src.Current(); tr != nil {
					_ = tr.Contains(netip.MustParseAddr("10.0.0.5"))
					_ = tr.Len()
				}
			}
		}()
	}

	// Writer: swap fresh tries until the readers are done.
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				src.Swap(trieWith(t, "10.0.0.0/24"))
			}
		}
	}()

	readerWG.Wait()
	close(stop)
	writerWG.Wait()

	require.NotNil(t, src.Current(), "Source must still hold a trie after the swap storm")
}
