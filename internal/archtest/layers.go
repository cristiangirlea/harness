package archtest

import "strings"

// layers is the reviewed package map; names are repository-relative.
var layers = map[string]int{
	"internal/gitenv": 0, "internal/gitretry": 0, "internal/mask": 0, "internal/owner": 0, "internal/prefs": 0, "internal/purpose": 0, "internal/qr": 0, "internal/retrieval": 0, "internal/route/table": 0, "internal/runpaths": 0, "internal/scripthint": 0, "internal/shape": 0, "internal/envpolicy": 0, "internal/watchaddr": 0,
	"internal/kernel": 1, "internal/runlog": 1, "internal/flow": 1, "internal/quota": 1, "internal/limits": 1,
	"internal/mcp": 2, "internal/tools": 2, "internal/remote": 2, "internal/ws": 2,
	"internal/agent": 3, "internal/resolve": 4,
	"internal/workmap": 5, "internal/batch": 5, "internal/followup": 5, "internal/gc": 5, "internal/report": 5, "internal/flowview": 5,
	"internal/eval": 6, "internal/route": 6, "internal/game": 7,
	"internal/execproto": 8, "internal/hrnmcp": 8, "internal/watch": 8, "internal/update": 8, "internal/archtest": 8, "cmd/hrn": 8,
}

func layer(p string) (int, bool) {
	if p == "server" || strings.HasPrefix(p, "server/") {
		return 9, true
	}
	if p == "scripts" || strings.HasPrefix(p, "scripts/") {
		return 8, true
	}
	n, ok := layers[p]
	return n, ok
}
func allowed(from, to string) bool {
	if from == to {
		return true
	}
	a, _ := layer(from)
	b, _ := layer(to)
	if a == 9 {
		return b == 9
	}
	if b == 9 {
		return false
	}
	if from == "internal/agent" && (to == "internal/remote" || to == "internal/prefs" || to == "internal/purpose" || to == "internal/route/table") {
		return false
	}
	if (from == "internal/mcp" || from == "internal/tools") && to == "internal/retrieval" {
		return false
	}
	if a == 1 && b == 1 {
		return (from == "internal/runlog" && to == "internal/kernel") || (from != "internal/kernel" && from != "internal/runlog" && (to == "internal/kernel" || to == "internal/runlog"))
	}
	if a == 2 && b == 2 {
		return from == "internal/tools" && to == "internal/mcp"
	}
	if a == 5 && b == 5 || a == 6 && b == 6 {
		return true
	}
	if a == 8 && b == 8 {
		return from == "cmd/hrn"
	}
	return b < a
}
