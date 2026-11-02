package kernel

import (
	"fmt"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"reflect"
)

// ValidateContinuationPin checks saved pins without altering history.
func ValidateContinuationPin(record *retrieval.Record, conv *Conversation) error {
	if conv == nil {
		return fmt.Errorf("missing saved conversation")
	}
	if record == nil && conv.Retrieval == nil {
		return nil
	}
	if record == nil || conv.Retrieval == nil {
		return fmt.Errorf("saved retrieval pin missing from conversation")
	}
	a, b := record, conv.Retrieval
	for _, r := range []*retrieval.Record{a, b} {
		s := r.Scope
		// Providers may report a resolved model; the run log checks it against the requested scope.
		if (r.Mode != "optional" && r.Mode != "required") || s.TenantID == "" || s.ProjectID == "" || s.Revision == "" || s.Backend != "api" || s.Model == "" || conv.Model == "" {
			return fmt.Errorf("invalid saved retrieval scope or mode")
		}
		if r.Failure != "" && (r.Mode != "optional" || r.Failure != "optional retrieval unavailable") {
			return fmt.Errorf("failed retrieval cannot be continued")
		}
		if r.Packet == nil && (r.Mode != "optional" || r.Failure != "optional retrieval unavailable") {
			return fmt.Errorf("saved retrieval packet missing")
		}
		if r.Packet != nil {
			if err := retrieval.ValidatePacket(*r.Packet, "", ""); err != nil {
				return err
			}
		}
	}
	if a.Scope != b.Scope || a.Mode != b.Mode || a.OriginalPrompt != b.OriginalPrompt || a.Failure != b.Failure {
		return fmt.Errorf("saved retrieval pin mismatch")
	}
	if !reflect.DeepEqual(a.Packet, b.Packet) {
		return fmt.Errorf("saved retrieval packet mismatch")
	}
	return nil
}
