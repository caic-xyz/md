# Development environment and tool integration guide for the md container.
---
name: md-container-environment
description: Development environment and tool integration guide for the md container. Reference for Chrome DevTools MCP, Node.js, virtualization, and other system tools. See ~/AGENTS.md for complete tool list.
---

# Container Environment & Tools

Complete environment documentation for the md development container. For the authoritative list of all installed tools and versions, see `~/AGENTS.md`.

## Browser Automation & Web Debugging

**When to use Chrome DevTools MCP**:
- You need to verify code changes work in a real browser
- Debugging layout, CSS, or JavaScript issues in a frontend webapp
- Testing form submissions or user interactions
- Analyzing network requests or API responses
- Checking page performance (Core Web Vitals, LCP, etc.)
- Extracting content or data from web pages
- Automating multi-step workflows on websites

**Google Chrome / Chromium Browser**:
- **amd64**: Google Chrome (latest stable) via extrepo - `/usr/bin/google-chrome`
- **arm64**: Chromium as fallback - `/usr/bin/chromium`
- Both configured to skip startup dialogs (OOBE disabled)

**Chrome DevTools MCP**: Official Google MCP server for browser automation and debugging
- Installed globally via npm (`chrome-devtools-mcp` package)
- **Configure in your MCP client with**:
  ```json
  {
    "mcpServers": {
      "chrome-devtools": {
        "command": "npx",
        "args": ["chrome-devtools-mcp"]
      }
    }
  }
  ```
- **Provides tools for**:
  - Screenshots (full-page, element-level, custom dimensions)
  - Page navigation and tab management
  - Network monitoring and request inspection
  - Performance tracing and Core Web Vitals analysis
  - Form filling and element interaction
  - DOM/CSS inspection
  - JavaScript execution in page context
  - Accessibility tree inspection

- **Example use cases**:
  - "Verify the changes I made are working in the browser"
  - "Check why the layout looks wrong on the deployed site"
  - "What API endpoints is this app calling?"
  - "Take a screenshot of localhost:3000 to see the current state"
  - "Find all clickable elements on this page"
  - "Is the performance acceptable? Check the LCP metric"

## Development Languages

**Node.js**: v24 (pinned for Amp compatibility; managed via nvm)
- Global packages: pnpm, npm, typescript, eslint, tsx
- MCP servers: chrome-devtools-mcp
- AI tools: @google/gemini-cli, @openai/codex, @qwen-code/qwen-code

**Python**: python3 with uv package manager and quality tools (pylint, ruff)

**Go, Rust, Java**: Latest versions with standard toolchains

**Bun**: JavaScript runtime alternative to Node.js

## Key Tools & Workflows

### Editor
**Neovim** is the primary editor. Full vim compatibility with modern extensions.

### Code Quality
- **Shell**: shellcheck (validate), shfmt (format)
- **Python**: pylint, ruff
- **TypeScript**: eslint, prettier
- **General**: actionlint

### Virtualization
- **KVM/QEMU**: VM with x86 and ARM emulation
- **Libvirt**: VM management

### Media Processing
- **FFmpeg**: Video/audio encoding and conversion
- **ImageMagick**: Image manipulation (magick command)

### Build Systems
- **make, cmake, gcc, g++**: C/C++ compilation
- **gradle**: Java/Android builds
- **cargo**: Rust package building

### System Inspection & Debugging
- **strace**: System call tracing (requires SYS_PTRACE capability, enabled by default)
- **lsof**: Open file inspection
- **dlv**: Go debugger (delve) - goroutine-aware, can attach to running processes
- **lldb / rust-lldb**: LLVM debugger with Rust pretty-printing support

## Version Information

View all installed tool versions with:
```bash
cat ~/src/tool_versions.md
```

## Quick Reference

Reference `~/AGENTS.md` for:
- Complete categorized tool list
- Core utilities, compression, development tools
- Android SDK details
- Database and network utilities

See this skill for:
- Chrome DevTools MCP integration and usage
- Quick command examples
- Development workflow guidance

## Quick Commands

```bash
# List Node.js global packages
pnpm list -g

# Check versions
go version && rustc --version && python3 --version && node --version

# Google Chrome with debugging
google-chrome --remote-debugging-port=9222

# View all tool versions
cat ~/src/tool_versions.md

# Access container GUI (via VNC)
# Port 5901 with TigerVNC client
```

## Environment Details

- **Shell**: bash with modern Unix utilities
- **Desktop**: XFCE4 with TigerVNC on port 5901
- **Display**: X11 support for GUI applications
- **Build Cache**: Docker layer caching for faster rebuilds
