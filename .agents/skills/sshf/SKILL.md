---
name: sshf
description: 使用 sshf(sshfleet)CLI 在多台 SSH 主机上批量执行命令与传输文件。覆盖 daemon 生命周期、hosts.yaml 清单管理(含从 ~/.ssh/config 导入)、单机 exec、并发 batch、多机 distribute 分发、持久 shell 会话、流式 tail、SFTP 上传下载,以及输出/退出码约定与常见坑。只要用户提到 sshf、sshfleet、多机/批量 SSH、把文件分发到多台服务器、跨主机 tail 日志、管理 SSH 主机清单,就应使用本技能——即使用户没有明确说出 sshf 这个名字。
---

# sshf — 多机 SSH 运维 CLI

sshf 的形态是「常驻 daemon + 无状态薄 CLI」:daemon 自己实现 SSH 协议并持有连接池与凭据,CLI 只负责把请求发给本机 daemon。因此**跨主机的并发、进度、会话延续都发生在 daemon 里**,CLI 只打印结果。

## 心智模型(先读这段)

- **CLI 无状态**。每次调用 CLI 都是新建连接 → 发 JSON 请求 → 打印结果 → 退出。不要指望 CLI 自己保存状态。
- **daemon 持有连接与凭据**。首次运行任何命令时,CLI 会自动拉起 daemon(首次会略慢),后续复用同一进程与连接池。`sshf status` 可查看池内连接、重连次数、活动任务。
- **状态目录 `~/.sshfleet/`**：`hosts.yaml`(清单)、`token`(一次性 IPC token,0600)、`daemon.pid`、`daemon.port`。用环境变量 `SSHFLEET_HOME` 可整体改目录(隔离/测试很有用)。
- **目标机零安装**:只要求目标有 sshd。
- **全局 `--config <path>`**:指向另一个 hosts.yaml,不改默认目录。

## 标准工作流

1. **看配置在哪 / 有没有主机**
   ```bash
   sshf hosts list --path     # 只打印生效的 hosts.yaml 路径
   sshf hosts list            # NAME USER ADDR PORT CREDENTIAL GROUPS
   ```
2. **没清单就先建**。已有 `~/.ssh/config` 时优先导入,否则逐台新增:
   ```bash
   sshf hosts import --dry-run          # 先预览,不写盘
   sshf hosts import --group prod       # 确认后写入,并加入分组
   sshf hosts add web-1 --addr 10.0.0.11 --user deploy --key ~/.ssh/id_ed25519
   ```
3. **验连通性**(建连 + 认证,不发命令):
   ```bash
   sshf hosts test --group prod
   ```
4. **干活**:`exec` / `batch` / `distribute` / `upload` / `download` / `session` / `tail`。

先跑 `hosts test` 再批量操作,能把「主机不可达」和「命令本身失败」两类问题分开。

## 命令速查

| 目的 | 命令 |
|---|---|
| 单机执行 | `sshf exec <host> <cmd>` |
| 多机并发执行 | `sshf batch --group prod -c 8 '<cmd>'` |
| 多机分发一个文件 | `sshf distribute ./app.tgz /opt/app.tgz --group prod` |
| 单机上传/下载 | `sshf upload <host> <local> <remote>` / `sshf download <host> <remote> <local>` |
| 持久 shell(cd/env 延续) | `sshf session open <host>` → `send` / `close` / `list` |
| 跟随远端日志 | `sshf tail <host> <file>` |
| 清单 | `sshf hosts add/list/rm/test/import` |
| 进程 | `sshf start` / `stop` / `status` |

各命令的完整标志见 `references/cli-reference.md`。

## 常见任务

**在多台机器执行命令**(并发默认 5,`-c` 调整;逐机输出块 + 汇总):
```bash
sshf batch --group web -c 8 'uptime'
sshf batch --host a,b --host c 'df -h'   # --host 可重复或逗号分隔
```

**把一个大文件发给很多机器**(磁盘只读一次,慢机不阻塞快机):
```bash
sshf distribute ./big.iso /data/big.iso --group prod --concurrency 16
sshf distribute ./big.iso /data/big.iso --group prod --resume   # 失败/中断后续传
```
`distribute` 三参数写法 `distribute <local> <host> <remote>` 可只发一台。

**持续性的多步操作(需要 cd/env 延续)**——用 session,不要用多条 `exec`:
```bash
id=$(sshf session open web-1)
sshf session send "$id" 'cd /srv/app'
sshf session send "$id" 'pwd'          # 输出 /srv/app
sshf session close "$id"
```

**跟随日志**。`tail` 是长驻的:agent 里务必用 `--for <秒>` 避免挂住。输出的 `#seq` 是重连游标:
```bash
sshf tail web-1 /var/log/app.log --for 5
# 断线重连:sshf tail --task <task_id> --since <最后收到的 seq>
```

## 输出与退出码约定

- **`exec` 用远端命令的退出码作为 CLI 退出码**(连接层失败为 1),便于脚本判断。
- **`batch` / `distribute` / `hosts test` 只要有失败机器(连接错误或非零退出码)就退出码 1**,并在输出尾部打印失败机器名单;全部成功才 0。
- **逐机输出块**格式为 `=== <host> ===`,便于人读与机器解析。
- **进度/长任务输出走 stderr**(如 SFTP 进度在 stderr 原地刷新),stdout 保留给数据本身,可安全用于管道。
- `exec` 的 stdout/stderr 原样透传,可直接 `| grep`。

## 坑与注意

- **SFTP 相关命令(distribute/upload/download)要求目标 sshd 启用 `Subsystem sftp`**。报错 `subsystem request failed` 时,让用户在其 sshd_config 里加 `Subsystem sftp <path>` 并重启 sshd。
- **目标 sshd 重启后,daemon 里是陈旧连接** → 先 `sshf stop` 再重试,强制重新握手。
- **`hosts add` 缺字段会交互式提示**。agent 场景请把字段用标志一次性给齐(`--addr/--user/--port/--key/--agent/--password/--group`),否则会等待输入。`hosts import` 完全非交互。
- **凭据优先级:agent > key > password**。没有 key/agent 时才会用密码;写明文 `--password` 会落盘并告警——优先用私钥或 ssh-agent。
- **`hosts import` 只导入具体别名**:`Host *` 通配、`Include`、`Match`、`ProxyJump`/`ProxyCommand` 会被跳过并告警;已存在的同名主机默认跳过,`--overwrite` 覆盖,`--dry-run` 只预览。
- **默认 shell 不硬编码 bash**:命令原样交给目标登录 shell(同 `ssh host cmd`),所以 Windows 目标也能用。需要确定性 shell 时用 `--shell auto|bash|sh|cmd|powershell|pwsh` 或 hosts.yaml 的 `shell` 字段。
- **Windows 目标输出 GBK 会自动转 UTF-8**,无需手工处理。
- **隔离实验**用 `SSHFLEET_HOME=<tmpdir>` 或 `--config <tmp>/hosts.yaml`,不会污染用户真实 `~/.sshfleet/`。

## 参考

- 完整命令、标志、hosts.yaml 字段、协议命令、状态文件与退出码:`references/cli-reference.md`
- 项目设计决策与里程碑:仓库根 `README.md`
