package llm

import (
	"bytes"
	"regexp"
	"strings"
)

const codeGenSystemPrompt = `你是一个代码生成助手。请根据用户请求生成“单文件、可直接运行”的最小可行脚本。

请遵循以下指南：
1. 仔细分析用户请求
2. 确定合适的编程语言（脚本优先使用 Python）
3. 只输出与当前任务直接相关的代码，不要生成邮件收发/轮询等系统工程代码
4. 包含必要的错误处理
5. 代码应该安全并遵循最佳实践
6. 默认只使用标准库（Python 优先使用 urllib/json 等），除非用户明确要求第三方库
7. 如需联网请求，直接实现请求逻辑；不要要求用户先手工安装依赖

输出格式：
- 首先简要说明代码的功能
- 然后在 markdown 代码块中提供代码，并指定语言
- 如果有先决条件或设置步骤，请在最后列出`

type Usage struct {
	Provider        string
	InputTokens     int
	OutputTokens    int
	TotalTokens     int
	IsEstimated     bool
}

func extractCode(text string) (string, string) {
	codeStart := -1
	codeEnd := -1

	lines := bytes.Split([]byte(text), []byte("\n"))
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("```")) {
			if codeStart == -1 {
				codeStart = i + 1
			} else {
				codeEnd = i
				break
			}
		}
	}

	if codeStart == -1 || codeEnd == -1 {
		return text, ""
	}

	explanation := string(bytes.Join(lines[:codeStart-1], []byte("\n")))
	code := string(bytes.Join(lines[codeStart:codeEnd], []byte("\n")))

	return strings.TrimSpace(explanation), code
}

func estimateTokensApprox(text string) int {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0
	}
	// 经验近似：中文/英文混合场景按 ~3.5 chars/token 估算
	charCount := len([]rune(trimmed))
	wordCount := len(regexp.MustCompile(`\s+`).Split(trimmed, -1))
	estimate := int(float64(charCount)/3.5 + float64(wordCount)*0.2)
	if estimate < 1 {
		estimate = 1
	}
	return estimate
}
