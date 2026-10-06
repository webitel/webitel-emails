package postgres

import "testing"

func TestLikePattern(t *testing.T) {
	tests := []struct {
		search string
		want   string
	}{
		{"support", "support"},
		{"*support*", "%support%"},
		{"sup*", "sup%"},
		{"50%", `50\%`},
		{"sales_team", `sales\_team`},
		{`a\b`, `a\\b`},
		{"*50%_*", `%50\%\_%`},
	}

	for _, tt := range tests {
		if got := likePattern(tt.search); got != tt.want {
			t.Errorf("likePattern(%q) = %q, want %q", tt.search, got, tt.want)
		}
	}
}
