# relay-install

IntelAlloc 中转站一键配置工具(Go 单二进制)。保底安装 Codex CLI,按 key 分组自动配置
Codex CLI / Claude Code / Claude Desktop(Win/macOS)/ CC Switch,结束即可使用。

## 下载(普通用户,无需安装 Go)

从 [Releases](https://github.com/truiqtrio/ia-reday/releases) 下载对应平台的单二进制,解压即用:

- Windows:`relay-install-windows-amd64.exe`
- macOS:`relay-install-darwin-arm64`(M 系)/ `relay-install-darwin-amd64`(Intel)
- Linux:`relay-install-linux-amd64` / `relay-install-linux-arm64`

可选校验:`sha256sum -c SHA256SUMS`。

## 构建(开发者)

```bash
go build -o relay-install ./cmd/relay-install  # 需要 Go ≥1.26
```

交叉编译:`CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o relay-install.exe ./cmd/relay-install`(macOS 同理 darwin/amd64、darwin/arm64)。

## 使用

```bash
relay-install plan      # 只读预览:检测 + 将执行 + 不会触碰(不带子命令等价 plan)
relay-install apply     # 交互向导:填 key → 一次授权 → 全自动配置
relay-install verify    # 事后重跑协议探针
relay-install recover   # 崩溃后按 journal 收尾
relay-install keys      # 列出已录入 key(名称+末4位+分组)
```

非交互:`relay-install apply --key-stdin`,stdin 每行一枚,可带分组前缀 `gpt:` / `anthropic:` / `china:`,裸 key 自动探测 `/v1/models` 判定分组。

常用 flag:`--base-url`(默认 https://backend.intelalloc.com)、`--skip-live`(关闭真实调用探针)、`--print-only`(只输出可手贴片段)、`--profile unrestricted`(无护栏档,显式 opt-in)。

## key 分组规则

录入时自动探测分组并绑定对应客户端:**GPT 组** → Codex profile(默认 model `gpt-5.6-sol-high`);
**Anthropic 组** → Claude Code / Claude Desktop(默认 `claude-opus-5`);**中国大模型组**(gemma4/glm-5.2/qwen3.5/kimi-k2.7-code/kimi-k2.6/minimax-m3)→ 独立 provider 条目。
订阅/按量由录入时声明(默认订阅)。GPT 订阅 key 不会被配到 Claude 端,反之亦然。

## 安全与边界

- 明文 key 只落目标配置文件(0600);日志/终端输出只显示 名称+末4位。
- 每次 apply 先备份(0600),失败自动回滚;备份会复制目标中已有明文 key(留存面已明示)。
- 不碰:`~/.codex/config.toml` 主配置、`claude_desktop_config.json`、CC Switch 的 `proxy_*`/`model_pricing` 表。
- 检测到 CC Switch 代理接管(live_takeover / 127.0.0.1:15721)→ 阻断,不硬写。
- 当前密钥现阶段为明文 0600 策略;后续版本将提供 OS 密钥环托管选项。

## 已知边界(v1)

- Claude Desktop 仅 Win/macOS;Linux(含 WSL2)只有终端客户端。
- Codex App 只检测不写入(复用 CLI profile,需完全重启 App)。
- keys 清单为会话内存,持久化存储待后续版本。
- 语义级真实调用需要有效 key;探针不通时报 UNCONFIRMED 而非失败,配置仍生效。
