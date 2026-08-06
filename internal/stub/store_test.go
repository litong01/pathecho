package stub

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMemoryStoreConsumesHitsAtomically(t *testing.T) {
	store := newMemoryStore()
	store.Set(http.MethodGet, "/limited", &responseEntry{Remaining: 10})

	var served int32
	var wait sync.WaitGroup
	for request := 0; request < 50; request++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if entry, ok := store.Take(http.MethodGet, "/limited"); ok {
				atomic.AddInt32(&served, 1)
				store.Complete(http.MethodGet, "/limited", entry, true)
			}
		}()
	}
	wait.Wait()
	if served != 10 {
		t.Fatalf("served hits = %d, want 10", served)
	}
}

func TestMemoryStoreHasBoundedEntries(t *testing.T) {
	store := newMemoryStore()
	for index := 0; index < maxStoredResponses; index++ {
		if err := store.Set(http.MethodGet, fmt.Sprintf("/%d", index), &responseEntry{}); err != nil {
			t.Fatalf("set entry %d: %v", index, err)
		}
	}
	if err := store.Set(http.MethodGet, "/overflow", &responseEntry{}); err == nil {
		t.Fatal("store accepted an entry beyond its limit")
	}
	if err := store.Set(http.MethodGet, "/0", &responseEntry{}); err != nil {
		t.Fatalf("store rejected overwrite at capacity: %v", err)
	}
}

func TestLimitedBufferStopsAtRenderLimit(t *testing.T) {
	remaining := 4
	buffer := limitedBuffer{remaining: &remaining}
	written, err := buffer.Write([]byte("123456"))
	if written != 4 || !errors.Is(err, errRenderedTooLarge) ||
		buffer.String() != "1234" || remaining != 0 {
		t.Fatalf("limited write = (%d, %v, %q, %d)", written, err, buffer.String(), remaining)
	}
}
