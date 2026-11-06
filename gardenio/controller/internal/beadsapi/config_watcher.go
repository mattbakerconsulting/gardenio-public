package beadsapi

// ConfigWatcher is step 3 of the env→config-bead campaign (ka-0000006607).
// It polls `type=config` beads for runtime-tunable settings, caches the
// effective values in memory, and exposes a typed accessor enforcing the
// precedence contract defined in the epic:
//
//	effective_value = config_bead_value   (if present and parses)
//	                ?? env_var_value      (if set)
//	                ?? compiled_default
//
// Bead layout (filling the gap from the crashed step-2 agent):
//
//   - One `type=config` bead per component, `title="tunables"`.
//   - Label `component:<name>` is required on every tunable bead.
//   - Optional scope labels `project:<name>`, `role:<name>` layer finer-grained
//     overrides on top of the component-wide bead. A bead with both a component
//     label and a project label beats a component-only bead for that project;
//     project+role beats project alone; role-only beats component-only.
//   - Key/value pairs live in the bead's `Fields` map. Keys mirror the env-var
//     name (e.g. `COOP_MAX_PODS`) so code, Helm, and beads stay greppable.
//
// These choices mirror the existing `config:auth` and `config:claude-instructions`
// patterns and keep the accessor surface independent of future layout tweaks —
// only the matcher in loadTunables() would change if step 4 decides to split
// per-key beads.
//
// Update detection is a simple poll loop (interval defaults to 30 s, override
// via WithPollInterval). NATS subscription is deferred; the polling loop is
// cheap because listBeads filters server-side by type=config + component label.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultConfigPollInterval is the default frequency at which ConfigWatcher
// refreshes its view of tunable config beads.
const DefaultConfigPollInterval = 30 * time.Second

// ConfigChange is emitted on the Subscribe() channel whenever a tunable key
// transitions to a different raw value (add, update, or delete).
type ConfigChange struct {
	Key      string
	OldValue string // empty if the key was added
	NewValue string // empty if the key was deleted
	Source   ValueSource
}

// ValueSource identifies which layer of the precedence stack provided an
// effective value — useful for logging, metrics, and tests.
type ValueSource string

const (
	SourceBead     ValueSource = "bead"
	SourceEnv      ValueSource = "env"
	SourceCompiled ValueSource = "compiled"
)

// ConfigWatcherOption configures a ConfigWatcher.
type ConfigWatcherOption func(*ConfigWatcher)

// WithPollInterval overrides the default poll interval.
func WithPollInterval(d time.Duration) ConfigWatcherOption {
	return func(w *ConfigWatcher) {
		if d > 0 {
			w.pollInterval = d
		}
	}
}

// WithLookupEnv overrides the process environment lookup (os.LookupEnv by
// default). Used by tests to isolate env state.
func WithLookupEnv(lookup func(string) (string, bool)) ConfigWatcherOption {
	return func(w *ConfigWatcher) {
		if lookup != nil {
			w.lookupEnv = lookup
		}
	}
}

// WithLogger installs a logger (default: slog.Default()).
func WithLogger(logger *slog.Logger) ConfigWatcherOption {
	return func(w *ConfigWatcher) {
		if logger != nil {
			w.logger = logger
		}
	}
}

// ConfigWatcher polls tunable config beads for a single component and serves
// the cached values to consumers via the Get* accessors.
type ConfigWatcher struct {
	client       *Client
	component    string
	project      string
	role         string
	pollInterval time.Duration
	lookupEnv    func(string) (string, bool)
	logger       *slog.Logger

	mu        sync.RWMutex
	values    map[string]string // key -> raw string value (most-specific bead wins)
	ready     bool              // true after the first successful load
	readyCh   chan struct{}
	subs      []chan ConfigChange
	stopOnce  sync.Once
	stopped   chan struct{}
}

// NewConfigWatcher constructs a watcher for the given component. The watcher
// is inert until Start is called.
func NewConfigWatcher(client *Client, component string, opts ...ConfigWatcherOption) (*ConfigWatcher, error) {
	if client == nil {
		return nil, fmt.Errorf("beadsapi: client is required")
	}
	if component == "" {
		return nil, fmt.Errorf("beadsapi: component is required")
	}
	w := &ConfigWatcher{
		client:       client,
		component:    component,
		pollInterval: DefaultConfigPollInterval,
		lookupEnv:    os.LookupEnv,
		logger:       slog.Default(),
		values:       make(map[string]string),
		readyCh:      make(chan struct{}),
		stopped:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w, nil
}

// Start performs an initial synchronous load, then spawns a background
// goroutine that refreshes on every poll tick until ctx is cancelled or Stop
// is called. Start returns after the first load completes (success or error).
// Callers may safely continue past a startup error — subsequent polls will
// retry.
func (w *ConfigWatcher) Start(ctx context.Context) error {
	err := w.refresh(ctx)
	w.markReady()

	go w.run(ctx)
	return err
}

// Stop terminates the background poll loop and closes subscriber channels.
// Safe to call multiple times.
func (w *ConfigWatcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopped)
		w.mu.Lock()
		for _, ch := range w.subs {
			close(ch)
		}
		w.subs = nil
		w.mu.Unlock()
	})
}

// Ready returns a channel that is closed once the watcher has completed its
// first load attempt. Intended for tests and shutdown coordination.
func (w *ConfigWatcher) Ready() <-chan struct{} { return w.readyCh }

// Subscribe returns a channel that receives a ConfigChange for every key
// whose raw bead value changed during a refresh. Callers must drain the
// channel promptly; the sender drops events for slow subscribers rather
// than blocking the poll loop.
func (w *ConfigWatcher) Subscribe() <-chan ConfigChange {
	ch := make(chan ConfigChange, 16)
	w.mu.Lock()
	w.subs = append(w.subs, ch)
	w.mu.Unlock()
	return ch
}

func (w *ConfigWatcher) markReady() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.ready {
		w.ready = true
		close(w.readyCh)
	}
}

func (w *ConfigWatcher) run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopped:
			return
		case <-ticker.C:
			if err := w.refresh(ctx); err != nil {
				w.logger.Warn("configwatcher: refresh failed",
					"component", w.component, "err", err)
			}
		}
	}
}

// refresh lists tunable config beads and updates the in-memory cache, firing
// change events for every key whose raw value moved.
func (w *ConfigWatcher) refresh(ctx context.Context) error {
	beads, err := w.listTunableBeads(ctx)
	if err != nil {
		return err
	}
	next := resolveBeadValues(beads, w.component, w.project, w.role)

	// Hold w.mu across the (non-blocking) sends so the producer can never send
	// on a channel that Stop() is concurrently closing — Stop() takes the same
	// lock to close w.subs. The select's default keeps every send non-blocking,
	// so the lock is held only for a cheap iteration; once Stop() has run,
	// w.subs is nil and nothing is sent.
	w.mu.Lock()
	defer w.mu.Unlock()
	diff := diffConfigMaps(w.values, next)
	w.values = next
	for _, ch := range diff {
		for _, sub := range w.subs {
			select {
			case sub <- ch:
			default:
				w.logger.Warn("configwatcher: subscriber dropped change",
					"component", w.component, "key", ch.Key)
			}
		}
	}
	return nil
}

func (w *ConfigWatcher) listTunableBeads(ctx context.Context) ([]*BeadDetail, error) {
	resp, err := w.client.ListBeadsFiltered(ctx, ListBeadsQuery{
		Kinds:    []string{"config"},
		Statuses: []string{"open"},
		Labels:   []string{"component:" + w.component},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing tunable beads for %q: %w", w.component, err)
	}
	return resp.Beads, nil
}

// resolveBeadValues walks beads ordered by scope specificity (least to most),
// letting more-specific beads overwrite less-specific ones. Specificity order:
//
//	1. component-only                         (lowest)
//	2. component + role
//	3. component + project
//	4. component + project + role             (highest)
//
// Beads whose component label does not match are skipped defensively —
// listTunableBeads already filters server-side, but fields can still surface
// from an accidental label mismatch.
func resolveBeadValues(beads []*BeadDetail, component, project, role string) map[string]string {
	ordered := make([]*BeadDetail, 0, len(beads))
	for _, b := range beads {
		if b == nil || b.Title != "tunables" {
			continue
		}
		if !slices.Contains(b.Labels, "component:"+component) {
			continue
		}
		// Skip beads labeled for a different project/role than ours.
		if beadProject := labelValue(b.Labels, "project:"); beadProject != "" && beadProject != project {
			continue
		}
		if beadRole := labelValue(b.Labels, "role:"); beadRole != "" && beadRole != role {
			continue
		}
		ordered = append(ordered, b)
	}
	slices.SortStableFunc(ordered, func(a, b *BeadDetail) int {
		return scopeRank(a) - scopeRank(b)
	})

	out := make(map[string]string)
	for _, b := range ordered {
		for k, v := range b.Fields {
			out[k] = v
		}
	}
	return out
}

// scopeRank returns 0 for component-only, 1 for +role, 2 for +project, 3 for
// both project and role. Higher wins.
func scopeRank(b *BeadDetail) int {
	var rank int
	if labelValue(b.Labels, "role:") != "" {
		rank += 1
	}
	if labelValue(b.Labels, "project:") != "" {
		rank += 2
	}
	return rank
}

func labelValue(labels []string, prefix string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, prefix); ok {
			return v
		}
	}
	return ""
}

func diffConfigMaps(prev, next map[string]string) []ConfigChange {
	var out []ConfigChange
	for k, v := range next {
		if oldVal, ok := prev[k]; !ok || oldVal != v {
			out = append(out, ConfigChange{Key: k, OldValue: oldVal, NewValue: v, Source: SourceBead})
		}
	}
	for k, v := range prev {
		if _, ok := next[k]; !ok {
			out = append(out, ConfigChange{Key: k, OldValue: v, NewValue: "", Source: SourceBead})
		}
	}
	return out
}

// rawValue returns the raw bead value for key plus a bool indicating presence.
// Exposed for Get[T] and the typed helpers.
func (w *ConfigWatcher) rawValue(key string) (string, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	v, ok := w.values[key]
	return v, ok
}

// resolveRaw returns the raw string value and its source, honoring bead > env > compiled.
// The `compiled` argument is formatted only when the bead and env miss — keeping
// this helper generic-friendly even though `any` cannot be trivially stringified.
func (w *ConfigWatcher) resolveRaw(key, envVar string, compiledStr string) (string, ValueSource) {
	if v, ok := w.rawValue(key); ok {
		return v, SourceBead
	}
	if envVar != "" {
		if v, ok := w.lookupEnv(envVar); ok {
			return v, SourceEnv
		}
	}
	return compiledStr, SourceCompiled
}

// Get returns the effective value for `key`, applying bead > env > compiled
// precedence and parsing the raw string into T. T must be one of:
// string, bool, int, int64, float64, or time.Duration. On parse failure the
// compiled default is returned and a warning is logged.
//
// Get is a free function (not a method) because Go does not allow type
// parameters on methods.
func Get[T ConfigValue](w *ConfigWatcher, key, envVar string, compiled T) T {
	if w == nil {
		return compiled
	}
	raw, src := w.resolveRaw(key, envVar, formatCompiled(compiled))
	if src == SourceCompiled {
		return compiled
	}
	parsed, err := parseValue[T](raw)
	if err != nil {
		w.logger.Warn("configwatcher: parse failed, falling back to compiled default",
			"component", w.component, "key", key, "raw", raw, "err", err)
		return compiled
	}
	return parsed
}

// ConfigValue is the constraint of types the generic Get accessor supports.
// time.Duration is a named int64 so it is covered by ~int64; the parser
// distinguishes it from plain int64 via a runtime type switch on the zero
// value.
type ConfigValue interface {
	~string | ~bool | ~int | ~int64 | ~float64
}

func parseValue[T ConfigValue](raw string) (T, error) {
	var zero T
	// A type switch on `any(zero)` lets the compiler pick the right branch
	// and cast the parsed result back to T.
	switch any(zero).(type) {
	case string:
		return any(raw).(T), nil
	case bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return zero, err
		}
		return any(b).(T), nil
	case int:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return zero, err
		}
		return any(n).(T), nil
	case int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return zero, err
		}
		return any(n).(T), nil
	case float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return zero, err
		}
		return any(f).(T), nil
	case time.Duration:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return zero, err
		}
		return any(d).(T), nil
	}
	return zero, fmt.Errorf("unsupported config value type %T", zero)
}

func formatCompiled(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case time.Duration:
		return x.String()
	}
	return fmt.Sprint(v)
}

// GetString is a typed convenience wrapper around Get[string].
func (w *ConfigWatcher) GetString(key, envVar, compiled string) string {
	return Get[string](w, key, envVar, compiled)
}

// GetBool is a typed convenience wrapper around Get[bool].
func (w *ConfigWatcher) GetBool(key, envVar string, compiled bool) bool {
	return Get[bool](w, key, envVar, compiled)
}

// GetInt is a typed convenience wrapper around Get[int].
func (w *ConfigWatcher) GetInt(key, envVar string, compiled int) int {
	return Get[int](w, key, envVar, compiled)
}

// GetDuration is a typed convenience wrapper around Get[time.Duration].
func (w *ConfigWatcher) GetDuration(key, envVar string, compiled time.Duration) time.Duration {
	return Get[time.Duration](w, key, envVar, compiled)
}

// Snapshot returns a copy of the currently cached key/value map. The copy is
// safe to mutate; the watcher continues to own the canonical state. Intended
// for debugging and for exposing the effective config on an admin HTTP
// endpoint.
func (w *ConfigWatcher) Snapshot() map[string]string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make(map[string]string, len(w.values))
	for k, v := range w.values {
		out[k] = v
	}
	return out
}
