# go-llm-bridge

把网页版 LLM 聊天界面伪装成一个本地 OpenAI 兼容 API,供 pi agent 等本地工具调用。

## ⚠️ 使用前必读

- 大多数网页版 LLM(ChatGPT 网页、Claude.ai、Gemini 网页等)的服务条款**禁止自动化/程序化访问**其网页界面,只允许人工手动使用。用本工具让脚本代替人操作网页,存在**违反服务条款、被限流或封号**的风险,请自行评估并承担后果。
- 如果目的只是省钱或绕过官方 API 限速,更稳妥的做法是使用官方 API 的低价模型,而不是自动化网页。
- 网页 DOM 结构会频繁变动,`content.js` 里的选择器需要你手动维护。
- 本工具默认不处理登录、验证码、风控挑战等场景,遇到这些需要人工介入。

## 架构

```
pi agent --HTTP--> Go服务(:8787) --WebSocket--> Chrome扩展background.js
                                                        │ runtime message
                                                        ▼
                                              content.js 注入到目标网页
                                          (填输入框→点发送→等待生成完成→取文本)
```

## 部署步骤

### 1. 启动 Go 服务

```bash
cd go-llm-bridge
go mod tidy   # 会自动拉取 github.com/gorilla/websocket
go run main.go
```

启动后会监听:
- `ws://localhost:8787/ws` — 供 Chrome 扩展连接
- `http://localhost:8787/v1/chat/completions` — 供 pi agent 调用(OpenAI 兼容格式)
- `http://localhost:8787/health` — 查看扩展是否已连接

### 2. 安装 Chrome 扩展

1. 打开 `chrome://extensions`,开启右上角"开发者模式"
2. 点击"加载已解压的扩展程序",选择 `extension/` 目录
3. 打开 `extension/manifest.json` 和 `extension/background.js`,把
   `https://your-llm-site.example.com/*` 换成你实际要用的网页地址
4. 打开 `extension/content.js`,根据目标网页的实际 DOM 结构修改四个选择器常量
   (`INPUT_SELECTOR`、`SEND_BUTTON_SELECTOR`、`ANSWER_CONTAINER_SELECTOR`、
   `GENERATING_INDICATOR_SELECTOR`)。可以用浏览器 F12 的 Elements 面板右键"复制 selector"
5. 修改完成后回到 `chrome://extensions` 点击刷新按钮重新加载扩展
6. 在浏览器里打开目标 LLM 网页并保持登录状态、保持该 tab 打开

### 3. 让 pi agent 指向本地服务

把 pi agent 的模型接口地址配置为:

```
http://localhost:8787/v1/chat/completions
```

如果 pi agent 支持自定义 `base_url` / `api_base`(类似 OpenAI SDK 的用法),
把它指向 `http://localhost:8787` 即可,`api_key` 可随便填一个占位值,
因为本地服务不校验。

### 4. 验证

```bash
curl http://localhost:8787/health
# {"extension_connected": true}   <- 确认扩展已连上

curl -X POST http://localhost:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"web-llm","messages":[{"role":"user","content":"你好"}]}'
```

正常情况下,Chrome 里对应网页会自动填入"你好"并发送,等网页回答生成完毕后,
`curl` 会收到 OpenAI 格式的 JSON 响应。

## 已知局限

- 一次只支持一个"生成中"任务排队处理(简单版本没做并发任务的页面隔离)
- 不处理多轮会话记忆的精确同步(默认只发最后一条 user 消息,依赖网页自己的上下文)
- 页面刷新、重新登录、验证码弹出等情况需要人工处理
- 未做流式(streaming)输出,是等待网页回答完整生成后一次性返回
