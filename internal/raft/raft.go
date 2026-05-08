// Package raft implements the Raft consensus algorithm.
// Leader election, log replication, and persistence are all included.
package raft

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	HeartbeatInterval  = 60 * time.Millisecond
	ElectionTimeoutMin = 200 * time.Millisecond
	ElectionTimeoutMax = 400 * time.Millisecond
)

// Raft is a single consensus node.
type Raft struct {
	mu   sync.Mutex
	id   int
	peers []string // peer HTTP addresses, e.g. "localhost:8002"

	// Persistent state (written to disk before responding to RPCs)
	currentTerm int
	votedFor    int // -1 if none this term
	log         []LogEntry

	// Volatile state
	commitIndex int
	lastApplied int

	// Leader-only volatile state (reinitialized on election)
	nextIndex  []int
	matchIndex []int

	role     Role
	leaderID int

	// Channels for the server layer
	applyCh chan<- LogEntry
	eventCh chan<- Event

	electionTimer *time.Timer

	// Persistence
	persistPath string

	// Lifecycle
	dead   int32 // atomic; 1 = killed
	paused int32 // atomic; 1 = paused (simulates network partition)
}

// New creates and starts a Raft node.
func New(
	id int,
	peers []string,
	persistPath string,
	applyCh chan<- LogEntry,
	eventCh chan<- Event,
) *Raft {
	rf := &Raft{
		id:          id,
		peers:       peers,
		currentTerm: 0,
		votedFor:    -1,
		log:         []LogEntry{},
		commitIndex: -1,
		lastApplied: -1,
		nextIndex:   make([]int, len(peers)),
		matchIndex:  make([]int, len(peers)),
		role:        Follower,
		leaderID:    -1,
		applyCh:     applyCh,
		eventCh:     eventCh,
		persistPath: persistPath,
	}

	rf.loadPersist()
	rf.electionTimer = time.AfterFunc(rf.randomTimeout(), rf.triggerElection)

	go rf.applyLoop()

	return rf
}

// ---- Timer helpers ----

func (rf *Raft) randomTimeout() time.Duration {
	span := int64(ElectionTimeoutMax - ElectionTimeoutMin)
	return ElectionTimeoutMin + time.Duration(rand.Int63n(span))
}

func (rf *Raft) resetTimer() {
	rf.electionTimer.Reset(rf.randomTimeout())
}

// ---- Election ----

func (rf *Raft) triggerElection() {
	rf.mu.Lock()
	if rf.killed() || rf.isPaused() {
		rf.mu.Unlock()
		return
	}

	rf.role = Candidate
	rf.currentTerm++
	rf.votedFor = rf.id
	rf.leaderID = -1
	rf.savePersist()

	term := rf.currentTerm
	lastIdx, lastTerm := rf.lastLogInfo()
	rf.emitEvent("roleChange", rf.snapshot())
	rf.mu.Unlock()

	votes := int32(1) // voted for self
	total := len(rf.peers) + 1
	majority := int32(total/2 + 1)

	for _, peer := range rf.peers {
		go func(p string) {
			args := RequestVoteArgs{
				Term:         term,
				CandidateID:  rf.id,
				LastLogIndex: lastIdx,
				LastLogTerm:  lastTerm,
			}
			var reply RequestVoteReply
			if err := rf.rpc(p, "/raft/request-vote", args, &reply); err != nil {
				return
			}

			rf.mu.Lock()
			defer rf.mu.Unlock()

			if reply.Term > rf.currentTerm {
				rf.stepDown(reply.Term)
				return
			}
			if rf.role != Candidate || rf.currentTerm != term {
				return
			}
			if reply.VoteGranted {
				if atomic.AddInt32(&votes, 1) == majority {
					rf.becomeLeader()
				}
			}
		}(peer)
	}

	// Reset timer so we retry if this election doesn't produce a leader.
	rf.mu.Lock()
	if rf.role == Candidate {
		rf.resetTimer()
	}
	rf.mu.Unlock()
}

func (rf *Raft) stepDown(newTerm int) {
	// caller must hold rf.mu
	rf.role = Follower
	rf.currentTerm = newTerm
	rf.votedFor = -1
	rf.leaderID = -1
	rf.savePersist()
	rf.resetTimer()
	rf.emitEvent("roleChange", rf.snapshot())
}

func (rf *Raft) becomeLeader() {
	// caller must hold rf.mu
	rf.role = Leader
	rf.leaderID = rf.id

	for i := range rf.nextIndex {
		rf.nextIndex[i] = len(rf.log)
	}
	for i := range rf.matchIndex {
		rf.matchIndex[i] = -1
	}

	rf.savePersist()
	rf.emitEvent("roleChange", rf.snapshot())

	go rf.heartbeatLoop()
}

// ---- Heartbeat / Replication ----

func (rf *Raft) heartbeatLoop() {
	for !rf.killed() {
		rf.mu.Lock()
		if rf.role != Leader {
			rf.mu.Unlock()
			return
		}
		rf.mu.Unlock()

		rf.replicateAll()
		time.Sleep(HeartbeatInterval)
	}
}

func (rf *Raft) replicateAll() {
	rf.mu.Lock()
	if rf.role != Leader {
		rf.mu.Unlock()
		return
	}
	term := rf.currentTerm
	rf.mu.Unlock()

	for i, peer := range rf.peers {
		go rf.replicate(i, peer, term)
	}
}

func (rf *Raft) replicate(peerIdx int, peer string, term int) {
	rf.mu.Lock()
	if rf.killed() || rf.role != Leader || rf.currentTerm != term {
		rf.mu.Unlock()
		return
	}

	ni := rf.nextIndex[peerIdx]
	prevIdx := ni - 1
	prevTerm := -1
	if prevIdx >= 0 && prevIdx < len(rf.log) {
		prevTerm = rf.log[prevIdx].Term
	}

	entries := []LogEntry{}
	if ni < len(rf.log) {
		entries = append(entries, rf.log[ni:]...)
	}

	args := AppendEntriesArgs{
		Term:         term,
		LeaderID:     rf.id,
		PrevLogIndex: prevIdx,
		PrevLogTerm:  prevTerm,
		Entries:      entries,
		LeaderCommit: rf.commitIndex,
	}
	rf.mu.Unlock()

	var reply AppendEntriesReply
	if err := rf.rpc(peer, "/raft/append-entries", args, &reply); err != nil {
		return
	}

	rf.mu.Lock()
	defer rf.mu.Unlock()

	if reply.Term > rf.currentTerm {
		rf.stepDown(reply.Term)
		return
	}
	if rf.role != Leader || rf.currentTerm != term {
		return
	}

	if reply.Success {
		newMatch := prevIdx + len(entries)
		if newMatch > rf.matchIndex[peerIdx] {
			rf.matchIndex[peerIdx] = newMatch
			rf.nextIndex[peerIdx] = newMatch + 1
		}
		rf.tryAdvanceCommit()
	} else {
		if rf.nextIndex[peerIdx] > 0 {
			rf.nextIndex[peerIdx]--
		}
		go rf.replicate(peerIdx, peer, term)
	}
}

func (rf *Raft) tryAdvanceCommit() {
	// caller must hold rf.mu
	for n := len(rf.log) - 1; n > rf.commitIndex; n-- {
		if rf.log[n].Term != rf.currentTerm {
			continue
		}
		count := 1
		for _, mi := range rf.matchIndex {
			if mi >= n {
				count++
			}
		}
		if count*2 > len(rf.peers)+1 { // strict majority of total cluster size
			rf.commitIndex = n
			break
		}
	}
}

// ---- Apply loop ----

func (rf *Raft) applyLoop() {
	for !rf.killed() {
		rf.mu.Lock()
		var toApply []LogEntry
		for rf.lastApplied < rf.commitIndex {
			rf.lastApplied++
			toApply = append(toApply, rf.log[rf.lastApplied])
		}
		rf.mu.Unlock()

		for _, entry := range toApply {
			rf.applyCh <- entry
			rf.emitEvent("commit", map[string]interface{}{
				"nodeId":  rf.id,
				"index":   entry.Index,
				"command": entry.Command,
			})
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// ---- RPC Handlers ----

// HandleRequestVote processes an incoming RequestVote RPC.
func (rf *Raft) HandleRequestVote(args RequestVoteArgs) RequestVoteReply {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply := RequestVoteReply{Term: rf.currentTerm}

	if args.Term < rf.currentTerm {
		return reply
	}
	if args.Term > rf.currentTerm {
		rf.stepDown(args.Term)
	}

	lastIdx, lastTerm := rf.lastLogInfo()
	logOK := args.LastLogTerm > lastTerm ||
		(args.LastLogTerm == lastTerm && args.LastLogIndex >= lastIdx)

	if (rf.votedFor == -1 || rf.votedFor == args.CandidateID) && logOK {
		rf.votedFor = args.CandidateID
		rf.savePersist()
		rf.resetTimer()
		reply.VoteGranted = true
	}

	reply.Term = rf.currentTerm
	return reply
}

// HandleAppendEntries processes an incoming AppendEntries RPC.
func (rf *Raft) HandleAppendEntries(args AppendEntriesArgs) AppendEntriesReply {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply := AppendEntriesReply{Term: rf.currentTerm}

	if args.Term < rf.currentTerm {
		return reply
	}
	if args.Term > rf.currentTerm || rf.role == Candidate {
		rf.stepDown(args.Term)
	}

	rf.leaderID = args.LeaderID
	rf.resetTimer()

	// Consistency check on prevLog
	if args.PrevLogIndex >= 0 {
		if args.PrevLogIndex >= len(rf.log) {
			reply.ConflictIndex = len(rf.log)
			reply.ConflictTerm = -1
			return reply
		}
		if rf.log[args.PrevLogIndex].Term != args.PrevLogTerm {
			reply.ConflictTerm = rf.log[args.PrevLogIndex].Term
			for i, e := range rf.log {
				if e.Term == reply.ConflictTerm {
					reply.ConflictIndex = i
					break
				}
			}
			return reply
		}
	}

	// Merge entries
	insertAt := args.PrevLogIndex + 1
	for i, entry := range args.Entries {
		idx := insertAt + i
		if idx < len(rf.log) {
			if rf.log[idx].Term != entry.Term {
				rf.log = rf.log[:idx]
				rf.log = append(rf.log, args.Entries[i:]...)
				break
			}
		} else {
			rf.log = append(rf.log, args.Entries[i:]...)
			break
		}
	}
	rf.savePersist()

	if args.LeaderCommit > rf.commitIndex {
		newCommit := args.LeaderCommit
		if lastIdx := len(rf.log) - 1; lastIdx < newCommit {
			newCommit = lastIdx
		}
		if newCommit > rf.commitIndex {
			rf.commitIndex = newCommit
		}
	}

	reply.Success = true
	return reply
}

// ---- Client Interface ----

// Submit appends a command to the log (leader only).
// Returns (logIndex, leaderID, isLeader).
func (rf *Raft) Submit(command string) (int, int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.role != Leader {
		return -1, rf.leaderID, false
	}

	index := len(rf.log)
	entry := LogEntry{
		Term:    rf.currentTerm,
		Index:   index,
		Command: command,
	}
	rf.log = append(rf.log, entry)
	rf.savePersist()

	rf.emitEvent("logAppend", map[string]interface{}{
		"nodeId":  rf.id,
		"index":   index,
		"term":    rf.currentTerm,
		"command": command,
	})

	term := rf.currentTerm
	for i, peer := range rf.peers {
		go rf.replicate(i, peer, term)
	}

	return index, rf.id, true
}

// WaitCommit polls until the entry at index is committed, then returns true.
// Returns false if this node is no longer leader.
func (rf *Raft) WaitCommit(index int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rf.mu.Lock()
		if rf.role != Leader {
			rf.mu.Unlock()
			return false
		}
		if rf.commitIndex >= index {
			rf.mu.Unlock()
			return true
		}
		rf.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// ---- Status / Admin ----

// Status returns a snapshot for the dashboard.
func (rf *Raft) Status() NodeStatus {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.snapshot()
}

func (rf *Raft) snapshot() NodeStatus {
	logCopy := make([]LogEntry, len(rf.log))
	copy(logCopy, rf.log)
	return NodeStatus{
		ID:          rf.id,
		Role:        rf.role.String(),
		Term:        rf.currentTerm,
		CommitIndex: rf.commitIndex,
		LeaderID:    rf.leaderID,
		LogLen:      len(rf.log),
		Log:         logCopy,
		Paused:      rf.isPaused(),
	}
}

// IsLeader reports whether this node is the current leader.
func (rf *Raft) IsLeader() bool {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.role == Leader
}

// LeaderID returns the last known leader ID, or -1 if unknown.
func (rf *Raft) LeaderID() int {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.leaderID
}

// Pause simulates a network partition: the node stops processing Raft RPCs.
func (rf *Raft) Pause() {
	atomic.StoreInt32(&rf.paused, 1)
	rf.mu.Lock()
	rf.role = Follower
	rf.leaderID = -1
	rf.electionTimer.Stop()
	rf.emitEvent("roleChange", rf.snapshot())
	rf.mu.Unlock()
}

// Resume re-enables the node.
func (rf *Raft) Resume() {
	atomic.StoreInt32(&rf.paused, 0)
	rf.mu.Lock()
	rf.resetTimer()
	rf.emitEvent("roleChange", rf.snapshot())
	rf.mu.Unlock()
}

// Reset wipes all state back to a clean follower, deletes the persist file,
// and restarts the election timer. The node rejoins the cluster from scratch.
func (rf *Raft) Reset() {
	rf.mu.Lock()
	rf.electionTimer.Stop()
	rf.currentTerm = 0
	rf.votedFor = -1
	rf.log = []LogEntry{}
	rf.commitIndex = -1
	rf.lastApplied = -1
	rf.role = Follower
	rf.leaderID = -1
	for i := range rf.nextIndex {
		rf.nextIndex[i] = 0
	}
	for i := range rf.matchIndex {
		rf.matchIndex[i] = -1
	}
	atomic.StoreInt32(&rf.paused, 0)
	if rf.persistPath != "" {
		_ = os.Remove(rf.persistPath)
	}
	rf.resetTimer()
	rf.emitEvent("roleChange", rf.snapshot())
	rf.mu.Unlock()
}

// Kill permanently stops the node.
func (rf *Raft) Kill() {
	atomic.StoreInt32(&rf.dead, 1)
	rf.electionTimer.Stop()
}

func (rf *Raft) killed() bool  { return atomic.LoadInt32(&rf.dead) == 1 }
func (rf *Raft) isPaused() bool { return atomic.LoadInt32(&rf.paused) == 1 }

// ---- Helpers ----

func (rf *Raft) lastLogInfo() (int, int) {
	// caller must hold rf.mu
	if len(rf.log) == 0 {
		return -1, -1
	}
	last := rf.log[len(rf.log)-1]
	return last.Index, last.Term
}

func (rf *Raft) emitEvent(eventType string, data interface{}) {
	select {
	case rf.eventCh <- Event{Type: eventType, Data: data}:
	default:
	}
}

func (rf *Raft) savePersist() {
	if rf.persistPath == "" {
		return
	}
	state := persistState{
		CurrentTerm: rf.currentTerm,
		VotedFor:    rf.votedFor,
		Log:         rf.log,
	}
	data, _ := json.Marshal(state)
	_ = os.WriteFile(rf.persistPath, data, 0644)
}

func (rf *Raft) loadPersist() {
	if rf.persistPath == "" {
		return
	}
	data, err := os.ReadFile(rf.persistPath)
	if err != nil {
		return
	}
	var state persistState
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	rf.currentTerm = state.CurrentTerm
	rf.votedFor = state.VotedFor
	rf.log = state.Log
	if len(rf.log) > 0 {
		rf.lastApplied = -1 // will re-apply on next applyLoop tick
	}
}

// rpc sends an HTTP POST RPC to a peer.
func (rf *Raft) rpc(peer, path string, args, reply interface{}) error {
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 80 * time.Millisecond}
	resp, err := client.Post("http://"+peer+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(reply)
}
