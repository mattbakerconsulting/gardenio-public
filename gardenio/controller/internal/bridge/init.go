// Package bridge registers bead types, views, and context configs that
// gardenio requires in the beads daemon.  Call EnsureConfigs at startup to
// upsert the canonical definitions; existing user overrides are left alone.
package bridge

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// initConfigsYAML holds the type, view and context entries configs()
// registers. Each top-level key is the config bead's title, and its prefix
// (type:, view: or context:) picks the schema the entry decodes into.
//
//go:embed init_configs.yaml
var initConfigsYAML []byte

// loadConfigs returns every config entry gardenio needs in the daemon. The
// YAML is embedded at build time and pinned by TestInitConfigs_Golden, so an
// error here means a bad edit to init_configs.yaml slipped past the tests.
func loadConfigs() (map[string]any, error) {
	m, err := decodeInitConfigs(initConfigsYAML)
	if err != nil {
		return nil, err
	}

	// Merge config bead entries (nudge-prompts, claude-instructions), the
	// type:message and type:permission registrations (init_message.go), the
	// type:mail and type:dm registrations (init_mail.go) and the
	// type:decision registration (init_decision.go).
	for k, v := range configBeadEntries() {
		m[k] = v
	}
	for k, v := range messageTypeConfigs() {
		m[k] = v
	}
	for k, v := range mailTypeConfigs() {
		m[k] = v
	}
	for k, v := range decisionTypeConfigs() {
		m[k] = v
	}

	return m, nil
}

// decodeInitConfigs decodes init_configs.yaml strictly: an unknown field, a
// repeated key or a key without a known prefix is an error.
func decodeInitConfigs(data []byte) (map[string]any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("init_configs.yaml: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("init_configs.yaml: top level is not a mapping")
	}
	top := doc.Content[0]
	m := make(map[string]any, len(top.Content)/2)
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, val := top.Content[i].Value, top.Content[i+1]
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("init_configs.yaml: %q is defined twice", key)
		}
		var err error
		switch {
		case strings.HasPrefix(key, "type:"):
			m[key], err = decodeInitConfig[TypeConfig](val)
		case strings.HasPrefix(key, "view:"):
			m[key], err = decodeInitConfig[ViewConfig](val)
		case strings.HasPrefix(key, "context:"):
			m[key], err = decodeInitConfig[ContextConfig](val)
		default:
			err = fmt.Errorf("want a type:, view: or context: prefix")
		}
		if err != nil {
			return nil, fmt.Errorf("init_configs.yaml: %q: %w", key, err)
		}
	}
	return m, nil
}

// decodeInitConfig decodes one entry, rejecting fields T does not have.
func decodeInitConfig[T any](n *yaml.Node) (T, error) {
	var v T
	raw, err := yaml.Marshal(n)
	if err != nil {
		return v, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	err = dec.Decode(&v)
	return v, err
}
