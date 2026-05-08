package raft

// Role represents the Raft node state.
type Role int

const (
	Follower Role = iota
	Candidate
	Leader
)

func (r Role) String() string {
	switch r {
	case Follower:
		return "follower"
	case Candidate:
		return "candidate"
	case Leader:
		return "leader"
	}
	return "unknown"
}

// LogEntry is a single entry in the replicated log.
type LogEntry struct {
	Term    int    `json:"term"`
	Index   int    `json:"index"`
	Command string `json:"command"`
}

// RequestVoteArgs is the argument to the RequestVote RPC.
type RequestVoteArgs struct {
	Term         int `json:"term"`
	CandidateID  int `json:"candidateId"`
	LastLogIndex int `json:"lastLogIndex"`
	LastLogTerm  int `json:"lastLogTerm"`
}

// RequestVoteReply is the reply from the RequestVote RPC.
type RequestVoteReply struct {
	Term        int  `json:"term"`
	VoteGranted bool `json:"voteGranted"`
}

// AppendEntriesArgs is the argument to the AppendEntries RPC (also used as heartbeat).
type AppendEntriesArgs struct {
	Term         int        `json:"term"`
	LeaderID     int        `json:"leaderId"`
	PrevLogIndex int        `json:"prevLogIndex"`
	PrevLogTerm  int        `json:"prevLogTerm"`
	Entries      []LogEntry `json:"entries"`
	LeaderCommit int        `json:"leaderCommit"`
}

// AppendEntriesReply is the reply from the AppendEntries RPC.
type AppendEntriesReply struct {
	Term          int  `json:"term"`
	Success       bool `json:"success"`
	ConflictTerm  int  `json:"conflictTerm"`
	ConflictIndex int  `json:"conflictIndex"`
}

// NodeStatus is the public snapshot of a node's state, used by the dashboard.
type NodeStatus struct {
	ID          int        `json:"id"`
	Role        string     `json:"role"`
	Term        int        `json:"term"`
	CommitIndex int        `json:"commitIndex"`
	LeaderID    int        `json:"leaderId"`
	LogLen      int        `json:"logLen"`
	Log         []LogEntry `json:"log"`
	Paused      bool       `json:"paused"`
}

// Event is an SSE event emitted to the dashboard.
type Event struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// persistState is what gets written to disk.
type persistState struct {
	CurrentTerm int        `json:"currentTerm"`
	VotedFor    int        `json:"votedFor"`
	Log         []LogEntry `json:"log"`
}
