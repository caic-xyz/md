# Coding harnesses

The container includes `/home/user/src/AGENTS.md` which provides information about
preinstalled tools (list available in `~/src/tool_versions.md`).

Harnesses preinstalled:

- [amp](https://ampcode.com/manual#AGENTS.md): `~/.config/amp/AGENTS.md`
- [claude](https://www.anthropic.com/engineering/claude-code-best-practices): `~/.claude/CLAUDE.md`
- [codex](https://developers.openai.com/codex/guides/agents-md/): `~/.codex/AGENTS.md`
- [gemini](https://geminicli.com/docs/cli/gemini-md/): `~/.gemini/GEMINI.md`
    - Recommended in `~/.qwen/settings.json` to change `"context"` / `"fileName"` to `AGENTS.md`
- [kilo](https://kilo.ai/docs/agent-behavior/custom-rules): `~/.kilocode/rules/*.md`
- [opencode](https://opencode.ai/docs/rules/): `~/.config/opencode/AGENTS.md`
- [pi](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/README.md): `~/.pi/agent/AGENTS.md`
- [qwen](https://qwenlm.github.io/qwen-code-docs/en/users/configuration/settings/#example-context-file-content-eg-qwenmd): `~/.qwen/QWEN.md`
    - Recommended in `~/.qwen/settings.json` to change `"context"` / `"fileName"` to `AGENTS.md`

## Claude Code

Here's a recommended `~/.claude/settings.json`. In particular, ensure to enable YOLO mode:

```
{
  "alwaysThinkingEnabled": true,
  "enabledPlugins": {
  },
  "mcpServers": {
  },
  "permissions": {
    "defaultMode": "dontAsk"
  },
  "skipDangerousModePermissionPrompt": true
}
```

## Readme for agents (https://agents.md) and Skills (https://agentskills.io)

Here's locations of AGENTS.md for each harness:

```bash
mkdir -p ~/.config/agents ~/.config/amp ~/.claude ~/.codex ~/.gemini ~/.kilocode/rules ~/.config/opencode ~/.pi/agent ~/.qwen
echo "Read ~/AGENTS.md if present." >> ~/.config/agents/AGENTS.md
ln -s ../../.config/agents/AGENTS.md ~/.config/amp/AGENTS.md
ln -s ../.config/agents/AGENTS.md ~/.claude/CLAUDE.md
ln -s ../.config/agents/AGENTS.md ~/.codex/AGENTS.md
ln -s ../.config/agents/AGENTS.md ~/.gemini/AGENTS.md
ln -s ../../.config/agents/AGENTS.md ~/.kilocode/rules/AGENTS.md
ln -s ../../.config/agents/AGENTS.md ~/.config/opencode/AGENTS.md
ln -s ../../.config/agents/AGENTS.md ~/.pi/agent/AGENTS.md
ln -s ../.config/agents/AGENTS.md ~/.qwen/AGENTS.md
```

Here's locations of skills for each harness:

- [amp](https://ampcode.com/manual#agent-skills): `~/.config/agents/skills/**/SKILL.md` (recursive)
    - Fallbacks to `~/.claude/skills/`
- [antigravity](https://antigravity.google/docs/skills): `~/.gemini/antigravity/skills/<name>/SKILL.md`
- [claude](https://code.claude.com/docs/en/skills): `~/.claude/skills/<name>/SKILL.md`
- [codex](https://developers.openai.com/codex/skills): `~/.codex/skills/**/SKILL.md` (recursive)
- [cursor](https://cursor.com/docs/context/skills): `~/.cursor/skills/<name>/SKILL.md`
    - Fallbacks to `~/.claude/skills/`
- [gemini](https://geminicli.com/docs/cli/skills/): `~/.gemini/skills/<name>/SKILL.md`
- [kilo](https://kilo.ai/docs/agent-behavior/skills): `~/.kilocode/skills/<name>/SKILL.md`
- [opencode](https://opencode.ai/docs/skills/): `~/.config/opencode/skill/<name>/SKILL.md`
    - Fallbacks to `~/.claude/skills/`
- [pi](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/README.md#skills): `~/.pi/agent/skills/**/SKILL.md` (recursive)
    - Fallbacks to `~/.claude/skills/`, `~/.codex/skills/` (recursive)
- [qwen](https://qwenlm.github.io/qwen-code-docs/en/users/features/skills/): `~/.qwen/skills/<name>/SKILL.md`

Centralize your skills with symlinks:

```bash
mkdir -p ~/.config/agents/skills ~/.claude ~/.codex ~/.cursor ~/.gemini/antigravity ~/.kilocode ~/.config/opencode ~/.pi/agent ~/.qwen
ln -s ../.config/agents/skills/ ~/.claude/skills
ln -s ../.config/agents/skills/ ~/.codex/skills
ln -s ../.config/agents/skills/ ~/.cursor/skills
ln -s ../../.config/agents/skills/ ~/.gemini/antigravity/skills
ln -s ../.config/agents/skills/ ~/.gemini/skills
ln -s ../.config/agents/skills/ ~/.kilocode/skills
ln -s ../../.config/agents/skills/ ~/.config/opencode/skill
ln -s ../../.config/agents/skills/ ~/.pi/agent/skills
ln -s ../.config/agents/skills/ ~/.qwen/skills
```

