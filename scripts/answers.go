// name: answers
// summary: Provide JSON test summaries and bounded TCP probes for Bash entry points.
// effects: read, exec, network
// parameters: mode:string=required; arguments:string[]=mode-specific
// example: test "$(go run scripts/answers.go self-test)" = '{"ready":true}'
// hash: 7cc827a3ba23e568a97f60e551a2da6fb63fe8b023c026bf9aeb912f7dedeef3
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

func die(message string) {
	_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": message})
	os.Exit(1)
}

func summary(args []string) {
	var input io.Reader = os.Stdin
	if len(args) > 1 {
		die("usage: summary [file]")
	}
	if len(args) == 1 {
		f, err := os.Open(args[0])
		if err != nil {
			die("cannot open input")
		}
		defer f.Close()
		input = f
	}
	result := struct {
		Passed    int      `json:"passed"`
		Failed    int      `json:"failed"`
		Skipped   int      `json:"skipped"`
		Failures  []string `json:"failures"`
		Truncated bool     `json:"truncated"`
	}{Failures: []string{}}
	decoder := json.NewDecoder(input)
	for {
		var event struct{ Action, Test string }
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			die("invalid test JSON")
		}
		if event.Test == "" {
			continue
		}
		switch event.Action {
		case "pass":
			result.Passed++
		case "skip":
			result.Skipped++
		case "fail":
			result.Failed++
			name := []rune(event.Test)
			if len(name) > 200 {
				name = name[:200]
				result.Truncated = true
			}
			if len(result.Failures) < 20 {
				result.Failures = append(result.Failures, string(name))
			} else {
				result.Truncated = true
			}
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		die("cannot write summary")
	}
}

// probeTimeout keeps a probe inside the deadline, with a floor so a zero timeout still dials once.
func probeTimeout(remaining time.Duration) time.Duration {
	return max(min(remaining, 200*time.Millisecond), 10*time.Millisecond)
}

func wait(args []string) {
	if len(args) < 2 || len(args) > 3 || args[0] == "" {
		die("usage: wait host port [timeout seconds]")
	}
	port, err := strconv.Atoi(args[1])
	if err != nil || port < 1 || port > 65535 {
		die("invalid port")
	}
	seconds := 10
	if len(args) == 3 {
		seconds, err = strconv.Atoi(args[2])
		if err != nil || seconds < 0 || seconds > 86400 {
			die("invalid timeout (0..86400 seconds)")
		}
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(args[0], args[1]), probeTimeout(time.Until(deadline)))
		if err == nil {
			conn.Close()
			fmt.Println(`{"ready":true}`)
			return
		}
		if time.Until(deadline) <= 0 {
			die("timeout")
		}
		time.Sleep(min(time.Until(deadline), 50*time.Millisecond))
	}
}

func main() {
	if len(os.Args) < 2 {
		die("mode required")
	}
	switch os.Args[1] {
	case "summary":
		summary(os.Args[2:])
	case "wait":
		wait(os.Args[2:])
	case "self-test":
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			die("cannot listen")
		}
		defer listener.Close()
		host, port, _ := net.SplitHostPort(listener.Addr().String())
		wait([]string{host, port, "1"})
	default:
		die("unknown mode")
	}
}
