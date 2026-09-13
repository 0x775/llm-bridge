// ==UserScript==
// @name         LLM 网页转发桥(油猴版)
// @namespace    llm-bridge
// @version      1.0
// @description  拦截网页版LLM的fetch流式响应,通过本地Go服务转发给pi agent
// @match        https://chat.deepseek.com/*
// @grant        GM_xmlhttpRequest
// @grant        unsafeWindow
// @connect      localhost
// @run-at       document-start
// ==/UserScript==

(function () {
  "use strict";
    let i2=0;
    let state = 0; // 0: 找THINK, 1: 收集THINK, 2: 收集RESPONSE, 3: 结束
    let ws = null;
    let agentMsg={};
    let prompt_num=0;
    let prompt_offset=15;
    const CHAT_ENDPOINT_HINT = "chat/completion";

    // 辅助函数：提取初始 content
    function getInitialContent(parsed, type) {
        if (parsed.v?.response?.fragments) {
        for (const frag of parsed.v.response.fragments) {
            if (frag.type === type && frag.content) return frag.content;
        }
        }
        if (Array.isArray(parsed.v)) {
        for (const item of parsed.v) {
            if (item.type === type && item.content) return item.content;
        }
        }
        return "";
    }

    // 创建一个流式解析器实例
function createStreamParser() {
    let buffer = "";
    let thinkData = "";
    let responseData = "";

    return {
        parse(chunk) {
            buffer += chunk;
            const lines = buffer.split(/\r?\n/);

            // 核心：保留最后一行（可能是不完整的），留给下次处理
            buffer = lines.pop() || "";

            for (const line of lines) {
                const trimmed = line.trim();
                if (!trimmed.startsWith("data: ")) continue;

                const jsonStr = trimmed.substring(6);
                // console.log("---->>>", jsonStr); // 调试用，确认没问题后可注释

                if (state === 0) {
                    if (/"type"\s*:\s*"THINK"/.test(jsonStr)) { //思考模式先找think
                        try {
                            const parsed = JSON.parse(jsonStr);
                            const content = getInitialContent(parsed, "THINK");
                            if (content) thinkData += content;
                        } catch (e) {}
                        state = 1;
                    }
                    if (/"type"\s*:\s*"RESPONSE"/.test(jsonStr)) { //不是思考模式直接找结果
                        try {
                            const parsed = JSON.parse(jsonStr);
                            const content = getInitialContent(parsed, "RESPONSE");
                            if (content) responseData += content;
                        } catch (e) {}
                        state = 2;
                    }
                } else if (state === 1) {
                    if (/"type"\s*:\s*"RESPONSE"/.test(jsonStr)) {
                        try {
                            const parsed = JSON.parse(jsonStr);
                            const content = getInitialContent(parsed, "RESPONSE");
                            if (content) responseData += content;
                        } catch (e) {}
                        state = 2;
                    } else {
                        try {
                            const parsed = JSON.parse(jsonStr);
                            if (typeof parsed.v === "string") thinkData += parsed.v;
                        } catch (e) {}
                    }
                } else if (state === 2) {
                    if (/"v"\s*:\s*"FINISHED"/.test(jsonStr)) {
                        state = 3;
                    } else {
                        try {
                            const parsed = JSON.parse(jsonStr);
                            if (typeof parsed.v === "string") responseData += parsed.v;
                        } catch (e) {}
                    }
                }

            }

            return {
                think: thinkData,
                data: responseData,
                finished: state === 3
            };
        }
    };
}

// === XHR Hook 逻辑 ===
(function hookXHR() {
    const RealXHR = unsafeWindow.XMLHttpRequest;
    const realOpen = RealXHR.prototype.open;
    const realSend = RealXHR.prototype.send;

    RealXHR.prototype.open = function (method, url, ...rest) {
        this.__llmBridgeUrl = url;
        return realOpen.call(this, method, url, ...rest);
    };

    RealXHR.prototype.send = function (...args) {
        const url = this.__llmBridgeUrl || "";

        // 只有在匹配的目标 URL 才进行拦截处理
        if (url.includes(CHAT_ENDPOINT_HINT)) {
            const parser = createStreamParser();
            let lastLength = 0; // 必须在这里初始化，追踪本次请求的读取进度

            // 使用 onprogress 覆盖，防止多次 addEventListener 导致回调执行多次
            const originalOnProgress = this.onprogress;
            this.onprogress = (e) => {
                // 1. 获取当前完整的 responseText
                const currentText = this.responseText || "";

                // 2. 截取本次新增的增量部分
                const chunk = currentText.substring(lastLength);

                // 3. ⚠️ 关键：更新 lastLength，确保下次只截取新增部分！
                lastLength = currentText.length;

                if (chunk) {
                    const result = parser.parse(chunk);

                    if (result.finished) {
                        state = 0;
                        prompt_num+=1;
                        console.log("-> 🎯 解析完成！");
                        console.log("实时 Think:", result.think);
                        console.log("实时 Data:", result.data);
                        ws.send(JSON.stringify({ type: "answer", taskId:agentMsg.taskId,content:result.data }));
                        // sendInputText("33+43=?");
                    }
                }

                // 执行原有的 onprogress 逻辑（如果有的话）
                if (typeof originalOnProgress === 'function') {
                    originalOnProgress.call(this, e);
                }
            };
        }

        return realSend.apply(this, args);
    };
})();

  // ===================== 页面操作 =====================
  function submitPrompt(prompt) {
      //看页面是否有增加的提示词|每 prompt_offset 轮增加一次提示词增强
      if (prompt_num % prompt_offset === 0) {
          var append_prompt="";
          var web_prompt = document.getElementById('prompt-textarea');
          if(web_prompt){
              append_prompt = web_prompt.value;
          }
          prompt= agentMsg.prompt +"\n"+append_prompt+"\n"+prompt;
      }

      //input赋值
      const textarea = document.querySelector('textarea[name="search"]');
      if (textarea) {
          const targetValue = prompt;
          // 2. 获取原生的 value setter (绕过 React/Vue 的劫持)
          const nativeTextAreaValueSetter = Object.getOwnPropertyDescriptor(
              window.HTMLTextAreaElement.prototype,
              'value'
          ).set;
          // 3. 使用原生 setter 设置值
          nativeTextAreaValueSetter.call(textarea, targetValue);

          // 4. 触发 input 和 change 事件，让前端框架感知到值的变化
          textarea.dispatchEvent(new Event('input', { bubbles: true }));
          textarea.dispatchEvent(new Event('change', { bubbles: true }));
          console.log("✅ 文本框赋值成功！");
      }else{
          throw new Error("找不到input框");
      }

      const buttonSelector = 'div[role="button"][class*="ds-button--primary"][class*="ds-button--circle"]';
      const btn = document.querySelector(buttonSelector);
      if (btn) {
            console.log("✅ 找到目标按钮，准备点击:", btn);
            // 确保按钮在可视区域内 (有些框架要求元素可见才能触发点击)
            btn.scrollIntoView({ behavior: 'smooth', block: 'center' });
            // 等待一小段时间让滚动完成 (可选，通常 100ms 足够)
            sleep(100);
            // 执行标准点击
            btn.click();
            console.log("🎯 已触发 btn.click()");
      }else {
         throw new Error("找不到发送按钮");
      }
  }

  // ===================== 与 Go 服务通信 =====================
   async function pollLoop() {
       console.log("🔌 正在连接 WebSocket:");
        ws = new WebSocket("ws://127.0.0.1:8787/ws");

        // 2. 连接成功
        ws.onopen = function(event) {
            console.log("✅ WebSocket 连接成功！");
            changeSvgColor(true);
            // 可选：连接成功后立即发送一条消息
            // ws.send(JSON.stringify({ type: "hello", data: "来自油猴的问候" }));
        };

        // 3. 接收到消息
        ws.onmessage = function(event) {
            console.log("📩 收到服务器消息:", event.data);
            try {
                // 1. 解析收到的 JSON
                agentMsg = JSON.parse(event.data);
                if (agentMsg.type === "prompt") {
                    //ws.send(JSON.stringify({ type: "answer", taskId:msg.taskId,content: "来自油猴的问候" }));
                    submitPrompt(agentMsg.content);
                }
            }catch (e) {
                console.error("❌ 解析服务器消息失败:", e);
            }
        };

        // 4. 连接发生错误
        ws.onerror = function(error) {
            console.error("❌ WebSocket 发生错误:", error);
            changeSvgColor(false);
        };

        // 5. 连接关闭
        ws.onclose = function(event) {
            changeSvgColor(false);
            console.warn("⚠️ WebSocket 连接已关闭。代码:", event.code, "原因:", event.reason);
        };
   }



  function sleep(ms) {
    return new Promise((r) => setTimeout(r, ms));
  }

  //页面tip
  function changeSvgColor(isSuccess) {
    const svgElement = document.querySelector('svg[viewBox="0 0 143 23"]');
    if (!svgElement) return;
    // 绿色 (正常) / 红色 (失败)
    const color = isSuccess ? '#22c55e' : '#ef4444';
    // 直接修改该 SVG 元素作用域内的 CSS 变量
    svgElement.style.setProperty('--dsw-alias-brand-primary', color);
  }

  /**
 * 在页面最上层创建一个可拖动、可折叠的 Prompt 输入浮动框
 * @param {string} titleText - 标题栏显示的文字，默认为 'Prompt 助手'
 * @returns {HTMLDivElement} 返回外层容器 DOM，方便后续获取 textarea 的值
 */
function createFloatingPromptBox(titleText = 'Prompt 助手') {
    // 防止重复创建
    const existingBox = document.getElementById('tm-prompt-box-container');
    if (existingBox) return existingBox;

    // 1. 创建外层容器 (最上层)
    const container = document.createElement('div');
    container.id = 'tm-prompt-box-container';
    Object.assign(container.style, {
        position: 'fixed',
        top: '80px',
        right: '50px',
        width: '320px',
        zIndex: '2147483647', // 确保在最上层
        background: 'rgba(255, 255, 255, 0.9)',
        backdropFilter: 'blur(10px)', // 毛玻璃效果
        WebkitBackdropFilter: 'blur(10px)',
        borderRadius: '8px',
        boxShadow: '0 8px 24px rgba(0, 0, 0, 0.15)',
        border: '1px solid rgba(0, 0, 0, 0.1)',
        fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif',
        fontSize: '14px',
        color: '#333',
        overflow: 'hidden'
    });

    // 2. 创建标题栏 (可拖动区域)
    const header = document.createElement('div');
    Object.assign(header.style, {
        display: 'flex',
        justifyContent: 'space-between',
        alignItems: 'center',
        padding: '10px 14px',
        background: 'linear-gradient(to bottom, #f8f9fa, #e9ecef)',
        borderBottom: '1px solid #dee2e6',
        cursor: 'move',
        userSelect: 'none',
        borderRadius: '8px 8px 0 0' // 初始展开状态下的圆角
    });

    const titleSpan = document.createElement('span');
    titleSpan.textContent = titleText;
    Object.assign(titleSpan.style, {
        fontWeight: '600',
        color: '#495057',
        letterSpacing: '0.5px'
    });

    const toggleBtn = document.createElement('span');
    toggleBtn.textContent = '▼';
    toggleBtn.title = '折叠/展开';
    Object.assign(toggleBtn.style, {
        cursor: 'pointer',
        fontSize: '12px',
        color: '#6c757d',
        width: '22px',
        height: '22px',
        display: 'flex',
        justifyContent: 'center',
        alignItems: 'center',
        borderRadius: '4px',
        transition: 'background 0.2s'
    });
    // 按钮悬停效果
    toggleBtn.onmouseenter = () => toggleBtn.style.background = 'rgba(0,0,0,0.1)';
    toggleBtn.onmouseleave = () => toggleBtn.style.background = 'transparent';

    header.appendChild(titleSpan);
    header.appendChild(toggleBtn);

    // 3. 创建内容区 (Textarea)
    const textarea = document.createElement('textarea');
    textarea.id = 'prompt-textarea';
    Object.assign(textarea.style, {
        width: '100%',
        minHeight: '240px',
        padding: '12px',
        border: 'none',
        outline: 'none',
        resize: 'vertical', // 允许用户手动拉伸高度
        boxSizing: 'border-box',
        fontFamily: 'inherit',
        fontSize: '13px',
        lineHeight: '1.5',
        color: '#212529',
        background: 'transparent',
        display: 'block'
    });
    textarea.placeholder = '请输入你的 Prompt...';

    // A. 页面初始化时，如果有值则加载进 textarea
    const savedContent = localStorage.getItem("dsk_prompt_content");
    if (savedContent !== null) {
        textarea.value = savedContent;
    }
    // B. 离开焦点 (blur) 时，把内容写入 localStorage
    textarea.addEventListener('blur', () => {
        localStorage.setItem("dsk_prompt_content", textarea.value);
        //console.log('Prompt 已自动保存至本地');
    });

    // 4. 组装 DOM
    container.appendChild(header);
    container.appendChild(textarea);
    document.body.appendChild(container);

    // ================= 交互逻辑 =================

    // 5. 折叠/展开逻辑
    let isExpanded = true;
    toggleBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        isExpanded = !isExpanded;
        textarea.style.display = isExpanded ? 'block' : 'none';
        toggleBtn.textContent = isExpanded ? '▼' : '▲';
        // 折叠时让标题栏底部也变成圆角，视觉上更协调
        header.style.borderRadius = isExpanded ? '8px 8px 0 0' : '8px';
    });

    // 6. 拖动逻辑
    let isDragging = false;
    let offsetX = 0;
    let offsetY = 0;

    header.addEventListener('mousedown', (e) => {
        // 如果点击的是折叠按钮，则不触发拖动
        if (e.target === toggleBtn) return;

        isDragging = true;
        const rect = container.getBoundingClientRect();
        offsetX = e.clientX - rect.left;
        offsetY = e.clientY - rect.top;

        document.body.style.userSelect = 'none'; // 防止拖动时选中页面文字
        e.preventDefault();
    });

    // 绑定在 document 上，防止鼠标移动过快脱离标题栏
    document.addEventListener('mousemove', (e) => {
        if (!isDragging) return;

        let newLeft = e.clientX - offsetX;
        let newTop = e.clientY - offsetY;

        // 边界限制，防止拖出屏幕可视区域
        const maxLeft = window.innerWidth - container.offsetWidth;
        const maxTop = window.innerHeight - container.offsetHeight;

        newLeft = Math.max(0, Math.min(newLeft, maxLeft));
        newTop = Math.max(0, Math.min(newTop, maxTop));

        container.style.left = `${newLeft}px`;
        container.style.top = `${newTop}px`;
        container.style.right = 'auto'; // 清除初始的 right 定位，改用 left 计算
    });

    document.addEventListener('mouseup', () => {
        if (isDragging) {
            isDragging = false;
            document.body.style.userSelect = '';
        }
    });

    // 返回容器引用，方便你在其他地方获取 textarea 的值
    return container;
  }

  // 页面刚加载时(document-start)DOM可能还没准备好,稍等再开始轮询,
  window.addEventListener("load", () => {
    console.log("[llm-bridge] 脚本已启动,开始轮询 Go 服务");
    pollLoop();
    createFloatingPromptBox();
  });
})();