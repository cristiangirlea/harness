package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/watch"
)

// cmdWatch serves the live page on a loopback address, and with --lan on this
// computer's private network addresses too, until Ctrl+C.
func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	addr := fs.String("addr", watch.DefaultAddr, "")
	lan := fs.Bool("lan", false, "")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid arguments")
	}
	if fs.NArg() != 0 {
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
	if *lan {
		// The same port on the network as on this computer, when it was named.
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		more, eps, err := watch.ListenLAN(port)
		if err != nil {
			ln.Close()
			return err
		}
		lns, opt.LAN = append(lns, more...), eps
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *lan {
		fmt.Printf("hrn watch: http://%s (Ctrl+C stops it)\n", ln.Addr())
		for _, ep := range opt.LAN {
			fmt.Printf("  on your network: %s (%s)\n", ep.URL, ep.Interface)
		}
		fmt.Println("  no login yet: anyone on each of these networks can read your runs while this runs;")
		fmt.Println("  a VPN listed here shares them with its network too")
	} else {
		fmt.Printf("hrn watch: http://%s (this computer only; --lan for a phone; Ctrl+C stops it)\n", ln.Addr())
	}
	return watch.Serve(ctx, opt, lns...)
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
