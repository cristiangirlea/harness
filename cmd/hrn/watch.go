package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/qr"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/watch"
)

// cmdWatch serves the live page on a loopback address, and with --lan on this
// computer's private network addresses too, until Ctrl+C.
func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	addr := fs.String("addr", watch.DefaultAddr, "")
	var lan lanFlag
	fs.Var(&lan, "lan", "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid arguments")
	}
	if fs.NArg() != 0 {
		if lan.on && lan.only == "" {
			return fmt.Errorf("--lan takes its adapter after an equals sign: --lan=%q", strings.Join(fs.Args(), " "))
		}
		return fmt.Errorf("unexpected argument %q; the address is set with --addr", fs.Arg(0))
	}
	bind, err := watch.LoopbackAddr(*addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return err
	}
	lns, opt := []net.Listener{ln}, watch.Options{}
	if lan.on {
		// The same port on the network as on this computer, when it was named.
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		more, eps, err := watch.ListenLAN(port, lan.only)
		if err != nil {
			ln.Close()
			return err
		}
		lns, opt.LAN = append(lns, more...), eps
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	printWatchStart(os.Stdout, ln.Addr().String(), lan.on, opt.LAN, isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "")
	return watch.Serve(ctx, opt, lns...)
}

// printWatchStart says where the page is. With one network address it also
// draws that link's QR code, for a phone to scan; with several, the codes
// are on the page itself. color forces the code white on black: on a
// terminal with a light theme it would otherwise come out inverted.
func printWatchStart(w io.Writer, local string, lan bool, eps []watch.Endpoint, color bool) {
	if !lan {
		fmt.Fprintf(w, "hrn watch: http://%s (this computer only; --lan for a phone; Ctrl+C stops it)\n", local)
		return
	}
	fmt.Fprintf(w, "hrn watch: http://%s (Ctrl+C stops it)\n", local)
	for _, ep := range eps {
		fmt.Fprintf(w, "  on your network: %s (%s)\n", ep.URL, ep.Interface)
	}
	fmt.Fprintln(w, "  viewing needs no login: anyone on each of these networks can read your runs while this runs;")
	fmt.Fprintln(w, "  a VPN listed here shares them with its network too")
	if len(eps) != 1 {
		fmt.Fprintf(w, "  QR codes for each are on http://%s/ (or name one network with --lan=NAME)\n", local)
		return
	}
	c, err := qr.Encode(eps[0].URL)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "\nscan with your phone (%s):\n", eps[0].Interface)
	for _, line := range strings.Split(strings.TrimSuffix(c.Terminal(), "\n"), "\n") {
		if color {
			line = "\x1b[97;40m" + line + "\x1b[0m"
		}
		fmt.Fprintln(w, line)
	}
}

// lanFlag is --lan (every private network address) or --lan=NAME (one
// adapter, by name or address); --lan=false turns it off again.
type lanFlag struct {
	on   bool
	only string
}

func (f *lanFlag) IsBoolFlag() bool { return true }

func (f *lanFlag) String() string {
	if f == nil || !f.on {
		return "false"
	}
	if f.only == "" {
		return "true"
	}
	return f.only
}

func (f *lanFlag) Set(s string) error {
	if strings.TrimSpace(s) == "" {
		// An unset variable in --lan="$ADAPTER" must not widen to every adapter.
		return errors.New("--lan= needs an adapter's name or address; plain --lan serves them all")
	}
	switch strings.ToLower(s) {
	case "true":
		f.on, f.only = true, ""
	case "false":
		f.on, f.only = false, ""
	default:
		f.on, f.only = true, s
	}
	return nil
}

// followRun prints a run as it is recorded, until its result is saved: the
// header, each step with a line for any wait of a second or more before it,
// then the summary. Interrupting it leaves the run alone.
func followRun(ctx context.Context, r *renderer, id string, every time.Duration) error {
	rec, err := runlog.Load(id)
	if err != nil {
		return err
	}
	showHeader(r, rec)
	prev := rec.Started
	final, err := watch.Follow(ctx, id, 0, every, func(l watch.Line) {
		if l.Err != nil {
			r.Line(r.c(red, l.Err.Error()))
			return
		}
		if t := l.Event.Time; !t.IsZero() {
			if d := t.Sub(prev); !prev.IsZero() && d >= time.Second {
				r.Line(r.c(dim, "  ⋯ "+watch.Gap(d)))
			}
			prev = t
		}
		r.Event(l.Event)
	})
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		r.Line(r.c(dim, fmt.Sprintf("stopped following; run %s is still going (hrn show -f %s picks it up again)", id, id)))
		return nil
	}
	if err != nil {
		return err
	}
	r.Summary(final.ID, final.Result)
	return nil
}
