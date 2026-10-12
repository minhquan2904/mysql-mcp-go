package source

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Registry holds the configured sources by name.
type Registry struct {
	byName map[string]*Source
	names  []string // sorted
}

// NewRegistry indexes srcs by Name.
func NewRegistry(srcs []*Source) *Registry {
	r := &Registry{byName: make(map[string]*Source, len(srcs))}
	for _, s := range srcs {
		r.byName[s.Name] = s
		r.names = append(r.names, s.Name)
	}
	slices.Sort(r.names)
	return r
}

// Names returns the source names, sorted.
func (r *Registry) Names() []string { return slices.Clone(r.names) }

// Resolve returns the named source. An empty name is accepted only when
// exactly one source is configured.
func (r *Registry) Resolve(name string) (*Source, error) {
	if name == "" {
		if len(r.names) == 1 {
			return r.byName[r.names[0]], nil
		}
		return nil, fmt.Errorf("source is required; available: %s", strings.Join(r.names, ", "))
	}
	s, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("unknown source %q; available: %s", name, strings.Join(r.names, ", "))
	}
	return s, nil
}

// Health pings every source concurrently. A nil value means healthy.
func (r *Registry) Health(ctx context.Context) map[string]error {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make(map[string]error, len(r.names))
	)
	for _, name := range r.names {
		wg.Go(func() {
			err := r.byName[name].Ping(ctx)
			mu.Lock()
			out[name] = err
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

// Close closes every source and joins the errors.
func (r *Registry) Close() error {
	var errs []error
	for _, name := range r.names {
		errs = append(errs, r.byName[name].Close())
	}
	return errors.Join(errs...)
}
