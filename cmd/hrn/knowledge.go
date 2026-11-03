package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/cristiangirlea/harness/internal/remote"
	"github.com/cristiangirlea/harness/internal/retrieval"
)

func readRetrievalJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err != nil {
		return err
	}
	if len(b) > 8<<20 {
		return fmt.Errorf("input exceeds 8 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("extra JSON input")
	}
	return nil
}

func runKnowledge(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "admit" {
		return runKnowledgeAdmit(args[1:], out)
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: hrn knowledge import|search|inspect|active|activate|disable|rollback request.json")
	}
	operation := args[0]
	var input any
	operator := false
	route := operation
	switch operation {
	case "import":
		input = &retrieval.Import{}
		operator = true
	case "search":
		input = &retrieval.Search{}
	case "inspect":
		input = &struct {
			retrieval.Scope
			VersionID string `json:"version_id"`
		}{}
	case "active":
		input = &retrieval.Scope{}
	case "activate", "disable", "rollback":
		input = &retrieval.Change{}
		operator = true
		route = "change"
	default:
		return fmt.Errorf("unknown knowledge operation")
	}
	if err := readRetrievalJSON(args[1], input); err != nil {
		return err
	}
	if change, ok := input.(*retrieval.Change); ok {
		change.Operation = operation
	}
	var c *remote.Client
	var err error
	if operator {
		c, err = remote.NewOperatorFromEnv()
	} else {
		c, err = remote.NewFromEnv()
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if search, ok := input.(*retrieval.Search); ok {
		packet, err := c.SearchKnowledge(ctx, *search)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(packet)
	}
	data, err := c.Request(ctx, http.MethodPost, "/v1/knowledge/"+route, input)
	if err != nil {
		return err
	}
	return remoteJSON(out, data)
}
