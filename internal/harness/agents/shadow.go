package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// ShadowVar maps every configured canonical agent to the primary environment
// variable that redirects its isolated shadow home or credential store. Claude
// uses CLAUDE_CONFIG_DIR and never overrides HOME, keeping the macOS login
// keychain reachable. Only verified agents are mapped; unknown agents and
// antigravity-native return false.
func ShadowVar(agent string) (string, bool) {
	switch agent {
	case "codex":
		return "CODEX_HOME", true
	case "claude":
		return "CLAUDE_CONFIG_DIR", true
	case "grok":
		return "GROK_HOME", true
	case "antigravity":
		return "ANTIGRAVITY_GEMINI_DIR", true
	case "antigravity-acp":
		return "GEMINI_HOME", true
	case "gemini":
		return "GEMINI_CLI_HOME", true
	case "copilot":
		return "COPILOT_HOME", true
	case "deepseek":
		return "DSH_HOME", true
	case "cline":
		return "CLINE_DATA_DIR", true
	case "opencode", "kilo":
		return "XDG_DATA_HOME", true
	case "pi":
		return "PI_CODING_AGENT_DIR", true
	case "qwen":
		return "QWEN_HOME", true
	case "kimi":
		return "KIMI_CODE_HOME", true
	case "kimi-cli":
		return "KIMI_SHARE_DIR", true
	case "mcode":
		return "MINIMAX_DATA_DIR", true
	case "muse":
		return "MUSE_AUTH_PATH", true
	case "fx":
		return "FX_HOME", true
	case "vibe", "vibe-acp":
		return "VIBE_HOME", true
	default:
		return "", false
	}
}

// ShadowEnv returns the primary environment variable name and value to set for
// an agent's isolated shadow home. Most agents take the shadow directory
// directly; Muse takes the per-profile auth.json file path via MUSE_AUTH_PATH.
func ShadowEnv(agent, shadowHome string) (key, val string, ok bool) {
	v, ok := ShadowVar(agent)
	if !ok || shadowHome == "" {
		return "", "", false
	}
	if agent == "muse" {
		return v, filepath.Join(shadowHome, "auth.json"), true
	}
	return v, shadowHome, true
}

// ShadowEnvMap returns the home redirects for an isolated agent launch.
func ShadowEnvMap(agent, shadowHome string) map[string]string {
	k, v, ok := ShadowEnv(agent, shadowHome)
	if !ok {
		return nil
	}
	out := map[string]string{k: v}
	if agent == "antigravity" || agent == "antigravity-acp" {
		out["HOME"] = shadowHome
		out["XDG_CONFIG_HOME"] = filepath.Join(shadowHome, ".config")
		out["XDG_DATA_HOME"] = filepath.Join(shadowHome, ".local", "share")
		out["XDG_STATE_HOME"] = filepath.Join(shadowHome, ".local", "state")
		out["XDG_CACHE_HOME"] = filepath.Join(shadowHome, ".cache")
		out["ANTIGRAVITY_GEMINI_DIR"] = filepath.Join(shadowHome, ".gemini")
		out["DBUS_SESSION_BUS_ADDRESS"] = "disabled:"
		sum := sha256.Sum256([]byte(filepath.Clean(shadowHome)))
		out["ANTIGRAVITY_CREDENTIAL_SCOPE"] = "harness-profile-" + hex.EncodeToString(sum[:6])

		if agent == "antigravity-acp" {
			out["AGY_ACP_FORCE_FILE_STORAGE"] = "1"
		} else {
			out["GEMINI_HOME"] = out["ANTIGRAVITY_GEMINI_DIR"]
		}
	}
	return out
}
