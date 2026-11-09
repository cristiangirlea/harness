package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/ws"
)

// Git leaves shared worktree metadata unlocked; serialize its mutations.
var worktreeMu sync.Mutex

var slotPoll = 5 * time.Second

// Allow lease writes to finish; bound waits on unreadable leftovers.
var unreadableWait = 30 * time.Second

// Bound retries against session starvation.
const sessionRetries = 3

func takeSlot(ctx context.Context, spec *Spec, batchID string, kept *keptSlots, id, branch, root, base string, notify func(Event), made func(mine bool) error) (ws.Taken, error) {
	return takeSlotFlow(ctx, spec, batchID, kept, id, branch, root, base, nil, notify, made)
}

// Save ownership before waiting; clear it before branch deletion.
// On save failure, leak rather than delete unproven work.
func takeSlotFlow(ctx context.Context, spec *Spec, batchID string, kept *keptSlots, id, branch, root, base string, fw *flow.Writer, notify func(Event), made func(mine bool) error) (_ ws.Taken, failed error) {
	step := SlotStep(id)
	fw.Emit(flow.Event{Step: step, Parent: TaskStep(id), Kind: flow.KindSlot, Task: id, Status: flow.StatusWaiting, Next: "waits for a free worktree slot"})
	defer func() {
		if failed == nil {
			return
		}
		st := flow.StatusFailed
		if ctx.Err() != nil {
			st = flow.StatusCanceled
		}
		fw.Emit(flow.Event{Step: step, Kind: flow.KindSlot, Task: id, Status: st, Text: firstLineOf(failed.Error())})
	}()
	limit, err := ws.ReadLimit(ctx, root)
	if err != nil {
		return ws.Taken{}, err
	}
	if _, err := gitOut(ctx, root, "branch", branch, base); err != nil {
		return ws.Taken{}, err
	}
	_ = made(true) // unsaved, the branch is only left behind if hrn crashes
	drop := func(cause error) (ws.Taken, error) {
		if err := made(false); err != nil {
			return ws.Taken{}, fmt.Errorf("%w; branch %s is left: saving that it is no longer the task's failed: %v", cause, branch, err)
		}
		dropBranch(root, branch)
		return ws.Taken{}, cause
	}
	prefix := "batch " + batchID + " "
	pid := os.Getpid()
	mine := func(l ws.Lease) bool { return l.PID == pid && strings.HasPrefix(l.Holder, prefix) }
	bound := min(max(spec.Parallel, ws.DefaultMax), limit.Max)
	opt := ws.TakeOptions{Repo: root, Branch: branch, Base: base, Holder: prefix + "task " + id, PID: pid, NoFetch: true}
	// Count and take under worktreeMu to enforce the slot bound.
	try := func() (ws.Taken, error) {
		worktreeMu.Lock()
		defer worktreeMu.Unlock()
		own, err := ws.CountLeases(ctx, root, mine)
		if err != nil {
			return ws.Taken{}, err
		}
		if own >= bound {
			return ws.Taken{}, &atBoundError{held: own, kept: kept.count()}
		}
		opt.Max, err = ws.RangeBeyondSessions(ctx, root, bound)
		if err != nil {
			return ws.Taken{}, err
		}
		// Recompute after racing sessions; bound retries against starvation.
		for retry := 0; ; retry++ {
			if rangeComputed != nil {
				rangeComputed(opt.Max)
			}
			got, err := ws.Take(ctx, opt)
			var full *ws.NoSlotError
			if !errors.As(err, &full) || !holdsASession(full) || retry == sessionRetries {
				return got, err
			}
			wider, rerr := ws.RangeBeyondSessions(ctx, root, bound)
			if rerr != nil {
				return ws.Taken{}, rerr
			}
			if wider <= opt.Max {
				return got, err
			}
			opt.Max = wider
		}
	}
	var said string
	unreadable := unreadableLeases{}
	for {
		got, err := try()
		if err == nil {
			fw.Emit(flow.Event{Step: step, Parent: TaskStep(id), Kind: flow.KindSlot, Task: id, Status: flow.StatusOK, Text: fmt.Sprintf("slot %d at %s", got.N, got.Path)})
			return got, nil
		}
		var full *ws.NoSlotError
		var bounded *atBoundError
		var wait string
		switch {
		case errors.As(err, &bounded):
			if bounded.kept >= bounded.held {
				return drop(fmt.Errorf("%w: none will come free while it runs", err))
			}
			// No lease observed means no consecutive unreadable observation.
			clear(unreadable)
			wait = "waiting for a slot this batch holds to come back: " + err.Error()
			if limit.Path != "" {
				wait += "; " + limit.String()
			}
		case errors.As(err, &full):
			// Only live processes or briefly unreadable leases may free slots.
			stuck := unreadable.note(full, time.Now())
			switch {
			case someoneWillFree(full, kept):
			case stuck:
				return drop(unreadableError(full))
			case len(full.Unreadable) == 0:
				return drop(fmt.Errorf("no slot is free and none will come free while this batch runs: %w", full))
			}
			wait = "waiting for a free slot: " + strings.Join(full.Passed, "; ")
			if full.Limit.Path != "" {
				wait += "; " + full.Limit.String()
			} else if full.Max == ws.Limit {
				wait += fmt.Sprintf("; at most %d slots per repository", ws.Limit)
			}
		default:
			return drop(err)
		}
		if wait != said {
			fw.Emit(flow.Event{Step: step, Parent: TaskStep(id), Kind: flow.KindSlot, Task: id, Status: flow.StatusWaiting, Text: wait})
			notify(Event{Task: id, Kind: "wait", Text: wait})
			said = wait
		}
		select {
		case <-ctx.Done():
			return drop(ctx.Err())
		case <-time.After(slotPoll):
		}
	}
}

type atBoundError struct{ held, kept int }

func (e *atBoundError) Error() string {
	return fmt.Sprintf("this batch already holds %d slots, the most it may (%d kept)", e.held, e.kept)
}

func holdsASession(full *ws.NoSlotError) bool {
	for _, l := range full.Holders {
		if l.PID == 0 {
			return true
		}
	}
	return false
}

// Sessions never free themselves; unreadable leases need a grace period.
func someoneWillFree(full *ws.NoSlotError, kept *keptSlots) bool {
	// More held than listed: a lease changed hands mid-take, which only a live process does.
	if full.Held > len(full.Holders)+len(full.Unreadable) {
		return true
	}
	for _, l := range full.Holders {
		if l.PID != 0 && !kept.has(l.Holder) {
			return true
		}
	}
	return false
}

type unreadableLeases map[int]time.Time

// Only consecutive unreadable observations prove a stuck lease.
func (u unreadableLeases) note(full *ws.NoSlotError, now time.Time) bool {
	seen := make(map[int]bool, len(full.Unreadable))
	stuck := len(full.Unreadable) > 0
	for _, n := range full.Unreadable {
		seen[n] = true
		since, ok := u[n]
		if !ok {
			since = now
			u[n] = now
		}
		if now.Sub(since) < unreadableWait {
			stuck = false
		}
	}
	for n := range u {
		if !seen[n] {
			delete(u, n)
		}
	}
	return stuck
}

// Remove lease files only with hrn stopped: live takes start empty and takeovers guard dead pids.
func unreadableError(full *ws.NoSlotError) error {
	var lines []string
	for _, n := range full.Unreadable {
		prefix := "slot " + strconv.Itoa(n) + " "
		for _, line := range full.Passed {
			if strings.HasPrefix(line, prefix) {
				lines = append(lines, line)
			}
		}
	}
	return fmt.Errorf("no slot is free: these leases could not be read for %s and no process will free them:\n  %s\ncheck the slots with `hrn ws list`. A lease that stays unreadable, or a left .takeover file, may be removed only while no hrn command at all runs on this repository, no hrn ws and no hrn batch: a live take creates its lease empty before writing it, and a live hrn ws free or takeover holds the .takeover file. Check with Task Manager or `tasklist | findstr hrn` on Windows, `pgrep hrn` elsewhere",
		unreadableWait, strings.Join(lines, "\n  "))
}

// Keep failed releases leased so other tasks cannot clean their work.
type keptSlots struct {
	mu      sync.Mutex
	holders map[string]bool
}

func (k *keptSlots) add(holder string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.holders == nil {
		k.holders = map[string]bool{}
	}
	k.holders[holder] = true
}

func (k *keptSlots) has(holder string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.holders[holder]
}

func (k *keptSlots) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.holders)
}

func dropBranch(root, branch string) {
	if droppingBranch != nil {
		droppingBranch()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = gitOut(ctx, root, "branch", "-D", branch)
}

// Release at base so branch deletion cannot leave an unreferenced slot tip.
// Dirty slots stay leased until exit; the next take must skip their work.
func giveBack(slot ws.Taken, base string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return ws.Release(ctx, slot, base)
}
