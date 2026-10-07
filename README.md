# sshfleet

> SSH 多机管理 CLI daemon：常驻进程持有连接池，提供多机批量执行、并发文件分发、长连接会话，附带 MCP 适配器供 AI Agent 使用。

本文档同时是**项目说明**和**Agent 开发计划指导**。Agent 按本文档的里程碑顺序开发，每个里程碑的验收标准见 §9。

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
- MCP server（stdio）暴露上述能力给 AI Agent

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
| MCP | `github.com/modelcontextprotocol/go-sdk`（官方 Go SDK，stdio 传输） |
| 并发控制 | `golang.org/x/sync/errgroup` + `golang.org/x/sync/semaphore` |
| 配置 | `gopkg.in/yaml.v3` |
| Windows agent | `github.com/Microsoft/go-winio`（dial OpenSSH agent named pipe） |
| IPC | `127.0.0.1:TCP` + 启动期生成的一次性 token，JSON 行协议 |

## 3. 总体架构

```
┌─ 本机（任意 OS）────────────────────────────┐      ┌ 目标机（只需 sshd）┐
│                                            │      │                    │
│  Agent / 用户                              │      │   ┌─────────────┐  │
│    ├─ CLI (sshm) ──┐                       │      │   │ host A sshd  │  │
│    └─ MCP server ──┤→ 127.0.0.1:port →│ daemon │──┤   │ host B sshd  │  │
│                    ┘   (JSON 行协议+token) │ 连接池  │   │ host C sshd  │  │
└────────────────────────────────────────────┘      └─────────────┘
```

- **daemon** 是唯一持有 SSH 连接和凭据的进程。启动时监听 TCP、写 token 文件，空闲（无客户端、无活动任务）超时自杀。
- **CLI / MCP** 是无状态薄客户端：连 daemon、发请求、收结果。daemon 未运行时，CLI 可用 `sshm start` 拉起（默认行为），或 `--no-daemon` 单次直连（一次性任务，不走连接池）。

## 4. 目录结构（规划）

```
sshfleet/
├── cmd/sshf/            # cobra 入口：root / start / stop / status / exec / batch /
│                        # distribute / session / tail / upload / download / mcp
├── internal/
│   ├── daemon/          # 监听、token 鉴权、请求路由、空闲退出、PID/端口文件
│   ├── protocol/        # 请求/响应结构体、JSON 行编解码（daemon 与客户端共用）
│   ├── pool/            # 连接池：dial、保活(keepalive)、断线重连退避、并发上限
│   ├── exec/            # exec / batch（errgroup+semaphore）、逐机超时与结果聚合
│   ├── session/         # shell 会话（channel 生命周期、cd/env 延续、send）
│   ├── dist/            # distribute：io.Pipe 扇出、逐机进度、断点续传、失败重试
│   ├── sftp/            # 单机 upload/download（pkg/sftp，带进度回调）
│   ├── tail/            # 流式跟随（长驻通道，多客户端订阅）
│   ├── auth/            # 密钥加载（OpenSSH 格式）、agent 适配（unix socket / winio pipe）
│   ├── config/          # hosts.yaml 加载校验、token/PID 文件路径（~/.sshfleet/）
│   └── mcp/             # MCP server，工具函数 = protocol 客户端调用
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
sshf exec <host> <cmd>
sshf batch --group web [-c 8] <cmd>   # 输出：逐机块 + 汇总统计（成功/失败/耗时）
sshf distribute <local> <host|--group> <remote> [--concurrency 16] [--resume]
sshf upload / download <host> <local> <remote>
sshf session open <host> / send <id> <input> / close <id> / list
sshf tail <host> <file>
sshf mcp                              # stdio 模式启动 MCP server
```

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

## 9. 里程碑（Agent 执行计划）

> 约定：每个里程碑独立提交（commit），完成标准 = 构建通过 + 验收项手动可验。开发环境为 Windows + Git Bash，交叉编译目标 `windows/amd64, linux/amd64, darwin/arm64`。

**进度**：M0 ✅ 已完成（commit `faff127`：go.mod + cobra 命令树 + 目录骨架）。
「第一梯队」✅ 已完成：`protocol` / `config` / `auth`（仅私钥文件部分，ssh-agent 留 M1）。M1–M4、M6 已完成；**M5（MCP server）按需求判定为非必要，暂不做**，代码中以 `TODO(M5)` 保留占位。可用 `rg 'TODO\(M'` 汇总。

**M1 ✅ 已完成**（commit `5823ce3`）：`config`（`shell` 字段）、`auth`（ssh-agent 双平台 + 降级、`ClientConfig` 组装）、`pool`（连接池：dial 去重 / keepalive / 退避重连 / 每主机 channel 限流）、`daemon`（TCP + token + JSON 行路由 + 空闲退出 + PID/端口/token 文件）、`exec`（单机 exec）、CLI 的 `start`/`stop`/`status`/`exec`（daemon 未运行时自动拉起）。验收方式：`internal/sshtest` 提供进程内 SSH 服务器，`pool`/`exec`/`daemon` 的集成测试覆盖「21 次取连接仅 1 次握手」「断线后自动重连并计数」「channel 上限」「超时」等；另有对真实 sshd（本机 `ssh win`）的手工验证。

**M1 与 README 原案的差异**：
- **默认 shell**：不硬编码 `/bin/bash -c`，默认原样交给目标机登录 shell；`shell` 字段 / `--shell` 可显式指定（见 §5.10）。这是原案未覆盖、但 Windows 目标必需的一点。
- Host key 校验暂用 `InsecureIgnoreHostKey`（known_hosts 校验按原计划留 M6）。
- `daemon --foreground` 对子进程用平台特定的 detach（Unix `setsid`，Windows `CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS`）。

**M2 ✅ 已完成**（commit `9b9efff`）：`exec.RunBatch`（`errgroup` + `semaphore` 并发、逐机独立超时、结果按名排序聚合；单机失败不中断其它机器）、`daemon` 的 `batch` 路由（`group`/主机选择集解析 + 逐机 `EventProgress` 事件流）、CLI `batch`（`=== host ===` 逐机块 + 汇总统计，有失败则退出码 1）。验收：进程内测试断言「5 台 `-c 2` 时服务端峰值并发 ≤ 2」与「逐机超时不互相影响」；并已在真实环境（gost 多端口代理本机 sshd，见下）跑通 3 台并发。

**M3 ✅ 已完成**（commit `81fd6e8`）：`internal/sftp`（单机 `Put`/`Get`，带节流进度回调、`Offset` 断点续传；每个会话占用一个 channel 名额）、`internal/dist`（**有界内存管道 + 广播者**：磁盘只读一次、chunk 扇出到每机独立管道，管道缓冲即流控边界，慢机只堵自己；`Task` 自包含 `Progress`/`Wait`/`Cancel`；并发度只限建连不限消费，避免反压死锁）、`daemon` 的 `dist_start`/`dist_status`/`dist_cancel`（异步长任务 + `EventDistDone` 事件 + 终态保留 10 分钟可查）、CLI `distribute`（`--host`/`--group`/`--concurrency`/`--resume`，轮询进度 + 逐机汇总）。验收：进程内 `TestSlowHostDoesNotBlockOthers` 直接断言「限速慢机完成前，快机均已完成且内容一致」；真实环境 8 MiB 分发 3 台，限速机（~1 MB/s）仍在 43% 时另两台已 100% 完成，md5 校验一致；`--resume` 从远端半截文件续传后 md5 一致。

**验收环境（M2/M3 真实节）**：本机 OpenSSH `sshd` 为 SYSTEM 服务、不可随意重启或另起实例，故用 `E:\gost_3.3.0_windows_amd64\gost.exe` 把**同一个 sshd** 代理成多个本地端口当作多机 —— `127.0.0.1:2201`/`2202` 全速，`2203` 加 `limiter=1MB` 模拟慢机；`hosts.yaml` 里 `sim1/sim2/sim3` 指向这三个端口。注意：该 sshd 的 `C:\ProgramData\ssh\sshd_config` 原本缺 `Subsystem sftp sftp-server.exe`，需补上并重启 sshd 后 SFTP 才可用。M2/M3 完成时测试 87 项、`-race` 干净、三平台交叉编译通过。

**M4 ✅ 已完成**（commit `8e2b327`）：`internal/session`（`session.Shell()` 持久 shell，`cd`/环境变量延续；**open 时先请求 PTY** —— Windows OpenSSH 对非 PTY 的 shell 请求会直接断开；`Close` 先关 stdin 并给收尾设上限，避免个别 sshd 不回 exit-status 时把请求挂死）、`internal/tail`（远端 `tail -f` + **环形缓冲**（2000 行）+ 带 `seq` 游标订阅，重连带 `--since` 从缓冲补回缺口，不丢不重）、`daemon`（连接级订阅表 `Event`/`AddSub`/`DelSub`，连接断开统一取消；`session_open/send/close/list` 与 `tail_start/stop` 路由；`client.Stream` 长驻读循环）、`internal/encoding`（`ToUTF8`：UTF-8 优先、GBK 兜底）、CLI `session open/send/close/list` 与 `tail --task/--since/--for`。验收：进程内测试覆盖「`cd` 后 `pwd` 反映新目录」「环形缓冲有界」「断线重连序号连续无缺口」「多订阅者」；真实机验证 `cd /c/Windows` 后 `pwd` 返回 `/c/Windows`、`tail` 跟随实时文件按 `seq` 输出。

**M6 ✅ 已完成（部分）**（commit `9ad9643`）：`internal/sftp` 单机传输已提前在 M3 落地，M6 补齐 CLI 侧与打磨 —— `upload`/`download`（进度事件在 stderr 原地刷新）、`hosts test`（逐机建连 + 认证，OK/耗时 或 FAIL/原因，含不可达主机时退出码 1）、`exec` 输出的 **GBK 兜底解码**。**按需求跳过**：`--no-daemon` 直连模式、三平台交叉编译脚本、goreleaser。验收：真实机 `upload`→`download` 往返 md5 一致、`hosts test` 逐机输出、`tail` 跟随；进程内覆盖 GBK 解码、`hosts_test` 可达/不可达、`sftp` 回环 + 进度事件。全部 105 项测试通过、`-race` 干净。

**M5（MCP server）— 暂缓**：按需求判定为非必要。`cmd/sshf/mcp.go` 与 `internal/mcp/doc.go` 保留 `TODO(M5)` 占位；协议层（`internal/protocol`）已是 CLI/MCP 共用的唯一契约，未来实现 MCP 只需在 `internal/mcp` 映射到协议命令，不涉及 SSH 逻辑改动。

**M0 — 骨架**
初始化 go.mod、cobra 命令树、目录结构、CI 可选。验收：`sshf --help` 完整命令树。

**M1 — 连接池 + 单机 exec**
`config`（hosts.yaml）、`auth`（密钥 + agent 双平台）、`pool`（含 keepalive/重连/singleflight）、`daemon`（TCP+token+JSON 行协议）+ `exec`。验收：daemon 常驻，`sshf exec` 走池内连接连续执行 20 条命令，观察仅 1 次握手；kill 目标 sshd 连接后自动重连。

**M2 — batch**
`errgroup` + semaphore 并发、逐机超时、结果聚合输出、失败清单。验收：5 台（可用本地 sshd 容器代替）并发执行，输出逐机块 + 汇总；并发数 -c 生效。

**M3 — distribute（本项目核心难点）**
pipe 扇出、逐机进度、offset 续传、`dist_status`/`dist_cancel`。验收：100MB 文件分发 5 台，慢机（`tc` 限速或小内存容器）不阻塞其他机器；中断后 `--resume` 只补未完成机器。

**M4 — session + tail**
shell channel 会话管理、cd/env 延续、事件推送、多客户端订阅 tail。验收：`session open` 后 `send "cd /tmp"` 再 `send "pwd"` 输出 `/tmp`；`tail` 客户端断开重连不丢已推事件（环形缓冲）。

**M5 — MCP server**
官方 Go SDK，工具映射 §6 全部命令，stdio 模式。验收：在 MCP 客户端（如 ZCode）中完成"对 group 执行命令 → 分发文件 → 查询进度"闭环。

**M6 — 打磨**
`sftp upload/download` 进度、`hosts test`、GBK 兜底解码、`--no-daemon` 直连模式、三平台交叉编译 + goreleaser。

## 10. 风险与已知坑（开发中随时对照）

1. `x/crypto/ssh` 无"高级会话对象"：shell 会话的窗口/信号/输出切分都要自己写（M4 工作量容易低估）。
2. Windows ssh-agent 的 named pipe 需 go-winio 手动 dial（`\\.\pipe\openssh-ssh-agent`），且要求 ssh-agent 服务已启动——`auth` 包要做优雅降级并给出可读错误。
3. `pkg/sftp` 并发写单文件：distribute 每机只开一个写句柄，chunk 顺序写，勿并发写同一文件。
4. 大量机器分发时监听端 fd 上限（Windows 无 ulimit 问题，Linux 侧 daemon 生命周期文档提示）。
5. 远程编码：目标 Windows 主机 GBK 输出在 M6 前会乱码，属已知限制。
6. token 文件与 hosts.yaml 权限：Windows 下用 ACL 收紧（至少当前用户），Unix 0600。
