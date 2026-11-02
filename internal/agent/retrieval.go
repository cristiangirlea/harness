package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

func boundedQuery(text string, n int) string {
	if len(text) > n {
		text = text[:n]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return text
}
func saveRetrieval(dir string, state *retrieval.Record) error {
	if dir == "" {
		return fmt.Errorf("retrieval requires a persistent run directory")
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "retrieval.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("cannot persist retrieval record")
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func prepareRetrieval(ctx context.Context, backend string, task Task) (Task, *retrieval.Record, error) {
	previous := task.RetrievalState
	if task.Resume != nil && task.Resume.Retrieval != nil {
		previous = task.Resume.Retrieval
	}
	enabled := task.Retrieval != "" && task.Retrieval != "off"
	if !enabled && previous == nil {
		return task, nil, nil
	}
	if task.Durable != nil {
		return task, nil, fmt.Errorf("retrieval is unsupported for durable execution")
	}
	if task.Resume != nil && backend != "api" {
		return task, nil, fmt.Errorf("retrieval continuation is supported only for API conversations")
	}
	original := task.OriginalPrompt
	if original == "" {
		original = task.Prompt
	}
	state := &retrieval.Record{Mode: task.Retrieval, OriginalPrompt: original}
	started := time.Now()
	fail := func(err error) (Task, *retrieval.Record, error) {
		state.Failure = "retrieval failed: " + err.Error()
		state.DurationNS = time.Since(started).Nanoseconds()
		if e := saveRetrieval(task.RunDir, state); e != nil {
			return task, state, e
		}
		return task, state, err
	}
	if task.ForkSession != "" {
		return fail(fmt.Errorf("retrieval is unsupported for native session continuation"))
	}
	if previous != nil && enabled && task.Resume == nil && task.Retrieval == previous.Mode && !previous.Replayed && previous.Packet == nil && previous.Failure != "" && previous.Failure != "optional retrieval unavailable" {
		previous = nil
	}
	if previous != nil {
		state.Scope = previous.Scope // a refused replay still records the pin
		state.Mode = previous.Mode
		state.Packet = previous.Packet
		if task.Resume != nil {
			if err := kernel.ValidateContinuationPin(previous, task.Resume); err != nil {
				return fail(err)
			}
		}
		if task.Retrieval == "off" {
			return fail(fmt.Errorf("cannot discard pinned retrieval on retry or continuation"))
		}
		if task.Retrieval != "" && task.Retrieval != previous.Mode {
			return fail(fmt.Errorf("retrieval mode differs from saved pin"))
		}
		if previous.Failure != "" && previous.Failure != "optional retrieval unavailable" {
			return fail(fmt.Errorf("previous retrieval failed; pin cannot be resumed"))
		}
		state.Failure = previous.Failure
		state.Replayed = true
		sc := state.Scope
		if sc.Backend != backend || task.Model != sc.Model || (task.RetrievalTenant != "" && task.RetrievalTenant != sc.TenantID) || (task.RetrievalProject != "" && task.RetrievalProject != sc.ProjectID) || (task.RetrievalRevision != "" && task.RetrievalRevision != sc.Revision) || (task.RetrievalVersion != "" && (state.Packet == nil || task.RetrievalVersion != state.Packet.VersionID)) {
			return fail(fmt.Errorf("retrieval pin scope differs from execution"))
		}
		if state.Packet != nil {
			if err := retrieval.ValidatePacket(*state.Packet, "", ""); err != nil {
				return fail(err)
			}
		}
	} else {
		if err := ValidateOptions(task.Options); err != nil {
			return fail(err)
		}
		state.Scope = retrieval.Scope{TenantID: task.RetrievalTenant, ProjectID: task.RetrievalProject, Revision: task.RetrievalRevision, Backend: backend, Model: task.Model}
	}
	// Resumes already contain the pinned packet; retrieving again would duplicate it.
	search := previous == nil || (task.Resume == nil && task.RetrievalRepair && state.Packet != nil && state.Packet.VersionID != "")
	if search {
		if task.Knowledge == nil {
			return fail(fmt.Errorf("retrieval requires Knowledge"))
		}
		client, err := task.Knowledge()
		if err != nil {
			return fail(fmt.Errorf("retrieval transport configuration invalid"))
		}
		query := boundedQuery(task.Prompt, 2048)
		if task.RetrievalRepair {
			tail := task.Prompt
			if len(tail) > 1400 {
				tail = tail[len(tail)-1400:]
				for !utf8.ValidString(tail) {
					tail = tail[1:]
				}
			}
			query = boundedQuery(original, 600) + "\n" + tail
		}
		version := task.RetrievalVersion
		if previous != nil {
			version = state.Packet.VersionID
		}
		packet, err := client.SearchKnowledge(ctx, retrieval.Search{Scope: state.Scope, VersionID: version, Query: query, RunRef: filepath.Base(task.RunDir)})
		if err != nil {
			if ctx.Err() != nil {
				return fail(fmt.Errorf("retrieval canceled"))
			}
			if state.Mode != "optional" || !errors.Is(err, retrieval.ErrUnavailable) {
				return fail(fmt.Errorf("retrieval request rejected or unavailable: %w", err))
			}
			if previous == nil {
				state.Packet = nil
			}
			state.Failure = "optional retrieval unavailable"
		} else {
			state.Packet = &packet
			state.Failure = ""
		}
	}
	if (task.Resume == nil || previous == nil) && state.Packet != nil {
		task.Prompt += retrieval.ReferenceBlock(state.Packet.Text)
	}
	state.DurationNS = time.Since(started).Nanoseconds()
	if err := saveRetrieval(task.RunDir, state); err != nil {
		return task, state, err
	}
	task.OriginalPrompt = original
	task.RetrievalState = state
	return task, state, nil
}
