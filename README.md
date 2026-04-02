# Gobbler

Gobbler is a local MCP gateway written in Go. It sits between your coding harness (Claude Code, ForgeCode, Codex) and your MCP servers, replacing their full tool schemas with 4 compressed wrapper tools. The harness never sees the upstream servers directly. Gobbler handles discovery, orchestration, and response shielding internally.

I built this because MCP tool schemas are expensive. The GitHub MCP server alone exposes 40+ tools. Each tool definition, with its JSON Schema and description, gets injected into every prompt. Cloudflare measured this problem and found that collapsing tool surfaces cut their token usage by 81%. Gobbler applies the same principle locally without requiring Cloudflare Workers, TypeScript, or V8 isolates.

## What it actually does

Gobbler exposes exactly 4 tools to the harness:

`search_tools` takes a query string and returns compressed capability summaries from all registered upstream servers. The harness uses this to discover what's available without loading full JSON Schemas.

`execute_plan` takes a JSON plan with ordered steps. Each step names a server, tool, and arguments. Gobbler executes the steps internally, stores intermediate results in memory, and returns only the final output after applying size limits. The harness never sees the raw intermediate payloads. Step arguments can reference previous results using `${stepId.field}` syntax.

`call_raw` is the escape hatch. It calls a single upstream tool and returns the result, still subject to output shielding.

`inspect_tool` returns the full parameter schema for a specific tool when the model needs more detail before building a plan.

## Response shielding

This is the part that matters beyond schema compression. When an upstream tool returns a 500KB JSON blob, Gobbler does not forward it to the harness. The output policy enforcer truncates arrays to 50 elements, strings to 8192 characters, and total output to 64KB. Field filtering lets the model request only specific keys from a result. These limits are configurable in the policy config.

The mechanism is straightforward. Every `execute_plan` invocation writes each step's raw result into an in-memory `resultstore.Store`. Only the final step's output, after policy enforcement, crosses the boundary back to the harness. The intermediate data stays inside the Go process.

## The compiler

The capability compiler is where token savings actually happen. For each upstream server, Gobbler connects via MCP, calls `tools/list`, and builds a `CapabilityIndex`. Each tool becomes a `Capability` struct: server name, tool name, a 120-character summary, inferred tags, risk level, and a short input shape description.

Risk classification uses word-level matching against tool names and descriptions. Words like `delete`, `remove`, and `destroy` produce a "dangerous" classification. Words like `create`, `update`, and `push` produce "write". Everything else defaults to "read". MCP's default annotations (which mark every tool as `DestructiveHint: true`) are ignored because they carry no signal.

The compiler estimates token savings using a 4-characters-per-token heuristic. In tests with 4 GitHub-style tools, raw schema JSON consumed ~377 tokens. The compiled capability index for the same tools consumed ~266 tokens. The real savings scale with tool count because the wrapper's 4-tool surface stays fixed regardless of how many upstream tools exist.

## Harness adapters

Gobbler supports 3 harnesses through a shared `Adapter` interface:

**ForgeCode** reads and writes `.mcp.json` at user (`~/.forge/.mcp.json`) and local (cwd) scopes. The adapter detects the `forge` binary or config directory, injects a single `gobbler` entry pointing to `gobbler serve`, and calls `forge mcp reload` after installation.

**Claude Code** uses `.mcp.json` at the project root. The adapter detects the `claude` binary or `~/.claude.json`. Claude Code has no reload command, so the user restarts their session after installation.

**Codex** uses the same `.mcp.json` format for compatibility. The adapter detects the `codex` binary or `~/.codex` directory.

Each adapter backs up the existing config before modification. `gobbler rollback --harness <name>` restores from the latest backup.

## Installation and usage

```
go install github.com/robinwhite/gobbler/cmd/gobbler@latest
```

Register an upstream MCP server:

```
gobbler mcp add --transport stdio github-mcp npx -y @modelcontextprotocol/server-github
gobbler mcp add --transport http figma https://mcp.figma.com/mcp
```

Build the capability index by connecting to the server and introspecting its tools:

```
gobbler wrap github-mcp
```

Install gobbler into your harness:

```
gobbler install --harness claude
gobbler install --harness forge
gobbler install --harness codex
```

The harness now sees only gobbler's 4 tools. All upstream tool calls route through `gobbler serve`, which the harness launches via stdio.

Verify everything works:

```
gobbler doctor
```

Roll back if needed:

```
gobbler rollback --harness claude
```

## Project structure

The codebase is 3,751 lines of Go across 27 files. No TypeScript, no containers, no cloud dependencies.

```
cmd/gobbler/           CLI entrypoint
internal/cli/          7 command files wired with cobra
internal/harness/      Adapter interface + forge/claude/codex implementations
internal/mcpclient/    MCP client wrapping mark3labs/mcp-go (stdio + HTTP)
internal/compiler/     Tool schema → capability index compiler
internal/wrapper/      The MCP wrapper server exposing 4 tools
internal/executor/     Multi-step plan executor with step references
internal/policy/       Output shielding: size limits, field filtering, truncation
internal/resultstore/  In-memory store for intermediate step results
internal/backup/       Timestamped config backup and restore
internal/registry/     Server registry persisted to ~/.config/gobbler/servers.json
internal/logging/      Stderr-only logger (stdout is reserved for MCP JSON-RPC)
pkg/config/            Config types and JSON file I/O
pkg/protocol/          MCP protocol types and tool schema parsing
```

## Dependencies

Two direct dependencies. `mark3labs/mcp-go` v0.46.0 handles MCP protocol plumbing: stdio transport, JSON-RPC framing, tool registration, and the server lifecycle. `spf13/cobra` v1.10.2 handles CLI parsing. Everything else is transitive.

## Tests

16 tests across 4 packages. The compiler tests verify tool compilation, capability search, index merging, tag inference, and risk classification. The policy tests verify output shielding for strings, arrays, JSON field filtering, and tool blocking. The registry tests verify CRUD operations and file persistence. The resultstore tests verify field extraction including array indexing.

```
go test ./...
```

## What this does not do

Gobbler does not run arbitrary model-generated code. The V1 execution model is structured plans: the model submits JSON describing which tools to call with which arguments, and gobbler executes them. There is no eval, no sandbox runtime, no embedded JS engine. That is a deliberate scope decision. Structured execution captures most of the token savings while avoiding the security surface of code execution.

Gobbler does not replace MCP servers. It proxies them. The upstream servers still run, still handle auth, still do the real work. Gobbler compresses what the harness sees and shields what it receives.

## What comes next

Milestone 2 adds mutating-tool approval gates and richer capability clustering. Milestone 3 adds an optional sandboxed script mode with a child process runtime and parent-process RPC bindings for upstream tool calls. Milestone 4 adds team policies and shared capability manifests.
