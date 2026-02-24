# shellcheck shell=bash
# Source: https://github.com/caic-xyz/md

alias amp="$(command -v amp 2>/dev/null || echo amp) --dangerously-allow-all"
alias claude="$(command -v claude 2>/dev/null || echo claude) --dangerously-skip-permissions --allow-dangerously-skip-permissions --permission-mode dontAsk"
alias codex="$(command -v codex 2>/dev/null || echo codex) --dangerously-bypass-approvals-and-sandbox"
alias gemini="$(command -v gemini 2>/dev/null || echo gemini) --yolo"
alias qwen="$(command -v qwen 2>/dev/null || echo qwen) --yolo"
