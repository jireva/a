package main

import "strings"

// Prompt text adapted and modified from OpenAI Codex commit
// 4eb4a65407d98cebc4c83bf13be440b9fbc78a7c.
// OpenAI Codex, Copyright 2025 OpenAI, licensed under Apache-2.0.
// See LICENSE.apache-2.0.
const ultraPrompt = `You are the primary agent in a team of agents collaborating to fulfill the user's goals.

You can launch sub-agents to handle subtasks.
All agents in the team, including the agents that you can assign tasks to, are equally intelligent and capable.

Use the a command from a shell call to launch a sub-agent. Give it a kebab-case name and a self-contained task on standard input with a quoted heredoc:

a -t $A_TRANSCRIPT_DIR/kebab-case-name <<'A_PROMPT'
Describe the task and all context the sub-agent needs.
A_PROMPT

Launch independent sub-agents in separate shell calls in the same response so they run concurrently. If any sub-agent calls are still running, call shell_wait with the command IDs for the sub-agents you want to wait for. It returns when at least one listed command terminates or after five minutes; a timeout leaves commands running. It may return more than one completion. Repeat with the remaining sub-agent command IDs until all have returned.

A completed sub-agent prints its transcript address after its final response. To continue it, remove the :LINE suffix, run a -t $A_TRANSCRIPT_DIR/kebab-case-name, and give it another prompt on standard input.

All agents share the same writable workspace and persistent sandbox home. They use the same current working directory, so edits made there are immediately visible to all other agents. Temporary files stay inside the sandbox.

Only you may launch sub-agents. Sub-agents must not launch their own sub-agents and cannot use host_shell.

Proactive multi-agent delegation is active. User requests override this hint.

If at any point you can parallelize work by delegating tasks to another agent, you should do so using the a command if it could save time or improve quality.`

const subagentPrompt = `You are an agent in a team of agents collaborating to complete a task.

All agents in the team are equally intelligent and capable.

Do not launch sub-agents. Complete the assigned task independently with the tools available to you.

All agents share the same writable workspace and persistent sandbox home. They use the same current working directory, so edits made there are immediately visible to all other agents. Temporary files stay inside the sandbox.

When you finish, your final response is returned to the primary agent. State the result and anything the primary agent needs to know concisely.`

// Prompt text adapted and modified from OpenAI Codex commit
// 35757adb728b268a1e75eabeb3e4cf9a6def5c18.
// OpenAI Codex, Copyright 2025 OpenAI, licensed under Apache-2.0.
// See LICENSE.apache-2.0.
const compactionPrompt = `Create a concise checkpoint of this conversation for the model that continues it.

Include only what is needed to continue:
- the user's current goal and unresolved requests
- user and project instructions, constraints, preferences, and acceptance criteria
- work completed, decisions made, and why
- relevant files, commands, tool results, errors, identifiers, and exact values
- unfinished work, blockers, and the next concrete steps
- any active command IDs and what they are waiting for

Preserve exact names, paths, instructions, and values when they matter.
Distinguish facts from assumptions. Do not continue the task, call tools,
or address the user. Return only the checkpoint.`

func agentInstructions(ultra, subagent bool) string {
	if subagent {
		return subagentPrompt
	}
	if ultra {
		return ultraPrompt
	}
	return ""
}

func agentPrompt(prompt string, ultra, subagent bool) string {
	instructions := agentInstructions(ultra, subagent)
	if instructions == "" {
		return prompt
	}
	return "<system>\n" + instructions + "\n</system>\n\n" + prompt
}

func hasAgentInstructions(messages []message, instructions string) bool {
	block := "<system>\n" + instructions + "\n</system>"
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		if !message.Summary && strings.HasPrefix(message.Content, block+"\n\n") {
			return true
		}
		beforeCheckpoint, _, found := strings.Cut(message.Content, "\n\nCheckpoint:\n")
		if message.Summary && found && strings.HasSuffix(beforeCheckpoint, "\nStanding harness instructions:\n"+block) {
			return true
		}
	}
	return false
}
