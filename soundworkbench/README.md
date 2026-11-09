# soundworkbench/cc-connect：改动与维护

> English summary: this fork carries our patches on top of the upstream cc-connect release v1.5.0, on the `local` branch. `soundworkbench/build.sh` tests the touched packages and builds one executable, versioned `<upstream version>-sw.<n>`.

## 为什么有这个分叉

我们用 cc-connect 把 Claude Code、Codex 等编程代理接到飞书，几个代理同时长期在线。上游的进度显示、插话、排队和定时任务不够这种用法，所以在上游发布版上加了补丁。补丁放在 `local` 分支：`local` = 上游发布标签 `v1.5.0` + 我们的提交。`main` 跟上游，不放我们的改动。

可执行文件只从 `local` 构建。全部提交见 `git log --first-parent v1.5.0..local`。

## 改动

### 进度卡

- 中文进度：代理在调用工具前没有输出文字时，卡片显示中文的工具和命令进度（`agent/claudecode/chinese_progress.go`，`core/tool_progress.go`，`core/shell_progress.go`）。
- 只追加：进度按时间往下追加，不改写已经显示的行。配置 `progress_history = "append" | "recent"`，`tool_progress_lines`（0 关闭），`keep_progress_card`（回复后是否保留进度卡）。
- 中途内容并入最终回复：中途有较长的实质文字或 Markdown 表格时，最终回复里保留它们，撤回进度卡不会丢内容。配置 `merge_progress_into_final`、`merge_progress_min_chars`、`merge_progress_tables`（`core/merge_progress.go`）。
- 最终回复与撤回的顺序：`final_reply_order = "recall_first"`（默认，先撤回进度卡再发回复）或 `"send_first"`（先发回复再撤回）。两种顺序在手机和手表上的通知效果不同。
- Codex 命令标签：只显示程序名和已知子命令，参数、路径、带口令字样的命令一律只显示“执行命令”（`agent/codex/command_progress.go`）。已知子命令表在 `progressSubcommands`，按需要增减。

### 运行中插话

- Claude Code：代理在回答时，新消息直接写进当前回合（`agent/claudecode/steering.go`），图片也可以；大图只传文件路径。送达后在原消息上加表情回执（`core/steering.go`）。
- 首条写入期限：每个回合的首条输入有期限，默认 60 秒，配置 `first_write_timeout`。超时、管道断开或只写了一半时，判定进程失效并结束整个进程组；下一条消息用 `--resume` 续上会话，并提示用户重发。
- 首条大消息写得慢时，插话不再等全局锁：插话返回“未接受”，消息进队列，下一回合送达。
- Codex：在 agent 的 options 里设 `backend = "app_server"`，运行中的消息经 `turn/steer` 并入当前回合。

### 消息排队

- 排队消息写到磁盘（`core/queue_store.go`），重启后恢复。`/stop` 只停当前回合，队列保留；`/new` 丢弃队列时列出丢了哪几条。见 [docs/queued-messages.md](../docs/queued-messages.md)。
- 合并排队消息：同一会话排队的多条消息合成一个回合，每条标出来源和时间。顶层配置 `[queue_batch]`：`enabled`（默认开）、`max_messages`（20）、`max_chars`（20000）、`hold_seconds`（60，自动消息最多等这么久再开回合）、`immediate_sources`（哪些来源立即开回合，默认 `["human"]`）。
- 单轮最长时长 `max_turn_time` 对每条排队消息重新计时。原来连续排队的几轮会累计计时，被误判超时。

### 定时与自动消息

- 自动消息（定时 timer/cron、角色间转告、本机助手接口、heartbeat、webhook）不发“思考中”占位卡和流式预览，只在结尾发一条。结尾是 `NO_REPLY` 时一条也不发。合并的一批里只要有一条真人消息，就按真人回合处理（`core/queue_batch.go` 的来源判断）。
- 定时提示带上来源，开始提示带图标（`core/scheduled_prompt.go`，`core/scheduled_start_notice.go`）。
- 静音的定时任务只隐藏开始提示，代理的回复和附件照常送达，静音不带到下一轮。
- `timer --at` 的绝对时间必须带时区偏移（`+08:00`、`Z` 等），不确定时用 `--delay`。创建和列表输出另外显示 UTC+8 时间（`core/timer.go` 的 `FormatTimerBeijingTime`）。

### 飞书

- 私聊话题：`p2p_thread_isolation = true` 时，私聊里每个话题一个会话；普通消息和引用回复仍在原会话。
- 交互卡片和合并转发：读出卡片里的文字、图片和嵌套的合并转发，交给代理（`platform/feishu/interactive_content.go`）。跨应用卡片的图片飞书不让下载，会写明原因。

### 用量与档位

- `/usage`：Claude Code 改为直接查账号的用量接口，读不到再用终端探测（`agent/claudecode/claude_usage_*.go`）。OpenCode 新增 Grok 订阅的额度窗口，需要在 agent options 里设 `usage_cmd` 指向已登录的 Grok 客户端（`agent/opencode/usage.go`）。
- 思考档位：Claude Code 和 Codex 按各模型官方支持的档位提供 `/effort` 选项（`effort_models.go`）。

### 本机助手接口

本机 Unix 套接字接口（`cc-connect send` 用的同一个）新增三个端点（`core/assistant_api.go`）：

- `/assistant/inject`：往已有的会话投递一条消息，按来源排队，可标记紧急。
- `/assistant/state`：读会话状态。
- `/assistant/rotate`：带一段摘要换新会话。

请求带 `request_id`，重复请求返回第一次的结果。只能投到已有的绑定，不会新建路由。

### Codex 后台任务唤醒

可选，`app_server` 后端。Codex 的后台子代理完成后，自动开一个回合让父会话处理结果。有轮数和时长上限，真人的 `/stop` 或停止类消息会关掉它。见 [docs/codex-background-wakeup.md](../docs/codex-background-wakeup.md)。

### 关停与启动

- 停止服务前先等正在跑的回合结束（`core/shutdown_drain.go`）。
- 异常退出留下的实例锁文件可以复用（`cmd/cc-connect/instance_lock.go`）。

### 其他

- 定时、心跳、cron 的代码从 `core/engine.go` 拆到 `engine_timer.go`、`engine_heartbeat.go`、`engine_cron.go`。行为不变，但上游改 `engine.go` 的这几块时，变基会冲突。
- `tests/release_local/` 下的 live 测试默认跳过，只在设了对应环境变量时连真实后端跑。

### 是否提给上游

都没有提。哪些适合提给上游，待确认。上游合入同等功能后，在下一次变基时丢掉我们的那个提交。

## 维护 local 分支

远端约定：`upstream` = chenhg5/cc-connect，`origin` = soundworkbench/cc-connect。

```sh
git remote add upstream https://github.com/chenhg5/cc-connect.git
git fetch upstream --tags
git fetch origin
```

### 升级到上游新版本

例：从 v1.5.0 升到 v1.6.0。

```sh
git tag sw/v1.5.0 origin/local              # 留住旧的 local，回退时能重新构建
git push origin sw/v1.5.0
git switch -C local origin/local
git rebase --onto v1.6.0 v1.5.0 local
```

`local` 里有合并提交。`git rebase` 会把它们拉平成一条线，原来在合并时解决过的冲突（主要在 `core/cuj_test.go`、`core/engine.go`）会在对应的提交上再出现一次。

冲突时：

1. 按上游新代码改我们的提交，保持行为不变。改完跑相关包的测试（`build.sh` 里的包列表）。
2. 根目录 `README.md` 冲突时，保留我们的版本（`git checkout --theirs README.md`，变基时 theirs 指我们的提交）。
3. 上游已经有同等功能时，`git rebase --skip` 丢掉这个提交，并在本文“改动”里删掉它。
4. 改不动又不能丢时，停在当前版本，不升级。

冲突太多时，可以先把我们的改动压成一个提交再变基，只解决一轮冲突，代价是丢掉逐个提交的历史：

```sh
git switch -C local-squash origin/local
git reset --soft v1.5.0 && git commit -m "soundworkbench patches on v1.5.0"
git rebase --onto v1.6.0 v1.5.0 local-squash
```

变基后用 `soundworkbench/build.sh` 构建并跑测试，通过后推送：

```sh
git push --force-with-lease origin local
```

提交信息沿用上游的写法：`<类型>(<模块>): <一句话描述行为>`，正文写问题和做法。

## 构建

```sh
soundworkbench/build.sh            # 从 local 构建
soundworkbench/build.sh <提交>     # 从指定提交构建
```

脚本做三件事：

1. 用 `git archive` 取出这个提交的源码，工作树里没提交的改动不进构建。
2. 在 `golang:1.25` 容器里跑改动相关包的测试：`./agent/claudecode/ ./agent/codex/ ./agent/opencode/ ./core/ ./platform/feishu/ ./config/ ./cmd/cc-connect/`。不过就停，退出码 3。
3. 用构建标签 `no_web,goolm`、`CGO_ENABLED=0` 编译出一个可执行文件，放在 `dist/`，打印它的 sha256 和版本。

版本号规则：

| 项 | 写法 | 例 |
|---|---|---|
| 程序版本（`cc-connect --version`） | `<上游版本>-sw.<序号>` | `v1.5.0-sw.41` |
| 文件名 | `cc-connect-<版本>-<系统>-<架构>` | `cc-connect-v1.5.0-sw.41-linux-amd64` |

上游版本取 `git describe --tags --match 'v*'`，即 `local` 下面最近的上游标签，所以要先 `git fetch upstream --tags`。序号是从这个标签到所构建提交的提交数，`local` 每加一个提交就变大，变基到新标签后重新计数。程序里另外记录提交号（`--version` 的 commit 一行）。

环境变量：`GOOS`、`GOARCH`（默认 linux/amd64），`CPUS`（容器可用的 CPU 数，默认 4），`GO_IMAGE`（默认 `golang:1.25`），`OUT`（输出目录，默认 `dist`），`SKIP_TESTS=1`（只编译，用于重建已测过的版本）。

不带网页管理界面（`no_web`）。需要网页界面时按上游 `Makefile` 先构建 `web/`。

## 换版与回退

换版：把新的可执行文件放到运行目录，旧文件改名保留，重启服务。验证：

- `cc-connect --version` 显示新版本。
- 在飞书里给每个项目发一条消息，代理正常回复，进度卡正常出现和撤回。
- `/usage` 能显示额度。

回退：换回旧的可执行文件并重启。旧文件已删时，从 `sw/<旧上游版本>` 标签或旧提交重新构建（`SKIP_TESTS=1 soundworkbench/build.sh <提交>`）。

会话、排队消息和配置都在数据目录里，换可执行文件不动它们。新版本新增的配置项在旧版本里会被忽略。

## 已知问题

- `timer` 的回显固定按 UTC+8 显示，没有做成配置项。
- `go build ./...` 在没有构建 `web/dist` 时会失败，这是上游的行为；用 `-tags no_web` 构建，或先构建网页。
