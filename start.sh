#!/bin/bash

# Email Automation Agent 快速启动脚本

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "======================================"
echo "  Email Automation Agent"
echo "  QQ 邮箱自动化代理"
echo "======================================"
echo ""

# 统一使用净化后的 Go 环境，避免 GOROOT/GOTOOLDIR 版本混用
GO_CMD=(env -u GOROOT -u GOTOOLDIR go)

# 检查 .env 文件
if [ ! -f ".env" ]; then
    echo "⚠️  未找到 .env 文件"
    echo "   请先配置环境变量："
    echo ""
    echo "   1. 复制配置模板："
    echo "      cp .env.example .env"
    echo ""
    echo "   2. 编辑 .env 文件，填入以下信息："
    echo "      - QQ_EMAIL_USERNAME: 你的 QQ 邮箱地址"
    echo "      - QQ_EMAIL_AUTH_CODE: QQ 邮箱授权码"
    echo "      - LLM_PROVIDER: claude_code 或 codex"
    echo "      - LLM_COMMAND: 本地命令（如 claude / codex）"
    echo ""
    exit 1
fi

# 加载 .env 文件
set -a
source .env
set +a

# 检查必要的环境变量
if [ -z "$QQ_EMAIL_USERNAME" ]; then
    echo "❌ 错误：QQ_EMAIL_USERNAME 未设置"
    exit 1
fi

if [ -z "$QQ_EMAIL_AUTH_CODE" ]; then
    echo "❌ 错误：QQ_EMAIL_AUTH_CODE 未设置"
    exit 1
fi

LLM_PROVIDER="${LLM_PROVIDER:-claude_code}"
LLM_COMMAND="${LLM_COMMAND:-}"
if [ -z "$LLM_COMMAND" ]; then
    if [ "$LLM_PROVIDER" = "codex" ]; then
        LLM_COMMAND="codex"
    else
        LLM_COMMAND="claude"
    fi
fi

if ! command -v "$LLM_COMMAND" &> /dev/null; then
    echo "❌ 错误：未找到本地 LLM 命令: $LLM_COMMAND"
    echo "   请先安装并登录对应 CLI（claude 或 codex）"
    exit 1
fi
echo "ℹ️  使用本地 LLM CLI: provider=$LLM_PROVIDER command=$LLM_COMMAND"

# 检查 Go 环境
if ! command -v go &> /dev/null; then
    echo "❌ 错误：未找到 Go 环境"
    echo "   请访问 https://go.dev/dl/ 下载安装"
    exit 1
fi

# 检查 Go 工具链是否可用
if ! "${GO_CMD[@]}" version &> /dev/null; then
    echo "❌ 错误：Go 工具链不可用，请检查本机 Go 安装"
    exit 1
fi

echo "✅ 环境检查通过"
echo ""

# 安装依赖
echo "📦 安装依赖..."
"${GO_CMD[@]}" mod tidy
echo ""

# 检查配置文件
if [ ! -f "configs/default.yaml" ]; then
    echo "⚠️  配置文件不存在，使用默认配置"
fi

# 启动服务
echo "🚀 启动 Email Automation Agent..."
echo "   按 Ctrl+C 停止服务"
echo ""
echo "--------------------------------------"

"${GO_CMD[@]}" run cmd/main.go -config configs/default.yaml
