package app

import "sync"

type keyedMutexSet struct {
	mu      sync.Mutex
	entries map[int]*keyedMutexEntry
}

type keyedMutexEntry struct {
	mu   sync.Mutex
	refs int
}

type keyedMutexHandle struct {
	set   *keyedMutexSet
	id    int
	entry *keyedMutexEntry
}

func (set *keyedMutexSet) handle(id int) *keyedMutexHandle {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.entries == nil {
		set.entries = make(map[int]*keyedMutexEntry)
	}
	entry := set.entries[id]
	if entry == nil {
		entry = &keyedMutexEntry{}
		set.entries[id] = entry
	}
	entry.refs++
	return &keyedMutexHandle{set: set, id: id, entry: entry}
}

func (handle *keyedMutexHandle) Lock() {
	handle.entry.mu.Lock()
}

func (handle *keyedMutexHandle) Unlock() {
	handle.entry.mu.Unlock()
	handle.set.mu.Lock()
	defer handle.set.mu.Unlock()
	handle.entry.refs--
	if handle.entry.refs == 0 && handle.set.entries[handle.id] == handle.entry {
		delete(handle.set.entries, handle.id)
	}
}
