# CLI Proxy API

[English](README.md) | [中文](README_CN.md) | 日本語

デスクトップで CLIProxyAPI を利用したい場合は、[EasyCLIProxyAPI](https://github.com/router-for-me/EasyCLIProxyAPI) デスクトップクライアントをおすすめします。グラフィカルな設定画面、自動更新、システムトレイ連携、CLIProxyAPI サービスのワンクリック起動/停止などの機能を提供します。

CLIProxyAPI は、CLI向けのOpenAI/Gemini/Claude/Codex/Grok互換APIインターフェースを提供するプロキシサーバーです。

ローカル環境や複数のCLIアカウントを通じて、OpenAI（Responses含む）、Gemini（Interactions含む）、またはClaude互換のクライアントやSDKから、以下のプロバイダーにアクセスできます。

<table>
<tbody>
    <tr>
        <th align="center" width="100">プロバイダー</th>
        <th align="center">説明</th>
    </tr>
    <tr>
        <td align="center"><a href="https://www.kimi.com/code/?aff=cliproxyapi"><img src="./assets/logo/kimi.svg" alt="Kimi" width="28" height="28" /></a></td>
        <td>Kimiシリーズモデル（Kimi K3、K2.8 Previewなど）。<a href="https://platform.kimi.ai/docs/guide/kimi-k3-quickstart">Kimi K3</a>は、Moonshot AIで最も高性能なモデルであり、世界初のオープンな3兆パラメータ級モデルです。2.8兆のパラメータ、ネイティブな視覚機能、100万トークンのコンテキストウィンドウを備え、長期間にわたるコーディング、知識作業、推論向けに構築されています。CLIProxyAPIはOAuthまたは互換APIインターフェース経由でKimiをサポートします。<strong>Kimi Code プラン</strong>（<a href="https://www.kimi.com/code?aff=cliproxyapi">中文站</a> | <a href="https://www.kimi.ai/code?aff=cliproxyapi">Global</a>）を試すか、Kimi Open Platform（<a href="https://platform.kimi.com?track_id=track-f15622e7182046baa22ca35e006e13a7&aff=cliproxyapi">中文站</a> | <a href="https://platform.kimi.ai?track_id=track-8a28e4b291d84f62af2fccc3e7a21cb3&aff=cliproxyapi">Global</a>）で<strong>APIキー</strong>を取得してください。CLIProxyAPIとオープンソースコミュニティを支援してくださるKimiに感謝します！</td>
    </tr>
    <tr>
        <td align="center"><a href="https://developers.openai.com/api/docs/models"><img src="./assets/logo/openai.svg" alt="OpenAI" width="28" height="28" /></a></td>
        <td>OpenAI GPT-6シリーズモデル（GPT-6 Astra、GPT-6.1 Sol、GPT-6 Luna）。Codex OAuth経由でも利用できます。Astraは複雑な推論とコーディング、Solは性能とコストのバランス、Lunaは大量の明確なタスクに適しています。</td>
    </tr>
    <tr>
        <td align="center"><a href="https://www.anthropic.com/claude"><img src="./assets/logo/claude.svg" alt="Anthropic" width="28" height="28" /></a></td>
        <td>Anthropic Claudeシリーズモデル（Claude Fable 5.1、Claude Opus 5.5、Claude Sonnet 5.5）。Fable 5.1は長期のコーディングや知識作業向けで、Opus 5.5は低いコストで高度なエージェント型コーディングに対応します。</td>
    </tr>
    <tr>
        <td align="center"><a href="https://antigravity.google/"><img src="./assets/logo/antigravity.svg" alt="Antigravity" width="28" height="28" /></a></td>
        <td>Google GeminiシリーズにはGemini 3.8 FlashやGemini 3.1 Pro Previewがあります。CLIProxyAPIはGemini API、AI Studio、Vertex AI、Gemini CLI、Antigravityのアカウントに対応し、利用できるモデルは経路によって異なります。Gemini 3.8 Flashは、長期のソフトウェア開発やエージェントのワークフロー向けのGoogleの最新Flashモデルです。</td>
    </tr>
    <tr>
        <td align="center"><a href="https://docs.x.ai/developers/grok-4-7"><img src="./assets/logo/xai.svg" alt="xAI" width="28" height="28" /></a></td>
        <td>xAI Grokシリーズモデル（Grok 4.7、Grok 4.7 Build Fastなど）。Grok 4.7は、コーディング、エージェントタスク、知識作業向けのSpaceXAIの最新モデルです。</td>
    </tr>
    <tr>
        <td align="center"><a href="https://dev.meta.ai/docs/overview"><img src="./assets/logo/meta.svg" alt="Meta" width="28" height="28" /></a></td>
        <td>Meta Museシリーズモデル（Muse Spark 1.3、Muse Spark 1.2など）。CLIProxyAPIはMetaログインによるMuse CodeアカウントとMeta Model APIキーに対応し、Muse Spark 1.3をコーディングやエージェントのワークフローに利用できます。</td>
    </tr>
    <tr>
        <td align="center"><a href="https://devin.ai/cli">Devin</a></td>
        <td>Devinのモデル（SWE-2、GPT-6 Astra、Claude Fable 5.1など）。<code>--devin-login</code>でDevinアカウントに接続し、そのアカウントで利用可能なモデルにリクエストを送れます。</td>
    </tr>
</tbody>
</table>

## 概要

- CLIモデル向けのOpenAI/Gemini/Claude/Grok互換APIエンドポイント
- OAuthログインによるOpenAI Codexサポート（GPTモデル）
- OAuthログインによるClaude Codeサポート
- OAuthログインによるGrok Buildサポート
- ストリーミング、非ストリーミング、および対応環境でのWebSocketレスポンス
- 関数呼び出し/ツールのサポート
- マルチモーダル入力サポート（テキストと画像）
- ラウンドロビン負荷分散による複数アカウント対応（Gemini、OpenAI、Claude、Grok）
- シンプルなCLI認証フロー（Gemini、OpenAI、Claude、Grok）
- Generative Language APIキーのサポート
- AI Studioビルドのマルチアカウント負荷分散
- Claude Codeのマルチアカウント負荷分散
- OpenAI Codexのマルチアカウント負荷分散
- Grok Buildのマルチアカウント負荷分散
- 設定によるOpenAI互換アップストリームプロバイダー（例：OpenRouter）
- プロキシ埋め込み用の再利用可能なGo SDK（`docs/sdk-usage.md`を参照）

## はじめに

CLIProxyAPIガイド：[https://help.router-for.me/](https://help.router-for.me/)

## 管理API

[MANAGEMENT_API.md](https://help.router-for.me/management/api)を参照

## 使用量統計

v6.10.0以降、CLIProxyAPIおよび [CPAMC](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) プロジェクトには使用量統計機能がプリセットされなくなりました。使用量統計が必要な場合は、次のプロジェクトをご利用ください：

### [CPA Usage Keeper](https://github.com/Willxup/cpa-usage-keeper)

CLIProxyAPI向けの独立した使用量永続化・可視化サービス。CLIProxyAPIデータを定期同期してSQLiteに保存し、集計APIと、使用量や各種統計を確認できる組み込みダッシュボードを提供します。

### [CPA-Manager-Plus](https://github.com/seakee/CPA-Manager-Plus)

リクエスト単位の監視とコスト推定を備えたCLIProxyAPI向けのフル管理センターです。CPA-Managerは、収集したリクエストをアカウント、モデル、チャネル、レイテンシ、ステータス、Token使用量ごとに追跡し、編集可能なモデル価格とLiteLLM価格のワンクリック同期でコストを推定します。SQLiteでイベントを永続化し、Codexアカウントプール向けに一括検査、クォータ判定、異常アカウント検出、クリーンアップ提案、ワンクリック実行を提供し、日常的なマルチアカウント運用に適しています。

### [Oh-My-CPA](https://github.com/WizisCool/oh-my-cpa)

Ant Design を採用した CLIProxyAPI v8+ 向けのモダンな管理コンソール。CPAMC に相当する主要な管理機能と、SQLite に永続化するリクエスト記録・使用量分析を統合し、OAuth アカウント、API プロバイダー、クライアントキー、クォータ、料金設定、CPA 運用を一つの画面で管理できます。リクエストごとのレイテンシ、最初のトークンまでの時間（TTFT）、トークン数、コストを追跡し、多条件フィルター、リアルタイムダッシュボード、トークンヒートマップを提供します。OpenRouter の価格同期、カスタム料金、リクエストごとの価格スナップショットで過去のコストを保持し、組み込み Agent と MCP ツールで分析と日常管理を支援します。

## SDKドキュメント

- 使い方：[docs/sdk-usage.md](docs/sdk-usage.md)
- 上級（エグゼキューターとトランスレーター）：[docs/sdk-advanced.md](docs/sdk-advanced.md)
- アクセス：[docs/sdk-access.md](docs/sdk-access.md)
- ウォッチャー：[docs/sdk-watcher.md](docs/sdk-watcher.md)
- カスタムプロバイダーの例：`examples/custom-provider`

## コントリビューション

コントリビューションを歓迎します！お気軽にPull Requestを送ってください。

1. リポジトリをフォーク
2. フィーチャーブランチを作成（`git checkout -b feature/amazing-feature`）
3. 変更をコミット（`git commit -m 'Add some amazing feature'`）
4. ブランチにプッシュ（`git push origin feature/amazing-feature`）
5. Pull Requestを作成

## オリジナルプロジェクト

本プロジェクトは [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) のフォークです。CLIProxyAPI をベースにした、またはそれに触発されたプロジェクトについては、オリジナルプロジェクトの README を参照してください。

## ライセンス

本プロジェクトはMITライセンスの下でライセンスされています - 詳細は[LICENSE](LICENSE)ファイルを参照してください。
