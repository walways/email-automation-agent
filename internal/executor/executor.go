package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ExecutionResult 执行结果
type ExecutionResult struct {
	Success  bool
	Output   string
	Error    string
	ExitCode int
	Duration time.Duration
	Language string
}

// Executor 代码执行器
type Executor struct {
	workDir             string
	timeout             time.Duration
	allowedLangs        []string
	maxOutputSize       int64
	sandbox             bool
	sandboxAllowNetwork bool
	sandboxCPUs         string
	sandboxMemory       string
	sandboxPidsLimit    int
	sandboxTmpfsSize    string
	sandboxReadOnly     bool
}

// NewExecutor 创建执行器
func NewExecutor(
	workDir string,
	timeout time.Duration,
	allowedLangs []string,
	maxOutputSize int64,
	sandbox bool,
	sandboxAllowNetwork bool,
	sandboxCPUs string,
	sandboxMemory string,
	sandboxPidsLimit int,
	sandboxTmpfsSize string,
	sandboxReadOnly bool,
) *Executor {
	return &Executor{
		workDir:             workDir,
		timeout:             timeout,
		allowedLangs:        allowedLangs,
		maxOutputSize:       maxOutputSize,
		sandbox:             sandbox,
		sandboxAllowNetwork: sandboxAllowNetwork,
		sandboxCPUs:         sandboxCPUs,
		sandboxMemory:       sandboxMemory,
		sandboxPidsLimit:    sandboxPidsLimit,
		sandboxTmpfsSize:    sandboxTmpfsSize,
		sandboxReadOnly:     sandboxReadOnly,
	}
}

// Execute 执行代码
func (e *Executor) Execute(code string, language string) (*ExecutionResult, error) {
	// 验证语言
	if !e.isAllowed(language) {
		return nil, fmt.Errorf("language %s is not allowed", language)
	}

	// 沙盒模式要求 Docker 可用
	if e.sandbox {
		if _, err := exec.LookPath("docker"); err != nil {
			return nil, fmt.Errorf("sandbox mode requires docker, but docker is not installed")
		}
	}

	// 创建工作目录
	if err := os.MkdirAll(e.workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	// 根据语言确定执行方式
	var cmd *exec.Cmd
	var err error

	switch language {
	case "python", "py":
		cmd, err = e.preparePython(code)
	case "go":
		cmd, err = e.prepareGo(code)
	case "bash", "sh":
		cmd, err = e.prepareBash(code)
	case "javascript", "js", "typescript", "ts":
		cmd, err = e.prepareNode(code, language)
	default:
		return nil, fmt.Errorf("unsupported language: %s", language)
	}

	if err != nil {
		return nil, err
	}

	// 执行
	result := &ExecutionResult{
		Language: language,
	}

	startTime := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()

	// 使用 exec.CommandContext 来支持超时
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	cmd.Dir = e.workDir

	output, err := cmd.CombinedOutput()
	result.Duration = time.Since(startTime)

	// 限制输出大小
	if int64(len(output)) > e.maxOutputSize {
		output = output[:e.maxOutputSize]
	}

	result.Output = string(output)

	if err != nil {
		result.Success = false
		result.Error = err.Error()
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
	} else {
		result.Success = true
		result.ExitCode = 0
	}

	return result, nil
}

// isAllowed 检查语言是否允许
func (e *Executor) isAllowed(language string) bool {
	for _, lang := range e.allowedLangs {
		if strings.EqualFold(lang, language) {
			return true
		}
	}
	return false
}

// preparePython 准备 Python 代码
func (e *Executor) preparePython(code string) (*exec.Cmd, error) {
	scriptPath := filepath.Join(e.workDir, "script.py")
	if err := os.WriteFile(scriptPath, []byte(code), 0644); err != nil {
		return nil, fmt.Errorf("failed to write script: %w", err)
	}

	if e.sandbox {
		return e.prepareDockerCommand("python:3.12-alpine", []string{"python", "/workspace/script.py"}), nil
	}

	// 查找 python 解释器
	pythonCmd := "python3"
	if runtime.GOOS == "windows" {
		pythonCmd = "python"
	}

	cmd := exec.Command(pythonCmd, scriptPath)
	cmd.Dir = e.workDir
	return cmd, nil
}

// prepareGo 准备 Go 代码
func (e *Executor) prepareGo(code string) (*exec.Cmd, error) {
	// 检查是否包含 main 包
	if !strings.Contains(code, "package main") {
		return nil, fmt.Errorf("Go code must contain 'package main'")
	}

	scriptPath := filepath.Join(e.workDir, "main.go")
	if err := os.WriteFile(scriptPath, []byte(code), 0644); err != nil {
		return nil, fmt.Errorf("failed to write go file: %w", err)
	}

	if e.sandbox {
		return e.prepareDockerCommand(
			"golang:1.22-alpine",
			[]string{"go", "run", "/workspace/main.go"},
			"--env", "GOCACHE=/tmp/go-build",
			"--env", "GOMODCACHE=/tmp/go-mod",
		), nil
	}

	// 使用 go run 执行
	cmd := exec.Command("go", "run", scriptPath)
	cmd.Dir = e.workDir
	return cmd, nil
}

// prepareBash 准备 Bash 脚本
func (e *Executor) prepareBash(code string) (*exec.Cmd, error) {
	scriptPath := filepath.Join(e.workDir, "script.sh")
	if err := os.WriteFile(scriptPath, []byte(code), 0755); err != nil {
		return nil, fmt.Errorf("failed to write script: %w", err)
	}

	if e.sandbox {
		return e.prepareDockerCommand("bash:5.2", []string{"bash", "/workspace/script.sh"}), nil
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = e.workDir
	return cmd, nil
}

// prepareNode 准备 Node.js 代码
func (e *Executor) prepareNode(code string, language string) (*exec.Cmd, error) {
	ext := ".js"
	if language == "typescript" || language == "ts" {
		ext = ".ts"
	}

	scriptPath := filepath.Join(e.workDir, "script"+ext)
	if err := os.WriteFile(scriptPath, []byte(code), 0644); err != nil {
		return nil, fmt.Errorf("failed to write script: %w", err)
	}

	if e.sandbox {
		if language == "typescript" || language == "ts" {
			return e.prepareDockerCommand("oven/bun:1.1", []string{"bun", "/workspace/script.ts"}), nil
		}
		return e.prepareDockerCommand("node:20-alpine", []string{"node", "/workspace/script.js"}), nil
	}

	cmdName := "node"
	if language == "typescript" || language == "ts" {
		// 检查是否有 ts-node
		if _, err := exec.LookPath("ts-node"); err == nil {
			cmdName = "ts-node"
		} else {
			return nil, fmt.Errorf("ts-node not found, please install it: npm install -g ts-node")
		}
	}

	cmd := exec.Command(cmdName, scriptPath)
	cmd.Dir = e.workDir
	return cmd, nil
}

func (e *Executor) prepareDockerCommand(image string, command []string, extraArgs ...string) *exec.Cmd {
	absWorkDir := e.workDir
	if dir, err := filepath.Abs(e.workDir); err == nil {
		absWorkDir = dir
	}
	mount := fmt.Sprintf("%s:/workspace:rw", absWorkDir)

	args := []string{
		"run",
		"--rm",
		"--cpus", e.sandboxCPUs,
		"--memory", e.sandboxMemory,
		"--pids-limit", strconv.Itoa(e.sandboxPidsLimit),
		"-v", mount,
		"-w", "/workspace",
	}
	if e.sandboxReadOnly {
		args = append(args, "--read-only")
	}
	tmpfs := "/tmp:rw,nosuid,size=" + e.sandboxTmpfsSize
	if e.sandboxReadOnly {
		// 只读根文件系统下，为 /tmp 添加 noexec 进一步收敛执行面
		tmpfs = "/tmp:rw,noexec,nosuid,size=" + e.sandboxTmpfsSize
	}
	args = append(args, "--tmpfs", tmpfs)
	if !e.sandboxAllowNetwork {
		args = append(args, "--network", "none")
	}

	args = append(args, extraArgs...)
	args = append(args, image)
	args = append(args, command...)

	return exec.Command("docker", args...)
}

// Cleanup 清理工作目录
func (e *Executor) Cleanup() error {
	return os.RemoveAll(e.workDir)
}
