package tools

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func (ws *Workspace) checkParse(path string, content []byte) error {
	ext := strings.ToLower(filepath.Ext(path))
	if !ws.ParseCheck || (ext != ".go" && ext != ".json") {
		return nil
	}
	rel := ws.Rel(path)
	parse := func(data []byte) error {
		if ext == ".go" {
			_, err := parser.ParseFile(token.NewFileSet(), rel, data, parser.SkipObjectResolution)
			return err
		}
		if json.Valid(data) {
			return nil
		}
		var value any
		return json.Unmarshal(data, &value)
	}
	err := parse(content)
	if err == nil {
		return nil
	}
	old, readErr := os.ReadFile(path)
	if readErr == nil && parse(old) != nil {
		return nil
	}
	line, col, message := 1, 1, err.Error()
	if list, ok := err.(scanner.ErrorList); ok && len(list) > 0 {
		line, col, message = list[0].Pos.Line, list[0].Pos.Column, list[0].Msg
	} else if syntax, ok := err.(*json.SyntaxError); ok {
		for i := int64(0); i < syntax.Offset-1 && i < int64(len(content)); i++ {
			if content[i] == '\n' {
				line, col = line+1, 1
			} else {
				col++
			}
		}
	}
	return fmt.Errorf("%s would not parse, so it was not changed: %d:%d: %s", rel, line, col, message)
}
