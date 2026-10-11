package agent

import (
	"encoding/json"
	"strings"
	"time"
)

// ---- gemini ---------------------------------------------------------------
func NewGeminiBackend() *CLIBackend {
	return &CLIBackend{
		name:        "gemini",
		mapsModes:   []Mode{ModeAuto, ModeYolo},
		mapsModel:   true,
		candidates:  func() []string { return npmShim("gemini") },
		promptStdin: true,
		args: func(t Task, _ string) []string {
			a := []string{"-o", "json"}
			if t.Model != "" {
				a = append(a, "-m", t.Model)
			}
			mode := map[Mode]string{ModeReadOnly: "default", ModeAsk: "default", ModeAuto: "auto_edit", ModeYolo: "yolo"}[t.Mode]
			return append(a, "--approval-mode", mode)
		},
		finalize: func(out []byte, emit func(Event), res *Result) {
			// stdout may carry log lines before the JSON document.
			start := strings.IndexByte(string(out), '{')
			var doc struct {
				Response string `json:"response"`
				Error    *struct {
					Message string `json:"message"`
				} `json:"error"`
				Stats struct {
					Models map[string]struct {
						Tokens struct {
							Prompt     int64 `json:"prompt"`
							Candidates int64 `json:"candidates"`
							Cached     int64 `json:"cached"`
						} `json:"tokens"`
					} `json:"models"`
				} `json:"stats"`
			}
			if start < 0 || json.Unmarshal(out[start:], &doc) != nil {
				res.Final = strings.TrimSpace(string(out))
				emit(Event{Time: time.Now(), Kind: EventText, Text: res.Final})
				return
			}
			if doc.Error != nil {
				res.Status, res.Error = StatusError, doc.Error.Message
			}
			for name, m := range doc.Stats.Models {
				res.Model = name
				res.ModelReported = true
				res.InputTokens += m.Tokens.Prompt
				res.OutputTokens += m.Tokens.Candidates
				res.CacheRead += m.Tokens.Cached
			}
			res.Final = doc.Response
			emit(Event{Time: time.Now(), Kind: EventText, Text: doc.Response})
		},
	}
}
