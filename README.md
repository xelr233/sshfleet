# sshfleet

> SSH 多机管理 CLI daemon：常驻进程持有连接池，提供多机批量执行、并发文件分发、长连接会话、SFTP 传输。MCP 适配器（供 AI Agent 使用）规划中、尚未实现。

本文档同时是**项目说明**和**开发计划**。里程碑与完成情况见 §9。

---

## 1. 背景与目标

- 现有 SSH MCP 工具大多单主机、无常驻连接，每次操作重新握手；SSH ControlMaster 在 Windows OpenSSH 上不可用。
- 本项目核心形态：**一个 Go 常驻 daemon + CLI 薄客户端 + 可选 MCP server**。daemon 用 `golang.org/x/crypto/ssh` 自己实现 SSH 连接（不调系统 ssh 二进制），天然绕开 ControlMaster 限制，Windows/Linux/macOS 通用。

**目标**

- 多机批量执行命令（batch），并发度可调，逐机结果聚合
- 单文件向大量机器并发分发（distribute），磁盘只读一次，慢机不阻塞快机，支持逐机断点续传
- 长连接 shell 会话（cd / 环境变量延续）、流式跟随（tail -f 类）
- SFTP 单机上传/下载
- 常驻连接池，空闲超时自动退出
- MCP server（stdio）暴露上述能力给 AI Agent（**规划中，未实现**）

**非目标**

- 不做 playbook / 模板引擎（不是 Ansible）
- 不做 TUI
- 远程端零安装：只要求目标机有 sshd
- 不支持 PuTTY .ppk 密钥（文档说明用户自行转换）

## 2. 技术栈

| 组件 | 选择 |
|---|---|
| 语言 | Go ≥ 1.22 |
| SSH | `golang.org/x/crypto/ssh` |
| SFTP | `github.com/pkg/sftp` |
| CLI | `spf13/cobra` |
| MCP | `github.com/modelcontextprotocol/go-sdk`（官方 Go SDK，stdio 传输）— **未实现，未引入依赖** |
| 并发控制 | `golang.org/x/sync/errgroup` + `golang.org/x/sync/semaphore` |
| 配置 | `gopkg.in/yaml.v3` |
| Windows agent | `github.com/Microsoft/go-winio`（dial OpenSSH agent named pipe） |
| IPC | `127.0.0.1:TCP` + 启动期生成的一次性 token，JSON 行协议 |

## 3. 总体架构

```
┌─ 本机（任意 OS）────────────────────────────┐      ┌ 目标机（只需 sshd）┐
│                                            │      │                    │
│  Agent / 用户                              │      │   ┌─────────────┐  │
│    ├─ CLI (sshf) ──┐                       │      │   │ host A sshd  │  │
│    └─ MCP server ──┤→ 127.0.0.1:port →│ daemon │──┤   │ host B sshd  │  │
│                    ┘   (JSON 行协议+token) │ 连接池  │   │ host C sshd  │  │
└────────────────────────────────────────────┘      └─────────────┘
```

- **daemon** 是唯一持有 SSH 连接和凭据的进程。启动时监听 TCP、写 token 文件，空闲（无客户端、无活动任务）超时自杀。
- **CLI / MCP** 是无状态薄客户端：连 daemon、发请求、收结果。daemon 未运行时，CLI 自动用 `sshf start` 拉起。

## 4. 目录结构

```
sshfleet/
├── cmd/sshf/            # cobra 入口：root / start / stop / status / hosts（含 import）/
│                        # exec / batch / distribute / session / tail / upload / download /
│                        # completion / mcp；CLI 全量中文界面与 help
├── internal/
│   ├── daemon/          # 监听、token 鉴权、请求路由、订阅推送、空闲退出、PID/端口文件
│   ├── protocol/        # 请求/响应/事件结构体、JSON 行编解码（daemon 与客户端共用）
│   ├── pool/            # 连接池：dial、保活(keepalive)、断线重连退避、并发上限
│   ├── exec/            # exec / batch（errgroup+semaphore）、逐机超时与结果聚合、shell 包装
│   ├── session/         # 持久 shell 会话（PTY、cd/env 延续、订阅输出）
│   ├── dist/            # distribute：有界管道扇出、逐机进度、断点续传
│   ├── sftp/            # 单机 upload/download（pkg/sftp，带进度回调、offset 续传）
│   ├── tail/            # 流式跟随（环形缓冲、seq 游标、多客户端订阅）
│   ├── encoding/        # 远端输出编码（UTF-8 优先、GBK 兜底）
│   ├── auth/            # 密钥加载（OpenSSH 格式）、agent 适配（unix socket / winio pipe）
│   ├── config/          # hosts.yaml 加载校验、token/PID 文件路径（~/.sshfleet/）
│   ├── sshconfig/       # OpenSSH ~/.ssh/config 子集解析（供 hosts import 导入）
│   ├── sshtest/         # 测试专用进程内 SSH 服务端（exec/shell/tail/sftp + 限速）
│   └── mcp/             # MCP server（未实现，占位）
├── .github/workflows/   # release.yml：手动触发，发布 windows/amd64 + linux/amd64
├── .agents/skills/sshf/ # 供 AI Agent 使用的 skill（SKILL.md + references/cli-reference.md）
├── go.mod
└── README.md
```

## 5. 关键设计决策（开发时必须遵守）

1. **IPC**：只做 `127.0.0.1:TCP` + token。不抽象 unix socket / named pipe，避免双套代码；token 为 daemon 启动时随机生成、写入 `~/.sshfleet/token`（0600），客户端首字节验证，失败即断开。
2. **协议**：JSON 行协议（每行一个请求/响应），请求带自增 `id`，响应回带 `id`。流式/长任务（tail、distribute 进度、session 输出）用同连接上的推送事件（`{"id":..,"event":..}`）。
3. **并发模型**：goroutine per 连接/任务。**禁止**在任务间共享单个 SSH channel；每台机器的并发 SSH channel 数受 pool 内 semaphore 限制（默认每主机 4）。
4. **distribute 扇出**：磁盘只读一次 → chunk 广播到每目标机独立的 `io.Pipe` → 每机 goroutine 从 pipe 读并 SFTP 写。pipe 缓冲即流控边界：慢机只堵自己的缓冲。逐机记录 offset，失败机器续传从 offset 起。
5. **加密与压缩**：cipher 协商优先 `aes128-gcm@openssh.com` / `aes256-gcm`；**禁用 SSH zlib 压缩**（大文件分发时 CPU 纯浪费，配置里可显式打开）。
6. **会话延续**：需要 cd/环境变量延续的操作必须用 shell channel（`session.Shell()` + 持久 stdin/stdout 解析），不能依赖 exec channel；exec channel 仅用于无状态单命令。
7. **凭据**：hosts.yaml 支持私钥路径 / 密文密码（daemon 启动时解密） / 引用 ssh-agent。密码明文落盘时启动告警。Windows 下 ssh-agent 走 `\\.\pipe\openssh-ssh-agent`（go-winio dial），Unix 走 `SSH_AUTH_SOCK`。
8. **重连**：连接池内连接断开时指数退避重连（100ms 起、×2、封顶 30s），对上层透明；任务执行中连接断开则该机任务失败并返回明确错误，由 batch/distribute 层决定重试。
9. **远程 Windows 目标机**：输出解码 UTF-8 优先、GBK 兜底（M1 不做，见里程碑顺序）。
10. **默认 shell**：exec/batch 默认**不包裹命令**，把命令原样交给目标机的登录 shell（与 `ssh host cmd` 一致）。目标可能是 Windows `cmd.exe`、PowerShell，也可能是 bash/sh，因此**禁止硬编码 `/bin/bash -c`**。需要确定性 shell 时用主机配置的 `shell` 字段或 `exec --shell` 指定（`auto`/`bash`/`sh`/`cmd`/`powershell`/`pwsh`）；除 `auto` 与 `cmd` 外，命令作为脚本经 stdin 喂给解释器，避免跨 shell 的引号与转义差异（见 `internal/exec/shell.go`）。

## 6. 协议命令集（v1）

| 命令 | 请求参数 | 语义 |
|---|---|---|
| `status` | — | daemon 版本、连接池状态、活动任务 |
| `exec` | host, command, timeout | 单机执行，回 stdout/stderr/exit code |
| `batch` | hosts[] 或 group, command, concurrency, timeout | 多机并发执行，逐机结果数组 |
| `dist_start` | hosts[]/group, local_path, remote_path, concurrency | 分发，返回 task_id |
| `dist_status` | task_id | 逐机进度（offset/total/state） |
| `dist_cancel` | task_id | 取消分发 |
| `sftp_put` / `sftp_get` | host, local, remote | 单机传输，progress 事件流 |
| `session_open` / `session_send` / `session_close` / `session_list` | host / session_id / input | 长连接 shell 会话 |
| `tail_start` / `tail_stop` | host, path / task_id | 远端 `tail -f` 事件流 |

CLI 子命令与 MCP 工具一一映射到上述协议命令，**不在 CLI/MCP 层实现任何 SSH 逻辑**。

## 7. CLI 命令面

```
sshf start / stop / status
sshf hosts add/list/rm/test           # test = 对清单逐一连通性检查
sshf hosts import [--file ~/.ssh/config] [--group g] [--overwrite] [--dry-run]
                                      # 从 OpenSSH 配置批量导入主机
sshf exec <host> <cmd>
sshf batch [--host h]... [--group g] [-c 8] <cmd>   # 输出：逐机块 + 汇总统计
sshf distribute <local> <remote> [--host h]... [--group g] [--concurrency 16] [--resume]
sshf upload <host> <local> <remote> / download <host> <remote> <local>
sshf session open <host> / send <id> <input> / close <id> / list
sshf tail <host> <file> [--task <id> --since <seq>]  # --task/--since 用于断线重连
sshf completion <bash|zsh|fish|powershell>           # 生成补全脚本
sshf mcp                              # stdio 模式启动 MCP server（未实现，占位）
```

CLI 界面（用法/帮助/补全/参数与选项报错）与命令说明均为中文；`sshf --help` 即完整中文命令树。

> **给 AI Agent**：仓库内置 skill `.agents/skills/sshf/`（`SKILL.md` + `references/cli-reference.md`），可在支持 skills 的 Agent（如 ZCode）中直接指导多机运维；`references/cli-reference.md` 也适合人读的命令/标志/字段权威参考。

### 7.1 从 OpenSSH 配置导入

`sshf hosts import` 解析 `~/.ssh/config` 的一个子集并写入 hosts.yaml：

- **支持**：`Host`（具体别名，含一个块内多别名）、`HostName`、`User`、`Port`、`IdentityFile`；遵循 OpenSSH「首个取值生效」规则，识别文件开头全局块与 `Host *` 通配、`!` 否定。
- **不支持**（告警并跳过其效果）：通配别名 `Host *`（无法展开为具名主机）、`Include`、`Match`、`ProxyJump`/`ProxyCommand`。
- 未写 `HostName` 时用别名作为地址（同 `ssh` 语义）。已存在同名主机默认**跳过**，`--overwrite` 覆盖，`--dry-run` 只预览。

## 8. GoSSHa 代码参考指引

仓库：<https://github.com/YuriyNasretdinov/GoSSHa>（BSD-2，可自由借鉴/搬运代码）。
它是"daemon + 连接池 + JSON 接口"的最小完整实现，**全部逻辑在一个 `main.go`**（约 1000 行）。按函数对照借鉴：

| GoSSHa 位置（main.go） | 是什么 | 借鉴到本项目 |
|---|---|---|
| `connHostsMap` (L53) + Get/Set/Close | hostname → `*ssh.Client` 的连接池，mutex 保护 | `internal/pool` 的核心数据结构原型；需扩展：keepalive、重连退避、per-host channel 限流 |
| `makeSigners` / `makeSigner` (L208, L293) | 启动时枚举加载 `~/.ssh` 全部私钥为 signers | `internal/auth`；保留"启动期一次性加载"思路，加 passphrase 与 agent |
| `makeConfig` (L169) | ClientConfig 组装 + ssh-agent 回调 | `internal/auth` |
| `getConnection` (L304) | 池内取连接、miss 时 dial 并入池 | `internal/pool`；注意它对 dial 无并发去重，我们要加 singleflight 防惊群 |
| `agentConnectionManagerThread` (L426) | 全局并发连接数上限的 semaphore 线程 | 并发上限思路并入 `internal/pool` semaphore，不用单独线程 |
| `executeCmd` (L398) | exec channel 跑命令收 stdout/stderr | `internal/exec` 单机执行原型 |
| `uploadFile` (L342) | `sftp.NewClient` + 整文件写入 | 仅适合小文件；distribute 用 §5.4 的 pipe 方案，不照搬 |
| `maxThroughputThread` (L525) | 全局任务并发 semaphore | `internal/exec` batch 的并发控制 |
| `inputDecoder` (L658) + `runProxy` (L680) | stdin 逐行 JSON 请求 → 路由执行 → stdout 回 JSON | 协议路由骨架；我们把它从 stdin/stdout 换成 TCP + token，加请求 `id` 关联 |
| `jsonReplierThread` (L486) + `sendProxyReply` (L521) | 响应队列串行写回，避免并发写 socket | daemon 的响应写出模型 |

**参考时的注意事项（写代码前先读）**

- GoSSHa 是 2017 年代码：全库无 `context.Context`，取消/超时靠 goroutine 泄漏容忍。**我们的所有阻塞操作必须带 context**（dial、exec、SFTP 读写、batch 等待）。
- 它没有 keepalive：连接死掉要等下次使用才失败。我们必须实现 `Client.SendKeepalive`（`x/crypto/ssh` 已内置 keepalive 包 `golang.org/x/crypto/ssh/keepalive`）+ 探活重连。
- 错误处理偏"报告给用户继续跑"，不适合 daemon 长期运行；按 Go 惯例重构为显式 error 返回。
- 单文件结构仅供阅读，落地必须按 §4 分包。

辅助参考：[windvalley/gossh](https://github.com/windvalley/gossh)（MIT，活跃）——不是 daemon，但它的 CLI 参数体系是 batch 场景的成熟答案，照抄语义：`-c/--concurrency`（默认 5）、`-t/--timeout`（单机）、`-l/--hostlist` 文件、逐机输出块格式 `=== host === `、失败机器汇总重跑提示。二开/搬代码均可（MIT）。

## 9. 里程碑与进度

> 约定：每个里程碑独立提交（commit），完成标准 = 构建通过 + 验收项手动可验。开发环境为 Windows + Git Bash；交叉编译目标 `windows/amd64, linux/amd64, darwin/arm64`。

| 里程碑 | 内容 | 状态 | commit |
|---|---|---|---|
| M0 | 骨架：go.mod / cobra 命令树 / 目录结构 | ✅ | `faff127` |
| — | 第一梯队：`protocol` / `config` / `auth`（私钥部分） | ✅ | `6f13860` |
| M1 | 连接池 + daemon + 单机 exec（含 ssh-agent） | ✅ | `5823ce3` |
| M2 | batch：errgroup+semaphore 并发、逐机超时、结果聚合 | ✅ | `9b9efff` |
| M3 | distribute：有界管道扇出、逐机进度、offset 续传 | ✅ | `81fd6e8` |
| M4 | session + tail：持久 shell、环形缓冲订阅 | ✅ | `8e2b327` |
| M5 | MCP server（stdio） | ⏸ 暂缓（按需求判定非必要） | — |
| M6 | upload/download、hosts test、GBK 解码 | ✓ 部分 | `9ad9643` |

**测试**：126 项通过，`-race` 干净，三平台交叉编译通过。

**里程碑后增补（非里程碑项）**：CLI 全量中文化（用法/帮助/补全/参数与选项报错）、`hosts import`（从 `~/.ssh/config` 导入，`internal/sshconfig`）、GitHub Actions 手动发布工作流（见下「构建与发布」）。

### 实现说明与原案的差异

- **默认 shell（M1）**：不硬编码 `/bin/bash -c`，默认把命令原样交给目标机登录 shell（同 `ssh host cmd`）；`shell` 字段 / `--shell` 可显式指定 `auto/bash/sh/cmd/powershell/pwsh`，见 §5.10。这是 Windows 目标必需的一点。
- **host key 校验（M1）**：暂用 `InsecureIgnoreHostKey`，known_hosts 校验未做。
- **daemon 子进程 detach（M1）**：平台特定（Unix `setsid`，Windows `CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS`）。
- **distribute 扇出（M3）**：未用 `io.Pipe`（无缓冲，慢机经广播器会拖住快机），改用**带预读窗口（4 MiB）的有界内存管道**；并发度只限制建连、不限制消费（限制消费会反压广播器而死锁）。
- **session（M4）**：`open` 时先请求 PTY —— Windows OpenSSH 对非 PTY 的 shell 请求会直接断开；`Close` 先关 stdin 并给收尾设上限，避免个别 sshd 不回 exit-status 时把请求挂死。
- **GBK 解码（M4/M6）**：`internal/encoding.ToUTF8`，UTF-8 优先、GBK 兜底，应用于 exec / session / tail 输出。
- **M6 按需求跳过**：`--no-daemon` 直连模式、goreleaser。（三平台交叉编译已可用；发布由 §9「构建与发布」的 GitHub Actions 手动工作流完成。）
- **M5 暂缓**：`cmd/sshf/mcp.go` 与 `internal/mcp/doc.go` 保留 `TODO(M5)` 占位；`internal/protocol` 已是 CLI/MCP 共用契约，实现 MCP 只需映射协议命令，不涉及 SSH 逻辑。

### 验收方式

- **进程内**：`internal/sshtest` 提供进程内 SSH 服务端（exec / 持久 shell / tail 流 / SFTP + 入站限速），覆盖连接复用、断线重连并计数、channel 限流、batch 并发上限、distribute「慢机不阻塞快机」、tail 断线重连不丢事件、session `cd` 延续等。
- **真实机**：本机 OpenSSH `sshd` 为 SYSTEM 服务、不可随意重启或另起实例，故用 `gost` 把**同一个 sshd** 代理成多个本地端口当多机（`2201`/`2202` 全速，`2203` 加 `limiter=1MB` 模拟慢机）。注意该 sshd 的 `sshd_config` 需含 `Subsystem sftp sftp-server.exe` 才能使用 SFTP。

### 构建与发布

```bash
go build ./...                                   # 本机构建
go test -race ./...                              # 全量测试
go build -trimpath -ldflags "-s -w -X main.version=0.2.0" -o dist/sshf ./cmd/sshf
```

发布走 **GitHub Actions 手动触发**（`.github/workflows/release.yml`）：在仓库 Actions 页选 `Release` → `Run workflow`，填写版本号（如 `v0.2.0`），可选 pre-release / draft。流水线先跑 `go vet` + `go test -race`，再交叉编译 **windows/amd64** 与 **linux/amd64**（`CGO_ENABLED=0`），打包为 `zip`/`tar.gz` 并生成 `sha256sums.txt`，最后用 `softprops/action-gh-release` 创建 Release 并附上产物。

### 剩余工作

- **M5 — MCP server**：官方 Go SDK，stdio，工具映射 §6 全部命令。验收：在 MCP 客户端（如 ZCode）中完成「对 group 执行命令 → 分发文件 → 查询进度」闭环。
- known_hosts 校验；`--no-daemon` 直连；darwin/arm64 发布目标（当前 Release 只发 windows/linux amd64）。

## 10. 风险与已知坑（开发中随时对照）

1. `x/crypto/ssh` 无"高级会话对象"：shell 会话的窗口/信号/输出切分都要自己写（M4 已落地：PTY + 静默边界 + 收尾上限）。
2. Windows ssh-agent 的 named pipe 需 go-winio 手动 dial（`\\.\pipe\openssh-ssh-agent`），且要求 ssh-agent 服务已启动——`auth` 包已做优雅降级并给出可读错误。
3. `pkg/sftp` 并发写单文件：distribute 每机只开一个写句柄，chunk 顺序写，勿并发写同一文件。
4. 大量机器分发时 fd 上限：每主机并发 SSH channel 受 `pool` 的 `MaxChannels`（默认 4）约束；Linux 侧 daemon 生命周期文档提示。
5. 远程编码：目标 Windows 主机 GBK 输出已由 `internal/encoding.ToUTF8`（UTF-8 优先、GBK 兜底）处理。
6. token 文件与 hosts.yaml 权限：Windows 下用 ACL 收紧（至少当前用户），Unix 0600。
7. 已知未做：host key 校验（当前 `InsecureIgnoreHostKey`）、`--no-daemon` 直连、MCP server。
