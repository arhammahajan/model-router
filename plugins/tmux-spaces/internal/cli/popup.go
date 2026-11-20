package cli

// cmdPopup opens the Navigator TUI. The TUI itself is wired by
// cmd/tmux-spaces through Env.RunPopup; without it the command reports
// that instead of failing obscurely.
func cmdPopup(args []string, env Env) int {
	f, err := parseFlags(args, []string{"query", "target"}, []string{"new", "rename-window", "rename-space"}, nil)
	if err != nil {
		if err == errHelp {
			env.outf("usage: tmux-spaces popup [--new] [--rename-window] [--rename-space] [--query Q] [--target SPACE]\n")
			return ExitOK
		}
		env.errf("tmux-spaces popup: %s\n", err)
		return ExitUsage
	}
	if len(f.pos) > 0 {
		env.errf("usage: tmux-spaces popup [--new] [--rename-window] [--rename-space] [--query Q] [--target SPACE]\n")
		return ExitUsage
	}
	if env.RunPopup == nil {
		env.errf("tmux-spaces popup: TUI not wired (cmd/tmux-spaces must provide RunPopup)\n")
		return ExitError
	}
	return env.RunPopup(env, args)
}

// cmdAgentsCMD opens the Agents selector TUI, wired the same way as popup.
func cmdAgentsCMD(args []string, env Env) int {
	f, err := parseFlags(args, nil, nil, nil)
	if err != nil {
		if err == errHelp {
			env.outf("usage: tmux-spaces agents\n")
			return ExitOK
		}
		env.errf("tmux-spaces agents: %s\n", err)
		return ExitUsage
	}
	if len(f.pos) > 0 {
		env.errf("usage: tmux-spaces agents\n")
		return ExitUsage
	}
	if env.RunAgents == nil {
		env.errf("tmux-spaces agents: TUI not wired (cmd/tmux-spaces must provide RunAgents)\n")
		return ExitError
	}
	return env.RunAgents(env, args)
}

// cmdInbox opens the Attention Inbox TUI, showing only agents across all
// spaces with a compact live viewport and status.
func cmdInbox(args []string, env Env) int {
	f, err := parseFlags(args, nil, nil, nil)
	if err != nil {
		if err == errHelp {
			env.outf("usage: tmux-spaces inbox\n")
			return ExitOK
		}
		env.errf("tmux-spaces inbox: %s\n", err)
		return ExitUsage
	}
	if len(f.pos) > 0 {
		env.errf("usage: tmux-spaces inbox\n")
		return ExitUsage
	}
	if env.RunInbox == nil {
		env.errf("tmux-spaces inbox: TUI not wired (cmd/tmux-spaces must provide RunInbox)\n")
		return ExitError
	}
	return env.RunInbox(env, args)
}

// cmdConversations opens the unified Conversations search/resume TUI.
func cmdConversations(args []string, env Env) int {
	f, err := parseFlags(args, []string{"query"}, nil, nil)
	if err != nil {
		if err == errHelp {
			env.outf("usage: tmux-spaces conversations [--query Q]\n")
			return ExitOK
		}
		env.errf("tmux-spaces conversations: %s\n", err)
		return ExitUsage
	}
	if len(f.pos) > 0 {
		env.errf("usage: tmux-spaces conversations [--query Q]\n")
		return ExitUsage
	}
	if env.RunConversations == nil {
		env.errf("tmux-spaces conversations: TUI not wired (cmd/tmux-spaces must provide RunConversations)\n")
		return ExitError
	}
	return env.RunConversations(env, args)
}
