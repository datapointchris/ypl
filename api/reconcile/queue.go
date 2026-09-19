package reconcile

import (
	"slices"
	"sync"
)

// Priority orders the jobs of a pass. The lowest goes first.
type Priority int

const (
	// PriorityEdit is a playlist whose order an edit changed, pushed before
	// anything else and allowed the quota kept back for edits.
	PriorityEdit Priority = iota
	// PriorityChanged is the probe, and each playlist it finds YouTube has
	// changed or the store has never read.
	PriorityChanged
	// PriorityUnfinished is a playlist whose last push stopped before YouTube
	// held the server's order.
	PriorityUnfinished
	// PrioritySweep is the playlist read longest ago, read whatever the probe
	// found, since a change that leaves its count alone reaches no probe.
	PrioritySweep
	// PriorityLengths is reading the lengths of videos the store holds none for.
	PriorityLengths
)

// Kind is what a job does.
type Kind int

const (
	// KindProbe lists every playlist the channel owns for one unit and queues a
	// sync of each whose count moved since its last read.
	KindProbe Kind = iota
	// KindSync reads one playlist's items, merges them into the server's order
	// and pushes that order back.
	KindSync
	// KindLengths reads the lengths of the videos the store holds none for.
	KindLengths
)

// Job is one piece of a pass.
type Job struct {
	Kind     Kind
	Priority Priority
	// Playlist is the playlist a sync reads.
	Playlist string
	// Full is a probe that syncs every playlist it lists, whatever its count.
	Full bool
}

// queue is the jobs a pass has yet to do, in the order it does them.
type queue struct {
	jobs []Job
}

// add queues j behind every job of its priority or ahead of it. A playlist
// already queued keeps one sync, at the earlier of the two priorities.
func (q *queue) add(j Job) {
	if j.Kind == KindSync {
		i := slices.IndexFunc(q.jobs, func(queued Job) bool { return queued.Kind == KindSync && queued.Playlist == j.Playlist })
		if i >= 0 {
			if q.jobs[i].Priority <= j.Priority {
				return
			}
			q.jobs = slices.Delete(q.jobs, i, i+1)
		}
	}
	at := slices.IndexFunc(q.jobs, func(queued Job) bool { return queued.Priority > j.Priority })
	if at < 0 {
		at = len(q.jobs)
	}
	q.jobs = slices.Insert(q.jobs, at, j)
}

// next takes the first job, and is false once none is left.
func (q *queue) next() (Job, bool) {
	if len(q.jobs) == 0 {
		return Job{}, false
	}
	j := q.jobs[0]
	q.jobs = q.jobs[1:]
	return j, true
}

// holds is whether a sync of playlist is queued.
func (q *queue) holds(playlist string) bool {
	return slices.ContainsFunc(q.jobs, func(j Job) bool { return j.Kind == KindSync && j.Playlist == playlist })
}

// Edits is the playlists whose order an edit changed and that no pass has taken
// up yet. The API adds to it and the worker takes from it, so it is safe for
// both at once. A nil Edits takes nothing and wakes nobody.
//
// It is only what wakes a pass early. A playlist an edit left unpushed is
// found again from the store on the next tick, so nothing here needs keeping
// across a restart.
type Edits struct {
	mu      sync.Mutex
	pending []string
	wake    chan struct{}
}

// NewEdits is an Edits holding nothing.
func NewEdits() *Edits {
	return &Edits{wake: make(chan struct{}, 1)}
}

// Edited queues a push of the playlist id ahead of every other job, and wakes
// the worker to make it.
func (e *Edits) Edited(id string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if !slices.Contains(e.pending, id) {
		e.pending = append(e.pending, id)
	}
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// take is every playlist edited since the last take, in the order they were.
func (e *Edits) take() []string {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	taken := e.pending
	e.pending = nil
	return taken
}

// waiting is whether an edit is waiting to be taken.
func (e *Edits) waiting() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pending) > 0
}

// woken is signaled once an edit arrives.
func (e *Edits) woken() <-chan struct{} {
	if e == nil {
		return nil
	}
	return e.wake
}
