# QuickRelay Studio v2.4.4

**QuickRelay Studio** 是一款基于 Go 与 WebView2 桌面端打造的独立双协议 AI 反代网关与账号池管理控制台。支持将多个 AWS Quick 账号聚合为标准的 **OpenAI** 与 **Anthropic Messages** API，免改造直连 **Claude Code**、**Codex CLI** 及各类开源 SDK，并内置完整的请求明细统计与 1M 上下文持久化会话追踪。

---

## 核心特性

1. **双协议原生网关**
   - **OpenAI 协议**：完整兼容 `POST /v1/chat/completions` 与 `GET /v1/models`。
   - **Anthropic Messages 协议**：原生支持 `POST /v1/messages`（含流式 SSE 与同步 JSON，完整支持 `type: "document"` PDF/文本文档附件直传）及本地 `POST /v1/messages/count_tokens` 估算。
   - **附件完整性与跨请求账号限流冷却（v2.4.3 新增 / v2.4.4 增强）**：
     - 严格校验单轮最多 16 个有效图片/二进制文档附件（纯文本文档内联不占配额）：超限、解码失败、不支持来源或上传/DLP 失败均明确返回协议错误，绝不静默丢弃或向模型派发残缺上下文；多轮长对话在升级或切号触发全量历史重放时，优先严格校验当前轮全部附件，并将剩余配额按从近到远分配给历史附件，超出配额或已失效的历史旧附件诚实标记省略（`[earlier image/document omitted]` / `[earlier image/document unavailable]`），既不虚标已附上也不再因历史累计超 16 张图锁死长会话（v2.4.4 增强）；
     - 明确识别上游 `429` / `Throttling` 限流与 `Retry-After` 并设置进程内跨请求冷却（默认 `45s`，上限 `5m`），新请求自动避开冷却账号并用完整历史接手，全员冷却时直接返回 `429` 并保留既有会话映射。
   - **深度思考 (`high` / `max` / `xhigh`) 与大工具流式加固（v2.4.0 ~ v2.4.2 持续增强）**：
     - 流式 `<client_tool_call>` 刚吐出工具名即实时闭合 `thinking`/`text` 并早启 `tool_use` / `tool_calls` 块，消除大工具参数写入期间长时间零帧输出导致的 180 秒假死断流；
     - 当请求携带 `max` / `xhigh` 思考强度时，自动将单轮总超时由常规 `request_timeout`（默认 `15m`）拉长至 `max_thinking_timeout`（默认 `30m`），上游 WebSocket 静默读超时由 `2m` 扩至 `5m`；
     - `high`/`max`/附件请求仅在 WebSocket 握手建连成功并已发送 `user_message` 进入等待模型响应阶段后，超过 `20s` 才触发首包 SSE 预热（`event: ping` / `: keepalive`），并将拨号握手超时收敛为 `12s`，彻底解除拨号与预热同在 `20s` 撞车导致的流式自动重试锁死（v2.4.2 修复）；
   - **Claude Code 专属优化**：
     - 直连 `/v1/messages` 免除额外适配开销；
     - `claude-haiku-*` 轻量模型自动分流至 `fast`，其余 `sonnet`/`opus` 走 `smart`；
     - 当请求携带 `AskUserQuestion` 工具时，自动将模型输出的 `<decision>` / `<decisions>` 交互标签转为标准原生 `tool_use` 卡片选项；
     - **全量 AWS Quick 私有标签静默清洗（v2.4.1 增强）**：在流式与非流式输出中自动清除 `<sources>` 来源引用块、`[@xxxxxx, @yyyyyy]` 多引用角标、`kg://`/`qw-file://`/`qw-tool://`/`mention://` 私有死链、`<artifact>` 工件占位及内部路由控制标记（同时 100% 保护代码块与行内反引号）。

2. **高稳定 1M 会话复用与续聊缓存瘦身（v2.3.0 ~ v2.4.3 重磅优化）**
   - **上轮助手输出指纹核对与双端同步失效防复活（v2.4.3 新增）**：同开场多窗口/多对话续聊时，额外校验上一轮最终对外输出的文本与规范化工具调用指纹，彻底防止误续到另一线程；单删、全清会话通过网关与 SQLite 双端同步失效，阻止在途旧请求写回复活；容量淘汰仅作用于空闲会话且排队支持 `context` 取消/超时退出。
   - **续聊临时故障原地重连保缓存与切号全量重建（v2.4.2 新增）**：续聊期间遇到网络抖动/超时先尝试在原绑定账号原地重连 1 次，尽力保住云端几十万 Token 缓存；若原账号连续失败或命中 429 限流需切换备号，自动切换为新建 `thread_id` 并完整重建历史消息与完整工具表，彻底杜绝将旧账号增量发给新账号；全网断连期间仍妥善保留原持久化会话绑定，待网络恢复后无损续聊。
   - **彻底消除续聊双倍重传与假死慢响应（v2.4.0 新增）**：续聊增量 `delta` 自动跳过云端既有会话里模型刚吐出的上轮 `Assistant` 万字正文与工具参数 JSON，仅发送带精简目标摘要的 `Tool result`，彻底告别每轮触发 `prompt-*.txt` 外置上传与双倍 Cache Write；按 `Esc` 取消、超长输出拦截或 WebSocket `close 1000` 正常收尾不再误清既有百万 Token 缓存。
   - **消除续聊每轮 `+20,000 Token` 重复工具膨胀**：同一 `thread_id` 首轮下发完整工具表；工具集未变时续聊仅传 ~90 Token 极简提醒（每 24 轮自动锚定刷新一次）；工具变更时在同一 `thread_id` 内平滑更新而不换号重开。
   - **高稳定性会话键与旁路隔离**：`conv_key` 不再绑定易变的系统提示词动态内容或 `work_mode`，支持前导 `system` 变动时依然无损命中增量上下文；自动隔离标题生成 / 输入建议等无工具旁路请求，Token 续期也不再误拆线程。

3. **工作场景模式与防撞墙保护（v2.2.0 新增与升级）**
   - **三档工作场景一键热切换（顶部状态栏免重启切换 `work_mode`）**：
     - **`general`（综合协作，默认）**：兼顾日常代码与文档任务，提示词中立通用；
     - **`document`（文档与学术办公专属）**：专为论文审稿、Word/PDF/PPT/Excel 办公用户优化——彻底去除“本地编程/命令执行”偏见措辞、明确告知模型文件在本机授权目录（无需搜云端 Quick 空间）、首轮尚未读入文件时自动将 `high`/`max` 平滑破冰为 `medium`（读到文件后自动恢复满血思考）、且超长系统规范不外置成容易引发防注入警觉的 `guidelines-*.txt` 附件；
     - **`coding`（代码工程）**：保留完整前后端开发脚手架、HTML/SVG 与增量验证指导。
   - **规避上游单步 64K Output 硬墙**：针对 AWS 上游单轮 64,000 Output Token 硬上限导致的“约 10 分钟断流”与 `/compact` 失败，提供可配置三项自动防护（可在「配置」页面独立开关）：
     - **`/compact` 自动降档（`guardrails.compact_downgrade`）**：识别会话压缩摘要指令后自动将本轮 `thinking_effort` 降为 `low`，避免过度推理耗尽输出预算而报 `Server error mid-response`；
     - **无工具请求 `max` 限幅（`guardrails.no_tool_max_cap`）**：纯文本长思考且无可用本地工具时自动将 `max` 平滑限幅至 `high`，既保留充分推理又降低单轮撞墙概率（仍保持下游流式心跳保活）；
     - **环境摘要内联（`guardrails.preserve_env_summary`）**：当超长工作区规范被自动外置为 `guidelines-*.txt` 附件时，自动抽取 `<env>` 及工作目录路径摘要保留在主消息帧内，消除模型翻查附件导致的无效思考。

3. **内置 GitHub 在线更新与国内源自动降级（v2.1.0 新增）**
   - **一键检测与安装**：控制台侧边栏与「配置」页支持一键检查最新版本、展示更新日志与 SHA-256 校验值、下载解压并自动重启完成原地升级；
   - **多源自动降级链路**：
     - `auto`（默认）：优先探测官方 GitHub Releases API 与 Raw 清单，超时或受阻时自动平滑降级至 `cdn.jsdelivr.net` / `fastly.jsdelivr.net` / `gh-proxy.com` / `ghfast.top` / `gh-proxy.org`（以及自定义镜像前缀）；
     - `cn`：优先直连国内加速源，适合直连 GitHub 受限环境；
     - `github`：仅使用官方 GitHub 源；
   - **安全与无损原则**：强制校验安装包 SHA-256（含防路径穿越解压），自动更新严格保留您的 `data/` 账号数据库与 `config.json` 本地配置。

4. **结构化使用记录与可视化分析**
   - 内置 SQLite `request_logs` 明细账本，记录每笔请求的协议、模型映射、账号、耗时、Token、缓存命中及错误详情；
   - 按 `24 小时 / 7 天 / 30 天 / 全部历史` 聚合趋势折线图与占比分布，支持多维度筛选检索与带 UTF-8 BOM 的 CSV 账单导出。

5. **多账号轮询池与智能体配置中心**
   - 支持 AWS Builder ID（个人 Social）与企业 IAM Identity Center（Enterprise）浏览器一键登录与手动回调 URL 登录；
   - 自动刷新 OAuth Token、配额同步、故障冷却摘除与 1M 长上下文会话（`thread_id`）跨轮次复用；
   - 在「智能体配置」页签一键安全配置 Windows 版 Claude Desktop / Cowork（支持预览改动、别名路由热更新、备份快照与一键还原）。

---

## 快速开始

### 1. 桌面端启动（推荐）
直接从 [GitHub Releases](https://github.com/zhao060206/quickrelay-studio/releases) 下载最新便携压缩包并解压，双击运行目录内的 **`QuickRelay-Studio.exe`**：
1. 软件将自动拉起本地守护后端 `quickrelay.exe`（默认监听 `127.0.0.1:8080`）。
2. 进入 **账号池** 页签，点击「新增账号」完成至少一个 AWS 账号授权。
3. 切换至 **客户端接入** 页签，先运行「端到端连通测试」确认通路畅通，随后一键复制对应终端命令启动 Claude Code 或 Codex CLI。

> **端口共存提示**：如果你本机已经运行了旧版代理占用 `8080` 端口，请在 **配置** 中将 `addr` 修改为其它端口（如 `127.0.0.1:18081`）并点击「保存并重启网关」，即可完全隔离共存运行。

### 2. Claude Code 接入示例

**PowerShell:**
```powershell
$env:ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
$env:ANTHROPIC_AUTH_TOKEN="quickrelay-local-key"
$env:ANTHROPIC_MODEL="claude-sonnet-4-5"
$env:ANTHROPIC_SMALL_FAST_MODEL="claude-haiku-4-5"
claude
```

**Bash / Git Bash:**
```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="quickrelay-local-key"
export ANTHROPIC_MODEL="claude-sonnet-4-5"
export ANTHROPIC_SMALL_FAST_MODEL="claude-haiku-4-5"
claude
```

*(若在 `config.json` 中配置了 `api_keys`，请将 `ANTHROPIC_AUTH_TOKEN` 替换为对应的真实 Key；若 `api_keys` 为空，则填任意占位字符串即可。)*

### 3. 命令行独立启动（无桌面模式）
也可在纯命令行或服务器环境中运行 `quickrelay.exe`：
```powershell
.\quickrelay.exe server -config config.json -addr 127.0.0.1:8080
.\quickrelay.exe login
.\quickrelay.exe accounts
```

---

## 从源码编译与运行测试

依赖环境：Go `1.26+`；构建 Windows 桌面控制端额外需要 `.NET Framework 4.x` (`csc.exe`) 与 WebView2 组件 DLL（位于 `bin/`）。

```powershell
# 1. 运行全量 Go 单元测试
go test ./...

# 2. 编译 Go 核心网关
go build -trimpath -ldflags="-s -w" -o bin\quickrelay.exe ./cmd/quickrelay

# 3. 编译 Windows 桌面控制端（内嵌 desktop\ui.html）
powershell -ExecutionPolicy Bypass -File .\desktop\build.ps1

# 4. 复制纯净配置模板（如尚未配置）
Copy-Item config.example.json config.json
```

---

## 目录结构说明

```text
QuickRelay-Studio/
├── QuickRelay-Studio.exe        # 桌面控制端主程序 (WebView2)
├── quickrelay.exe               # Go 高性能双协议核心后端
├── config.json                  # 本地配置文件（请参考 config.example.json）
├── data/                        # 运行后生成的数据目录（quickrelay.db、更新缓存等）
├── WebView2Loader.dll           # WebView2 运行库组件
├── Microsoft.Web.WebView2.*.dll
├── QuickRelay-Studio-使用教程.pdf
├── README.md                    # 本说明文档
└── LICENSE                      # 开源许可协议
```
