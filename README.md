# CLI Proxy API

English | [中文](README_CN.md) | [日本語](README_JA.md)

If you want to use CLIProxyAPI on your desktop, we recommend our [EasyCLIProxyAPI](https://github.com/router-for-me/EasyCLIProxyAPI) desktop client. It provides a graphical configuration UI, automatic updates, system tray integration, and one-click start/stop for the CLIProxyAPI service.

CLIProxyAPI is a proxy server that provides OpenAI/Gemini/Claude/Codex/Grok compatible API interfaces for CLI.

You can access the following providers locally and with multiple CLI accounts through any OpenAI (including Responses), Gemini (including Interactions), or Claude-compatible client or SDK.

<table>
<tbody>
    <tr>
        <th align="center" width="100">Provider</th>
        <th align="center">Description</th>
    </tr>
    <tr>
        <td align="center"><a href="https://www.kimi.com/code/?aff=cliproxyapi"><img src="./assets/logo/kimi.svg" alt="Kimi" width="28" height="28" /></a></td>
        <td>Kimi series models (Kimi K3, K2.8 Preview, etc.). <a href="https://platform.kimi.ai/docs/guide/kimi-k3-quickstart">Kimi K3</a> is Moonshot AI’s most capable model and the world’s first open 3T-class model. With 2.8 trillion parameters, native vision, and a 1-million-token context window, K3 is built for long-horizon coding, knowledge work, and reasoning. CLIProxyAPI supports Kimi through OAuth or compatible API interfaces. Try a <strong>Kimi Code plan</strong> (<a href="https://www.kimi.com/code?aff=cliproxyapi">中文站</a> | <a href="https://www.kimi.ai/code?aff=cliproxyapi">Global</a>), or get an <strong>API key</strong> from the Kimi Open Platform (<a href="https://platform.kimi.com?track_id=track-f15622e7182046baa22ca35e006e13a7&aff=cliproxyapi">中文站</a> | <a href="https://platform.kimi.ai?track_id=track-8a28e4b291d84f62af2fccc3e7a21cb3&aff=cliproxyapi">Global</a>). Thanks to Kimi for supporting CLIProxyAPI and the open-source community!</td>
    </tr>
    <tr>
        <td align="center"><a href="https://developers.openai.com/api/docs/models"><img src="./assets/logo/openai.svg" alt="OpenAI" width="28" height="28" /></a></td>
        <td>OpenAI GPT-6 models (GPT-6 Astra, GPT-6.1 Sol, GPT-6 Luna), including access through Codex OAuth. Astra is the flagship for complex reasoning and coding, Sol balances capability and cost, and Luna handles focused, high-volume work.</td>
    </tr>
    <tr>
        <td align="center"><a href="https://www.anthropic.com/claude/fable"><img src="./assets/logo/claude.svg" alt="Anthropic" width="28" height="28" /></a></td>
        <td>Anthropic Claude models (Claude Fable 5.1, Claude Opus 5.5, Claude Sonnet 5.5). Fable 5.1 is built for ambitious, long-running coding and knowledge work; Opus 5.5 brings strong agentic coding at a lower cost.</td>
    </tr>
    <tr>
        <td align="center"><a href="https://antigravity.google/"><img src="./assets/logo/antigravity.svg" alt="Antigravity" width="28" height="28" /></a></td>
        <td>Google Gemini models include Gemini 3.8 Flash and Gemini 3.1 Pro Preview. CLIProxyAPI supports Gemini API, AI Studio, Vertex AI, Gemini CLI, and Antigravity accounts; model availability varies by channel. Gemini 3.8 Flash is Google's latest Flash model for long-horizon software engineering and agentic workflows.</td>
    </tr>
    <tr>
        <td align="center"><a href="https://docs.x.ai/developers/grok-4-7"><img src="./assets/logo/xai.svg" alt="xAI" width="28" height="28" /></a></td>
        <td>xAI Grok models (Grok 4.7, Grok 4.7 Build Fast, etc.). Grok 4.7 is SpaceXAI's latest model for coding, agentic tasks, and knowledge work.</td>
    </tr>
    <tr>
        <td align="center"><a href="https://dev.meta.ai/docs/overview"><img src="./assets/logo/meta.svg" alt="Meta" width="28" height="28" /></a></td>
        <td>Meta Muse models (Muse Spark 1.3, Muse Spark 1.2, etc.). CLIProxyAPI supports Muse Code accounts through Meta login and Meta Model API keys, with Muse Spark 1.3 for coding and agentic workflows.</td>
    </tr>
    <tr>
        <td align="center"><a href="https://devin.ai/cli">Devin</a></td>
        <td>Devin models (SWE-2, GPT-6 Astra, Claude Fable 5.1, etc.). Connect a Devin account with <code>--devin-login</code> to route the models available to that account.</td>
    </tr>
</tbody>
</table>


## Overview

- OpenAI/Gemini/Claude/Grok compatible API endpoints for CLI models
- OpenAI Codex support (GPT models) via OAuth login
- Claude Code support via OAuth login
- Grok Build support via OAuth login
- Streaming, non-streaming, and WebSocket responses where supported
- Function calling/tools support
- Multimodal input support (text and images)
- Multiple accounts with round-robin load balancing (Gemini, OpenAI, Claude, Grok)
- Simple CLI authentication flows (Gemini, OpenAI, Claude, Grok)
- Generative Language API Key support
- AI Studio Build multi-account load balancing
- Claude Code multi-account load balancing
- OpenAI Codex multi-account load balancing
- Grok Build multi-account load balancing
- OpenAI-compatible upstream providers via config (e.g., OpenRouter)
- Reusable Go SDK for embedding the proxy (see `docs/sdk-usage.md`)

## Getting Started

CLIProxyAPI Guides: [https://help.router-for.me/](https://help.router-for.me/)

## Management API

see [MANAGEMENT_API.md](https://help.router-for.me/management/api)

## Usage Statistics

Since v6.10.0, CLIProxyAPI and [CPAMC](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) no longer ship built-in usage statistics. If you need usage statistics, use:

### [CPA Usage Keeper](https://github.com/Willxup/cpa-usage-keeper)

Standalone persistence and visualization service for CLIProxyAPI, with periodic data sync, SQLite storage, aggregate APIs, and a built-in dashboard for usage and statistics.

### [CPA-Manager-Plus](https://github.com/seakee/CPA-Manager-Plus)

Full CLIProxyAPI management center with request-level monitoring and cost estimates. CPA-Manager tracks collected requests by account, model, channel, latency, status, and token usage; estimates cost with editable model prices and one-click LiteLLM price sync; persists events in SQLite; and provides Codex account-pool operations with batch inspection, quota detection, unhealthy account discovery, cleanup suggestions, and one-click execution for day-to-day multi-account maintenance.

### [Oh-My-CPA](https://github.com/WizisCool/oh-my-cpa)

Modern Ant Design-based management console for CLIProxyAPI v8+, combining CPAMC-style administration with SQLite-backed request records and usage analytics. Covers OAuth accounts, API providers, client keys, quotas, pricing and CPA operations in one interface. Tracks request-level latency, TTFT, tokens and cost, with multi-dimensional filtering, live dashboards and token heatmaps. OpenRouter price sync, custom rates and per-request price snapshots keep historical costs stable; a built-in Agent and MCP tools support assisted investigation and management.

## SDK Docs

- Usage: [docs/sdk-usage.md](docs/sdk-usage.md)
- Advanced (executors & translators): [docs/sdk-advanced.md](docs/sdk-advanced.md)
- Access: [docs/sdk-access.md](docs/sdk-access.md)
- Watcher: [docs/sdk-watcher.md](docs/sdk-watcher.md)
- Custom Provider Example: `examples/custom-provider`

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## Original Project

This is a fork of [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). See its README for the projects built on or inspired by CLIProxyAPI.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
