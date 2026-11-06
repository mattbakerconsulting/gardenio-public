package bridge

import "testing"

// TestThreadCardsEnabled covers the resolution order documented in
// docs/research/thread-cards/03-config-and-rollback.md §1:
// config-bead kill switch → config-bead project allow-list → env mode → off.
func TestThreadCardsEnabled(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		cfg     ThreadCardsConfig
		project string
		channel string
		want    bool
	}{
		{
			name:    "default mode off is disabled",
			mode:    "off",
			cfg:     DefaultThreadCardsConfig(),
			project: "gardenio",
			want:    false,
		},
		{
			name:    "empty mode (unset) is disabled",
			mode:    "",
			cfg:     DefaultThreadCardsConfig(),
			project: "gardenio",
			want:    false,
		},
		{
			name:    "owned mode, no allow-list, kill switch on",
			mode:    "owned",
			cfg:     ThreadCardsConfig{Enabled: true},
			project: "gardenio",
			want:    true,
		},
		{
			name:    "all mode, no allow-list",
			mode:    "all",
			cfg:     ThreadCardsConfig{Enabled: true},
			project: "anything",
			want:    true,
		},
		{
			name:    "kill switch off overrides owned mode",
			mode:    "owned",
			cfg:     ThreadCardsConfig{Enabled: false},
			project: "gardenio",
			want:    false,
		},
		{
			name:    "allow-list match",
			mode:    "owned",
			cfg:     ThreadCardsConfig{Enabled: true, Projects: []string{"gardenio-e2e"}},
			project: "gardenio-e2e",
			want:    true,
		},
		{
			name:    "allow-list miss",
			mode:    "owned",
			cfg:     ThreadCardsConfig{Enabled: true, Projects: []string{"gardenio-e2e"}},
			project: "gardenio",
			want:    false,
		},
		{
			name:    "excluded channel is disabled",
			mode:    "all",
			cfg:     ThreadCardsConfig{Enabled: true, ExcludeChannels: []string{"C0C847U6Y9Z"}},
			project: "gardenio",
			channel: "C0C847U6Y9Z",
			want:    false,
		},
		{
			name:    "other channel unaffected by exclude list",
			mode:    "all",
			cfg:     ThreadCardsConfig{Enabled: true, ExcludeChannels: []string{"C0C847U6Y9Z"}},
			project: "gardenio",
			channel: "C0C3RH33A81",
			want:    true,
		},
		{
			name:    "invalid mode treated as off",
			mode:    "bogus",
			cfg:     ThreadCardsConfig{Enabled: true},
			project: "gardenio",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &Bot{threadCardsMode: tt.mode, threadCardsConfig: tt.cfg}
			if got := b.threadCardsEnabled(tt.project, tt.channel); got != tt.want {
				t.Errorf("threadCardsEnabled(%q, %q)=%v, want %v", tt.project, tt.channel, got, tt.want)
			}
		})
	}
}
