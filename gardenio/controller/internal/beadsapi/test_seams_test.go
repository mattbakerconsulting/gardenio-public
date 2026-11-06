package beadsapi

// WithProject restricts the watcher to beads scoped to a given project
// (label project:<name>). Matching beads override unscoped component beads.
func WithProject(project string) ConfigWatcherOption {
	return func(w *ConfigWatcher) { w.project = project }
}

// WithRole restricts the watcher to beads scoped to a given role
// (label role:<name>). Matching beads override unscoped component beads.
func WithRole(role string) ConfigWatcherOption {
	return func(w *ConfigWatcher) { w.role = role }
}
