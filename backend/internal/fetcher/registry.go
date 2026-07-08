package fetcher

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/collectors"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// CollectorFactory constructs a collectors.Collector from project config and
// shared dependencies.
type CollectorFactory func(cfg *project.Config, backend storage.Backend, client *http.Client) (collectors.Collector, error)

// CollectorRegistry maps artifacts.collector names to factories.
// The zero value is not usable; use NewCollectorRegistry.
type CollectorRegistry struct {
	factories map[string]CollectorFactory
}

// NewCollectorRegistry returns an empty registry.
func NewCollectorRegistry() *CollectorRegistry {
	return &CollectorRegistry{factories: map[string]CollectorFactory{}}
}

// Register adds a factory under name. Panics on duplicate registration since
// that is always a programming error.
func (r *CollectorRegistry) Register(name string, f CollectorFactory) {
	if _, exists := r.factories[name]; exists {
		panic(fmt.Sprintf("collector %q already registered", name))
	}
	r.factories[name] = f
}

// Has reports whether a factory is registered under name.
func (r *CollectorRegistry) Has(name string) bool {
	_, ok := r.factories[name]
	return ok
}

// Names returns the sorted list of registered collector names.
func (r *CollectorRegistry) Names() []string {
	names := make([]string, 0, len(r.factories))
	for n := range r.factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Build picks the factory named by cfg.CollectorName() and invokes it. The
// error message lists registered alternatives so a missing collector points at
// the fix.
func (r *CollectorRegistry) Build(cfg *project.Config, backend storage.Backend, client *http.Client) (collectors.Collector, error) {
	name := cfg.CollectorName()
	f, ok := r.factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown collector %q (registered: %s)", name, strings.Join(r.Names(), ", "))
	}
	return f(cfg, backend, client)
}
