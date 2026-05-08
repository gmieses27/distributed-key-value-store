// Package store is the key-value state machine that sits on top of Raft.
// Every committed log entry is applied here in order, on every node.
package store

import (
	"strings"
	"sync"
)

// KVStore is a thread-safe in-memory key-value store.
type KVStore struct {
	mu   sync.RWMutex
	data map[string]string
}

// New returns an empty KVStore.
func New() *KVStore {
	return &KVStore{data: make(map[string]string)}
}

// Apply executes a committed command string.
// Commands are encoded as:  "put:key=value"  |  "delete:key"
// Returns (result, ok).
func (s *KVStore) Apply(command string) (string, bool) {
	parts := strings.SplitN(command, ":", 2)
	if len(parts) != 2 {
		return "", false
	}
	op, rest := parts[0], parts[1]

	switch op {
	case "put":
		kv := strings.SplitN(rest, "=", 2)
		if len(kv) != 2 {
			return "", false
		}
		s.mu.Lock()
		s.data[kv[0]] = kv[1]
		s.mu.Unlock()
		return kv[1], true

	case "delete":
		s.mu.Lock()
		delete(s.data, rest)
		s.mu.Unlock()
		return "", true
	}

	return "", false
}

// Get returns the value for key. ok is false if the key doesn't exist.
func (s *KVStore) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

// Clear wipes all keys from the store.
func (s *KVStore) Clear() {
	s.mu.Lock()
	s.data = make(map[string]string)
	s.mu.Unlock()
}

// All returns a copy of the entire store (for the dashboard).
func (s *KVStore) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}
