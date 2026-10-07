# sshf CLI 完整参考

本文件是 `SKILL.md` 的按需展开:完整命令与标志、hosts.yaml 字段、状态文件、协议命令、退出码。

## 全局标志

| 标志 | 说明 |
|---|---|
| `--config <path>` | hosts.yaml 路径;默认 `~/.sshfleet/hosts.yaml`(受 `SSHFLEET_HOME` 影响) |
| `-h, --help` | 显示帮助 |
| `-v, --version` | 显示版本 |

环境变量:`SSHFLEET_HOME` 覆盖状态目录 `~/.sshfleet/`(清单、token、pid、port 全在其中)。

## 进程管理

| 命令 | 说明 | 标志 |
|---|---|---|
| `sshf start` | 启动 daemon(监听 127.0.0.1,写 token/pid/port 文件) | `--foreground` 前台运行不 detach |
| `sshf stop` | 停止 daemon;未运行则提示并成功返回 | — |
| `sshf status` | 版本、运行时长、连接池表(HOST/STATE/CHANNELS/RECONNECTS/DIALED)、活动任务 | — |

daemon 空闲(无客户端、无活动任务)会自动退出。**任何命令在 daemon 未运行时都会自动拉起它**,所以通常不需要显式 `start`。

## hosts 子命令

| 命令 | 说明 | 标志 |
|---|---|---|
| `sshf hosts add <name>` | 新增/整体替换一台主机;缺失字段交互式补全 | `--addr` `--user` `--port` `--key` `--passphrase` `--password` `--agent` `--group` |
| `sshf hosts list` | 列出主机与分组(缺失的 user/port 会补默认值) | `--path` 只打印配置文件路径 |
| `sshf hosts rm <name>` | 删除主机,并同步清理所有分组里的引用 | — |
| `sshf hosts test [name...]` | 逐台建连 + 认证;输出 `OK 耗时` 或 `FAIL 原因` | `--group` |
| `sshf hosts import` | 从 OpenSSH 配置导入主机 | `--file` `--group` `--overwrite` `-n/--dry-run` |

- `hosts test` 至少要给主机名或 `--group`;有不可达主机时退出码 1。
- `hosts add` 输出示例:`已添加主机 web-1 (deploy@10.0.0.11:22, 凭据: key)`;若用明文密码会告警。

### hosts import 细节

解析 `~/.ssh/config`(**默认路径**,可用 `--file` 指定)的子集:

- **支持**:`Host`(一个块内可多个别名)、`HostName`、`User`、`Port`、`IdentityFile`;遵循 OpenSSH「首个取值生效」;识别文件开头全局块、`Host *` 通配、`!` 否定;`Key=Value` 语法与引号路径。
- **不支持**(告警并跳过其效果):通配别名(不展开为具名主机)、`Include`、`Match`、`ProxyJump`、`ProxyCommand`。
- 未写 `HostName` 时用**别名**当地址(同 `ssh` 语义)。
- 已存在同名主机默认**跳过**;`--overwrite` 覆盖;`--dry-run` 只预览不写盘。

## 执行

### `sshf exec <host> <cmd>`

单机执行,stdout/stderr 原样透传,退出码 = 远端命令退出码。

| 标志 | 默认 | 说明 |
|---|---|---|
| `-t, --timeout <duration>` | `0` | 单机超时(`0` = 无超时);如 `30s`、`2m` |
| `--shell <kind>` | 空 | 覆盖主机的 shell 设定:`auto/bash/sh/cmd/powershell/pwsh` |

### `sshf batch [--host h]... [--group g] <cmd>`

多机并发执行;输出逐机块 `=== host ===` + 汇总统计;有失败机器退出码 1。

| 标志 | 默认 | 说明 |
|---|---|---|
| `--host <h>` | — | 目标主机,可重复或逗号分隔 |
| `--group <g>` | — | 目标分组 |
| `-c, --concurrency <n>` | 5 | 并发度 |
| `-t, --timeout <duration>` | `0` | 逐机超时(互不影响) |
| `--shell <kind>` | 空 | 覆盖 shell |

`--host` 与 `--group` 至少给一个,取并集去重、按名排序。单台失败不影响其它机器。

## 文件

### `sshf distribute <local> <remote> [--host h]... [--group g]`

把一个本地文件并发分发到多台。**磁盘只读一次**、每条目标独立管道(慢机不阻塞快机)、逐机 offset 续传。

| 标志 | 默认 | 说明 |
|---|---|---|
| `--host <h>` | — | 目标主机,可重复/逗号分隔 |
| `--group <g>` | — | 目标分组 |
| `--concurrency <n>` | 16 | 并发度(只限制建连) |
| `--resume` | false | 对远端已有文件从 offset 续传 |

也支持三参数写法 `distribute <local> <host> <remote>` 指定单台(此时 `remote` 是最后一个参数,`<host>` 是中间参数)。

### `sshf upload <host> <local> <remote>` / `sshf download <host> <remote> <local>`

单机 SFTP 传输,进度在 stderr 原地刷新,结束时 stdout 打印 `host: 传输 <大小>,耗时 <时长>`。

> SFTP 要求目标 sshd 启用 `Subsystem sftp`。

## 会话与跟随

### `sshf session open <host>`

打开持久 shell,返回 `session_id`(会话存活于 daemon,CLI 退出后仍在)。

- `sshf session send <id> <input>` — 写入输入并回本次输出;`-n/--newline`(默认 true)在末尾追加换行。
- `sshf session close <id>` — 关闭会话。
- `sshf session list` — 列出活动会话(id / host / 已开时间 / 空闲时长)。

`cd`、环境变量等状态在同一会话内延续;这正是不能用多条 `exec` 代替的地方。

### `sshf tail <host> <file>`

跟随远端文件新增行(远端跑 `tail -f`),每行前带 `#seq` 序号。

| 标志 | 默认 | 说明 |
|---|---|---|
| `--for <秒>` | `0` | 最长跟随秒数(`0` = 直到 Ctrl-C);agent 里建议设置 |
| `--task <id>` | — | 重连到已有 tail 任务 |
| `--since <seq>` | `0` | 只回放 seq 大于该值的缓冲行(配合 `--task`) |

daemon 侧保留环形缓冲(约 2000 行),断线重连用 `--task` + `--since` 从缓冲补回缺口,不丢不重。首次调用会在 stderr 打印 `# task_id=...`。

## 补全

`sshf completion <bash|zsh|fish|powershell>` 生成补全脚本到 stdout。

## hosts.yaml 结构

```yaml
defaults:              # 可选:填充到未显式设置该字段的主机上
  user: deploy
  port: 22
  agent: true

hosts:
  web-1:
    addr: 10.0.0.11    # 必填:host(不含端口)
    user: deploy       # 缺省用当前 OS 用户
    port: 2222         # 缺省 22
    key: ~/.ssh/id_ed25519      # 私钥路径(支持 ~)
    passphrase: ""     # 私钥口令
    password: ""       # 明文密码(落盘,会告警)
    agent: false       # 允许走 ssh-agent
    cipher: [aes128-gcm@openssh.com]   # 协商优先级覆盖(空=默认)
    shell: auto        # 执行命令所用 shell;见下

groups:
  web: [web-1, web-2]
```

- **凭据优先级:`agent` > `key` > `password`**。至少要有一种;都没有则报错。
- **`shell`** 取值 `auto`(默认,原样交给目标登录 shell)/`bash`/`sh`/`cmd`/`powershell`/`pwsh`。除 `auto`、`cmd` 外,命令作为脚本经 stdin 喂给解释器以规避跨 shell 引号差异。
- 写入是原子的(临时文件 + rename),文件权限 0600(Unix)。
- 注意:`hosts add/rm/import` 会用 yaml 重新序列化,**丢失原文件里的注释与键顺序**——全手写维护的 hosts.yaml 建议少用这些命令改。

## 状态文件(`~/.sshfleet/`)

| 文件 | 内容 |
|---|---|
| `hosts.yaml` | 主机清单 |
| `token` | daemon 启动时生成的一次性 IPC token(客户端首字节校验,失败即断开) |
| `daemon.pid` | daemon 进程 PID |
| `daemon.port` | daemon 监听端口(127.0.0.1) |

## 协议命令集(v1,CLI 只是它的薄封装)

`status` `exec` `batch` `dist_start` `dist_status` `dist_cancel` `sftp_put` `sftp_get` `session_open` `session_send` `session_close` `session_list` `tail_start` `tail_stop` `hosts_test`。

CLI 与(未来的)MCP 工具一一映射到上述命令,SSH 逻辑只在 daemon 内。

## 退出码

| 情形 | 退出码 |
|---|---|
| 成功 | 0 |
| `exec` 远端命令非零退出 | 远端退出码 |
| 连接层失败(任意命令) | 1 |
| `batch` / `distribute` / `hosts test` 有失败机器 | 1 |
| CLI/参数/配置错误 | 1,stderr 打印 `错误: ...` |
