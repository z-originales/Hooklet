package store

import "testing"

func TestMatchTopic(t *testing.T) {
	tests := []struct {
		name      string
		pattern   string
		topicName string
		want      bool
	}{
		// Exact matches
		{"exact match", "orders.created", "orders.created", true},
		{"exact no match", "orders.created", "orders.updated", false},

		// ** (match all)
		{"double star matches everything", "**", "orders.created", true},
		{"double star matches deep", "**", "orders.eu.paris.created", true},
		{"double star matches single", "**", "orders", true},

		// Single * (one level)
		{"star matches one level", "orders.*", "orders.created", true},
		{"star matches any value", "orders.*", "orders.updated", true},
		{"star does not match deep", "orders.*", "orders.eu.created", false},
		{"star at start", "*.created", "orders.created", true},
		{"star at start no match", "*.created", "orders.updated", false},
		{"star in middle", "orders.*.created", "orders.eu.created", true},
		{"star in middle no match", "orders.*.created", "orders.created", false},

		// ** (multi-level)
		{"double star suffix", "orders.**", "orders.created", true},
		{"double star suffix deep", "orders.**", "orders.eu.created", true},
		{"double star suffix very deep", "orders.**", "orders.eu.paris.warehouse.created", true},
		{"double star prefix", "**.created", "orders.created", true},
		{"double star prefix deep", "**.created", "orders.eu.created", true},
		{"double star in middle", "orders.**.created", "orders.eu.created", true},
		{"double star in middle deep", "orders.**.created", "orders.eu.paris.created", true},
		{"double star in middle direct", "orders.**.created", "orders.created", true},

		// Mixed patterns
		{"star and double star", "orders.*.eu.**", "orders.shipping.eu.paris.warehouse", true},

		// No wildcards = exact match only
		{"no wildcard exact", "orders.created", "orders.created", true},
		{"no wildcard no match", "orders.created", "orders", false},

		// Edge cases
		{"empty pattern", "", "", true},
		{"pattern longer than topic", "orders.created.extra", "orders.created", false},
		{"topic longer than pattern no star", "orders.created", "orders.created.extra", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchTopic(tt.pattern, tt.topicName)
			if got != tt.want {
				t.Errorf("MatchTopic(%q, %q) = %v, want %v", tt.pattern, tt.topicName, got, tt.want)
			}
		})
	}
}
