package llm

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Client 统一的代码生成接口
type Client interface {
	GenerateCode(ctx context.Context, taskDescription string) (string, string, *Usage, error)
}

// NewClient 根据 provider 创建 LLM 客户端
func NewClient(provider, model, command string, timeout time.Duration) (Client, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude_code", "claude-code", "local_claude_code":
		return NewClaudeCodeClient(command, timeout), nil
	case "codex", "codex_cli", "codex-cli":
		return NewCodexCLIClient(command, timeout), nil
	case "":
		return NewClaudeCodeClient(command, timeout), nil
	default:
		return nil, fmt.Errorf("unsupported llm provider: %s", provider)
	}
}
