// Package game is Pager Panic's game: the shift clock, stages, lives, tickets, score and
// medals. It never touches the cluster. Everything it wants done there goes out as an
// Action in the answer to DBCanvas's next sync, and comes back as a Result.
package game

import "encoding/json"

// The sync protocol, as DBCanvas speaks it (app/pagerpanic_sync.go).

type Action struct {
	ID       string `json:"id"`
	Op       string `json:"op"` // diagnose | setup | check | cleanup
	Task     string `json:"task,omitempty"`
	Seed     int64  `json:"seed,omitempty"`
	Instance string `json:"instance,omitempty"`
}

type Result struct {
	ID       string        `json:"id"`
	Op       string        `json:"op"`
	Done     bool          `json:"done"`
	OK       bool          `json:"ok"`
	Error    string        `json:"error,omitempty"`
	Steps    []DiagStep    `json:"steps,omitempty"`
	Env      *Env          `json:"env,omitempty"`
	Instance string        `json:"instance,omitempty"`
	Worker   string        `json:"worker,omitempty"`
	View     *TaskView     `json:"view,omitempty"`
	Checks   []CheckResult `json:"checks,omitempty"`
	Solved   bool          `json:"solved,omitempty"`
}

type DiagStep struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type Env struct {
	ControlPlane string   `json:"controlPlane"`
	Workers      []string `json:"workers"`
	CNI          string   `json:"cni"`
	Version      string   `json:"version"`
}

type TaskView struct {
	Title    string   `json:"title"`
	Brief    string   `json:"brief"`
	Hints    []string `json:"hints"`
	Solution string   `json:"solution"`
	Explain  string   `json:"explain"`
}

type CheckResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Guard bool   `json:"guard,omitempty"`
	Got   string `json:"got"`
	Want  string `json:"want"`
}

type Node struct {
	Host      string `json:"host"`
	Role      string `json:"role"`
	NodeID    string `json:"nodeId"`
	Container string `json:"container"`
}

type StackRef struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Frame string `json:"frame"`
}

type Category struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Medal   string `json:"medal"`
	MedalAt int    `json:"medal_at"`
}

type Stage struct {
	Open       int     `json:"open"`
	SpawnEvery float64 `json:"spawnEvery"`
	TimeScale  float64 `json:"timeScale"`
	Difficulty []int   `json:"difficulty"`
}

type Rank struct {
	Min   int    `json:"min"`
	Title string `json:"title"`
}

type PublicTask struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Kind        string   `json:"kind"`
	Category    string   `json:"category"`
	Difficulty  int      `json:"difficulty"`
	Points      int      `json:"points"`
	TimeLimit   float64  `json:"timeLimit"`
	Boss        bool     `json:"boss,omitempty"`
	Locks       []string `json:"locks,omitempty"`
	Blocks      []string `json:"blocks,omitempty"`
	Needs       []string `json:"needs,omitempty"`
	Hints       []int    `json:"hints,omitempty"`
	Unsupported string   `json:"unsupported,omitempty"`
}

type Bank struct {
	Bank       string       `json:"bank"`
	Title      string       `json:"title"`
	Shift      float64      `json:"shift"`
	Lives      int          `json:"lives"`
	Categories []Category   `json:"categories"`
	Stages     []Stage      `json:"stages"`
	Ranks      []Rank       `json:"ranks"`
	Tasks      []PublicTask `json:"tasks"`
}

type SyncRequest struct {
	Stack      StackRef        `json:"stack"`
	Nodes      []Node          `json:"nodes"`
	Env        Env             `json:"env"`
	BankHash   string          `json:"bankHash"`
	Bank       *Bank           `json:"bank,omitempty"`
	BankErrors []string        `json:"bankErrors,omitempty"`
	ClusterErr string          `json:"clusterError,omitempty"`
	Results    []Result        `json:"results"`
	Saved      json.RawMessage `json:"saved,omitempty"`
}

type SyncResponse struct {
	Actions   []Action        `json:"actions"`
	Ack       []string        `json:"ack"`
	NeedBank  bool            `json:"needBank"`
	NeedSaved bool            `json:"needSaved"`
	Save      json.RawMessage `json:"save,omitempty"`
	Active    []string        `json:"active"`
}
