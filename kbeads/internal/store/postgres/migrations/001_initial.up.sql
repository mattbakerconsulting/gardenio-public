CREATE TABLE IF NOT EXISTS beads (
    id TEXT PRIMARY KEY,
    slug TEXT,
    kind TEXT NOT NULL,
    type TEXT NOT NULL,
    title TEXT NOT NULL,
    description TEXT DEFAULT '',
    notes TEXT DEFAULT '',
    status TEXT NOT NULL DEFAULT 'open',
    priority INTEGER NOT NULL DEFAULT 2,
    assignee TEXT DEFAULT '',
    owner TEXT DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by TEXT DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    closed_at TIMESTAMPTZ,
    closed_by TEXT DEFAULT '',
    due_at TIMESTAMPTZ,
    defer_until TIMESTAMPTZ,
    fields JSONB
);

CREATE INDEX idx_beads_status ON beads(status);
CREATE INDEX idx_beads_type ON beads(type);
CREATE INDEX idx_beads_kind ON beads(kind);
CREATE INDEX idx_beads_assignee ON beads(assignee);
CREATE INDEX idx_beads_priority ON beads(priority);

CREATE TABLE IF NOT EXISTS labels (
    bead_id TEXT NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    label TEXT NOT NULL,
    PRIMARY KEY (bead_id, label)
);

CREATE INDEX idx_labels_label ON labels(label);

CREATE TABLE IF NOT EXISTS deps (
    bead_id TEXT NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    depends_on_id TEXT NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by TEXT DEFAULT '',
    metadata TEXT DEFAULT '',
    PRIMARY KEY (bead_id, depends_on_id, type)
);

CREATE TABLE IF NOT EXISTS comments (
    id BIGSERIAL PRIMARY KEY,
    bead_id TEXT NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    author TEXT NOT NULL DEFAULT '',
    text TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_comments_bead_id ON comments(bead_id);

CREATE TABLE IF NOT EXISTS configs (
    key TEXT PRIMARY KEY,
    value JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS events (
    id         BIGSERIAL PRIMARY KEY,
    topic      TEXT NOT NULL,
    bead_id    TEXT NOT NULL,
    actor      TEXT NOT NULL DEFAULT '',
    payload    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_events_bead_id ON events (bead_id);
CREATE INDEX idx_events_topic ON events (topic);
