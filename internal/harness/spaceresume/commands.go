package spaceresume

import (
	"os"
	"strconv"
	"strings"
)

// Hook is the `zj hook AGENT [Event]` entry: normalize the hook payload,
// emit the OSC record, publish or spool. With an empty agent it behaves as
// the RESURRECT_COMMAND post_command_discovery_hook.
func Hook(agent, event string) int {
	if strings.TrimSpace(agent) == "" {
		return HookResurrect()
	}
	return HookClient(agent, event)
}

// SessionHook is the `zj session-hook AGENT [Event]` entry (backward
// compatible alias for the hook client).
func SessionHook(agent, event string) int {
	return HookClient(agent, event)
}

// HookClient is the harness-facing hook entry point: it reads the provider
// JSON on stdin, infers nothing (the caller names the agent), normalizes via
// the seam, emits the OSC record, publishes or spools, and records the
// resume target. Outside Zellij, or for non-interactive ancestors, it exits 0
// silently.
func HookClient(agentName, event string) int {
	return hookClientWithPayload(agentName, event, nil, true)
}

func hookClientWithPayload(agentName, event string, payload map[string]any, transport bool) int {
	agent := Normalize(agentName)
	if agent == "" {
		agent = agentName
	}
	if payload == nil {
		payload = ReadHookPayload(0.5)
	}
	if AgentIsNoninteractive(agent) {
		return 0
	}
	session, pane, ok := hookTarget()
	if !ok {
		// Outside Zellij there is no pane to attribute the event to. Do not
		// emit OSC either: an Orca-hosted agent answers OSC 7501 itself.
		return 0
	}
	if HookDisabled == nil || HookDisabled() {
		EmitAgentEvent(agent, event)
		return 0
	}
	if NormalizeEvent == nil {
		EmitAgentEvent(agent, event)
		return 0
	}
	normalized := NormalizeEvent(agent, event, payload, environMap())
	if normalized == nil {
		return 0
	}
	if ResolvePaneEvent != nil {
		normalized = ResolvePaneEvent(session, pane, normalized)
	}
	if ev, _ := normalized["event"].(string); (ev == "session_start" || ev == "prompt" || ev == "pre_invocation" || ev == "stop" || ev == "stop_failure" || ev == "session_end") &&
		!truthy(normalized["child"]) {
		sid := ExtractSessionID(payload)
		if title, _ := normalized["title"].(string); strings.TrimSpace(title) == "" {
			cwd, _ := normalized["cwd"].(string)
			if cwd == "" {
				cwd = CurrentDirectory()
			}
			if discovered := DiscoverTitle(agent, cwd, sid); discovered != "" {
				normalized["title"] = discovered
			}
		}
		EmitHookStatus(normalized)
		RecordResumeTarget(agent, payload)
		if RecordConversationOverlay != nil && strings.TrimSpace(sid) != "" {
			prof, _ := normalized["profile"].(string)
			if strings.TrimSpace(prof) == "" {
				prof = strings.TrimSpace(os.Getenv("HARNESS_PROFILE"))
			}
			cwdStr, _ := normalized["cwd"].(string)
			if strings.TrimSpace(cwdStr) == "" {
				cwdStr = CurrentDirectory()
			}
			titleStr, _ := normalized["title"].(string)
			modelStr, _ := normalized["model"].(string)
			outcome := ""
			if ev == "stop" || ev == "stop_failure" {
				if st, _ := normalized["state"].(string); st == "done" || st == "failed" {
					outcome = st
				}
			}
			RecordConversationOverlay(map[string]any{
				"agent":         agent,
				"session_id":    strings.TrimSpace(sid),
				"profile":       strings.TrimSpace(prof),
				"space":         session,
				"cwd":           cwdStr,
				"title":         strings.TrimSpace(titleStr),
				"model":         strings.TrimSpace(modelStr),
				"last_outcome":  outcome,
				"updated_at_ms": NowFunc().UnixMilli(),
			})
		}
		if ev == "stop" {
			if st, _ := normalized["state"].(string); st == "done" {
				UnsetTmuxPaneOption("", "@spaces_prior_outcome")
			}
		}
	} else {
		EmitHookStatus(normalized)
	}
	if transport {
		envelope := map[string]any{
			"session": session, "pane": pane,
			"agent": agent, "event": event,
			"payload": payload, "normalized": normalized,
		}
		published := false
		if Publish != nil {
			published = Publish(envelope)
		}
		if !published {
			if AppendSpool != nil {
				entry := map[string]any{"session": session, "pane": pane}
				for k, v := range normalized {
					entry[k] = v
				}
				_ = AppendSpool(entry)
			}
			if EnsureServer != nil {
				_ = EnsureServer(helperPath())
			}
		}
	}
	return 0
}

// Hooks is the `zj hooks [status|start|stop|restart|serve]` entry.
func Hooks(args []string) int {
	if HooksStatus == nil {
		errf("zj hooks: hook core unavailable\n")
		return 2
	}
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "status":
		return HooksStatus()
	case "stop":
		if HooksStop == nil {
			errf("zj hooks: hook core unavailable\n")
			return 2
		}
		return HooksStop()
	case "start", "restart":
		if HooksStart == nil {
			errf("zj hooks: hook core unavailable\n")
			return 2
		}
		return HooksStart(helperPath())
	case "serve":
		if Serve == nil {
			errf("zj hooks: hook core unavailable\n")
			return 2
		}
		return Serve(true, nil)
	default:
		errf("usage: zj hooks [status|start|stop|restart|serve]\n")
		return 2
	}
}

// HookServer is the `zj hook-server [--daemon] [--tcp PORT]` entry.
func HookServer(args []string) int {
	if Daemonize == nil && Serve == nil {
		return 2
	}
	daemon := false
	var tcpPort *int
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--daemon":
			daemon = true
		case "--foreground":
			// Default; accepted for the CLI contract.
		case "--tcp":
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil {
					tcpPort = &n
				}
				i++
			}
		default:
			if n, err := strconv.Atoi(args[i]); err == nil && tcpPort == nil {
				tcpPort = &n
			}
		}
	}
	if daemon {
		if Daemonize == nil {
			return 2
		}
		return Daemonize()
	}
	if Serve == nil {
		return 2
	}
	return Serve(true, tcpPort)
}
