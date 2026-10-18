// Package capability provides a provider-independent registry for executable
// problem-solving capabilities. It does not require an LLM or external service.
package capability

import (
 "context"
 "errors"
 "fmt"
 "sort"
 "sync"
)

var (
 ErrInvalid = errors.New("invalid capability")
 ErrDuplicate = errors.New("capability already registered")
 ErrNotFound = errors.New("capability not found")
 ErrUnauthorized = errors.New("capability side effects not permitted")
)

// Descriptor describes a versioned, domain-independent operation.
// Effects are permission labels enforced by the caller's policy.
type Descriptor struct {
 ID string
 Version string
 Domain string
 Description string
 Effects []string
}

// Request contains the input and explicitly authorized side effects.
type Request struct {
 Input []byte
 AllowedEffects []string
}

// Result is returned by a capability. Evidence must be checked independently.
type Result struct {
 Output []byte
 Evidence []byte
}

// Handler implements one operation. Implementations must honor context cancellation.
type Handler func(context.Context, []byte) (Result, error)

type entry struct {
 descriptor Descriptor
 handler Handler
}

// Registry is safe for concurrent registration and execution.
type Registry struct {
 mu sync.RWMutex
 entries map[string]entry
}

func NewRegistry() *Registry { return &Registry{entries: make(map[string]entry)} }

func key(id, version string) string { return id + "@" + version }

func (r *Registry) Register(d Descriptor, h Handler) error {
 if d.ID == "" || d.Version == "" || d.Domain == "" || h == nil {
  return ErrInvalid
 }
 for _, effect := range d.Effects {
  if effect == "" { return ErrInvalid }
 }
 r.mu.Lock()
 defer r.mu.Unlock()
 if r.entries == nil { r.entries = make(map[string]entry) }
 k := key(d.ID, d.Version)
 if _, exists := r.entries[k]; exists { return ErrDuplicate }
 d.Effects = append([]string(nil), d.Effects...)
 r.entries[k] = entry{d, h}
 return nil
}

func (r *Registry) List() []Descriptor {
 r.mu.RLock()
 defer r.mu.RUnlock()
 out := make([]Descriptor, 0, len(r.entries))
 for _, e := range r.entries {
  d := e.descriptor
  d.Effects = append([]string(nil), d.Effects...)
  out = append(out, d)
 }
 sort.Slice(out, func(i, j int) bool {
  if out[i].ID == out[j].ID { return out[i].Version < out[j].Version }
  return out[i].ID < out[j].ID
 })
 return out
}

func (r *Registry) Execute(ctx context.Context, id, version string, req Request) (Result, error) {
 if err := ctx.Err(); err != nil { return Result{}, err }
 r.mu.RLock()
 e, ok := r.entries[key(id, version)]
 r.mu.RUnlock()
 if !ok { return Result{}, ErrNotFound }
 allowed := make(map[string]bool, len(req.AllowedEffects))
 for _, effect := range req.AllowedEffects { allowed[effect] = true }
 for _, effect := range e.descriptor.Effects {
  if !allowed[effect] { return Result{}, fmt.Errorf("%w: %s", ErrUnauthorized, effect) }
 }
 return e.handler(ctx, append([]byte(nil), req.Input...))
}
