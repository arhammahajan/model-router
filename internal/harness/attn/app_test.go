package attn

import "testing"

func TestCanonicalApp(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude", "claude-code"},
		{"kimi-cli", "kimi"},
		{"dsh", "deepseek"},
		{"dsh-tui", "deepseek"},
		{"cursor", "cursor-agent"},
		{"agy", "antigravity"},
		{"opencode", "opencode"},
		{"codex", "codex"},
		{"claude-code", "claude-code"}, // already canonical stays put
		{"", ""},
	}
	for _, c := range cases {
		if got := CanonicalApp(c.in); got != c.want {
			t.Errorf("CanonicalApp(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
