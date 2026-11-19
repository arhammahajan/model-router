package attn

// CanonicalApp maps an agent name to the app label carried in OSC
// 7501 records — the same mapping the zj helper uses. Unknown agents
// keep their name unchanged.
func CanonicalApp(agent string) string {
	switch agent {
	case "claude":
		return "claude-code"
	case "kimi-cli":
		return "kimi"
	case "dsh", "dsh-tui":
		return "deepseek"
	case "cursor":
		return "cursor-agent"
	case "agy":
		return "antigravity"
	default:
		return agent
	}
}
