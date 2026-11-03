package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/workmap"
)

var mapPoll = 30 * time.Second
var buildWorkMap = workmap.Build

var mapStages = []workmap.Stage{workmap.Idea, workmap.Branch, workmap.Agent, workmap.Gates, workmap.Review, workmap.PR, workmap.CI, workmap.Merged, workmap.Released}

type mapItem struct {
	Title string `json:"title"`
	Stuck string `json:"stuck"`
	URL   string `json:"url,omitempty"`
}
type mapView struct {
	SVG   string    `json:"svg"`
	Stuck []mapItem `json:"stuck"`
	Error string    `json:"error,omitempty"`
}
type mapPage struct {
	page
	View mapView
}

func mapLink(p workmap.Piece) string {
	for _, u := range []string{p.Links.PR, p.Links.Issue} {
		if strings.HasPrefix(u, "https://github.com/") {
			return u
		}
	}
	if n := len(p.Links.Runs); n > 0 && idRE.MatchString(p.Links.Runs[n-1]) {
		return "/flow/" + p.Links.Runs[n-1]
	}
	return ""
}
func mapRepoBase(root string) string {
	return filepath.Base(strings.ReplaceAll(strings.TrimRight(root, `/\`), `\`, "/"))
}

func mapViewOf(pieces []workmap.Piece, err error) mapView {
	v := mapView{Stuck: []mapItem{}}
	if err != nil {
		v.Error = "Some work map sources could not be read."
	}
	lanes := map[string][]workmap.Piece{}
	for _, p := range pieces {
		lanes[p.Repo] = append(lanes[p.Repo], p)
		if p.Stuck != "" {
			title, _ := firstLine(p.Title)
			stuck, _ := firstLine(p.Stuck)
			v.Stuck = append(v.Stuck, mapItem{title, stuck, mapLink(p)})
		}
	}
	repos := make([]string, 0, len(lanes))
	for r := range lanes {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	names := make(map[string]int)
	for _, r := range repos {
		names[mapRepoBase(r)]++
	}
	heights := make([]int, len(repos))
	total := 50
	for i, r := range repos {
		counts := map[workmap.Stage]int{}
		max := 1
		for _, p := range lanes[r] {
			counts[p.Stage]++
			if counts[p.Stage] > max {
				max = counts[p.Stage]
			}
		}
		heights[i] = 45 + max*65
		total += heights[i]
	}
	var b strings.Builder
	width := len(mapStages) * 200
	fmt.Fprintf(&b, `<svg class="work-map" width="%d" viewBox="0 0 %d %d" role="img" aria-label="Work by repository and stage" xmlns="http://www.w3.org/2000/svg">`, (width*7+9)/10, width, total)
	for i, stage := range mapStages {
		fmt.Fprintf(&b, `<text class="map-heading" x="%d" y="25">%s</text>`, i*200+10, html.EscapeString(string(stage)))
	}
	y := 40
	for i, r := range repos {
		name := mapRepoBase(r)
		if names[name] > 1 {
			parent := filepath.Dir(strings.ReplaceAll(strings.TrimRight(r, `/\`), `\`, "/"))
			name = mapRepoBase(parent) + "/" + name
		}
		fmt.Fprintf(&b, `<text class="map-repo" x="10" y="%d">%s</text>`, y+17, html.EscapeString(name))
		counts := map[workmap.Stage]int{}
		for _, p := range lanes[r] {
			col := -1
			for j, s := range mapStages {
				if p.Stage == s {
					col = j
					break
				}
			}
			if col < 0 {
				continue
			}
			x := col*200 + 5
			py := y + 27 + counts[p.Stage]*65
			counts[p.Stage]++
			title := p.Title
			if title == "" {
				title = p.Links.Branch
			}
			short := []rune(title)
			if len(short) > 19 {
				short = append(short[:18], '…')
			}
			link := mapLink(p)
			if link != "" {
				fmt.Fprintf(&b, `<a href="%s">`, html.EscapeString(link))
			}
			class := "map-piece"
			if p.Stuck != "" {
				class += " map-stuck"
			}
			if p.ActiveSession {
				class += " map-session"
				title += " - Claude Code session"
			}
			fmt.Fprintf(&b, `<g class="%s"><title>%s</title><rect x="%d" y="%d" width="190" height="55" rx="4"/><text x="%d" y="%d">%s</text>`, class, html.EscapeString(title), x, py, x+6, py+20, html.EscapeString(string(short)))
			if p.Stuck != "" {
				stuck := []rune(p.Stuck)
				if len(stuck) > 19 {
					stuck = append(stuck[:18], '…')
				}
				fmt.Fprintf(&b, `<text x="%d" y="%d">%s</text>`, x+6, py+42, html.EscapeString(string(stuck)))
			}
			b.WriteString(`</g>`)
			if link != "" {
				b.WriteString(`</a>`)
			}
		}
		y += heights[i]
	}
	b.WriteString(`</svg>`)
	v.SVG = b.String()
	return v
}
func (s *server) mapOwner(r *http.Request) bool {
	if s.opt.Public == nil {
		return s.fromThisComputer(r)
	}
	return s.who(r).ID == owner.Local()
}
func (s *server) mapData(ctx context.Context, vw viewer) mapView {
	p, err := s.mapPieces(ctx, vw)
	return mapViewOf(p, err)
}
func (s *server) mapPieces(ctx context.Context, vw viewer) ([]workmap.Piece, error) {
	p, err := buildWorkMap(ctx, runlog.Home(), nil)
	for i := range p {
		if p[i].Title == "" {
			p[i].Title = p[i].Links.Branch
		}
		p[i].Title, p[i].Stuck = s.clean(p[i].Title), s.clean(p[i].Stuck)
		if !vw.local {
			sh := vw.shortener(p[i].Repo)
			p[i].Title, p[i].Stuck = sh.shorten(p[i].Title), sh.shorten(p[i].Stuck)
		}
		p[i].Title, _ = firstLine(p[i].Title)
		p[i].Stuck, _ = firstLine(p[i].Stuck)
	}
	return p, err
}
func (s *server) workMap(w http.ResponseWriter, r *http.Request) {
	if !s.mapOwner(r) {
		http.NotFound(w, r)
		return
	}
	s.page(w, 200, "workmap", mapPage{page{Title: "Work map", Home: s.homeFor(r)}, s.mapData(r.Context(), s.viewerOf(r))})
}
func (s *server) streamMap(w http.ResponseWriter, r *http.Request) {
	if !s.mapOwner(r) {
		http.NotFound(w, r)
		return
	}
	st := s.startStream(w)
	s.streamChanges(r, st, mapPoll, func() (any, string, error) {
		v := s.mapData(r.Context(), s.viewerOf(r))
		b, e := json.Marshal(v)
		return template.HTML(b), "", e
	}, "mapjson")
}
