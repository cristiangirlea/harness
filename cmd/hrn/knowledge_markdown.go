package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/retrieval"
)

func runKnowledgeMarkdown(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("knowledge markdown", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	request := fs.String("request", "", "base import request")
	min := fs.Int("min-words", 100, "minimum words before merging sections")
	max := fs.Int("max-words", 800, "maximum words per entry")
	overlap := fs.Int("overlap-words", 40, "overlap between split parts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *request == "" {
		return fmt.Errorf("usage: hrn knowledge markdown -request base.json [-min-words 100] [-max-words 800] [-overlap-words 40] FILE...")
	}
	var in retrieval.Import
	if err := readRetrievalJSON(*request, &in); err != nil {
		return err
	}
	if len(in.Entries) != 0 || strings.TrimSpace(in.Revision) == "" {
		return fmt.Errorf("%s: base request must have no entries and a non-empty revision", *request)
	}
	entries, err := markdownEntries(fs.Args(), in.Revision, *min, *max, *overlap)
	if err != nil {
		return err
	}
	in.Entries = entries
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(in)
}

type markdownSection struct {
	heading, content string
}

func markdownRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return filepath.EvalSymlinks(dir)
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("working directory has no repository root (.git)")
		}
		dir = parent
	}
}

func markdownEntries(files []string, revision string, min, max, overlap int) ([]retrieval.Entry, error) {
	if min < 1 || max < min || overlap < 0 || overlap >= max {
		return nil, fmt.Errorf("invalid markdown word limits")
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("markdown requires at least one file")
	}
	root, err := markdownRoot()
	if err != nil {
		return nil, err
	}
	entries := []retrieval.Entry{}
	used := map[string]bool{}
	evidence := fmt.Sprintf("markdown section; min-words=%d max-words=%d overlap-words=%d", min, max, overlap)
	for _, name := range files {
		abs, err := filepath.Abs(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		abs, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		compareRoot, compareAbs := root, abs
		if runtime.GOOS == "windows" {
			compareRoot, compareAbs = strings.ToLower(root), strings.ToLower(abs)
		}
		inside, err := filepath.Rel(compareRoot, compareAbs)
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
			return nil, fmt.Errorf("%s: file is outside repository root", name)
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		rel = filepath.ToSlash(rel)
		source := rel + "@" + revision
		if len(source) > 512 {
			return nil, fmt.Errorf("%s: source exceeds 512 bytes", name)
		}
		if markdownFrame(source) || markdownFrame(evidence) {
			return nil, fmt.Errorf("%s: source or evidence contains a frame marker", name)
		}
		text, err := readMarkdown(name, abs)
		if err != nil {
			return nil, err
		}
		sections := markdownSections(text)
		for i := 0; i < len(sections); i++ {
			section := sections[i]
			for len(strings.Fields(section.content)) < min && i+1 < len(sections) {
				combined := section.content + "\n\n" + sections[i+1].content
				if len(strings.Fields(combined)) > max {
					break
				}
				section.content = combined
				i++
			}
			key := rel
			if section.heading != "" {
				key += " > " + section.heading
			}
			if markdownFrame(key) {
				return nil, fmt.Errorf("%s: key contains a frame marker", name)
			}
			for part, content := range markdownChunks(section.content, max, overlap) {
				fullKey := key
				if part > 0 {
					fullKey += fmt.Sprintf(" (part %d)", part+1)
				}
				entryKey := markdownKey(fullKey, "")
				for duplicate := 2; used[entryKey]; duplicate++ {
					entryKey = markdownKey(fullKey, fmt.Sprintf(" #%d", duplicate))
				}
				used[entryKey] = true
				entries = append(entries, retrieval.Entry{Key: entryKey, Content: content, Source: source, Evidence: evidence, Identifiers: markdownIdentifiers(content)})
				if len(entries) > 100 {
					return nil, fmt.Errorf("%s: markdown exceeds 100 entries", name)
				}
			}
		}
	}
	return entries, nil
}

func readMarkdown(name, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if len(b) > 4<<20 {
		return "", fmt.Errorf("%s: input exceeds 4 MiB", name)
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%s: input is not valid UTF-8", name)
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	for i, line := range strings.Split(text, "\n") {
		if markdownFrame(line) {
			return "", fmt.Errorf("%s:%d: content contains a frame marker", name, i+1)
		}
	}
	return text, nil
}

func markdownFrame(s string) bool {
	for _, marker := range []string{"<project-reference", "</project-reference>", "[Reference ", "[/Reference]"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func markdownFence(line string, char byte, length int) (byte, int, bool) {
	trim := strings.TrimLeft(line, " ")
	if len(line)-len(trim) > 3 || len(trim) < 3 {
		return char, length, false
	}
	if trim[0] != '`' && trim[0] != '~' {
		return char, length, false
	}
	n := 0
	for n < len(trim) && trim[n] == trim[0] {
		n++
	}
	if length > 0 {
		if trim[0] == char && n >= length && strings.TrimSpace(trim[n:]) == "" {
			return 0, 0, true
		}
		return char, length, false
	}
	if n < 3 || (trim[0] == '`' && strings.Contains(trim[n:], "`")) {
		return char, length, false
	}
	return trim[0], n, true
}

func markdownSections(text string) []markdownSection {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[0] == "---" {
		end := 1
		for end < len(lines) && lines[end] != "---" && lines[end] != "..." {
			end++
		}
		// Without a closing line it is a thematic break, not front matter.
		if end < len(lines) {
			lines = lines[end+1:]
		}
	}
	sections := []markdownSection{}
	var body []string
	type heading struct {
		level int
		text  string
	}
	var ancestors []heading
	path := ""
	flush := func() {
		content := strings.Trim(strings.Join(body, "\n"), "\n")
		if strings.TrimSpace(content) != "" {
			sections = append(sections, markdownSection{path, content})
		}
		body = nil
	}
	var char byte
	length := 0
	for _, line := range lines {
		wasFenced := length > 0
		var fence bool
		char, length, fence = markdownFence(line, char, length)
		if !wasFenced && !fence {
			trim := strings.TrimLeft(line, " ")
			n := 0
			for n < len(trim) && trim[n] == '#' {
				n++
			}
			if len(line)-len(trim) <= 3 && n >= 1 && n <= 6 && (n == len(trim) || trim[n] == ' ') {
				flush()
				for len(ancestors) > 0 && ancestors[len(ancestors)-1].level >= n {
					ancestors = ancestors[:len(ancestors)-1]
				}
				title := strings.TrimSpace(trim[n:])
				if p := strings.LastIndex(title, " "); p >= 0 && strings.Trim(title[p+1:], "#") == "" {
					title = strings.TrimSpace(title[:p])
				}
				ancestors = append(ancestors, heading{n, title})
				titles := make([]string, len(ancestors))
				for i, h := range ancestors {
					titles[i] = h.text
				}
				path = strings.Join(titles, " > ")
			}
		}
		body = append(body, line)
	}
	flush()
	return sections
}

func markdownKey(full, suffix string) string {
	if len(full)+len(suffix) <= 128 {
		return full + suffix
	}
	hash := sha256.Sum256([]byte(full))
	tail := fmt.Sprintf("~%x", hash[:4]) + suffix
	n := 128 - len(tail)
	for n > 0 && !utf8.RuneStart(full[n]) {
		n--
	}
	return full[:n] + tail
}

func markdownFits(s string, max int) bool { return len(s) <= 16384 && len(strings.Fields(s)) <= max }

func markdownChunks(content string, max, overlap int) []string {
	if markdownFits(content, max) {
		return []string{content}
	}
	var units []string
	var paragraph []string
	flush := func() {
		if len(paragraph) > 0 {
			units = append(units, strings.Join(paragraph, "\n"))
			paragraph = nil
		}
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
		} else {
			paragraph = append(paragraph, line)
		}
	}
	flush()
	var chunks []string
	current := ""
	emit := func() {
		chunks = append(chunks, current)
		words := strings.Fields(current)
		if len(words) > overlap {
			words = words[len(words)-overlap:]
		}
		current = strings.Join(words, " ")
	}
	for _, unit := range units {
		joined := unit
		if current != "" {
			joined = current + "\n\n" + unit
		}
		if markdownFits(joined, max) {
			current = joined
			continue
		}
		if current != "" {
			emit()
		}
		joined = unit
		if current != "" {
			joined = current + "\n\n" + unit
		}
		if markdownFits(joined, max) {
			current = joined
			continue
		}
		for _, word := range strings.Fields(unit) {
			for word != "" {
				separator := ""
				if current != "" {
					separator = " "
				}
				if markdownFits(current+separator+word, max) {
					current += separator + word
					break
				}
				if current != "" {
					if len(chunks) == 0 || current != strings.Join(lastMarkdownWords(chunks[len(chunks)-1], overlap), " ") {
						emit()
					}
					for current != "" && !markdownFits(current+" "+word, max) {
						fields := strings.Fields(current)
						current = strings.Join(fields[1:], " ")
					}
					continue
				}
				n := 16384
				if n > len(word) {
					n = len(word)
				}
				for n > 0 && n < len(word) && !utf8.RuneStart(word[n]) {
					n--
				}
				current, word = word[:n], word[n:]
				if word != "" {
					emit()
					current = ""
				}
			}
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

func lastMarkdownWords(s string, n int) []string {
	words := strings.Fields(s)
	if len(words) > n {
		words = words[len(words)-n:]
	}
	return words
}

var markdownToken = regexp.MustCompile(`[A-Za-z0-9_./:\\-]+`)
var markdownIdentifier = regexp.MustCompile(`^[A-Za-z0-9_./:\\-]+( +[A-Za-z0-9_./:\\-]+)*$`)
var markdownExtension = regexp.MustCompile(`\.[A-Za-z0-9_]+$`)

func markdownIdentifiers(content string) []string {
	ids := []string{}
	seen := map[string]bool{}
	add := func(s string) {
		if len(ids) < 32 && len(s) <= 256 && markdownIdentifier.MatchString(s) && !seen[s] {
			seen[s] = true
			ids = append(ids, s)
		}
	}
	var char byte
	length := 0
	for _, line := range strings.Split(content, "\n") {
		wasFenced := length > 0
		var fence bool
		char, length, fence = markdownFence(line, char, length)
		type candidate struct {
			start int
			value string
		}
		var candidates []candidate
		for _, span := range markdownToken.FindAllStringIndex(line, -1) {
			token := line[span[0]:span[1]]
			if !strings.HasSuffix(token, "/...") {
				token = strings.TrimRight(token, ".:")
			}
			if strings.ContainsAny(token, `/\`) || markdownExtension.MatchString(token) {
				candidates = append(candidates, candidate{span[0], token})
			}
		}
		for i := 0; !wasFenced && !fence && i < len(line); {
			if line[i] == '`' {
				n := 1
				for i+n < len(line) && line[i+n] == '`' {
					n++
				}
				close := strings.Index(line[i+n:], strings.Repeat("`", n))
				if close >= 0 {
					end := i + n + close
					if n == 1 && (end+1 == len(line) || line[end+1] != '`') {
						candidates = append(candidates, candidate{i, line[i+1 : end]})
					}
					i = end + n
					continue
				}
				i += n
				continue
			}
			i++
		}
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].start < candidates[j].start })
		for _, c := range candidates {
			add(c.value)
		}
	}
	return ids
}
