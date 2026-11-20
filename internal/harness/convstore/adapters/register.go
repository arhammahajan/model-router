package adapters

import "github.com/arhammahajan/model-router/internal/harness/convstore"

// All returns every registered coding-agent conversation adapter.
func All() []convstore.Adapter {
	return []convstore.Adapter{
		NewClaudeAdapter(),
		NewCodexAdapter(),
		NewOpencodeAdapter(),
		NewKiloAdapter(),
		NewAntigravityAdapter(),
		NewMuseAdapter(),
		NewMcodeAdapter(),
		NewCrushAdapter(),
		NewPiAdapter(),
		NewOmpAdapter(),
		NewQwenAdapter(),
		NewCopilotAdapter(),
		NewGeminiAdapter(),
		NewKimiAdapter(),
		NewVibeAdapter(),
		NewClineAdapter(),
		NewGrokAdapter(),
		NewDeepseekAdapter(),
		NewFxAdapter(),
	}
}
