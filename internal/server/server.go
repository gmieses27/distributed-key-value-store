// Package server wires the Raft node, KV store, and HTTP layer together.
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gmieses27/distributed-kvstore/internal/raft"
	"github.com/gmieses27/distributed-kvstore/internal/store"
)

// Server is an HTTP server wrapping one Raft node and one KV store.
type Server struct {
	id      int
	addr    string
	rf      *raft.Raft
	kv      *store.KVStore
	applyCh chan raft.LogEntry
	eventCh chan raft.Event

	// SSE subscribers
	subs   map[chan string]struct{}
	subsMu sync.Mutex
}

// New creates a Server. Call ListenAndServe to start it.
func New(id int, addr string, peers []string, persistPath string) *Server {
	applyCh := make(chan raft.LogEntry, 256)
	eventCh := make(chan raft.Event, 256)

	s := &Server{
		id:      id,
		addr:    addr,
		applyCh: applyCh,
		eventCh: eventCh,
		kv:      store.New(),
		subs:    make(map[chan string]struct{}),
	}

	s.rf = raft.New(id, peers, persistPath, applyCh, eventCh)

	go s.applyWorker()
	go s.eventBroadcaster()

	return s
}

// ListenAndServe starts the HTTP server (blocking).
func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()

	// Raft RPCs (called by peers)
	mux.HandleFunc("/raft/request-vote", s.handleRequestVote)
	mux.HandleFunc("/raft/append-entries", s.handleAppendEntries)

	// Client API
	mux.HandleFunc("/kv/put", s.handlePut)
	mux.HandleFunc("/kv/get", s.handleGet)
	mux.HandleFunc("/kv/delete", s.handleDelete)
	mux.HandleFunc("/kv/all", s.handleAll)

	// Dashboard helpers
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/events", s.handleSSE)

	// Admin
	mux.HandleFunc("/admin/pause", s.handlePause)
	mux.HandleFunc("/admin/resume", s.handleResume)
	mux.HandleFunc("/admin/reset", s.handleReset)

	log.Printf("node %d listening on %s", s.id, s.addr)
	return http.ListenAndServe(s.addr, corsMiddleware(mux))
}

// ---- Raft RPC handlers ----

func (s *Server) handleRequestVote(w http.ResponseWriter, r *http.Request) {
	if s.rf.Status().Paused {
		http.Error(w, "paused", http.StatusServiceUnavailable)
		return
	}
	var args raft.RequestVoteArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	reply := s.rf.HandleRequestVote(args)
	json.NewEncoder(w).Encode(reply)
}

func (s *Server) handleAppendEntries(w http.ResponseWriter, r *http.Request) {
	if s.rf.Status().Paused {
		http.Error(w, "paused", http.StatusServiceUnavailable)
		return
	}
	var args raft.AppendEntriesArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	reply := s.rf.HandleAppendEntries(args)
	json.NewEncoder(w).Encode(reply)
}

// ---- KV handlers ----

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if body.Key == "" {
		http.Error(w, "key required", 400)
		return
	}

	cmd := fmt.Sprintf("put:%s=%s", body.Key, body.Value)
	index, leaderID, isLeader := s.rf.Submit(cmd)
	if !isLeader {
		w.Header().Set("X-Leader-ID", strconv.Itoa(leaderID))
		http.Error(w, "not leader", http.StatusMisdirectedRequest)
		return
	}

	if !s.rf.WaitCommit(index) {
		http.Error(w, "commit timeout", http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    true,
		"key":   body.Key,
		"value": body.Value,
		"index": index,
	})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "key required", 400)
		return
	}
	val, ok := s.kv.Get(key)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    ok,
		"key":   key,
		"value": val,
	})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if body.Key == "" {
		http.Error(w, "key required", 400)
		return
	}

	cmd := fmt.Sprintf("delete:%s", body.Key)
	index, leaderID, isLeader := s.rf.Submit(cmd)
	if !isLeader {
		w.Header().Set("X-Leader-ID", strconv.Itoa(leaderID))
		http.Error(w, "not leader", http.StatusMisdirectedRequest)
		return
	}

	if !s.rf.WaitCommit(index) {
		http.Error(w, "commit timeout", http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "key": body.Key})
}

func (s *Server) handleAll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.kv.All())
}

// ---- Dashboard / SSE ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.rf.Status())
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan string, 64)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()

	defer func() {
		s.subsMu.Lock()
		delete(s.subs, ch)
		s.subsMu.Unlock()
	}()

	// Send current status immediately on connect
	st := s.rf.Status()
	if data, err := json.Marshal(raft.Event{Type: "roleChange", Data: st}); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			// Heartbeat ping to keep connection alive
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.rf.Pause()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"paused": true})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.rf.Resume()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"paused": false})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.kv.Clear()
	s.rf.Reset()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// ---- Background workers ----

// applyWorker applies committed log entries to the KV store.
func (s *Server) applyWorker() {
	for entry := range s.applyCh {
		s.kv.Apply(entry.Command)
	}
}

// eventBroadcaster forwards Raft events to all SSE subscribers.
func (s *Server) eventBroadcaster() {
	for event := range s.eventCh {
		data, err := json.Marshal(event)
		if err != nil {
			continue
		}
		msg := string(data)

		s.subsMu.Lock()
		for ch := range s.subs {
			select {
			case ch <- msg:
			default:
			}
		}
		s.subsMu.Unlock()
	}
}

// ---- CORS middleware ----

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}
