package watch

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// DefaultAddr is where hrn watch listens unless told otherwise: next to hrn
// sessions' 8088, so both can run.
const DefaultAddr = "127.0.0.1:8089"

// Options tunes a watch server. The zero value is the hrn watch default.
type Options struct {
	Poll time.Duration // how often records are checked for changes; default 500ms
	Runs int           // runs on the index page; default 50
	// LAN is where hrn watch --lan also serves the page: this computer's
	// private network addresses. Requests naming one of them are let in, and
	// the index shows them as links for a phone.
	LAN []Endpoint
	// Public serves only the named HTTPS origin behind Cloudflare Access.
	// The listener remains on loopback and every request needs a valid JWT.
	Public *PublicAccess
	// Key is what a request that would act on hrn (anything but GET and
	// HEAD) must send in X-Hrn-Watch-Key, from this computer or the network
	// alike. Viewing needs none. Empty refuses every action.
	Key string
}

// Endpoint is one private network address the page is served on.
type Endpoint struct {
	Addr      string // ip:port, as listened on
	Interface string // the network adapter's name, such as Wi-Fi
	URL       string
}

// ListenLAN listens on port at each private IPv4 address of this computer's
// network adapters that are up, leaving out point-to-point links such as most
// VPN tunnels. Never 0.0.0.0, which would take in a public address too. A VPN
// that shows up as an ordinary adapter with a private address is still taken:
// the caller lists every endpoint so the person running it sees each network.
// only, when set, keeps one adapter: its name (any case) or its address.
// The page is read-only: anything that acts on hrn needs a key (see guard).
func ListenLAN(port, only string) ([]net.Listener, []Endpoint, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}
	var adapters []adapter
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		adapters = append(adapters, adapter{ifc.Name, ifc.Flags, addrs})
	}
	picked, err := pickLAN(lanAddrs(adapters), only)
	if err != nil {
		return nil, nil, err
	}
	var lns []net.Listener
	var eps []Endpoint
	for _, a := range picked {
		ln, err := net.Listen("tcp", net.JoinHostPort(a.ip, port))
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return nil, nil, fmt.Errorf("--lan: %w", err)
		}
		lns = append(lns, ln)
		eps = append(eps, Endpoint{Addr: ln.Addr().String(), Interface: a.name, URL: "http://" + ln.Addr().String() + "/"})
	}
	if len(lns) == 0 {
		return nil, nil, errors.New("--lan: this computer has no private network address (192.168.x, 10.x or 172.16-31.x) on an adapter that is up")
	}
	return lns, eps, nil
}

// adapter is what ListenLAN needs to know of a network interface.
type adapter struct {
	name  string
	flags net.Flags
	addrs []net.Addr
}

type lanAddr struct{ ip, name string }

// lanAddrs picks the addresses --lan serves on: the private IPv4 addresses
// of adapters that are up and are neither loopback nor point-to-point.
func lanAddrs(adapters []adapter) []lanAddr {
	var out []lanAddr
	for _, a := range adapters {
		if a.flags&net.FlagUp == 0 || a.flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		for _, ip := range privateAddrs(a.addrs) {
			out = append(out, lanAddr{ip, a.name})
		}
	}
	return out
}

// pickLAN keeps the address or the adapter --lan=NAME names; with no name,
// all of them. A name that matches nothing is an error naming the choices.
func pickLAN(all []lanAddr, only string) ([]lanAddr, error) {
	if only == "" {
		return all, nil
	}
	var out []lanAddr
	for _, a := range all {
		if a.ip == only || strings.EqualFold(a.name, only) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		var choices []string
		for _, a := range all {
			choices = append(choices, fmt.Sprintf("%s (%s)", a.name, a.ip))
		}
		return nil, fmt.Errorf("--lan=%s: no network adapter with a private address by that name or address; there are: %s", only, strings.Join(choices, ", "))
	}
	return out, nil
}

// privateAddrs keeps the private IPv4 addresses (RFC 1918) of an adapter.
// IPv6 is left out: a phone's address bar takes an IPv4 address more easily.
func privateAddrs(addrs []net.Addr) []string {
	var out []string
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := n.IP.To4(); ip4 != nil && ip4.IsPrivate() {
			out = append(out, ip4.String())
		}
	}
	return out
}

// LoopbackAddr checks that addr is a loopback IP, or localhost, and a port,
// and returns it ready to listen on. Records hold prompts, file contents and
// command output, so the page is never offered beyond this computer.
func LoopbackAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("--addr %q must be a loopback address and a port, such as %s", addr, DefaultAddr)
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !ip.Unmap().IsLoopback() {
		return "", fmt.Errorf("--addr %q: hrn watch listens on a loopback address (127.0.0.1, ::1 or localhost), since runs hold file contents and command output; --lan also serves it on this computer's private network addresses", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("--addr %q: the port must be a number from 0 to 65535", addr)
	}
	return net.JoinHostPort(ip.Unmap().String(), port), nil
}

// Serve answers on every listener until ctx ends. It refuses a listener that
// is neither on a loopback address nor one of opt.LAN's private addresses;
// public mode permits loopback only, whoever opened the listener.
func Serve(ctx context.Context, opt Options, lns ...net.Listener) error {
	lan := opt.LAN
	if opt.Public != nil {
		lan = nil
	}
	for _, ln := range lns {
		if err := checkListener(ln.Addr(), lan); err != nil {
			return err
		}
	}
	var access *accessVerifier
	if opt.Public != nil {
		access = &accessVerifier{opt: *opt.Public}
		keys, err := access.fetch()
		if err != nil {
			return fmt.Errorf("Cloudflare Access certificates at %s: %w", access.certsURL(), err)
		}
		access.keys = keys
	}
	ctx, stopStreams := context.WithCancel(ctx)
	defer stopStreams()
	srv := &http.Server{
		Handler:           newHandler(ctx, opt, access),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// No WriteTimeout: streams stay open. Each write sets its own deadline.
		// "OPTIONS *" goes to the handler, and so through the door, too.
		DisableGeneralOptionsHandler: true,
	}
	errs := make(chan error, len(lns))
	for _, ln := range lns {
		go func() { errs <- srv.Serve(ln) }()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errs: // a listener failed: stop the others too
	}
	stopStreams()
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func checkListener(a net.Addr, lan []Endpoint) error {
	tcp, ok := a.(*net.TCPAddr)
	if ok && tcp.IP.IsLoopback() {
		return nil
	}
	for _, ep := range lan {
		if ok && tcp.IP.IsPrivate() && ep.Addr == tcp.String() {
			return nil
		}
	}
	return fmt.Errorf("hrn watch serves on a loopback address, and with --lan on this computer's private network addresses; not on %s", a)
}

// idRE is the shape of every run and batch id hrn makes (runlog.NewID). An id
// from a request must have it before it is used as a file name: on Windows 10
// a name such as CON opens a device.
var idRE = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

type server struct {
	ctx    context.Context // ends every stream when the server stops
	opt    Options
	redact *strings.Replacer // blanks secret values; see secretReplacer
	home   string            // the user's home folder, shown as ~
	system []string          // folders whose programs are shown by name; see systemFolders
	access *accessVerifier
	logMu  sync.Mutex
}

// New is the watch page's handler. Streams end when ctx does, so a server
// shutting down does not wait for its browsers to leave.
func New(ctx context.Context, opt Options) http.Handler {
	return newHandler(ctx, opt, nil)
}

func newHandler(ctx context.Context, opt Options, access *accessVerifier) http.Handler {
	if opt.Poll <= 0 {
		opt.Poll = 500 * time.Millisecond
	}
	if opt.Runs <= 0 {
		opt.Runs = 50
	}
	s := &server{ctx: ctx, opt: opt, redact: secretReplacer(os.Environ()), system: systemFolders()}
	s.home, _ = os.UserHomeDir()
	if opt.Public != nil {
		if access == nil {
			access = &accessVerifier{opt: *opt.Public}
		}
		s.access = access
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /backends", s.saveBackends)
	mux.HandleFunc("GET /run/{id}", s.run)
	mux.HandleFunc("GET /batch/{id}", s.batch)
	mux.HandleFunc("GET /stream/index", s.streamIndex)
	mux.HandleFunc("GET /stream/run/{id}", s.streamRun)
	mux.HandleFunc("GET /stream/batch/{id}", s.streamBatch)
	mux.HandleFunc("GET /bot", s.bot)
	mux.HandleFunc("GET /stream/bot", s.streamBot)
	mux.HandleFunc("GET /voice", s.voice)
	mux.HandleFunc("GET /static/{name}", s.static)
	lan := map[string]bool{}
	for _, ep := range opt.LAN {
		lan[ep.Addr] = true
	}
	h := s.guard(mux, lan, opt.Key)
	if opt.Public != nil {
		return s.publicLog(h)
	}
	return h
}

const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// guard is hrn sessions' guard: local requests only, and headers that keep
// the page to itself. lan holds the ip:port addresses hrn watch --lan serves
// on; a Host naming one exactly is let in too. A rebinding attack needs a DNS
// name, so IP literals keep that defence.
//
// Anything but GET and HEAD would act on hrn, so it must also carry key in
// X-Hrn-Watch-Key. A custom header is also the CSRF check: a form on another
// site cannot set one, and a script elsewhere cannot send one here without a
// CORS answer this server never gives.
func (s *server) guard(next http.Handler, lan map[string]bool, key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp)
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if s.opt.Public != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
			aw, ok := w.(*accessWriter)
			if !ok {
				http.Error(w, "Public watch needs its access log", http.StatusInternalServerError)
				return
			}
			publicHost := strings.TrimPrefix(s.opt.Public.Origin, "https://")
			if !publicHostMatches(r.Host, publicHost) {
				aw.reason = "bad host"
			}
			// A top-level page load from elsewhere may skip the Origin check only
			// when it carries no Origin or `null`; any other Origin is checked.
			origin := r.Header.Get("Origin")
			navigation := (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.EqualFold(r.Header.Get("Sec-Fetch-Mode"), "navigate") && strings.EqualFold(r.Header.Get("Sec-Fetch-Dest"), "document") && (origin == "" || origin == "null")
			if aw.reason == "" && !navigation && (strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") || origin != "" && origin != s.opt.Public.Origin) {
				aw.reason = "bad origin"
			}
			if aw.reason == "" {
				token := r.Header.Get("Cf-Access-Jwt-Assertion")
				if token == "" {
					if c, err := r.Cookie("CF_Authorization"); err == nil {
						token = c.Value
					}
				}
				aw.email, aw.reason = s.access.verify(token)
			}
			if aw.reason != "" {
				http.Error(w, "Cloudflare Access login required", http.StatusForbidden)
				return
			}
		} else if !(isLoopbackHost(r.Host) || lan[withPort(r.Host)]) || strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") || !isSameOrigin(r) {
			http.Error(w, "Local requests only", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			sent := r.Header.Get("X-Hrn-Watch-Key")
			if key == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(key)) != 1 {
				if aw, ok := w.(*accessWriter); ok {
					aw.email, aw.reason = "", "missing action key"
				}
				http.Error(w, "Acting on hrn needs hrn watch's key", http.StatusForbidden)
				return
			}
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		next.ServeHTTP(w, r)
	})
}

func publicHostMatches(authority, host string) bool {
	return strings.EqualFold(authority, host) || strings.EqualFold(authority, host+":443")
}

// isLoopbackHost checks the Host header, so a DNS name an attacker rebinds to
// 127.0.0.1 cannot have a browser read the page for them.
func isLoopbackHost(authority string) bool {
	if authority == "" {
		return false
	}
	host := authority
	if h, port, err := net.SplitHostPort(authority); err == nil {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return false
		}
		host = h
	} else if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		host = authority[1 : len(authority)-1]
	} else if strings.Contains(authority, ":") {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Zone() == "" && ip.Unmap().IsLoopback()
}

// withPort is a Host header with the port a browser leaves out of it.
func withPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), "80")
}

// isSameOrigin: Origin is absent on navigation and most same-origin GETs;
// when present it must be this server.
func isSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Scheme == "http" && strings.EqualFold(u.Host, r.Host)
}

// render runs a template and blanks any secret value in what it made: a key
// or token in hrn watch's environment may have reached a run (an agent that
// ran env), and the page may be open to the network.
func (s *server) render(name string, data any) (string, error) {
	out, err := render(name, data)
	if err == nil && s.redact != nil {
		out = s.redact.Replace(out)
	}
	return out, err
}

// homeFor is the hrn home to show: to this computer only, as it names the
// user's account.
func (s *server) homeFor(r *http.Request) string {
	if s.fromThisComputer(r) {
		return runlog.Home()
	}
	return ""
}

func (s *server) page(w http.ResponseWriter, status int, name string, data any) {
	body, err := s.render(name, data)
	if err != nil {
		msg := "the page could not be rendered"
		if s.opt.Public == nil {
			msg += ": " + err.Error()
		}
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (s *server) missing(w http.ResponseWriter, r *http.Request, what string, err error) {
	msg := what + " not found"
	if !errors.Is(err, os.ErrNotExist) {
		msg = what + " could not be read"
		if s.fromThisComputer(r) { // the error names paths
			msg = fmt.Sprintf("%s could not be read: %v", what, err)
		}
	}
	s.page(w, http.StatusNotFound, "missing", struct {
		page
		Message string
	}{page{Title: "Not found", Home: s.homeFor(r)}, msg})
}

func (s *server) lists() lists {
	var l lists
	runs, err := runlog.List(s.opt.Runs)
	if err != nil {
		msg := "Runs could not be listed."
		if s.opt.Public == nil {
			msg = "Runs could not be listed: " + err.Error()
		}
		l.Problems = append(l.Problems, msg)
	}
	// The rows are the same for every viewer: shortened, never with a title.
	// A run's purpose comes from its prompt as recorded, as hrn report has
	// it; only the prompt shown is shortened.
	vw := viewer{home: s.home, system: s.system}
	for i, rec := range runs {
		runs[i] = s.cleanRecord(rec)
		r := runRow(runs[i])
		r.Prompt, _ = firstLine(vw.shortener(rec.Task.Dir).shorten(runs[i].Task.Prompt))
		l.Runs = append(l.Runs, r)
	}
	l.Purposes = purposesOf(runs)
	batches, unreadable, err := batch.List()
	if unreadable > 0 {
		// Most likely one caught mid-save: on Windows a read can land on
		// the rename. Look again before calling it unreadable.
		time.Sleep(50 * time.Millisecond)
		batches, unreadable, err = batch.List()
	}
	if err != nil {
		msg := "Batches could not be listed."
		if s.opt.Public == nil {
			msg = "Batches could not be listed: " + err.Error()
		}
		l.Problems = append(l.Problems, msg)
	}
	if unreadable > 0 {
		l.Problems = append(l.Problems, fmt.Sprintf("%d batch record(s) could not be read.", unreadable))
	}
	for i, res := range batches {
		batches[i] = vw.batch(s.cleanBatch(res)) // the task names below come from these too
		if i < 20 {
			l.Batches = append(l.Batches, batchRow(batches[i]))
		}
	}
	// Every batch attempt is a run too: link each one back to its batch and
	// lead with its task, from every batch on disk, not only the 20 listed.
	of, task := batchOf(batches), taskNameOf(batches)
	for i := range l.Runs {
		r := &l.Runs[i]
		r.Batch = of[r.ID]
		r.Label = labelOf(r.ID, r.Repo, task[r.ID])
	}
	return l
}

// taskNameOf maps each run that was a batch task's attempt to the task's name.
func taskNameOf(batches []batch.Result) map[string]string {
	m := map[string]string{}
	for _, res := range batches {
		for _, t := range res.Tasks {
			for _, a := range t.Attempts {
				if a.RunID != "" {
					m[a.RunID] = taskName(t)
				}
			}
		}
	}
	return m
}

// batchOf maps each run that was a batch task's attempt to its batch.
func batchOf(batches []batch.Result) map[string]string {
	m := map[string]string{}
	for _, res := range batches {
		for _, t := range res.Tasks {
			for _, a := range t.Attempts {
				if a.RunID != "" {
					m[a.RunID] = res.ID
				}
			}
		}
	}
	return m
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	p := indexPage{page: page{Title: "Runs and batches", Home: s.homeFor(r)}, Lists: s.lists()}
	p.Backends = s.backendsCard(w, r)
	// The phone links and the hints about --lan are for the person at this
	// computer; a phone that came in over the network needs neither.
	if s.fromThisComputer(r) {
		p.Local = true
		p.Phone = phoneLinks(s.opt.LAN)
	}
	s.page(w, http.StatusOK, "index", p)
}

func (s *server) viewerOf(r *http.Request) viewer {
	return viewer{local: s.fromThisComputer(r), home: s.home, system: s.system}
}

// fromThisComputer: the connection came from a loopback address. A request
// to this computer's network address comes from that address, not loopback.
func (s *server) fromThisComputer(r *http.Request) bool {
	if s.opt.Public != nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// run renders the run as it stands; a run still going streams the rest from
// where this left off.
func (s *server) run(w http.ResponseWriter, r *http.Request) {
	if !idRE.MatchString(r.PathValue("id")) {
		s.missing(w, r, "Run "+r.PathValue("id"), os.ErrNotExist)
		return
	}
	f, _, err := NewFollower(r.PathValue("id"), 0)
	if err != nil {
		s.missing(w, r, "Run "+r.PathValue("id"), err)
		return
	}
	lines, rec, done, err := f.Poll()
	if err != nil {
		s.missing(w, r, "Run "+rec.ID, err)
		return
	}
	vw := s.viewerOf(r)
	sh := vw.shortener(rec.Task.Dir)
	prev := rec.Started
	events := make([]eventView, 0, len(lines))
	var usage []agent.Event
	for _, l := range lines {
		if backendStart(l.Event, rec.Result.Backend) {
			continue
		}
		var v eventView
		l.Event = sh.event(s.cleanEvent(l.Event))
		if feedsChart(l) {
			usage = append(usage, l.Event)
		}
		if isUsage(l) {
			continue
		}
		v, prev = eventOf(l, prev)
		events = append(events, v)
	}
	p := runPageOf(sh.record(s.cleanRecord(rec)), events, prev, vw)
	p.Usage = usageOf(usage)
	p.Home = s.homeFor(r)
	if !done && p.Stopped == "" {
		p.Stream = fmt.Sprintf("/stream/run/%s?after=%s", url.PathEscape(f.ID()), cursor{f.Offset(), prev})
	}
	s.page(w, http.StatusOK, "run", p)
}

func (s *server) batch(w http.ResponseWriter, r *http.Request) {
	if !idRE.MatchString(r.PathValue("id")) {
		s.missing(w, r, "Batch "+r.PathValue("id"), os.ErrNotExist)
		return
	}
	res, err := batch.LoadResult(r.PathValue("id"))
	if err != nil {
		s.missing(w, r, "Batch "+r.PathValue("id"), err)
		return
	}
	vw := s.viewerOf(r)
	p := batchPageOf(vw.batch(s.cleanBatch(res)), vw)
	p.Home = s.homeFor(r)
	s.page(w, http.StatusOK, "batch", p)
}

func (s *server) static(w http.ResponseWriter, r *http.Request) {
	types := map[string]string{"watch.css": "text/css; charset=utf-8", "watch.js": "text/javascript; charset=utf-8", "bot.js": "text/javascript; charset=utf-8",
		"voice.js": "text/javascript; charset=utf-8", "voice-worklet.js": "text/javascript; charset=utf-8"}
	name := r.PathValue("name")
	data, err := assets.ReadFile("assets/" + name)
	if types[name] == "" || err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", types[name])
	_, _ = w.Write(data)
}

// cursor is where a run's stream resumes: the byte offset after the last
// event sent, and that event's time, so the gap before the next one is right
// after a reconnect too. It is each message's SSE id.
type cursor struct {
	off  int64
	prev time.Time
}

func (c cursor) String() string {
	var ns int64
	if !c.prev.IsZero() {
		ns = c.prev.UnixNano()
	}
	return fmt.Sprintf("%d.%d", c.off, ns)
}

func parseCursor(s string) (cursor, error) {
	o, t, ok := strings.Cut(s, ".")
	off, err1 := strconv.ParseInt(o, 10, 64)
	ns, err2 := strconv.ParseInt(t, 10, 64)
	if !ok || err1 != nil || err2 != nil || off < 0 {
		return cursor{}, fmt.Errorf("bad stream position %q", s)
	}
	c := cursor{off: off}
	if ns != 0 {
		c.prev = time.Unix(0, ns)
	}
	return c, nil
}

// stream is one Server-Sent Events response.
type stream struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	last time.Time
}

func (s *server) startStream(w http.ResponseWriter) *stream {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	st := &stream{w: w, rc: http.NewResponseController(w)}
	_ = st.write("retry: 2000\n\n")
	return st
}

// send writes one message. SSE ends a line at \r, \n or \r\n, so data is
// split on all three: event text can never end a data line early and forge an
// event, an id or a retry of its own.
func (st *stream) send(event, id, data string) error {
	var b strings.Builder
	b.WriteString("event: " + event + "\n")
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	data = strings.ReplaceAll(strings.ReplaceAll(data, "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	return st.write(b.String())
}

func (st *stream) write(s string) error {
	_ = st.rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(st.w, s); err != nil {
		return err
	}
	st.last = time.Now()
	return st.rc.Flush()
}

const keepAlive = 15 * time.Second

// loop calls step every interval until step says it is done or fails, the
// browser leaves or the server stops. It keeps a quiet connection alive.
func (s *server) loop(r *http.Request, st *stream, every time.Duration, step func() (bool, error)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if done, err := step(); done || err != nil {
			return
		}
		if time.Since(st.last) >= keepAlive && st.write(": keep-alive\n\n") != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// streamChanges sends the part a template renders whenever it changes, and
// ends once next supplies an ending ("done" or "stopped").
func (s *server) streamChanges(r *http.Request, st *stream, every time.Duration, next func() (data any, end string, err error), name string) {
	last := ""
	s.loop(r, st, every, func() (bool, error) {
		data, end, err := next()
		if err != nil {
			return true, s.problem(r, st, err)
		}
		html, err := s.render(name, data)
		if err != nil {
			return true, s.problem(r, st, err)
		}
		if html != last {
			last = html
			if err := st.send("replace", "", html); err != nil {
				return true, err
			}
		}
		if end != "" {
			return true, st.send("end", "", end)
		}
		return false, nil
	})
}

func (s *server) problem(r *http.Request, st *stream, err error) error {
	msg := "The stream stopped: the record could not be read."
	if s.fromThisComputer(r) { // the error names paths
		msg = err.Error()
	}
	html, _ := s.render("problem", msg)
	_ = st.send("problem", "", html)
	return err
}

func (s *server) streamIndex(w http.ResponseWriter, r *http.Request) {
	st := s.startStream(w)
	s.streamChanges(r, st, 4*s.opt.Poll, func() (any, string, error) { return s.lists(), "", nil }, "lists")
}

func (s *server) streamBatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		http.Error(w, "batch not found", http.StatusNotFound)
		return
	}
	last, err := batch.LoadResult(id)
	if err != nil {
		http.Error(w, "batch not found", http.StatusNotFound)
		return
	}
	vw := s.viewerOf(r)
	st := s.startStream(w)
	s.streamChanges(r, st, 2*s.opt.Poll, func() (any, string, error) {
		res, err := batch.LoadResult(id)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil, "", fmt.Errorf("batch %s was removed", id)
		case err != nil:
			// Caught mid-save (a rename on Windows): show the last good copy.
			res = last
		}
		last = res
		body := batchBodyOf(vw.batch(s.cleanBatch(res)), vw)
		end := ""
		if body.Stopped != "" {
			end = "stopped"
		} else if !batchUnfinished(res) {
			end = "done"
		}
		return body, end, nil
	}, "batchbody")
}

func (s *server) streamRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	var cur cursor
	var err error
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		cur, err = parseCursor(last) // a browser reconnecting resumes here
	} else if after := r.URL.Query().Get("after"); after != "" {
		cur, err = parseCursor(after)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, rec, err := NewFollower(id, cur.off)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	prev := cur.prev
	if prev.IsZero() {
		prev = rec.Started
	}
	sh := s.viewerOf(r).shortener(rec.Task.Dir)
	// The chart is drawn over every turn, so a stream that starts partway
	// reads the turns before it first.
	before := Tail{Path: filepath.Join(runlog.Root(), id, "events.jsonl")}
	earlier, err := before.Read()
	if err != nil {
		http.Error(w, "run not readable", http.StatusInternalServerError)
		return
	}
	var usage []agent.Event
	for _, l := range earlier {
		if l.End <= cur.off && feedsChart(l) {
			usage = append(usage, sh.event(s.cleanEvent(l.Event)))
		}
	}
	st := s.startStream(w)
	s.loop(r, st, s.opt.Poll, func() (bool, error) {
		lines, rec, done, err := f.Poll()
		for _, l := range lines {
			if backendStart(l.Event, rec.Result.Backend) {
				continue
			}
			var v eventView
			l.Event = sh.event(s.cleanEvent(l.Event))
			if feedsChart(l) {
				usage = append(usage, l.Event)
				html, rerr := s.render("usage", usageOf(usage))
				if rerr != nil {
					return true, s.problem(r, st, rerr)
				}
				if werr := st.send("usage", cursor{l.End, prev}.String(), html); werr != nil {
					return true, werr
				}
			}
			if isUsage(l) {
				continue
			}
			v, prev = eventOf(l, prev)
			html, rerr := s.render("event", v)
			if rerr != nil {
				return true, s.problem(r, st, rerr)
			}
			if werr := st.send("item", cursor{l.End, prev}.String(), html); werr != nil {
				return true, werr
			}
		}
		if err != nil {
			return true, s.problem(r, st, err)
		}
		if !done {
			if at := runStopped(rec); !at.IsZero() {
				html, err := s.render("stopped", noWord(at))
				if err != nil {
					return true, s.problem(r, st, err)
				}
				if err := st.send("result", "", html); err != nil {
					return true, err
				}
				return true, st.send("end", "", "stopped")
			}
			return false, nil
		}
		html, err := s.render("result", resultOf(sh.record(s.cleanRecord(rec)).Result))
		if err != nil {
			return true, s.problem(r, st, err)
		}
		if err := st.send("result", "", html); err != nil {
			return true, err
		}
		return true, st.send("end", "", "done")
	})
}
