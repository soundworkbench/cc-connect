# soundworkbench/cc-connect

这是 [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect) 的分叉。cc-connect 把 Claude Code、Codex 等编程代理接到飞书等聊天工具上。我们在上游的发布版上加了一组补丁，主要改进飞书里的进度卡、运行中插话、消息排队和定时任务，自己构建单个可执行文件运行。

> English: a fork of chenhg5/cc-connect. The `local` branch is an upstream release (v1.5.0) plus our patches, mostly for Feishu progress cards, steering a running turn, message queueing and scheduled turns. For cc-connect itself, read the [upstream README](https://github.com/chenhg5/cc-connect#readme).

## 和上游的区别

| 方面 | 改动 |
|---|---|
| 进度卡 | 代理没有输出文字时，用中文显示它正在跑的命令和工具。进度按时间只追加、不改写。中途有实质内容时并入最终回复。可选先撤回进度卡再发最终回复，或反过来。Codex 的命令只显示程序名和已知子命令，不显示参数。 |
| 运行中插话 | 代理正在回答时，新消息（含图片）直接送进当前回合，不必等它结束。送达后在原消息上加表情回执。Claude Code 首条写入有期限，写失败时结束进程，下一条消息用 `--resume` 续上会话并提示重发。Codex 用 `app_server` 后端插话。 |
| 消息排队 | 排队的消息写到磁盘，重启后恢复。`/stop` 只停当前回合，不丢队列。同一会话排队的几条消息合成一个回合。每条排队消息各自计算单轮最长时长。 |
| 定时与自动消息 | 定时、转告、助手接口发来的消息不发占位卡和流式预览，结尾是 `NO_REPLY` 时不发任何消息。静音的定时任务只隐藏开始提示，回复和附件照常送达。`timer --at` 必须带时区偏移，回显 UTC+8 时间。 |
| 飞书 | 私聊里每个话题一个会话（可选）。交互卡片和合并转发的内容读成文字交给代理。 |
| 用量与档位 | `/usage` 读 Claude Code 的额度改为直接查账号的用量接口；新增 OpenCode（Grok 订阅）的额度窗口。思考档位按各模型官方支持的档位提供。 |
| 本机助手接口 | 本机 Unix 套接字接口新增 `/assistant/inject`、`/assistant/state`、`/assistant/rotate`：本机程序可以往已有会话投递消息，或带摘要换新会话。 |
| Codex 后台任务 | 可选：Codex 的后台子代理完成后，自动唤醒父会话处理结果，有轮数和时长上限。 |
| 关停 | 停止服务前先等正在跑的回合结束。异常退出留下的实例锁可以复用。 |

附件回传和多项目是上游已有的功能，这个分叉没有改它们的用法。每项改动的细节和配置项见 [soundworkbench/README.md](soundworkbench/README.md)。

## 分支

- `local`：上游发布标签 `v1.5.0` 加我们的提交。可执行文件只从这个分支构建。
- `main`：跟上游，不放我们的改动。

## 构建

```sh
soundworkbench/build.sh            # 从 local 构建
soundworkbench/build.sh <提交号>    # 从指定提交构建
```

脚本先在 Docker 的 Go 镜像里跑改动相关包的测试，不通过就停；通过后编译出一个 Linux 可执行文件，放在 `dist/`。版本号是 `<上游版本>-sw.<序号>`，例如 `v1.5.0-sw.41`。

## 升级与回退

升级上游版本时，把 `local` 变基到新的发布标签，变基前给旧的 `local` 打标签留底。回退时，换回上一版可执行文件，或从留底的标签重新构建。步骤见 [soundworkbench/README.md](soundworkbench/README.md)。

## 上游

cc-connect 的功能、安装和配置，见上游的 [README](https://github.com/chenhg5/cc-connect#readme)（[中文](https://github.com/chenhg5/cc-connect/blob/main/README.zh-CN.md)）。本仓库沿用上游的 MIT 许可证，见 [LICENSE](LICENSE)。
