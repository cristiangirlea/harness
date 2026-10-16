package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/remote"
)

var remoteID = regexp.MustCompile("^[tpj]_[0-9a-f]{32}$")
var remoteRef = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")

type remoteConnection struct {
	*remote.Client
}

func newRemoteConnection() (*remoteConnection, error) {
	client, err := remote.NewFromEnv()
	if err != nil {
		return nil, err
	}
	return &remoteConnection{Client: client}, nil
}

func (c *remoteConnection) request(ctx context.Context, method, path string, input any) ([]byte, error) {
	return c.Client.Request(ctx, method, path, input)
}

func (c *remoteConnection) watch(ctx context.Context, path string, after int64, out io.Writer) error {
	return c.Client.Watch(ctx, path, after, out)
}

func readRemoteEvents(input io.Reader, after int64, out io.Writer) (int64, error) {
	return remote.ReadEvents(input, after, out)
}
func remoteJSON(out io.Writer, data []byte) error {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, pretty.String())
	return err
}

func remoteScope(fs *flag.FlagSet) (*string, *string) {
	tenant := fs.String("tenant", "", "server-issued tenant ID")
	project := fs.String("project", "", "server-issued project ID")
	return tenant, project
}

func validateRemoteScope(tenant, project string) error {
	if !remoteID.MatchString(tenant) || !strings.HasPrefix(tenant, "t_") ||
		!remoteID.MatchString(project) || !strings.HasPrefix(project, "p_") {
		return errors.New("--tenant and --project must be server-issued IDs")
	}
	return nil
}

func runRemote(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: hrn remote submit|show|events|cancel [flags]")
	}
	action := args[0]
	fs := flag.NewFlagSet("remote "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant, project := remoteScope(fs)
	var ref, key string
	var cost float64
	var turns int
	var after int64
	var watch bool
	switch action {
	case "submit":
		fs.StringVar(&ref, "base-ref", "", "registered full Git commit")
		fs.StringVar(&key, "key", "", "idempotency key")
		fs.Float64Var(&cost, "max-cost", 0, "USD ceiling")
		fs.IntVar(&turns, "max-turns", 3, "maximum model turns")
	case "events":
		fs.Int64Var(&after, "after", 0, "last durable event ID")
		fs.BoolVar(&watch, "watch", false, "stream until the job ends, reconnecting using durable event IDs")
	case "show", "cancel":
	default:
		return fmt.Errorf("unknown remote action %q", action)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateRemoteScope(*tenant, *project); err != nil {
		return err
	}
	connection, err := newRemoteConnection()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	extras := fs.Args()
	if action == "submit" {
		if !remoteRef.MatchString(ref) || len(key) == 0 || len(key) > 128 || cost <= 0 || turns <= 0 {
			return errors.New("submit requires --base-ref (full commit), --key (1..128), positive --max-cost and --max-turns")
		}
		prompt := strings.Join(extras, " ")
		if len(extras) == 0 || (len(extras) == 1 && extras[0] == "-") {
			input, err := io.ReadAll(io.LimitReader(os.Stdin, 100_001))
			if err != nil {
				return err
			}
			prompt = strings.TrimSpace(string(input))
		}
		if len(prompt) == 0 || len(prompt) > 100_000 {
			return errors.New("prompt must be 1..100000 bytes")
		}
		options := agent.Options{Mode: agent.ModeReadOnly, MaxCostUSD: cost, MaxTurns: turns}
		if err := options.Validate(); err != nil {
			return err
		}
		input := struct {
			TenantID       string        `json:"tenant_id"`
			ProjectID      string        `json:"project_id"`
			Prompt         string        `json:"prompt"`
			Options        agent.Options `json:"options"`
			Backend        string        `json:"backend"`
			BaseRef        string        `json:"base_ref"`
			IdempotencyKey string        `json:"idempotency_key"`
		}{
			TenantID: *tenant, ProjectID: *project, Prompt: prompt,
			Options: options,
			Backend: "api", BaseRef: ref, IdempotencyKey: key,
		}
		data, err := connection.request(ctx, http.MethodPost,
			"/v1/projects/"+*project+"/jobs", input)
		if err != nil {
			return err
		}
		return remoteJSON(out, data)
	}
	if len(extras) != 1 || !remoteID.MatchString(extras[0]) || !strings.HasPrefix(extras[0], "j_") {
		return errors.New("show, events and cancel require one server-issued job ID")
	}
	job := extras[0]
	scope := url.Values{"tenant_id": {*tenant}, "project_id": {*project}}
	switch action {
	case "show":
		data, err := connection.request(ctx, http.MethodGet, "/v1/jobs/"+job+"?"+scope.Encode(), nil)
		if err != nil {
			return err
		}
		return remoteJSON(out, data)
	case "cancel":
		data, err := connection.request(ctx, http.MethodPost, "/v1/jobs/"+job+"/cancel?"+scope.Encode(), nil)
		if err != nil {
			return err
		}
		return remoteJSON(out, data)
	case "events":
		if after < 0 {
			return errors.New("--after must be nonnegative")
		}
		if watch {
			return connection.WatchJob(ctx, job, scope, after, out)
		}
		scope.Set("after", strconv.FormatInt(after, 10))
		data, err := connection.request(ctx, http.MethodGet, "/v1/jobs/"+job+"/events?"+scope.Encode(), nil)
		if err != nil {
			return err
		}
		return remoteJSON(out, data)
	}
	return errors.New("unreachable remote action")
}
