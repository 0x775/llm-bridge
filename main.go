package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 🎯 修复点 1：删除了内部所有的反引号，删除了结尾无意义的 \n\n
const DefaultPrompt = `You are an expert coding assistant operating, a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files.

Available tools:
- read: Read file contents
- bash: Execute bash commands (ls, grep, find, etc.)
- edit: Make precise file edits with exact text replacement, including multiple disjoint edits in one call
- write: Create or overwrite files

In addition to the tools above, you may have access to other custom tools depending on the project.

Guidelines:
- Use bash for file operations like ls, rg, find
- Use read to examine files instead of cat or sed.
- Use edit for precise changes (edits[].oldText must match exactly)
- When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls
- Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.
- Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.
- Use write only for new files or complete rewrites.
- Be concise in your responses
- Show file paths clearly when working with files

## 🛠️ 可用工具
你可以通过调用以下工具与本地环境交互：

1. bash: 执行 shell 命令（如 ls, grep, find, git, 编译, 跑测试等）。
   - 参数: command (string, 必需) - 要执行的 shell 命令。
2. read: 读取文件内容（替代 cat/sed 查看文件）。
   - 参数: path (string, 必需) - 文件的绝对或相对路径。
3. write: 创建新文件或整体重写文件。
   - 参数: path (string, 必需) - 文件路径。
   - 参数: content (string, 必需) - 完整的文件内容。
4. edit: 精确文件编辑（用 oldText/newText 替换）。
   - 参数: path (string, 必需) - 文件路径。
   - 参数: oldText (string, 必需) - 要被替换的精确原文本。
   - 参数: newText (string, 必需) - 替换后的新文本。

## 工具调用格式
使用 XML parameter 格式调用工具。每次调用必须带唯一随机的 call_id（5位以上字母数字随机字符串，如 "a3f9k"、"x7m2p"，禁止使用自增数字或重复 ID）。

格式示例：
<tool name="工具名" call_id="随机字符串">
  <parameter name="参数名">参数值</parameter>
</tool>

重要约束：
1. 参数值可包含引号、换行、反斜杠等任意字符，必须原样保留，不要进行额外的转义。
2. 严禁在 <tool> 标签内部或外部包裹 Markdown 代码块，直接输出纯 XML 文本。
3. 你可以在 <tool> 标签前后输出你的思考过程或解释说明。
4. 如果不需要调用工具，或者任务已完成，请直接输出最终的文本回复，不要包含任何 <tool> 标签。

## 核心工作流
1. 分析需求: 明确用户意图，规划步骤。
2. 环境探查: 如果需要，使用 bash 或 read 了解当前项目结构和代码。
3. 精准执行: 使用 write 或 edit 修改代码。遵循最小必要改动原则。
4. 验证闭环: 修改后使用 bash 运行测试或编译检查。\n`

// ---------- 数据结构 ----------

type ToolCallInfo struct {
	Name      string
	CallID    string
	Arguments map[string]string
}

// 🎯 修复点 2：必须加上 ToolCallID 字段，否则接收 Agent 工具结果时会报错
type ChatMessage struct {
	Role       string `json:"role"`
	Content    any    `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type UnifiedRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Prompt   string        `json:"prompt"`
	Stream   bool          `json:"stream"`
}

type BridgeMessage struct {
	Type    string `json:"type"`
	TaskID  string `json:"taskId"`
	Content any    `json:"content"`
	Prompt  string `json:"prompt"`
}

// ---------- 任务等待表 ----------
type pendingTask struct {
	resultCh chan string
	errCh    chan string
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	wsConn   *websocket.Conn
	wsMutex  sync.Mutex
	tasksMu  sync.Mutex
	tasks    = map[string]*pendingTask{}
	taskSeq  int64
	seqMutex sync.Mutex
)

func nextTaskID() string {
	seqMutex.Lock()
	defer seqMutex.Unlock()
	taskSeq++
	return time.Now().Format("20060102150405") + "-" + strconv.FormatInt(taskSeq, 10)
}

func parseToolCalls(text string) ([]ToolCallInfo, string) {
	toolRegex := regexp.MustCompile(`(?is)<tool\s+name="([^"]+)"\s+call_id="([^"]+)">(.*?)</tool>`)
	paramRegex := regexp.MustCompile(`(?is)<parameter\s+name="([^"]+)">(.*?)</parameter>`)

	matches := toolRegex.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil, text
	}

	var calls []ToolCallInfo
	cleanText := text

	for _, match := range matches {
		fullMatch := match[0]
		name := match[1]
		callID := match[2]
		body := match[3]

		params := make(map[string]string)
		paramMatches := paramRegex.FindAllStringSubmatch(body, -1)
		for _, pm := range paramMatches {
			params[pm[1]] = pm[2]
		}

		calls = append(calls, ToolCallInfo{
			Name:      name,
			CallID:    callID,
			Arguments: params,
		})

		cleanText = strings.Replace(cleanText, fullMatch, "", 1)
	}

	return calls, strings.TrimSpace(cleanText)
}

// ---------- WebSocket 处理 ----------
func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("升级 WebSocket 失败:", err)
		return
	}
	log.Println("✅ Chrome 扩展已连接")

	wsMutex.Lock()
	wsConn = conn
	wsMutex.Unlock()

	defer func() {
		wsMutex.Lock()
		if wsConn == conn {
			wsConn = nil
		}
		wsMutex.Unlock()
		conn.Close()
		log.Println("⚠️ Chrome 扩展已断开")
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			log.Println("读取 WS 消息失败:", err)
			return
		}
		log.Printf("📥 收到扩展原始消息: %s", string(raw))

		var msg BridgeMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Println("解析 WS 消息失败:", err)
			continue
		}

		tasksMu.Lock()
		task, ok := tasks[msg.TaskID]
		tasksMu.Unlock()
		if !ok {
			continue
		}

		switch msg.Type {
		case "answer", "chat":
			cleanContent := strings.Trim(fmt.Sprintf("%v", msg.Content), "\"")
			task.resultCh <- cleanContent
		case "error":
			task.errCh <- fmt.Sprintf("%v", msg.Content)
		}
	}
}

func sendPromptAndWait(content any, timeout time.Duration) (string, error) {
	wsMutex.Lock()
	conn := wsConn
	wsMutex.Unlock()
	if conn == nil {
		return "", &bridgeError{"Chrome 扩展未连接,请先打开对应网页并确认扩展已连上 ws://localhost:8787/ws"}
	}

	taskID := nextTaskID()
	task := &pendingTask{
		resultCh: make(chan string, 1),
		errCh:    make(chan string, 1),
	}
	tasksMu.Lock()
	tasks[taskID] = task
	tasksMu.Unlock()
	defer func() {
		tasksMu.Lock()
		delete(tasks, taskID)
		tasksMu.Unlock()
	}()

	// 🎯 修复点 3：去掉了这里的 DefaultPrompt 拼接，防止提示词被重复发送两次！
	out := BridgeMessage{Type: "prompt", TaskID: taskID, Content: content, Prompt: DefaultPrompt}
	payload, _ := json.Marshal(out)

	wsMutex.Lock()
	err := conn.WriteMessage(websocket.TextMessage, payload)
	wsMutex.Unlock()
	if err != nil {
		return "", err
	}

	select {
	case ans := <-task.resultCh:
		return ans, nil
	case errMsg := <-task.errCh:
		return "", &bridgeError{errMsg}
	case <-time.After(timeout):
		return "", &bridgeError{"等待网页 LLM 回答超时"}
	}
}

type bridgeError struct{ msg string }

func (e *bridgeError) Error() string { return e.msg }

// ---------- HTTP API 处理 ----------
func chatHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("📥 收到请求: %s %s", r.Method, r.URL.Path)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	var req UnifiedRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// 🎯 1. 智能提取 Prompt (支持识别 Agent 传回来的工具执行结果)
	var finalPrompt string
	isToolResult := false

	if len(req.Messages) > 0 {
		lastMsg := req.Messages[len(req.Messages)-1]

		if lastMsg.Role == "tool" {
			isToolResult = true
			contentStr := ""
			if str, ok := lastMsg.Content.(string); ok {
				contentStr = str
			}
			finalPrompt = fmt.Sprintf("[请求的工具 (call_id: %s) 已执行完毕]\n执行结果如下：\n---\n%s\n---\n请根据上述结果继续你的任务。如果还需要工具，请继续输出 <tool> 标签；如果已完成，请输出最终回复。", lastMsg.ToolCallID, contentStr)
		}
	}

	if !isToolResult {
		var prompt string
		if len(req.Messages) > 0 {
			for i := len(req.Messages) - 1; i >= 0; i-- {
				msg := req.Messages[i]
				if msg.Role == "user" {
					switch v := msg.Content.(type) {
					case string:
						prompt = v
					case []any:
						for _, item := range v {
							if m, ok := item.(map[string]any); ok {
								if text, ok := m["text"].(string); ok {
									prompt = text
									break
								}
							}
						}
					}
					if prompt != "" {
						break
					}
				}
			}
		}
		if prompt == "" && req.Prompt != "" {
			prompt = req.Prompt
		}
		if prompt == "" {
			http.Error(w, "bad request: 无法提取提示词", http.StatusBadRequest)
			return
		}
		//finalPrompt = DefaultPrompt + "\n\n--- 当前输入 ---\n" + prompt
		finalPrompt = prompt
	}

	// 🎯 2. 发送给网页 LLM
	answer, err := sendPromptAndWait(finalPrompt, 120*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// 🎯 3. 核心翻译：解析网页 LLM 的回复
	toolCalls, cleanContent := parseToolCalls(answer)

	taskID := "bridge-" + nextTaskID()
	created := time.Now().Unix()

	// 🚀 4. 根据 req.Stream 决定返回格式
	if req.Stream {
		// ================= 流式输出 (SSE) =================
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		var chunk1, chunk2 map[string]any

		if len(toolCalls) > 0 {
			// --- 情况 A：工具调用流式 ---
			var streamToolCalls []map[string]any
			for i, tc := range toolCalls {
				argsBytes, _ := json.Marshal(tc.Arguments)
				streamToolCalls = append(streamToolCalls, map[string]any{
					"index": i,
					"id":    tc.CallID,
					"type":  "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": string(argsBytes),
					},
				})
			}

			chunk1 = map[string]any{
				"id": taskID, "object": "chat.completion.chunk", "created": created, "model": req.Model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"role":       "assistant",
						"content":    cleanContent, // 思考过程
						"tool_calls": streamToolCalls,
					},
					"finish_reason": nil, // 流式中间块必须是 null
				}},
			}
			chunk2 = map[string]any{
				"id": taskID, "object": "chat.completion.chunk", "created": created, "model": req.Model,
				"choices": []map[string]any{{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": "tool_calls", // 结束标记
				}},
			}
		} else {
			// --- 情况 B：普通文本流式 ---
			chunk1 = map[string]any{
				"id": taskID, "object": "chat.completion.chunk", "created": created, "model": req.Model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"role":    "assistant",
						"content": answer, // 一次性吐出完整内容
					},
					"finish_reason": nil,
				}},
			}
			chunk2 = map[string]any{
				"id": taskID, "object": "chat.completion.chunk", "created": created, "model": req.Model,
				"choices": []map[string]any{{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": "stop",
				}},
			}
		}

		// 发送数据块
		sendSSEChunk(w, flusher, chunk1)
		sendSSEChunk(w, flusher, chunk2)

		// 🚨 关键：必须发送 [DONE] 告诉 Agent 流已结束！
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()

	} else {
		// ================= 非流式输出 (标准 JSON) =================
		var resp map[string]any
		message := make(map[string]any)
		message["role"] = "assistant"
		var finishReason string

		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
			if cleanContent != "" {
				message["content"] = cleanContent
			} else {
				message["content"] = nil
			}

			var openAIToolCalls []map[string]any
			for _, tc := range toolCalls {
				argsBytes, _ := json.Marshal(tc.Arguments)
				openAIToolCalls = append(openAIToolCalls, map[string]any{
					"id":   tc.CallID,
					"type": "function",
					"function": map[string]string{
						"name":      tc.Name,
						"arguments": string(argsBytes),
					},
				})
			}
			message["tool_calls"] = openAIToolCalls
		} else {
			finishReason = "stop"
			message["content"] = answer
		}

		resp = map[string]any{
			"id": taskID, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []map[string]any{
				{"index": 0, "message": message, "finish_reason": finishReason},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// 辅助函数：发送 SSE 数据块
func sendSSEChunk(w http.ResponseWriter, flusher http.Flusher, chunk map[string]any) {
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", string(data))
	flusher.Flush()
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	wsMutex.Lock()
	connected := wsConn != nil
	wsMutex.Unlock()
	json.NewEncoder(w).Encode(map[string]any{
		"extension_connected": connected,
	})
}

func main() {
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/v1/chat/completions", chatHandler)
	http.HandleFunc("/v1/completions", chatHandler)
	http.HandleFunc("/health", healthHandler)

	addr := "0.0.0.0:8787"
	log.Println("🚀 Go 中转服务启动, 监听", addr)
	log.Println("🔗 扩展连接地址: ws://localhost:8787/ws")
	log.Println("🤖 Pi Agent 调用地址: http://localhost:8787/v1/chat/completions 或 /v1/completions")

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
