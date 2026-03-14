package llm

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CodexCLIClient 调用本地 Codex CLI 的客户端
type CodexCLIClient struct {
	command string
	timeout time.Duration
}

func NewCodexCLIClient(command string, timeout time.Duration) *CodexCLIClient {
	if strings.TrimSpace(command) == "" {
		command = "codex"
	}
	return &CodexCLIClient{
		command: command,
		timeout: timeout,
	}
}

func (c *CodexCLIClient) GenerateCode(ctx context.Context, taskDescription string) (string, string, *Usage, error) {
	if _, err := exec.LookPath(c.command); err != nil {
		return "", "", nil, fmt.Errorf("codex command not found: %s", c.command)
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
		return "", "", nil, fmt.Errorf("codex execution failed: %w; output: %s", err, strings.TrimSpace(string(output)))
	}

	text := strings.TrimSpace(string(output))
	if text == "" {
		return "", "", nil, fmt.Errorf("codex returned empty output")
	}

	explanation, code := extractCode(text)
	input := estimateTokensApprox(prompt)
	outputTokens := estimateTokensApprox(text)
	usage := &Usage{
		Provider:     "codex",
		InputTokens:  input,
		OutputTokens: outputTokens,
		TotalTokens:  input + outputTokens,
		IsEstimated:  true,
	}
	return explanation, code, usage, nil
}
