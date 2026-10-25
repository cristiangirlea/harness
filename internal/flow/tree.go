package flow

import "time"

// Node is one step with its latest state and its child steps in the order they began.
type Node struct {
	Step     string    `json:"step"`
	Kind     Kind      `json:"kind"`
	Name     string    `json:"name,omitempty"`
	Status   Status    `json:"status,omitempty"`
	Text     string    `json:"text,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Next     string    `json:"next,omitempty"`
	Run      string    `json:"run,omitempty"`
	Task     string    `json:"task,omitempty"`
	Attempt  int       `json:"attempt,omitempty"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitzero"`
	Derived  bool      `json:"derived,omitempty"`
	Orphan   bool      `json:"orphan,omitempty"` // its parent was never recorded: a gap in the record
	Children []*Node   `json:"children,omitempty"`
}

// Build folds events into a forest; an event for a known step updates it rather than adding one.
func Build(events []Event) []*Node {
	byStep := map[string]*Node{}
	var roots []*Node
	parentOf := map[*Node]string{}
	var order []*Node
	for _, e := range events {
		n := byStep[e.Step]
		if n == nil {
			n = &Node{Step: e.Step, Kind: e.Kind, Started: e.Time, Derived: e.Derived}
			byStep[e.Step] = n
			parentOf[n] = e.Parent
			order = append(order, n)
		}
		apply(n, e)
	}
	for _, n := range order {
		p := parentOf[n]
		switch parent := byStep[p]; {
		case p == "":
			roots = append(roots, n)
		case parent == nil || loops(n, parent, byStep, parentOf):
			n.Orphan = true
			roots = append(roots, n)
		default:
			parent.Children = append(parent.Children, n)
		}
	}
	return roots
}

// loops reports whether parent descends from n, which would hide both from every root.
func loops(n, parent *Node, byStep map[string]*Node, parentOf map[*Node]string) bool {
	for i, p := 0, parent; p != nil && i <= len(byStep); i++ {
		if p == n {
			return true
		}
		p = byStep[parentOf[p]]
	}
	return false
}

func apply(n *Node, e Event) {
	if e.Name != "" {
		n.Name = e.Name
	}
	if e.Status != "" {
		n.Status = e.Status
		if e.Status.Done() {
			n.Ended = e.Time
		}
	}
	if e.Text != "" {
		n.Text = e.Text
	}
	if e.Reason != "" {
		n.Reason = e.Reason
	}
	// Next is cleared once a step ends: what was planned has happened or will not.
	n.Next = e.Next
	if e.Run != "" {
		n.Run = e.Run
	}
	if e.Task != "" {
		n.Task = e.Task
	}
	if e.Attempt != 0 {
		n.Attempt = e.Attempt
	}
}

// Walk visits nodes depth first with their depth.
func Walk(nodes []*Node, fn func(n *Node, depth int)) {
	var walk func([]*Node, int)
	walk = func(ns []*Node, d int) {
		for _, n := range ns {
			fn(n, d)
			walk(n.Children, d+1)
		}
	}
	walk(nodes, 0)
}
