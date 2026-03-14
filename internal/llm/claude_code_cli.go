package llm

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCodeClient 调用本地 Claude Code CLI 的客户端
type ClaudeCodeClient struct {
	command string
	timeout time.Duration
}

func NewClaudeCodeClient(command string, timeout time.Duration) *ClaudeCodeClient {
	if strings.TrimSpace(command) == "" {
		command = "claude"
	}
	return &ClaudeCodeClient{
		command: command,
		timeout: timeout,
	}
}

func (c *ClaudeCodeClient) GenerateCode(ctx context.Context, taskDescription string) (string, string, *Usage, error) {
	if _, err := exec.LookPath(c.command); err != nil {
		return "", "", nil, fmt.Errorf("claude code command not found: %s", c.command)
	}

	prompt := fmt.Sprintf("%s\n\n用户请求：\n%s", codeGenSystemPrompt, taskDescription)
	runCtx := ctx
	var cancel context.CancelFunc = func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && c.timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, c.timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(runCtx, c.command, "-p", prompt)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", nil, fmt.Errorf("claude code execution failed: %w; output: %s", err, strings.TrimSpace(string(output)))
	}

	text := strings.TrimSpace(string(output))
	if text == "" {
		return "", "", nil, fmt.Errorf("claude code returned empty output")
	}

	explanation, code := extractCode(text)
	input := estimateTokensApprox(prompt)
	outputTokens := estimateTokensApprox(text)
	usage := &Usage{
		Provider:     "claude_code",
		InputTokens:  input,
		OutputTokens: outputTokens,
		TotalTokens:  input + outputTokens,
		IsEstimated:  true,
	}
	return explanation, code, usage, nil
}
