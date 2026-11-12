# soundworkbench/cc-connect：改动与维护

本分叉把补丁放在上游 cc-connect 发布版 `v1.5.2-beta.1` 之上，位于 `local` 分支。`main` 跟随上游，不含这些改动。发布产物只从已推送到 `origin/local` 的提交构建。查看全部提交：`git log v1.5.2-beta.1..local`。

建立这个分叉，是因为多个编码 Agent 全天在飞书中在线。这种用法需要更安静的进度显示、能并入进行中回合的消息、持久的队列，以及不会刷屏的定时回合。

## 改动

### 流式进度卡（飞书）

默认已打开：只写 `[stream_preview] enabled = true`，或干脆不写这一段，飞书就用这张卡；写 `segmented = false` 或 `step_list = false` 可关。这张卡是飞书 CardKit 流式卡（`platform/feishu/segmented_preview.go`、`core/streaming.go`）。

- 卡片开头只有一行：一个转圈 GIF 动画和“思考中…”。没有内容的元素不放进卡片，所以没有空行。
- Agent 写出的简短单行文字（最多 60 个字符）先不显示。随后有工具调用，它就成为一行带勾图标的已完成步骤；文字超过一行或带换行，就是回复，进正文；回合结束时也作为回复。转圈行不会闪现这类文字，正文也不会出现进度说明。
- 没有说明的工具调用自成一步，用工具的描述做标签：Bash 或 Agent 的描述、文件基本名、网页主机名或搜索词。当前工具以灰色显示在运行行下方。
- 已完成行和运行行共用一个图标位置，所以所有步骤文字对齐。卡片显示最近 5 个已完成行，更早的合并成一行“另有 N 步已完成”。图标上传失败时，卡片改用文本符号。
- 更长或多行的文字是回复，进入卡片正文。设置 `reveal_cps` 后，正文以稳定的速度逐字显示。事件循环从不等待卡片 API 调用。
- `end_mode = "settle"` 时的回合结束：短于 `notify_after_ms` 的回合在卡片内结束，卡片只留回复。更长的回合把卡片收成一行灰色的“已完成”，并把回复作为新消息发出，让手机收到通知。`end_mode = "recall"`时，卡片被撤回，回复作为新消息发出；撤回失败时，卡片变成回复，所以回复不会出现两次。
- 所有卡片都用 `width_mode = "default"`（固定的默认宽度），不用全宽。
- `trace_file` 把每个事件的流式耗时写成 JSON 行，用于调优。

不开 `step_list` 时，`segmented` 给出更简单的流式卡：在逐字显示的回复上方显示已用时间、步骤数和最近的步骤，结束时原地定稿。

### 失败可见（飞书）

模型服务出问题时，进度卡说清楚现在的情况，不再一直显示“思考中”。没有开关（`core/turn_failure.go`、`platform/feishu/turn_failure.go`、`platform/feishu/live_cards.go`）。

- 重试：Claude Code 每次自动重试失败的模型请求，都输出 `system/api_retry`（第几次、错误码、错误类别）。转圈行改成“模型服务连不上，正在重试（第 n 次）”；错误码 429 或类别 `rate_limit` 时改成“模型用量到了上限，正在等待”。下一个正常事件到来，转圈行恢复原来的步骤。
- 久等：回合 90 秒没有任何事件、且不是在等工具、权限或用户时，转圈行改成“还在等模型响应”。只改文字，不打断回合。
- 失败：CLI 重试用尽后，结果帧带 `is_error` 和 `api_error_status` / `api_error_code`，或前面的助手消息带 `error` 类别，这一轮按失败结束。卡片原地定稿：保留已写出的正文，加一行红字“这次没做成：模型服务暂时连不上”（用量上限、其他错误各有措辞）和“重试”按钮。点“重试”把原请求（含图片、文件）重新提交一次，卡片改成“已重新发送”，按钮去掉；同一个按钮只生效一次，1 小时内有效，重启后失效；失效的按钮点了只弹出提示“这个按钮已失效，请重新发送消息”。原请求连同附件留在内存里，最多 200 个、附件合计 256MB，超出时先丢最旧的，过期的每 5 分钟清一次；单个请求的附件超过 256MB 时，失败卡不带“重试”按钮。回合超过 1 分钟时，另发一条失败消息让手机响。进程中途退出、`idle_timeout_mins`、`max_turn_time` 同样走失败卡，第二行灰字写原因。
- 重启：有转圈的回合卡在 `data_dir/feishu/live-cards-<hash>.json` 登记，定稿、撤回或失败时移除。启动时把登记表里剩下的卡改成一行灰字“已中断，请重发”，再清空登记。上一个进程的 CardKit 序号已丢失，改卡用当前 Unix 秒数作序号，保证大于旧序号。

### 后台任务进度卡（飞书）

后台任务每个一张卡，原地更新，做完变成结果摘要。没有开关（`core/task_cards.go`、`agent/claudecode/tasks.go`、`platform/feishu/task_card.go`）。

- 数据：Claude Code 的 `system/task_started`、`task_progress`、`task_updated`、`task_notification` 帧（按 CLI 2.1.322 的 SDK 类型核对）转成 `EventTask`。前台回合和两轮之间的主动输出读取器都把它交给任务卡；它不算回合里的活动，不重置看门狗、不触发插话、不开始转发回合。
- 开卡：只给后台任务（`is_backgrounded`，或之后由 `task_updated` 改到后台）开卡，并且开始 10 秒后还在跑才开；`ambient`、`skip_transcript` 的看守类任务不开。卡片回复在触发它的那条消息下。每个会话同时最多 3 张，多出的不显示，只记日志。
- 卡片：标题“后台任务 · 正在进行：任务名”（加粗），让人一眼看出它在后台、不占着对话；当前步骤，带转圈图标，取 `task_progress` 的 `description` 去掉“Running ”前缀，即模型给命令写的中文说明（如“删除临时目录”）；不含中文或像命令原文（带 `|&;<>$=/` 引号等）时退回最近工具的中文标签（后台命令为“运行命令”）。任务名只取 `task_started` 和 `task_updated` 的 `description`，`task_progress` 的 `description` 是“当前在做什么”，不改任务名；灰字“已跑 N 分钟”（不到 1 分钟写“已跑不到 1 分钟”，不读秒，不显示步数）；“最近进展”；其下一行灰字“在后台进行，不影响聊天，你可以接着跟我说话；做完我会告诉你。要停的话直接跟我说。”（英文界面用同义英文）；折叠的“详情”（最近 10 个步骤）。卡上没有按钮。
- 最近进展：优先用 CLI 的 `task_progress.summary`。没有，或 CLI 的这一句超过 60 秒没更新时，用 `quick_line` 的小模型配置写一句：同一张卡两次补写至少隔 30 秒、且有新步骤才调用；提示里带上一句，要求不重复；小模型回复里同一句连写多遍的只留一遍；小模型写的过程中 CLI 来了新的一句，以 CLI 的为准；失败就略过。
- 停止：卡上不设停止按钮。要停就直接跟助理说，模型自己停掉后台任务，CLI 回 `task_notification`（`stopped`）后卡片定稿。适配层仍保留按任务停止的控制请求 `stop_task`（`claudeSession.StopTask`），目前没有界面调用它。
- 结束：卡片原地变成“后台任务 · ✓ 已完成：任务名”（绿色）、“后台任务 · 没做成：任务名”（红色）或“后台任务 · 已停止：任务名”，下面最多 3 行摘要（`task_notification.summary`），去掉灰字提示，保留详情。完成的卡只写一到两句：摘要本身是一行中文、最多两句、不含路径和代码时照原样写；摘要很长或像原始输出（多行、带路径或代码）时，请 `quick_line` 的小模型写一句中文概括，卡片定稿等它（最多等小模型的超时，默认 5 秒），失败就用摘要的第一句，截到 40 字；完整原文放进“详情”里的“原文”。没做成的卡不放英文原始报错：摘要本身是一行中文、不含路径和代码时照原样写，否则写一句中文（有退出码时写“命令出错结束（退出码 N），原始信息在“详情”里。”，没有时写“出错结束，原始信息在“详情”里。”），原文以纯文本放进“详情”里的“原文”。主动停止的任务按“已停止”（灰色）收尾，不算没做成：模型调了 `TaskStop`（或旧名 `KillShell`、`KillBash`），或桥发了 `stop_task`，之后这个任务回 `failed` 时改记为停止；后台命令以退出码 143、144 结束（被信号杀掉，11-11 实测停掉的命令回的是 144）也算停止；`/stop`（命令或卡片按钮）关掉进程时，它的后台任务卡写“已停止”，其他关闭路径仍写“已中断”。完成汇报照旧由主模型作为新消息发出。智能体进程一关闭，它的后台任务就跟着结束，还在跑的卡改成“后台任务 · 已中断：任务名”，并让出名额。所有关闭路径都会这样处理：`/stop`、`/new`、`/switch`、会话切换、空闲回收（`agent_session_idle_timeout_mins`；注意空闲回收不看后台任务，会连同后台任务一起结束）、重置队列、进程在回合中或两轮之间退出。桥重启时的遗留卡同样改写（见“失败可见”的登记表）。
- 频率：同一张卡两次更新至少隔 3 秒，期间的变化合并成一次；耗时每分钟刷新一次；内容没变的不重写。卡片 25 秒左右没有任何变化时（例如子代理在写东西、不调工具），给一处看得见的变化：步骤行在“仍在进行：<当前步骤>”和“<当前步骤>”之间切换，同时允许小模型不等新步骤补一句进展（仍受 30 秒间隔和 CLI 进展 60 秒内不补的限制）。任务卡不开流式模式（流式 10 分钟后自动关闭），每次整卡更新 CardKit 实体。

PLAN 第 6 节的疑问，代码层面能查清的部分（CLI 2.1.322）：

- 前台回合结束后还有没有进度事件：有。`task_*` 帧由 CLI 的全局输出队列发出（`enqueue`），不依赖进行中的请求；桥的 stdout 读取循环一直在读，两轮之间由主动输出读取器接住，现已转给任务卡。例外：上一轮异常结束后开始的新回合会先清空缓冲事件，任务卡少一次更新，下一个进度事件补上。实际节奏仍需实测。
- 进度摘要：子代理（`local_agent`）任务的 `summary` 只在 SDK `initialize` 控制请求带 `agentProgressSummaries: true` 时由 CLI 生成（与子代理同模型的分叉调用；不是心跳，等模型时可能延迟到 5 分钟）；MCP 后台任务的 `summary` 不需要选项。桥目前不发 `initialize`，所以这一期子代理任务的进展行由小模型兜底。是否开启、经公司 magpie 能否生成、花多少，实测后再定。另：`initialize` 里的 `perTaskStopAffordance` 声明后，`/stop` 会放过后台子代理；桥不声明，`/stop` 照旧连后台任务一起停。
- 纯问答回合里插话：插话是否写进当前回合不看有没有工具调用；CLI 的结果帧还没带上这条消息的编号时，桥延后结束回合（`inputPending`），所以留在同一张卡。CLI 在纯问答回合里何时读到插话，需实测。
- 撤回后会话里留什么、用户点开的折叠面板在卡片更新后是否保持展开：代码查不清，需在飞书实测。任务卡每次整卡更新，点开的“详情”可能被收起。

### 出站中文兜底

中文界面（项目语言为简体中文，或自动识别为简体中文）下，回合的最终回复、两轮之间的主动回复（后台任务完成、叫醒）和 `cc-connect send` 的文字，发出前检查一次（`core/outbound_chinese.go`）。没有开关；没配 `quick_line` 时不检查。

- 判断：先把代码块、行内代码、Markdown 链接、网址、路径、文件名和 `snake_case` 名称换成占位符 `⟦n⟧`，再数剩下文字里的拉丁字母和汉字。字母多于 40 个、且多于汉字的 2.5 倍，才改写。
- 改写：把带占位符的文字交给 `quick_line` 的小模型改写成简体中文，最多等 5 秒（`timeout_ms` 更短时按它）；代码、链接、路径不会发给小模型，改写后原样放回。改写失败、超时、改写后仍是英文、占位符少了或多了，都发原文。超过 1500 字的不改写（5 秒内写不完），直接发原文。
- 不改写：最近一条人发的消息不含汉字、且有 6 个以上字母时，认为对方在用英文，回复照原样发；之后对方发中文就恢复。卡片按钮、决定卡片、任务卡不经过这里。
- 会话历史和 CLI 里留的是助理的原文；与 `send` 发过的文字去重时也按原文比。
- 日志：INFO `outbound: chinese`，带会话、结果（`rewritten`、`kept`、`skipped`）、原因（`mostly_english`、`rewrite_failed`、`rewrite_not_chinese`、`placeholder_lost`、`person_wrote_english`、`too_long`）和耗时，不记正文。

### 小模型前台分流

没有回合在跑、但有后台任务在跑或有消息排队时，短消息先由 `quick_line` 的小模型判一次类，有两类不唤醒大模型（`core/triage.go`）。完全空闲的会话不分流，消息直接交给大模型，“查一下日志”这类短指令不等小模型。没有开关；没配 `quick_line` 时不分流。

- 问进度：12 字以内、含“进度、进展、好了吗、怎么样了、到哪了”这类词，或整条就是“更新”“加速”“快点”的消息，直接按问进度处理，不调小模型；“更新文档”这类带宾语的不算。其余由小模型判为“问进度 / 在忙什么 / 好了没”时同样处理。桥按自己的状态直接回一条文字：每个后台任务一行“后台任务「任务名」还在进行（已跑 N 分钟），现在在：步骤。”（最多列 3 个）。步骤用模型给工具写的中文说明（Bash 的 description 等）；只有“运行命令”这类通用词时，改用任务最近一句进展，再没有就用助理最近一条回复的第一句；都没有就不写“现在在”。再加“进展看上面的任务卡，做完我会告诉你。”；有排队时再说还有几条在排。数字和步骤都由桥填，小模型只负责分类。这条消息和回答都写进桥的会话历史（快速回复等用到的那份）；CLI 自己的对话记录里没有这一来一回。
- 寒暄、致谢：判为“纯寒暄或致谢”时，只在消息上加一个表情（`THUMBSUP`），不开卡；平台不支持表情时交给大模型。
- 硬规则：有待回答的问题时，除了问进度，其余一律交给大模型。待回答的问题指：权限请求或 AskUserQuestion 未答、未答的决定卡片或表单、或者上一条回复的最后 200 字里有问号。这样“好的”“可以”“谢谢”“嗯”这类可能就是回答的话不会被当成寒暄吞掉。问进度（“进度怎么样”“好了吗”“在忙什么”）不可能是对卡片或问题的回答，照常由桥直接回；否则一张几小时没人点的旧决定卡片会让每次问进度都去等大模型。小模型调用失败、超时（最多 2.5 秒；11-12 实测最快的小模型线路首字要 1.1–1.3 秒，1.5 秒会频繁超时），或答案不是这两类，也一律交给大模型。
- 范围：只在没有回合运行、并且有后台任务或排队时判；回合进行中的消息照旧并入或排队。只判 60 字以内、不带图片和文件、不以 `/` 或 `!` 开头的人发的消息。判完类才开卡，所以这时的短消息卡片最多晚出现 2.5 秒。
- 日志：每次分流都记 INFO `card: triage`，带会话、分类（`progress`、`greeting`、`other`）、问进度的判定方式（`matched=keyword | classifier`）和原因（如 `answered_from_bridge_state`、`reaction_only`、`pending_question_last_reply_asks`、`classifier_failed`），不记正文；配了 `trace_file` 时也写入（事件 `triage`）。

### 进行中的卡跟到最下面（飞书）

回合进行中又发的消息会把进度卡顶上去。插话被并入当前回合后，进度卡在最下面重发，旧卡编辑成一行灰字“↓ 已移到下方”，不撤回。没有开关（`core/preview_follow.go`、`platform/feishu/preview_move.go`）。

- 新卡回复刚发的那条消息，直接带上已有的步骤和已收到的全部正文，不重新打字；之后的更新都写新卡。挪卡前等旧卡上在途的正文更新落地，再切到新卡，所以不会有内容写到旧卡上。旧卡先关流式，再整卡改成一行；有转圈卡的登记表同步换成新卡。
- 一阵连发只挪一次：插话被收下后先不挪，等 2.5 秒；等待中又有插话就重新计时，计时到了挪一次，挪到最后一条下面。等 2.5 秒而不是 1.5 秒，是因为 11-11 实测连发的间隔是 1.3 秒和 1.6 秒，1.5 秒会把这一阵拆成两次。回合的第一张卡出现后 10 秒内不挪，到点再挪。一个回合的卡一般只挪一次：挪过之后 60 秒内的插话不再挪卡，留在卡片下面；60 秒后再有插话，照上面的规则再挪一次。11-11 第二次实测的插话在 +13.7、+18.7、+25.4 秒，间隔都超过 2.5 秒，原来挪了两次。
- 卡片已经是会话里最新的一条消息时不挪（挪之前用读历史消息接口查最新一条；查到的就是这张卡，说明下面没有别的消息）。这个接口需要应用有读历史消息的权限；查不到时（缺权限或接口出错）跳过这条检查，照常挪，并记一行 INFO 日志。
- 只在插话被 CLI 收下时挪。回合已结束、排队开新回合的消息、机器人用 `send` 发的消息，都不挪。不比较消息与卡片的时间（飞书和本机的时钟可能有偏差）；卡片刚出来前就发出的消息，最多多挪一次。群聊里桥只收到 @ 机器人的消息，别人不 @ 的发言不会触发。
- 只对 CardKit 流式卡生效；已退回整卡更新的卡不挪。
- 日志：挪卡（`card: card_move`，含新旧卡片的消息编号和原因）、因已在最下面而不挪（`card: card_move_skipped`）、开任务卡（`task_card_open`）、任务卡更新（`task_card_update`）和结束（`task_card_end`）、失败卡（`fail_card`，原因为 `usage_limit`、`model_unreachable`、`model_error`、`agent_exited`、`idle_timeout` 或 `turn_error`）、点重试（`retry`，`resent` 或 `button_expired`）都记 INFO，只记会话、卡片消息编号和原因，不记正文。配置了 `trace_file` 时同样写入，事件名相同。

### 其他预览选项

- `stream_partial_text = true`（Claude Code Agent 选项）传入 `--include-partial-messages`，把文本增量流式送入预览。增量只用于预览；回复仍是最终文本。
- `keep_preview_on_finish = true`（飞书选项）把预览卡变成最终回复。原地编辑不会通知，所以只有在卡片仍是聊天中最新的消息、且回合在 `keep_preview_max_secs`（默认 120）内完成时才这样做。否则撤回卡片，把回复作为新消息发出。
- `typing_marker` 在运行中的预览下方显示一个固定标记（例如 `...`）。最终回复不带标记。

### 表情回应

- 新消息上的表情回应（`reaction_emoji`，默认 `OnIt`，`"none"` 表示关闭）在发出预览卡之前添加。
- 注入进行中回合的消息，在被接收时得到表情回应。Agent 读入后，先加上 `DONE`，再移除旧表情，所以消息始终有表情回应。
- `cc-connect send --react <emoji>` 给发起本回合的消息加一个表情回应。

### 回合中的消息

- Claude Code：Agent 工作时发的消息会写入当前回合（`agent/claudecode/steering.go`、`core/steering.go`）。图片和文件保存在工作目录下，注入的文本带有它们的路径。大图只传路径。
- 不能立即注入的消息留在队列中，在每个工具边界按先进先出的顺序重试。已注入的消息在写入之前就从队列快照中移除，所以重启不会重放它。
- 每个回合的第一次写入有截止时间 `first_write_timeout`（默认 `60s`）。超时、管道断开或写入不完整时，进程组被结束；下一条消息用 `--resume` 恢复会话，并请用户重发。第一次写入较慢而占住输入时，注入的消息不被接收，而是进入队列，不会等待。
- CLI 自己发起的回合（例如后台任务通知）会在它回放我们的消息之前产生结果，这个结果不会结束我们的回合。只有这样的较早结果以静默结束时才被丢弃。
- Codex：在 Agent 选项中设置 `backend = "app_server"`；消息通过 `turn/steer` 并入回合。

### 私聊重启补收（飞书 / Lark）

飞书平台 options 的 `p2p_catchup` 默认是 `true`：

```toml
[[projects.platforms]]
type = "feishu"
[projects.platforms.options]
app_id = "cli_xxx"
app_secret = "xxx"
p2p_catchup = true
```

重启后的 10 分钟内旧私聊会正常处理；群聊仍丢弃启动前的旧消息。消息编号在 `data_dir/feishu/p2p-<hash>.json` 中持久去重，文件按应用、域名和项目隔离，以同目录临时文件和原子重命名更新。最多保留 10,000 个编号，11 分钟后淘汰；私聊类型记录也最多保留 10,000 个，超限淘汰最久未活跃的记录。状态不含凭据或消息正文。

WebSocket 真正连上后，按项目已有会话记录（包括多工作区记录）补拉上次心跳之后、最多最近 10 分钟的私聊用户消息。只补拉曾经由接收事件确认是私聊的会话，不扫描远端会话列表，不补拉群聊或机器人消息。启动期间的新私聊暂存，与历史消息按时间排序后经过原有消息处理流程，避免新消息提前推进迟到消息过滤的时间水位。心跳每 15 秒原子更新；恢复完成前保留旧心跳。

首次启用时尚无心跳和私聊类型记录，只建立基线，之后的重启开始补收。第一次启用前已存在的会话，需要先收到一次私聊事件才会成为补拉对象。设 `p2p_catchup = false` 可恢复原来的启动时间过滤规则，并关闭补拉和此状态文件的更新。Webhook 模式不主动补拉。

历史接口使用应用身份，需要 `im:message`、`im:message:readonly`、`im:message.history:readonly` 中任意一项。缺权时仅记录一行 Warn 并跳过本次补拉，正常连接和实时消息处理继续工作。`cc-connect feishu new` 使用飞书服务端 `PersonalAgent` 模板，源码没有显式 scope 列表，因此无法仅凭源码确认每个个人机器人的实际授权；本改动不修改创建流程。如果模板没有上述权限，需要 Jason 批准后增加 `im:message:readonly`。

去重在消息交给处理流程前落盘，以避免重推造成重复回答；落盘后、处理完成前崩溃仍可能丢失该条消息。这不是对 Agent 回复的事务保证。

本地验证：`CPUS=4 soundworkbench/check-p2p-catchup.sh` 会先证明旧提交 `4a245e70` 丢失私聊，再运行当前补收测试；只使用 Docker 和模拟 API。

### 首次欢迎语（飞书 / Lark）

从没聊过天的机器人，主动给它唯一的主人发一条私聊欢迎语（`platform/feishu/welcome.go`）。默认开启，不需要新配置。

- 主人：`allow_from` 里唯一的 `ou_` 开头的 open_id。`allow_from` 为空、为 `*` 或有多个人时不发，日志记 `welcome: skipped reason=no_owner | multiple_owners | owner_not_open_id`。
- 只在“从没聊过”时发：项目会话记录（包括多工作区记录）里没有任何带历史的会话，私聊补收也没有私聊记录。
- 只发一次：标记文件 `data_dir/feishu/welcome-sent`，发送前先以独占方式创建，同一个 `data_dir` 下的多个项目也只发一次。
- 时间：北京时间周一到周五 09:30–19:30。窗口内启动时 1 分钟后发；窗口外排到下一个工作日 09:30，再加 0–20 分钟随机延迟。发送前再查一次是否已经聊过。不考虑法定节假日。
- 失败：记 WARN，删掉标记，下一个窗口重试；连同重启一共最多 3 次（次数记在 `data_dir/feishu/welcome-attempts`），第 3 次失败后保留标记不再发。
- 日志：`welcome: scheduled`、`welcome: sent`、`welcome: skipped reason=...`（还有 `already_sent`、`attempts_exhausted`、`has_conversation`、`records_unreadable`），不记 open_id。
- 文字：默认按项目语言（`language`）取内置文案，有简体、繁体、英、日、西五种；没有引擎语言时飞书用简体、Lark 用英文。`welcome_message` 可改成自己的文字；`welcome = false` 关闭。

```toml
[projects.platforms.options]
allow_from = "ou_xxx"
# welcome = false
# welcome_message = "你好，有事直接跟我说。"
```

### 队列

- 排队消息写入 `data_dir/queues/`，重启后恢复（`core/queue_store.go`）。`/stop` 只结束当前回合并保留队列；`/new` 丢弃队列，并列出被丢弃的内容。见 [docs/queued-messages.md](../docs/queued-messages.md)。
- 合并：同一会话中连续排队的消息按顺序作为一个回合发给 Agent，每一段的标题带序号、来源和 `HH:MM` 时间（Asia/Shanghai）。在顶层的 `[queue_batch]` 段设置。
- `max_turn_time` 对每条排队消息重新计时。

### 静默回复和自动回合

- 整个回复只有 `NO_REPLY` 时，不发任何内容。这个标记从不渲染进卡片；重复出现（`NO_REPLYNO_REPLY`，即一个回合结束两次）或后面跟着页脚或上下文指示时也一样。
- 自动回合（定时器、cron、其他机器人转来的消息、本地助手 API、心跳、webhook）没有占位消息，也没有预览卡。它们在结束时一次发出整个回复；回复为 `NO_REPLY` 则不发。含一条人工消息的批次算作人工回合（`core/queue_batch.go`）。
- 定时提示带有来源，开始通知带有图标（`core/scheduled_prompt.go`、`core/scheduled_start_notice.go`）。
- 被静音的定时任务只隐藏开始通知。回复和附件照常送达，静音不会延续到下一个回合。
- `timer --at` 必须带时区偏移（`+08:00`、`Z`）；否则用 `--delay`。创建和列表的输出也显示 UTC+8 时间。

### 早期的进度显示（quiet 模式，无步骤列表）

- Agent 在工具调用前没有写文字时，卡片用中文显示工具或命令（`agent/claudecode/chinese_progress.go`、`core/tool_progress.go`、`core/shell_progress.go`）。
- 进度按时间顺序追加，从不改写：`progress_history = "append" | "recent"`、`tool_progress_lines`（0 表示关闭）、`keep_progress_card`。
- 较长的实质性进度文字或 Markdown 表格保留在最终回复中，所以撤回卡片不会丢失内容：`merge_progress_into_final`、`merge_progress_min_chars`、`merge_progress_tables`（`core/merge_progress.go`）。
- `final_reply_order = "recall_first"`（默认）或 `"send_first"`：两种顺序在手机和手表上的通知方式不同。
- Codex 命令标签只显示程序名和已知子命令（`agent/codex/command_progress.go` 中的 `progressSubcommands`）；参数和路径从不显示。

### 飞书

- `p2p_thread_isolation = true`：私聊中每个话题一个会话。普通消息和引用回复留在已有会话中。
- 交互卡片和合并转发被读成文本、图片和嵌套转发，供 Agent 使用（`platform/feishu/interactive_content.go`）。来自其他应用的卡片中的图片无法下载；文本中会说明这一点。
- 决策卡（`core/decision.go`，用 `[decision] enabled = true` 开启）：`cc-connect send --decision card.json` 发出一张卡片，每个选项一个按钮。JSON 含 `question`、`options`（推荐项在前）、`note`、`no_default` 和 `deadline_minutes`。一次点选，或截止时间到后的默认选项（`default_hours`，12），会作为消息回到会话，卡片被改写，不能再次使用。
  - `"danger": true`：删除、花钱、对外发送这类做了撤不回的操作。卡片标题为红色“需要你确认”，按钮变红，点了先弹确认框，确认后才算数；强制没有推荐项、没有截止时间（等同 `no_default`），不会到期自动处理。
  - `"multi": true`：可以多选。卡片改成下拉多选框，下面一个“提交”按钮；没有推荐项和截止时间。回到会话的消息把选中的项按原顺序用“、”连起来，例如“【决定】周报包含哪些部分？：进展、计划”。
  - 选项超过 6 个时，单选也自动改成下拉框加“提交”按钮；最多 20 个选项。没选就提交时，飞书前端先拦下；绕过时只弹提示“请先选择”，卡片不变。
  - 下拉和提交按钮用通用的表单元素 `core.CardForm` 渲染；提交时飞书回调带 `form_value`，桥把它以 JSON 接在动作后面（`act:/decision-form <id> {"choice":[...]}`）。
- 卡片归属（飞书）：卡片按钮带着它所属的会话（`session_key`）。按人分的会话（`feishu:<群>:<用户>`）里，别人点了这张卡，只给点的人弹一个提示“这个按钮是给 <名字> 的”，卡片不变，按钮也不生效；名字 1.5 秒内查不到就写“这个按钮不是给你的”（查询会在后台完成并缓存，下一次就有名字）。整群共用一个会话（`share_session_in_channel`）或按话题分的会话没有单一归属，照旧谁点都算。不在 `allow_from` 里、又不属于上面情况的点击，不回提示：同一个应用可能挂着多个项目，它们轮流处理回调，提前回提示会挡住真正负责的项目。日志记 `card: button_not_owner`。
- 没处理的点击都记一行 INFO：飞书侧 `card: callback_dropped`，原因 `reason` 为 `empty_event`、`chat_not_allowed`、`user_not_allowed`、`no_action`（回调里没有按钮的 value，带按钮 tag、name 和表单字段数）、`no_card_update`、`no_handler`、`unknown_action`；核心侧 `card: decision_dropped`、`decision_form_dropped`、`form_dropped`，原因如 `unknown_decision`、`unknown_form`、`form_values_unreadable`。日志里只有动作名，不记表单填的内容。
- 表单卡是飞书 JSON 1.0 卡片，提交按钮用 `"action_type": "form_submit"` 绑定提交（`form_action_type` 是 JSON 2.0 的写法，1.0 卡片里不生效，按钮点了不提交）。下拉、多选和 `send --form` 的表单都靠它提交。
- 表单（`core/form.go`，需开 `[decision]`）：`cc-connect send --form form.json` 发一张表单卡，一次收齐几个参数。提交后卡片改成“✅ 已提交：标题”并列出填的内容，不能再提交；填的内容作为一条消息回到会话，与决定卡片一样落盘、重启后照常送达。没有截止时间。JSON：

  ```json
  {
    "title": "周报参数",
    "note": "可选，标题下的一行说明",
    "submit": "提交",
    "fields": [
      {"label": "开始日期", "type": "date", "required": true},
      {"label": "对象", "type": "select", "options": ["全组", "只看我"], "required": true},
      {"label": "包含", "type": "multi_select", "options": ["进展", "风险", "计划"]},
      {"label": "备注", "placeholder": "可以不填"}
    ]
  }
  ```

  `type` 为 `input`（默认，单行文字）、`date`、`select`、`multi_select`；下拉类 1 到 20 个选项；最多 10 个字段，标签不能重复。字段名由桥生成（`f1`、`f2`……），模型只写标签。必填项在飞书前端校验；绕过前端时只弹提示“请把必填项填完”。回到会话的消息第一行是“【表单】标题”，下面每个字段一行“字段：值”：日期写成 `2026-11-12`（去掉时区），多选用“、”连起来，没填的写“（未填）”，例如：

  ```
  【表单】周报参数
  开始日期：2026-11-12
  对象：全组
  包含：计划、进展
  备注：（未填）
  ```

### 用量和推理强度

- Claude Code 的 `/usage` 先读账号用量 API，失败时回退到终端探测（`agent/claudecode/claude_usage_*.go`）。OpenCode 为 Grok 订阅提供用量窗口；在 Agent 选项中设置 `usage_cmd`（`agent/opencode/usage.go`）。
- `/effort` 为 Claude Code 和 Codex 列出每个模型官方支持的强度级别（`effort_models.go`）。
- Claude Code 的 `config_dirs`（Agent 选项）：存放对话记录的额外 Claude 配置目录，用于只在子进程上设置 `CLAUDE_CONFIG_DIR` 的启动器。项目环境或进程环境中的 `CLAUDE_CONFIG_DIR` 也会被采用。已保存的会话会在所有这些目录中校验并列出。

### 本地助手 API

本地 Unix socket API（`cc-connect send` 使用的那个）多了三个端点（`core/assistant_api.go`）：

- `/assistant/inject`：向已有会话投递一条消息，按来源排队，可设为紧急。
- `/assistant/state`：读取会话状态。
- `/assistant/rotate`：用交接摘要开始一个新会话。

请求带 `request_id`；重复的请求返回第一次的结果。只使用已有的绑定，不创建新路由。

### Codex 后台唤醒

可选，需要 `app_server` 后端。Codex 的后台子 Agent 结束时，会启动一个回合，让父会话处理结果。回合数和时间有上限；人工 `/stop` 会结束它。见 [docs/codex-background-wakeup.md](../docs/codex-background-wakeup.md)。

### 关闭和启动

- 停止时，服务等待运行中的回合结束（`core/shutdown_drain.go`）。
- 重启后续做（`core/turn_resume.go`）：桥把每个会话正在跑的回合（取会话历史里最近一条用户消息的前 60 字作名字）和开了卡的后台任务记在 `data_dir/inflight/<项目哈希>.json`，回合结束、任务结束就删；关机或进程被杀时来不及删的留下。文件每分钟刷新一次时间（只在有记录时），所以中断时刻误差不超过 1 分钟。启动时读出上次留下的记录、清空文件，每条记录只处理一次；第一个平台就绪 20 秒后（先让私聊补收把重启期间的消息补进来）各会话分别处理：中断不到 6 小时的，以定时提示的方式补一轮内部提示“【重启续做】上次在 HH:MM 因重启中断，未完成的有：请求「…」；后台任务「…」。请核对状态后续做，并用一句话告诉用户已接着做。”，按自动回合发出，回复就是给用户的那一句；这一轮和其他自动消息一样受 `[queue_batch]` 的合并等待（默认最多 60 秒）。超过 6 小时的，或者被打断的正是上次补的那一轮，只发一句“上次 HH:MM 的任务被中断，需要的话说一声我再继续。”。没有开关；没有 `data_dir` 时不记录。按次运行的定时任务会话不记录。回合卡在重启时改成的灰字由“已中断，请重发”改为“已中断”。
- 崩溃后遗留的实例锁可以复用（`cmd/cc-connect/instance_lock.go`）。

### 代码布局

- 定时器、心跳和 cron 的代码从 `core/engine.go` 移到 `engine_timer.go`、`engine_heartbeat.go` 和 `engine_cron.go`。行为不变，但上游对 `engine.go` 这些部分的改动在变基时会冲突。
- `tests/release_local/` 下的实机测试，在未设置其环境变量时会跳过。

这些改动都没有提交给上游。上游加入等效功能后，在下次变基时丢弃本分叉的对应提交。

## 配置项参考

本分叉新增的配置项。括号内是默认值。

| 段 | 键 | 含义 |
|---|---|---|
| `[stream_preview]` | `segmented` (true) | 飞书 CardKit 流式卡 |
| | `step_list` (true) | 与 `segmented` 一起用：带转圈和勾图标的步骤行 |
| | `end_mode` (`"settle"`) | `"settle"`：从不撤回；在卡片内结束，或收成“已完成” |
| | `notify_after_ms` (60000) | settle 模式：达到或超过这个时长的回合，把回复作为新消息发出 |
| | `reveal_cps` (90) | 回复逐字显示的速度，约每秒这么多个字符；写 0 关闭 |
| | `reveal_catchup_ms` (4000) | 加快显示，使积压在这个时间内清完 |
| | `reveal_lead_ms` (250) | 让客户端的打字缓冲保持这么多毫秒的打字量，卡片调用偶尔变慢时不会断流；写 0 关闭 |
| | `body_inflight` (6) | 正文更新同时在途的最大个数，每 `push_interval_ms` 发一次、不等上一次返回；0 或 1 表示一次一个。单张卡片每秒操作上限 10 次，`push_interval_ms` 不要低于 100 |
| | `spinner_image` (空) | 用这个 GIF 文件替换步骤行的转圈图（48x48）；空表示用内置图，文件读不到或不是 GIF 时回退内置图并记警告 |
| | `drain_max_ms` (8000) | 回合结束时，继续显示剩余内容的最长时间 |
| | `finish_catchup_ms` (3000) | 撤回前等待卡片打完回复的最长时间；负数表示关闭 |
| | `dots_pause_ms` (2000) | 文本在这个时间内增长过，转圈符号就停住不动 |
| | `print_strategy` (`"delay"`) | 客户端打字方式：`"fast"` 在有新文本时立即输出缓冲文本，`"delay"` 先打完缓冲文本 |
| | `push_interval_ms` (150)、`typing_interval_ms` (40)、`typing_step` (4) | 卡片推送节奏和客户端打字速度 |
| | `typing_marker` (飞书 `"..."`，其他平台关) | 运行中的预览下方的标记；写了就对所有平台生效 |
| | `interval_ms` (飞书 700，其他平台 1500)、`min_delta_chars` (飞书 12，其他 30)、`max_chars` (飞书 4000，其他 2000) | 预览更新节奏和长度；写了就对所有平台生效 |
| | `trace_file` (off) | 流式耗时，写成 JSON 行 |
| agent 选项 | `stream_partial_text` (含飞书平台的 claudecode 项目为 true) | 卡片显示正在写的文本块；写 `false` 关闭 |
| `[display]` | `progress_history`、`keep_progress_card`、`tool_progress_lines` | quiet 模式的进度（见上文） |
| `[queue_batch]` | `enabled` (true)、`max_messages` (20)、`max_chars` (20000)、`hold_seconds` (60)、`immediate_sources` (`["human"]`) | 队列合并；自动消息最多等待 `hold_seconds` 秒，看是否有更多消息 |
| `[decision]` | `enabled` (false)、`default_hours` (12) | 决策卡 |
| Claude Code 选项 | `stream_partial_text` (false)、`first_write_timeout` (`"60s"`)、`config_dirs` | 见上文 |
| Codex 选项 | `backend` (`"exec"`)、`background_wakeup`、`background_wakeup_max_turns`、`background_wakeup_timeout_mins` | 回合并入和后台唤醒 |
| 飞书选项 | `keep_preview_on_finish` (false)、`keep_preview_max_secs` (120)、`p2p_thread_isolation` (false)、`final_reply_order`、`merge_progress_into_final`、`merge_progress_min_chars`、`merge_progress_tables` | 见上文 |
| | `welcome` (true)、`welcome_message` (空，用内置文案) | 首次欢迎语（见上文） |

`config.example.toml` 为主要配置项提供了注释。

## 维护 local 分支

远程仓库：`upstream` 是 chenhg5/cc-connect，`origin` 是 soundworkbench/cc-connect。

```sh
git remote add upstream https://github.com/chenhg5/cc-connect.git
git fetch upstream --tags
git fetch origin
```

### 变基到新的上游版本

示例：从 v1.5.0 到 v1.6.0。

```sh
git tag sw/v1.5.0 origin/local              # keep the old local so it can be rebuilt
git push origin sw/v1.5.0
git switch -C local origin/local
git rebase --onto v1.6.0 v1.5.0 local
```

`local` 含有合并提交。`git rebase` 会把它们展平，所以原先在合并中解决过的冲突（主要在 `core/cuj_test.go` 和 `core/engine.go`）会在相关的提交上再出现一次。

遇到冲突时：

1. 让本分叉的提交适配新的上游代码，并保持它的行为。运行涉及的包的测试（列表见 `build.sh`）。
2. 如果 `README.md` 冲突，保留本分叉的版本：`git checkout --theirs README.md`（在变基中，“theirs”是本分叉的提交）。
3. 如果上游已有同样的功能，用 `git rebase --skip` 丢弃该提交，并从本文件中删除它。
4. 如果一个提交既不能适配也不能丢弃，就停留在当前版本。

冲突很多时，先把本分叉压成一个提交，一次解决，代价是丢失逐个提交的历史：

```sh
git switch -C local-squash origin/local
git reset --soft v1.5.0 && git commit -m "soundworkbench patches on v1.5.0"
git rebase --onto v1.6.0 v1.5.0 local-squash
```

用 `soundworkbench/build.sh` 构建并测试，然后推送：

```sh
git push --force-with-lease origin local
```

提交信息沿用上游格式：`<type>(<area>): <改动做了什么>`，正文写问题和方法。

## 版本规则

版本号的唯一来源是 `local` 分支的 `soundworkbench/VERSION`，文件中只有一行 `vX.Y.Z`。版本和代码在同一个提交中修改：

- 修缺陷、调默认值：加 Z。
- 新功能或新配置键：加 Y，Z 归零。
- 配置键改名、删除或行为不兼容：加 X，Y、Z 归零。

第一个新版本是 `v2.0.0`，沿用旧发布 `v1.5.2-beta.1-sw.167-sw.14`（`c36b64db`）的运行逻辑，仅更新构建发布流程和 `--version` 输出。上游基线、分叉提交号和相对上一发布的改动写在 Release 说明中，不再拼进版本号。

## 构建

```sh
soundworkbench/build.sh            # build local
soundworkbench/build.sh <commit>   # build a given snapshot
```

脚本用 `git archive` 取出目标快照的源码和 VERSION，不读取工作树中的未提交改动。先在 `golang:1.25` Docker 容器中测试 `./agent/claudecode/ ./agent/codex/ ./agent/opencode/ ./core/ ./platform/feishu/ ./config/ ./cmd/cc-connect/`，失败时以退出码 3 停止，再构建。容器和 Go 并行度默认均限制为 4 核。

构建标签为 `no_web,goolm`，`CGO_ENABLED=0`。不含 Web 管理界面；要包含它，先按上游 `Makefile` 构建 `web/`。

| 项目 | 格式 | 示例 |
|---|---|---|
| `cc-connect --version` 的完整输出 | `cc-connect <VERSION>` | `cc-connect v2.0.0` |
| 文件名 | `cc-connect-<VERSION>-<os>-<arch>` | `cc-connect-v2.0.0-linux-amd64` |

环境变量：`GOOS`、`GOARCH`（默认 linux/amd64）、`CPUS`（正整数，默认 4）、`GO_IMAGE`（默认 `golang:1.25`）、`OUT`（默认 `dist`）、`SKIP_TESTS=1`（仅本地重建已测试快照）。发布脚本强制测试，不接受跳过。

## 发布

唯一的现行发布入口是 `soundworkbench/release.sh`。`soundworkbench/tools/` 的旧发布/安装脚本为历史副本，不再使用；`/srv/xiaoman/research/` 下的旧入口由维护人另行删除。新入口不安装二进制、不修改配置、不重启服务。

前提：Git、Docker、sha256sum、jq、已授权的 gh CLI；工作树干净，origin 指向 `soundworkbench/cc-connect`，目标提交已推送到 `origin/local`，本地及远端均没有同名版本标签，也没有同名 Release。脚本每次都重新 fetch `origin/local`，不依赖过期的跟踪分支。

```sh
# 改代码和 soundworkbench/VERSION，验证后提交在 local 分支。
python3 -B -m unittest discover -s soundworkbench -p '*_test.py' -v
CPUS=4 soundworkbench/build.sh

# 提交后、尚未推送时，仅用于准备的完整演练：
CPUS=4 soundworkbench/release.sh --dry-run --allow-unpushed

# 维护人确认后推送；推送本身不创建 Release。
git push origin local
CPUS=4 soundworkbench/release.sh --dry-run

# 正式发布；默认 HEAD，也可传当前检出提交的 SHA。
CPUS=4 soundworkbench/release.sh
```

`--dry-run` 做远端检查、测试、linux-amd64 构建、二进制版本核对和 SHA-256 校验，生成说明和本地构建回执，但不创建任何标签或 Release。`--allow-unpushed` 只能与 `--dry-run` 一起使用，只绕过“提交已在 origin/local”检查，并打印警告；正式发布没有这个例外。

脚本按发布时间选择最新的非草稿 Release（含预发布，与公司的 Bridge bump 一致），核实其标签为当前提交的祖先，使用两者之间的提交列表生成改动说明。Release 说明还包含上游基线 `v1.5.2-beta.1`、完整分叉提交号、比较链接及校验命令。`dist/<VERSION>-receipt.json` 记录版本、提交、基线、上一发布、构建标签、时间和 sha256；`dist/<VERSION>-notes.md` 保存将提交给 GitHub 的说明，可在 dry-run 后审阅。

正式发布通过 `gh release create <VERSION> --target <SHA> --title <VERSION> --notes-file ...` 创建标签和 Release，只上传两个资产：

- `cc-connect-<VERSION>-linux-amd64`
- `cc-connect-<VERSION>-linux-amd64.sha256`

校验文件仅含文件基本名，下载到同一目录后用 `sha256sum -c cc-connect-<VERSION>-linux-amd64.sha256` 校验。公司仓库 `soundworkbench/workpartner` 的 `tools/bridge-bump.py` 据此查找 Linux amd64 资产。

发布成功后脚本再次读取 GitHub，核对 Release 的标签、标题与 VERSION 一致，且资产恰为上述两个名字。标签已存在时拒绝重发；如果创建或上传中途失败，先人工检查 GitHub 状态再处理，不自动覆盖。安装到 `releases/<VERSION>`、同步 lock 和升级各助理由独立部署流程负责。

## 部署与回滚

部署：放入新的可执行文件，把旧的另存为别的名字，重启服务。然后检查：

- `cc-connect --version` 显示新版本。
- 在飞书中向每个项目发一条消息：Agent 有回复，进度卡出现，并定稿或被撤回。
- `/usage` 显示用量窗口。

回滚：放回旧的可执行文件并重启。如果旧文件已经没有，就从 `sw/<旧的上游版本>` 标签或旧提交重新构建（`SKIP_TESTS=1 soundworkbench/build.sh <commit>`）。

会话、排队消息和配置都在数据目录中，替换可执行文件不会动它们。新版本新增的配置键，旧版本会忽略。

## 已知问题

- 定时器输出、决策截止时间和队列合并的标题使用 UTC+8（Asia/Shanghai）。这一点不可配置。
- `web/dist` 未构建时，`go build ./...` 会失败；这是上游的行为。用 `-tags no_web` 构建，或先构建 Web 界面。
- 不带存储路径运行的引擎测试会遗留 `core/test_ws_*.json` 文件；`.gitignore` 已忽略它们。
