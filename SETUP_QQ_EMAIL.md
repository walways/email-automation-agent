# QQ 邮箱配置指南

## 第一步：获取 QQ 邮箱授权码

1. 使用浏览器登录 QQ 邮箱网页版 (https://mail.qq.com)

2. 点击左上角的「设置」

3. 选择「账户」标签页

4. 找到「POP3/IMAP/SMTP/Exchange/CardDAV/CalDAV 服务」部分

5. 确保「IMAP/SMTP 服务」状态为「开启」
   - 如果未开启，点击「开启」按钮
   - 可能需要手机验证

6. 点击「生成授权码」
   - 按提示发送短信验证
   - 复制生成的授权码（是一串字母，不是数字密码）

## 第二步：配置环境变量

### 方式一：使用 .env 文件（推荐）

1. 进入项目目录
   ```bash
   cd /Users/choupengfei/email-automation-agent
   ```

2. 复制配置模板
   ```bash
   cp .env.example .env
   ```

3. 编辑 .env 文件，填入你的配置
   ```bash
   # QQ 邮箱地址
   QQ_EMAIL_USERNAME=123456789@qq.com

   # QQ 邮箱授权码（第一步获取的）
   QQ_EMAIL_AUTH_CODE=abcdefghijklmnop

   # 本地 LLM CLI 配置
   LLM_PROVIDER=claude_code
   LLM_COMMAND=claude
   ```

### 方式二：直接设置环境变量

```bash
export QQ_EMAIL_USERNAME="123456789@qq.com"
export QQ_EMAIL_AUTH_CODE="abcdefghijklmnop"
export LLM_PROVIDER="claude_code"   # 或 codex
export LLM_COMMAND="claude"         # 或 codex
```

## 第三步：安装并登录本地 CLI

1. Claude Code 模式：
   - 确保 `claude` 命令可用
   - 首次运行按提示登录

2. Codex 模式：
   - 确保 `codex` 命令可用
   - 首次运行按提示登录

## 第四步：验证配置

运行以下命令检查配置：

```bash
cd /Users/choupengfei/email-automation-agent
./start.sh
```

如果配置正确，你会看到：
```
✅ 环境检查通过
📦 安装依赖...
🚀 启动 Email Automation Agent...
   按 Ctrl+C 停止服务
```

## 常见问题

### 1. 授权码错误

**错误信息**: `failed to authenticate: authentication failed`

**解决方案**:
- 确认使用的是授权码，不是 QQ 登录密码
- 授权码是一串字母，通常 16 位
- 重新生成授权码试试

### 2. IMAP 连接失败

**错误信息**: `failed to connect to IMAP server`

**解决方案**:
- 检查网络连接
- 确认 QQ 邮箱已开启 IMAP 服务
- 检查防火墙设置

### 3. 本地 CLI 未找到

**错误信息**: `command not found: claude/codex`

**解决方案**:
- 安装对应 CLI
- 检查 PATH（`which claude` / `which codex`）
- 确认 `.env` 中 `LLM_COMMAND` 配置正确

## 安全提示

1. **不要将 .env 文件提交到 Git**
   - 已配置在 .gitignore 中
   - 不要通过聊天工具发送授权码

2. **定期更换授权码**
   - 在 QQ 邮箱设置中可以管理授权码
   - 可以删除旧的授权码

3. **限制访问权限**
   - 建议设置 .env 文件权限：`chmod 600 .env`
