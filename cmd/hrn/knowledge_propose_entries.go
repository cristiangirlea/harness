package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

type proposalEntry struct {
	Title       string   `json:"title"`
	Content     string   `json:"content"`
	Evidence    string   `json:"evidence"`
	Identifiers []string `json:"identifiers"`
}

func parseProposal(text string) ([]proposalEntry, error) {
	text = strings.TrimSpace(text)
	if !utf8.ValidString(text) {
		return nil, errors.New("invalid UTF-8")
	}
	if first, rest, ok := strings.Cut(text, "\n"); ok && strings.TrimSuffix(first, "\r") == "```json" {
		body, last, ok := strings.Cut(rest, "\n```")
		if !ok || last != "" {
			return nil, errors.New("invalid JSON fence")
		}
		text = body
	}
	var result struct {
		Entries []proposalEntry `json:"entries"`
	}
	d := json.NewDecoder(strings.NewReader(text))
	d.DisallowUnknownFields()
	if err := d.Decode(&result); err != nil || result.Entries == nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("expected one JSON object with an entries array")
	}
	return result.Entries, nil
}

var proposalSlugBreak = regexp.MustCompile(`[^a-z0-9]+`)

func buildProposal(runID, grounding string, p proposalEntry, number int) (retrieval.Entry, string) {
	content := strings.TrimSpace(p.Content)
	if len(content) == 0 || len(content) > 2000 || !utf8.ValidString(content) {
		return retrieval.Entry{}, "invalid content"
	}
	for _, marker := range []string{proposeBegin, proposeEnd} {
		if strings.Contains(content, marker) {
			return retrieval.Entry{}, "content matched " + marker
		}
		if strings.Contains(p.Evidence, marker) {
			return retrieval.Entry{}, "evidence matched " + marker
		}
	}
	slug := strings.Trim(proposalSlugBreak.ReplaceAllString(strings.ToLower(p.Title), "-"), "-")
	if slug == "" {
		slug = fmt.Sprintf("entry-%d", number)
	}
	e := retrieval.Entry{
		Key:      proposalHead("learned/"+runID+"/"+slug, retrieval.MaxEntryKeyBytes),
		Source:   "run:" + runID,
		Content:  content,
		Evidence: proposalHead("run "+runID+": "+strings.TrimSpace(p.Evidence), 1024),
	}
	// Clipping evidence can turn a longer token into a run ID.
	for _, text := range []string{content, p.Evidence, e.Evidence} {
		for id := range admissionRunIDs(text) {
			if id != runID {
				return retrieval.Entry{}, "evidence names another run"
			}
		}
	}
	for _, field := range []struct {
		name, text string
		paths      bool
	}{
		{"key", e.Key, true}, {"source", e.Source, true}, {"content", e.Content, false}, {"evidence", e.Evidence, false},
	} {
		if pattern := retrieval.UnsafeText(field.text, field.paths); pattern != "" {
			return retrieval.Entry{}, fmt.Sprintf("%s matched %q", field.name, pattern)
		}
	}
	for _, id := range p.Identifiers {
		if len(id) < 3 || len(id) > retrieval.MaxIdentifierBytes || id == runID || !markdownIdentifier.MatchString(id) {
			continue
		}
		if pattern := retrieval.UnsafeText(id, true); pattern != "" {
			return retrieval.Entry{}, fmt.Sprintf("identifiers matched %q", pattern)
		}
		if !strings.Contains(grounding, id) {
			continue
		}
		e.Identifiers = append(e.Identifiers, id)
		if len(e.Identifiers) == retrieval.MaxEntryIdentifiers {
			break
		}
	}
	if len(e.Identifiers) == 0 {
		return retrieval.Entry{}, "ungrounded"
	}
	return e, ""
}

func proposalHead(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

func proposalWords(s string) map[string]bool {
	words := map[string]bool{}
	for _, word := range strings.Fields(s) {
		words[word] = true
	}
	return words
}

func proposalDuplicate(content string, active, kept []retrieval.Entry) bool {
	normalize := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	text := normalize(content)
	words := proposalWords(text)
	for i, entries := range [][]retrieval.Entry{active, kept} {
		for _, entry := range entries {
			other := normalize(entry.Content)
			if i == 0 && strings.Contains(other, text) {
				return true
			}
			otherWords := proposalWords(other)
			common := 0
			for word := range words {
				if otherWords[word] {
					common++
				}
			}
			union := len(words) + len(otherWords) - common
			if 5*common >= 4*union {
				return true
			}
		}
	}
	return false
}
