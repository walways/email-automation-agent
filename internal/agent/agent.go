package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"email-automation-agent/internal/channel"
	"email-automation-agent/internal/config"
	"email-automation-agent/internal/executor"
	"email-automation-agent/internal/llm"
)

// Agent 自动化代理
type Agent struct {
	config             *config.Config
	configPath         string
	configModTime      time.Time
	msgChannel         channel.Channel
	llmClient          llm.Client
	executor           *executor.Executor
	stopChan           chan struct{}
	processedUIDs      map[uint32]struct{}
	pendingTasks       map[string]*PendingTask
	toolCache          map[string]*CachedTool
	cacheEnabled       bool
	cacheTTL           time.Duration
	cacheMaxItems      int
	cacheLLMValidate   bool
	maxConcurrentTasks int
	reportEnabled      bool
	reportInterval     time.Duration
	reportRecipients   []string
	reportEnabledOVR   *bool
	reportIntervalOVR  time.Duration
	reportWakeCh       chan struct{}
	useSubAgent        bool
	subagentQueueSize  int
	taskQueue          chan *channel.Message
	runningSubtasks    int
	completedSubtasks  uint64
	totalTasks         uint64
	successTasks       uint64
	failedTasks        uint64
	cacheHitTasks      uint64
	totalInputTokens   uint64
	totalOutputTokens  uint64
	totalTokens        uint64
	skippedByWhitelist uint64
	lastTaskAt         time.Time
	lastTaskSummary    string
	fetchErrorCount    int
	nextReconnectAt    time.Time
	statePath          string
	mu                 sync.Mutex
}

type PendingTask struct {
	BaseTaskDescription string
	LastError           string
	UpdatedAt           time.Time
}

type CachedTool struct {
	Explanation string    `json:"explanation"`
	Code        string    `json:"code"`
	Language    string    `json:"language"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	HitCount    int       `json:"hit_count"`
}

type persistedState struct {
	ProcessedUIDs  []uint32                `json:"processed_uids"`
	PendingTasks   map[string]*PendingTask `json:"pending_tasks"`
	ToolCache      map[string]*CachedTool  `json:"tool_cache"`
	TotalInput     uint64                  `json:"total_input_tokens"`
	TotalOutput    uint64                  `json:"total_output_tokens"`
	TotalTokens    uint64                  `json:"total_tokens"`
	ReportEnabled  *bool                   `json:"report_enabled_override,omitempty"`
	ReportInterval string                  `json:"report_interval_override,omitempty"`
}

// NewAgent 创建代理
func NewAgent(cfg *config.Config, configPath string) (*Agent, error) {
	llmClient, err := llm.NewClient(
		cfg.LLM.Provider,
		cfg.LLM.Model,
		cfg.LLM.Command,
		cfg.LLM.Timeout,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize llm client: %w", err)
	}

	var msgChannel channel.Channel
	switch strings.ToLower(strings.TrimSpace(cfg.Interaction.Provider)) {
	case "", "email":
		msgChannel = channel.NewEmailChannel(
			cfg.Email.IMAP.Host,
			cfg.Email.IMAP.Port,
			cfg.Email.IMAP.Username,
			cfg.Email.IMAP.Password,
			cfg.Email.IMAP.UseSSL,
			cfg.Email.SMTP.Host,
			cfg.Email.SMTP.Port,
			cfg.Email.SMTP.Username,
			cfg.Email.SMTP.Password,
			cfg.Email.SMTP.UseSSL,
		)
	case "im":
		msgChannel = channel.NewIMChannel(channel.IMConfig{
			Platform:     cfg.Interaction.IM.Platform,
			Endpoint:     cfg.Interaction.IM.Endpoint,
			BotID:        cfg.Interaction.IM.BotID,
			BotToken:     cfg.Interaction.IM.BotToken,
			PollInterval: cfg.Interaction.IM.PollInterval,
		})
	default:
		return nil, fmt.Errorf("unsupported interaction provider: %s", cfg.Interaction.Provider)
	}

	return &Agent{
		config:     cfg,
		configPath: configPath,
		msgChannel: msgChannel,
		llmClient:  llmClient,
		executor: executor.NewExecutor(
			cfg.Executor.WorkDir,
			cfg.Executor.Timeout,
			cfg.Executor.AllowedLangs,
			cfg.Executor.MaxOutputSize,
			cfg.Executor.Sandbox,
			cfg.Executor.SandboxAllowNetwork,
			cfg.Executor.SandboxCPUs,
			cfg.Executor.SandboxMemory,
			cfg.Executor.SandboxPidsLimit,
			cfg.Executor.SandboxTmpfsSize,
			cfg.Executor.SandboxReadOnly,
		),
		stopChan:           make(chan struct{}),
		processedUIDs:      make(map[uint32]struct{}),
		pendingTasks:       make(map[string]*PendingTask),
		toolCache:          make(map[string]*CachedTool),
		cacheEnabled:       cfg.Cache.Enabled,
		cacheTTL:           cfg.Cache.TTL,
		cacheMaxItems:      cfg.Cache.MaxEntries,
		cacheLLMValidate:   cfg.Cache.LLMValidateOnMiss,
		maxConcurrentTasks: cfg.Email.MaxConcurrentTasks,
		reportEnabled:      cfg.StatusReport.Enabled,
		reportInterval:     cfg.StatusReport.Interval,
		reportRecipients:   append([]string(nil), cfg.StatusReport.Recipients...),
		reportWakeCh:       make(chan struct{}, 1),
		useSubAgent:        cfg.Email.UseSubAgent,
		subagentQueueSize:  cfg.Email.SubAgentQueueSize,
		statePath:          filepath.Join(cfg.Executor.WorkDir, "agent_state.json"),
	}, nil
}

// Start 启动代理
func (a *Agent) Start() error {
	log.Println("Starting email automation agent...")

	// 连接 IMAP
	if err := a.msgChannel.Connect(); err != nil {
		return fmt.Errorf("failed to connect channel: %w", err)
	}
	log.Println("Channel connected")

	if err := a.loadState(); err != nil {
		log.Printf("Warning: failed to load persisted state: %v", err)
	}
	a.initConfigHotReload()

	// 可选：启动时处理最新一封邮件（默认关闭，避免吃历史任务）
	if a.config.Email.ProcessLatestOnStartup {
		a.processLatestEmailAtStartup()
	}

	// 首次启动（无持久状态）时，记录当前邮箱里的最近邮件，避免处理历史积压
	if len(a.processedUIDs) == 0 {
		if err := a.bootstrapProcessedUIDs(); err != nil {
			log.Printf("Warning: failed to bootstrap processed UIDs: %v", err)
		}
	}

	// 启动后先立即检查一次，避免首次等待 poll_interval
	a.checkAndProcessEmails()

	// 启动轮询
	go a.pollEmails()
	go a.reportStatusLoop()
	if a.useSubAgent {
		a.startSubAgents()
	}

	return nil
}

func (a *Agent) processLatestEmailAtStartup() {
	messages, err := a.msgChannel.FetchLatestMessages(a.config.Email.Inbox, 1)
	if err != nil {
		log.Printf("Warning: failed to fetch latest email on startup: %v", err)
		return
	}
	if len(messages) == 0 {
		log.Println("No email found when processing latest message at startup")
		return
	}

	latest := messages[0]
	log.Printf("Startup processing latest email from %s, subject: %s", latest.From, latest.Subject)

	if latest.UID != 0 {
		if _, ok := a.processedUIDs[latest.UID]; ok {
			log.Printf("Skipping startup latest email UID=%d: already processed", latest.UID)
			return
		}
	}

	if !a.isSenderAllowed(latest.From) {
		log.Printf("Skipping startup latest email from %s: sender is not in whitelist", latest.From)
		return
	}

	if latest.UID != 0 {
		a.markUIDProcessed(latest.UID)
	}

	if a.tryHandleStatusCommand(latest) {
		if a.config.Email.MarkAsRead {
			if err := a.msgChannel.MarkAsRead(latest.ID); err != nil {
				log.Printf("Error marking startup command message as read: %v", err)
			}
		}
		return
	}

	a.processEmail(latest)
	if a.config.Email.MarkAsRead {
		if err := a.msgChannel.MarkAsRead(latest.ID); err != nil {
			log.Printf("Error marking startup latest message as read: %v", err)
		}
	}
}

// Stop 停止代理
func (a *Agent) Stop() error {
	close(a.stopChan)
	if err := a.saveState(); err != nil {
		log.Printf("Warning: failed to persist state on stop: %v", err)
	}
	return a.msgChannel.Close()
}

// pollEmails 轮询邮件
func (a *Agent) pollEmails() {
	for {
		interval := a.config.Email.PollInterval
		if interval <= 0 {
			interval = 30 * time.Second
		}

		select {
		case <-time.After(interval):
			a.maybeReloadConfig()
			log.Printf("Polling mailbox %s for new messages...", a.config.Email.Inbox)
			a.checkAndProcessEmails()
		case <-a.stopChan:
			log.Println("Stopping email polling...")
			return
		}
	}
}

func (a *Agent) reportStatusLoop() {
	for {
		enabled, interval, recipients := a.getStatusReportConfig()
		if interval <= 0 {
			interval = time.Hour
		}

		select {
		case <-time.After(interval):
			if enabled && len(recipients) > 0 {
				a.sendStatusReport(recipients)
			}
		case <-a.reportWakeCh:
			// 配置被动态调整，立即进入下一轮以应用新间隔。
			continue
		case <-a.stopChan:
			return
		}
	}
}

func (a *Agent) startSubAgents() {
	workers := a.getMaxConcurrentTasks()
	if workers <= 0 {
		workers = 1
	}
	queueSize := a.getSubAgentQueueSize()
	if queueSize <= 0 {
		queueSize = 100
	}

	a.mu.Lock()
	if a.taskQueue == nil {
		a.taskQueue = make(chan *channel.Message, queueSize)
	}
	a.mu.Unlock()

	for i := 0; i < workers; i++ {
		workerID := i + 1
		go func() {
			log.Printf("SubAgent-%d started", workerID)
			for {
				select {
				case msg := <-a.taskQueue:
					if msg == nil {
						continue
					}
					a.setSubTaskRunning(1)
					log.Printf("SubAgent-%d handling task: %s", workerID, msg.Subject)
					a.processEmail(msg)
					if a.config.Email.MarkAsRead {
						if err := a.msgChannel.MarkAsRead(msg.ID); err != nil {
							log.Printf("Error marking message as read: %v", err)
						}
					}
					a.setSubTaskRunning(-1)
					a.incrementCompletedSubtasks()
				case <-a.stopChan:
					log.Printf("SubAgent-%d stopped", workerID)
					return
				}
			}
		}()
	}
}

func (a *Agent) sendStatusReport(recipients []string) {
	body := a.buildStatusReportHTML()
	subject := fmt.Sprintf("Email Automation Agent 状态汇报 %s", time.Now().Format("2006-01-02 15:04:05"))

	for _, to := range recipients {
		recipient := normalizeEmailAddress(strings.TrimSpace(to))
		if recipient == "" {
			continue
		}
		if err := a.msgChannel.SendReply(recipient, subject, body, ""); err != nil {
			log.Printf("Error sending status report to %s: %v", recipient, err)
		}
	}
}

func (a *Agent) buildStatusReportHTML() string {
	total, success, failed, cacheHits, skipped, pendingCount, cacheCount, runningSubs, queuedSubs, completedSubs, inputTokens, outputTokens, totalTokens, lastAt, lastSummary := a.getRuntimeStats()
	lastTask := "无"
	if !lastAt.IsZero() {
		lastTask = fmt.Sprintf("%s (%s)", lastSummary, lastAt.Format("2006-01-02 15:04:05"))
	}

	return fmt.Sprintf(`
<!DOCTYPE html>
<html>
<head><meta charset="UTF-8"></head>
<body style="font-family: Arial, sans-serif; line-height: 1.6;">
  <h2>Email Automation Agent 状态汇报</h2>
  <table border="1" cellpadding="8" cellspacing="0" style="border-collapse: collapse;">
    <tr><td>总任务数</td><td>%d</td></tr>
    <tr><td>成功任务</td><td>%d</td></tr>
    <tr><td>失败任务</td><td>%d</td></tr>
    <tr><td>缓存命中数</td><td>%d</td></tr>
    <tr><td>白名单跳过数</td><td>%d</td></tr>
    <tr><td>待继续任务</td><td>%d</td></tr>
    <tr><td>缓存条目数</td><td>%d</td></tr>
    <tr><td>Token 输入总量</td><td>%d</td></tr>
    <tr><td>Token 输出总量</td><td>%d</td></tr>
    <tr><td>Token 总量</td><td>%d</td></tr>
    <tr><td>SubAgent 运行中</td><td>%d</td></tr>
    <tr><td>SubAgent 排队中</td><td>%d</td></tr>
    <tr><td>SubAgent 已完成</td><td>%d</td></tr>
    <tr><td>最近任务</td><td>%s</td></tr>
  </table>
  <p style="color:#666;font-size:12px;">此邮件由系统自动生成，收件人由 status_report.recipients 配置。</p>
</body>
</html>`, total, success, failed, cacheHits, skipped, pendingCount, cacheCount, inputTokens, outputTokens, totalTokens, runningSubs, queuedSubs, completedSubs, lastTask)
}

func (a *Agent) initConfigHotReload() {
	if strings.TrimSpace(a.configPath) == "" {
		return
	}
	if stat, err := os.Stat(a.configPath); err == nil {
		a.configModTime = stat.ModTime()
	}
}

func (a *Agent) maybeReloadConfig() {
	if strings.TrimSpace(a.configPath) == "" {
		return
	}

	stat, err := os.Stat(a.configPath)
	if err != nil {
		log.Printf("Warning: cannot stat config for hot reload: %v", err)
		return
	}
	if !stat.ModTime().After(a.configModTime) {
		return
	}

	newCfg, err := config.Load(a.configPath)
	if err != nil {
		log.Printf("Warning: config changed but reload failed: %v", err)
		return
	}

	oldLLM := a.llmClient
	newLLM, err := llm.NewClient(
		newCfg.LLM.Provider,
		newCfg.LLM.Model,
		newCfg.LLM.Command,
		newCfg.LLM.Timeout,
	)
	if err != nil {
		log.Printf("Warning: failed to reload LLM client, keeping old one: %v", err)
		newLLM = oldLLM
	}

	if ec, ok := a.msgChannel.(*channel.EmailChannel); ok {
		ec.UpdateSMTP(
			newCfg.Email.SMTP.Host,
			newCfg.Email.SMTP.Port,
			newCfg.Email.SMTP.Username,
			newCfg.Email.SMTP.Password,
			newCfg.Email.SMTP.UseSSL,
		)
	}
	a.executor = executor.NewExecutor(
		newCfg.Executor.WorkDir,
		newCfg.Executor.Timeout,
		newCfg.Executor.AllowedLangs,
		newCfg.Executor.MaxOutputSize,
		newCfg.Executor.Sandbox,
		newCfg.Executor.SandboxAllowNetwork,
		newCfg.Executor.SandboxCPUs,
		newCfg.Executor.SandboxMemory,
		newCfg.Executor.SandboxPidsLimit,
		newCfg.Executor.SandboxTmpfsSize,
		newCfg.Executor.SandboxReadOnly,
	)
	a.mu.Lock()
	a.config = newCfg
	a.llmClient = newLLM
	a.maxConcurrentTasks = newCfg.Email.MaxConcurrentTasks
	a.useSubAgent = newCfg.Email.UseSubAgent
	a.subagentQueueSize = newCfg.Email.SubAgentQueueSize
	a.cacheEnabled = newCfg.Cache.Enabled
	a.cacheTTL = newCfg.Cache.TTL
	a.cacheMaxItems = newCfg.Cache.MaxEntries
	a.cacheLLMValidate = newCfg.Cache.LLMValidateOnMiss
	a.reportEnabled = newCfg.StatusReport.Enabled
	a.reportInterval = newCfg.StatusReport.Interval
	a.reportRecipients = append([]string(nil), newCfg.StatusReport.Recipients...)
	a.mu.Unlock()
	a.configModTime = stat.ModTime()
	a.wakeStatusReportLoop()

	log.Printf("Configuration hot reloaded at %s", a.configModTime.Format(time.RFC3339))
}

func (a *Agent) wakeStatusReportLoop() {
	select {
	case a.reportWakeCh <- struct{}{}:
	default:
	}
}

// checkAndProcessEmails 检查并处理邮件
func (a *Agent) checkAndProcessEmails() {
	messages, err := a.msgChannel.FetchLatestMessages(a.config.Email.Inbox, 100)
	if err != nil {
		log.Printf("Error fetching emails: %v", err)
		a.handleFetchError(err)
		return
	}
	a.resetFetchErrorState()

	if len(messages) == 0 {
		log.Println("No email found in this polling cycle")
		return
	}

	newMessages := make([]*channel.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.UID == 0 {
			continue
		}
		if a.isUIDProcessed(msg.UID) {
			continue
		}
		newMessages = append(newMessages, msg)
	}

	if len(newMessages) == 0 {
		log.Println("No new email found in this polling cycle")
		return
	}

	log.Printf("Found %d new email(s)", len(newMessages))

	if a.isSubAgentEnabled() {
		for _, msg := range newMessages {
			if msg.UID != 0 {
				a.markUIDProcessed(msg.UID)
			}

			if a.tryHandleStatusCommand(msg) {
				if a.config.Email.MarkAsRead {
					if err := a.msgChannel.MarkAsRead(msg.ID); err != nil {
						log.Printf("Error marking command message as read: %v", err)
					}
				}
				continue
			}

			if !a.isSenderAllowed(msg.From) {
				log.Printf("Skipping email from %s: sender is not in whitelist", msg.From)
				a.recordWhitelistSkip()
				if a.config.Email.MarkAsRead {
					if err := a.msgChannel.MarkAsRead(msg.ID); err != nil {
						log.Printf("Error marking message as read: %v", err)
					}
				}
				continue
			}

			if err := a.enqueueSubTask(msg); err != nil {
				log.Printf("Failed to enqueue subagent task, fallback direct execution: %v", err)
				a.processEmail(msg)
				if a.config.Email.MarkAsRead {
					if err := a.msgChannel.MarkAsRead(msg.ID); err != nil {
						log.Printf("Error marking message as read: %v", err)
					}
				}
			}
		}
		return
	}

	workers := a.getMaxConcurrentTasks()
	if workers <= 0 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for _, msg := range newMessages {
		// 先登记 UID，避免同一封邮件在下个轮询重复触发
		if msg.UID != 0 {
			a.markUIDProcessed(msg.UID)
		}

		m := msg
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if a.tryHandleStatusCommand(m) {
				if a.config.Email.MarkAsRead {
					if err := a.msgChannel.MarkAsRead(m.ID); err != nil {
						log.Printf("Error marking command message as read: %v", err)
					}
				}
				return
			}

			if !a.isSenderAllowed(m.From) {
				log.Printf("Skipping email from %s: sender is not in whitelist", m.From)
				a.recordWhitelistSkip()
			} else {
				a.processEmail(m)
			}

			// 标记为已读
			if a.config.Email.MarkAsRead {
				if err := a.msgChannel.MarkAsRead(m.ID); err != nil {
					log.Printf("Error marking message as read: %v", err)
				}
			}
		}()
	}

	wg.Wait()
}

func (a *Agent) handleFetchError(fetchErr error) {
	now := time.Now()

	a.mu.Lock()
	if !a.nextReconnectAt.IsZero() && now.Before(a.nextReconnectAt) {
		wait := a.nextReconnectAt.Sub(now).Round(time.Second)
		failures := a.fetchErrorCount
		a.mu.Unlock()
		log.Printf("Reconnect backoff active (%d failures), next reconnect attempt in %v", failures, wait)
		return
	}
	a.mu.Unlock()

	if err := a.msgChannel.Reconnect(); err != nil {
		a.mu.Lock()
		a.fetchErrorCount++
		failures := a.fetchErrorCount
		backoff := reconnectBackoff(failures)
		a.nextReconnectAt = now.Add(backoff)
		nextAt := a.nextReconnectAt
		a.mu.Unlock()
		log.Printf("Reconnect failed (attempt #%d): %v; next retry at %s", failures, err, nextAt.Format("2006-01-02 15:04:05"))
		return
	}

	a.mu.Lock()
	prevFailures := a.fetchErrorCount
	a.fetchErrorCount = 0
	a.nextReconnectAt = time.Time{}
	a.mu.Unlock()
	log.Printf("Channel reconnected successfully after fetch error (previous consecutive failures=%d): %v", prevFailures, fetchErr)
}

func reconnectBackoff(failures int) time.Duration {
	if failures <= 0 {
		return time.Second
	}
	backoff := time.Second << (failures - 1) // 1s,2s,4s,8s...
	if backoff > 2*time.Minute {
		backoff = 2 * time.Minute
	}
	return backoff
}

func (a *Agent) resetFetchErrorState() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fetchErrorCount = 0
	a.nextReconnectAt = time.Time{}
}

func (a *Agent) bootstrapProcessedUIDs() error {
	messages, err := a.msgChannel.FetchLatestMessages(a.config.Email.Inbox, 200)
	if err != nil {
		return err
	}

	for _, msg := range messages {
		if msg.UID != 0 {
			a.processedUIDs[msg.UID] = struct{}{}
		}
	}

	log.Printf("Bootstrap complete: %d existing message UID(s) cached", len(a.processedUIDs))
	if err := a.saveState(); err != nil {
		log.Printf("Warning: failed to persist bootstrap state: %v", err)
	}
	return nil
}

// processEmail 处理单封邮件
func (a *Agent) processEmail(msg *channel.Message) {
	log.Printf("Processing email from %s, subject: %s", msg.From, msg.Subject)
	a.recordTaskStart(fmt.Sprintf("%s | %s", normalizeEmailAddress(msg.From), msg.Subject))
	taskUsages := make([]*llm.Usage, 0, 4)

	ctx, cancel := context.WithTimeout(context.Background(), a.config.LLM.Timeout+a.config.Executor.Timeout)
	defer cancel()

	taskKey := buildTaskKey(msg.From, msg.Subject)
	taskDescription := a.buildTaskDescription(msg)
	if pending, ok := a.getPendingTask(taskKey); ok {
		taskDescription = a.buildFollowupTaskDescription(pending, msg)
		log.Printf("Continuing pending task for %s", taskKey)
	}
	cacheKeys := buildToolCacheKeys(msg)

	// 命中工具缓存时，优先直接执行
	if matchedKey, cached, ok := a.getCachedToolByKeys(cacheKeys); ok {
		log.Printf("Tool cache hit key=%s (hits=%d), executing cached code for subject: %s", matchedKey, cached.HitCount, msg.Subject)
		cacheResult, execErr := a.executor.Execute(cached.Code, cached.Language)
		if execErr == nil && cacheResult != nil && cacheResult.Success {
			a.touchToolCache(matchedKey)
			a.clearPendingTask(taskKey)
			a.recordTaskSuccess(true)
			a.sendReply(
				msg.From,
				fmt.Sprintf("任务执行成功（缓存命中，耗时：%v）", cacheResult.Duration),
				fmt.Sprintf("%s<h3>缓存说明</h3><p>命中近期成功工具缓存，跳过代码生成阶段。</p><h3>Token 使用</h3><p>本次任务未调用 LLM（0 token）。</p><h3>执行输出</h3><pre>%s</pre>", a.formatTaskContext(msg), cacheResult.Output),
				msg.ReplyToID,
			)
			return
		}
		log.Printf("Cached tool execution failed, fallback to regeneration. error=%v", execErr)
		a.removeToolCache(matchedKey)
	}

	if a.isCacheLLMValidationEnabled() {
		if matchedKey, cached, usage, ok := a.tryLLMSelectCachedTool(msg); ok {
			a.recordTokenUsage(usage)
			taskUsages = append(taskUsages, usage)
			log.Printf("LLM selected cached tool key=%s for subject: %s", matchedKey, msg.Subject)
			cacheResult, execErr := a.executor.Execute(cached.Code, cached.Language)
			if execErr == nil && cacheResult != nil && cacheResult.Success {
				a.touchToolCache(matchedKey)
				a.clearPendingTask(taskKey)
				a.recordTaskSuccess(true)
				a.sendReply(
					msg.From,
					fmt.Sprintf("任务执行成功（智能缓存复用，耗时：%v）", cacheResult.Duration),
					fmt.Sprintf("%s%s<h3>缓存说明</h3><p>通过 LLM 选择了可复用缓存，跳过代码生成阶段。</p><h3>执行结果</h3><pre>%s</pre>", a.formatTaskContext(msg), a.formatTaskUsageHTML(taskUsages), cacheResult.Output),
					msg.ReplyToID,
				)
				return
			}
			log.Printf("LLM-selected cached tool execution failed, fallback to regeneration. error=%v", execErr)
		}
	}

	// 调用 LLM 生成代码
	explanation, code, usage, err := a.getLLMClient().GenerateCode(ctx, taskDescription)
	if err != nil {
		log.Printf("Error generating code: %v", err)
		a.recordTaskFailed()
		a.storePendingTask(taskKey, taskDescription, err.Error())
		a.sendClarificationRequest(
			msg,
			"任务处理失败，请直接回复此邮件补充信息后继续。",
			fmt.Sprintf("%s%s<h3>错误信息</h3><pre>%v</pre>", a.formatTaskContext(msg), a.formatTaskUsageHTML(taskUsages), err),
		)
		return
	}
	a.recordTokenUsage(usage)
	taskUsages = append(taskUsages, usage)

	// 检测代码语言
	language := a.detectLanguage(code)
	if language == "" {
		language = "python" // 默认使用 Python
	}

	// 执行代码
	result, err := a.executor.Execute(code, language)
	if err != nil {
		log.Printf("Error executing code: %v", err)
		a.recordTaskFailed()
		a.storePendingTask(taskKey, taskDescription, err.Error())
		a.sendClarificationRequest(
			msg,
			"任务执行失败，请直接回复此邮件补充信息后继续。",
			fmt.Sprintf("%s%s<h3>错误信息</h3><pre>%v</pre>", a.formatTaskContext(msg), a.formatTaskUsageHTML(taskUsages), err),
		)
		return
	}

	// 构建回复
	if result.Success {
		a.clearPendingTask(taskKey)
		a.storeToolCacheForKeys(cacheKeys, explanation, code, language)
		a.recordTaskSuccess(false)
		log.Printf("Code executed successfully in %v", result.Duration)
		a.sendReply(
			msg.From,
			fmt.Sprintf("任务执行成功 (耗时：%v)", result.Duration),
			fmt.Sprintf("%s%s<h3>执行结果</h3><pre>%s</pre>", a.formatTaskContext(msg), a.formatTaskUsageHTML(taskUsages), result.Output),
			msg.ReplyToID,
		)
	} else {
		fixedExplanation, fixedCode, fixedLanguage, fixedResult, fixUsage, fixErr := a.tryAutoFixOnce(taskDescription, explanation, code, language, result)
		a.recordTokenUsage(fixUsage)
		taskUsages = append(taskUsages, fixUsage)
		if fixErr == nil && fixedResult != nil && fixedResult.Success {
			a.clearPendingTask(taskKey)
			a.storeToolCacheForKeys(cacheKeys, fixedExplanation, fixedCode, fixedLanguage)
			a.recordTaskSuccess(false)
			log.Printf("Auto-fix succeeded in %v", fixedResult.Duration)
			a.sendReply(
				msg.From,
				fmt.Sprintf("任务执行成功（自动修复后，耗时：%v）", fixedResult.Duration),
				fmt.Sprintf("%s%s<h3>执行结果</h3><pre>%s</pre>", a.formatTaskContext(msg), a.formatTaskUsageHTML(taskUsages), fixedResult.Output),
				msg.ReplyToID,
			)
			return
		}

		a.storePendingTask(taskKey, taskDescription, result.Error)
		a.recordTaskFailed()
		log.Printf("Code execution failed: %s", result.Error)
		if fixErr != nil {
			log.Printf("Auto-fix attempt failed: %v", fixErr)
		}
		a.sendClarificationRequest(
			msg,
			"任务执行失败，请直接回复此邮件补充信息后继续。",
			fmt.Sprintf("%s%s<h3>错误信息</h3><pre>%s</pre><h3>运行输出</h3><pre>%s</pre>", a.formatTaskContext(msg), a.formatTaskUsageHTML(taskUsages), result.Error, result.Output),
		)
	}
}

// buildTaskDescription 构建任务描述
func (a *Agent) buildTaskDescription(msg *channel.Message) string {
	var sb strings.Builder

	sb.WriteString("请根据以下用户请求生成可执行的代码：\n\n")
	sb.WriteString(fmt.Sprintf("邮件主题：%s\n", msg.Subject))
	sb.WriteString(fmt.Sprintf("发件人：%s\n\n", msg.From))
	sb.WriteString("用户请求内容：\n")
	sb.WriteString(msg.Body)

	return sb.String()
}

func (a *Agent) formatTaskContext(msg *channel.Message) string {
	if msg == nil {
		return ""
	}

	body := strings.TrimSpace(msg.Body)
	if len(body) > 1000 {
		body = body[:1000] + "\n...(已截断)"
	}

	return fmt.Sprintf("<h3>任务信息</h3><p><strong>主题：</strong>%s</p><p><strong>发件人：</strong>%s</p><h4>邮件内容</h4><pre>%s</pre>",
		msg.Subject, msg.From, body)
}

func (a *Agent) formatTaskUsageHTML(usages []*llm.Usage) string {
	if len(usages) == 0 {
		return "<h3>Token 使用</h3><p>本次任务未调用 LLM（0 token）。</p>"
	}

	var sb strings.Builder
	sb.WriteString("<h3>Token 使用</h3><ul>")
	totalIn := 0
	totalOut := 0
	total := 0
	for i, u := range usages {
		if u == nil {
			continue
		}
		stepTotal := u.TotalTokens
		if stepTotal <= 0 {
			stepTotal = u.InputTokens + u.OutputTokens
		}
		totalIn += u.InputTokens
		totalOut += u.OutputTokens
		total += stepTotal
		estimateLabel := ""
		if u.IsEstimated {
			estimateLabel = "（估算）"
		}
		sb.WriteString(fmt.Sprintf("<li>阶段 %d - %s%s：输入 %d，输出 %d，合计 %d</li>", i+1, u.Provider, estimateLabel, u.InputTokens, u.OutputTokens, stepTotal))
	}
	sb.WriteString("</ul>")
	sb.WriteString(fmt.Sprintf("<p><strong>任务总计</strong>：输入 %d，输出 %d，合计 %d token。</p>", totalIn, totalOut, total))
	return sb.String()
}

func (a *Agent) buildFollowupTaskDescription(pending *PendingTask, msg *channel.Message) string {
	var sb strings.Builder
	sb.WriteString(pending.BaseTaskDescription)
	sb.WriteString("\n\n---\n用户补充说明（最新回复）：\n")
	sb.WriteString(msg.Body)
	if pending.LastError != "" {
		sb.WriteString("\n\n上一次失败原因：\n")
		sb.WriteString(pending.LastError)
	}
	return sb.String()
}

func (a *Agent) tryAutoFixOnce(taskDescription, explanation, code, language string, failedResult *executor.ExecutionResult) (string, string, string, *executor.ExecutionResult, *llm.Usage, error) {
	if failedResult == nil {
		return "", "", "", nil, nil, fmt.Errorf("missing failed result")
	}

	fixPrompt := fmt.Sprintf(`请修复下面这段代码的运行错误，并返回可直接运行的修复后代码。

要求：
1. 保持原任务目标不变
2. 优先做最小改动修复
3. 默认仅使用标准库（除非任务明确要求第三方库）
4. 输出格式仍然是：先简短说明，再给出 markdown 代码块

原始任务：
%s

代码说明：
%s

原始代码（语言：%s）：
~~~%s
%s
~~~

执行错误：
%s

执行输出：
%s
`, taskDescription, explanation, language, language, code, failedResult.Error, failedResult.Output)

	fixCtx, cancel := context.WithTimeout(context.Background(), a.config.LLM.Timeout)
	defer cancel()

	fixedExplanation, fixedCode, usage, err := a.getLLMClient().GenerateCode(fixCtx, fixPrompt)
	if err != nil {
		return "", "", "", nil, usage, err
	}
	if strings.TrimSpace(fixedCode) == "" {
		return "", "", "", nil, usage, fmt.Errorf("auto-fix returned empty code")
	}

	fixedLanguage := a.detectLanguage(fixedCode)
	if fixedLanguage == "" {
		fixedLanguage = language
	}

	fixedResult, err := a.executor.Execute(fixedCode, fixedLanguage)
	if err != nil {
		return fixedExplanation, fixedCode, fixedLanguage, nil, usage, err
	}

	return fixedExplanation, fixedCode, fixedLanguage, fixedResult, usage, nil
}

// detectLanguage 检测代码语言
func (a *Agent) detectLanguage(code string) string {
	// 检查 shebang
	if strings.HasPrefix(code, "#!/") {
		firstLine := strings.SplitN(code, "\n", 2)[0]
		if strings.Contains(firstLine, "python") {
			return "python"
		}
		if strings.Contains(firstLine, "bash") || strings.Contains(firstLine, "sh") {
			return "bash"
		}
	}

	// 检查包声明
	if strings.Contains(code, "package main") {
		return "go"
	}

	// 检查 import
	if strings.Contains(code, "import ") && strings.Contains(code, "func main()") {
		return "go"
	}

	// 检查 Python 特征
	if regexp.MustCompile(`^import\s+\w+`).MatchString(code) || regexp.MustCompile(`^from\s+\w+\s+import`).MatchString(code) {
		return "python"
	}

	// 检查 Node.js 特征
	if strings.Contains(code, "require(") || strings.Contains(code, "console.log") {
		return "javascript"
	}

	return ""
}

// sendReply 发送回复邮件
func (a *Agent) sendReply(to, subject, body, inReplyTo string) {
	recipient := normalizeEmailAddress(to)
	htmlBody := fmt.Sprintf(`
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <style>
        body { font-family: Arial, sans-serif; line-height: 1.6; }
        pre { background: #f4f4f4; padding: 10px; border-radius: 5px; overflow-x: auto; }
        h3 { color: #333; border-bottom: 1px solid #ddd; padding-bottom: 5px; }
        .success { color: #28a745; }
        .error { color: #dc3545; }
    </style>
</head>
<body>
    <h2 class="%s">Email Automation Agent</h2>
    <p>您的任务已处理完成。</p>
    %s
    <hr>
    <p style="color: #666; font-size: 12px;">此邮件由 Email Automation Agent 自动生成</p>
</body>
</html>
`,
		strings.ToLower(strings.Split(subject, " ")[0]),
		body,
	)

	// 发送策略：
	// 1) 优先按线程回复（带 In-Reply-To）
	// 2) 若失败，重试并在最后一次降级为普通邮件（去掉 In-Reply-To）
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		replyTo := inReplyTo
		if attempt == 3 {
			// 某些 SMTP/网关对 In-Reply-To 头较严格，最后一次做降级发送
			replyTo = ""
		}

		if err := a.msgChannel.SendReply(recipient, subject, htmlBody, replyTo); err == nil {
			if attempt > 1 {
				log.Printf("Reply sent successfully after retry (attempt=%d, downgradedThreadHeader=%t)", attempt, replyTo == "")
			}
			return
		} else {
			lastErr = err
			log.Printf("Error sending reply (attempt=%d): %v", attempt, err)
		}

		time.Sleep(time.Duration(attempt) * time.Second)
	}
	log.Printf("Error sending reply after retries: %v", lastErr)
}

func (a *Agent) sendClarificationRequest(msg *channel.Message, tip, details string) {
	subject := fmt.Sprintf("需要补充信息：%s", msg.Subject)
	body := fmt.Sprintf("<p>%s</p><pre>%s</pre><p>请直接回复此邮件，我会基于你的回复继续执行（不会从头丢失上下文）。</p>", tip, details)
	a.sendReply(msg.From, subject, body, msg.ReplyToID)
}

func (a *Agent) storePendingTask(taskKey, baseTaskDescription, lastError string) {
	a.mu.Lock()
	a.pendingTasks[taskKey] = &PendingTask{
		BaseTaskDescription: baseTaskDescription,
		LastError:           lastError,
		UpdatedAt:           time.Now(),
	}
	a.mu.Unlock()
	if err := a.saveState(); err != nil {
		log.Printf("Warning: failed to persist pending task state: %v", err)
	}
}

func (a *Agent) clearPendingTask(taskKey string) {
	a.mu.Lock()
	if _, ok := a.pendingTasks[taskKey]; !ok {
		a.mu.Unlock()
		return
	}
	delete(a.pendingTasks, taskKey)
	a.mu.Unlock()
	if err := a.saveState(); err != nil {
		log.Printf("Warning: failed to persist pending task cleanup: %v", err)
	}
}

func (a *Agent) markUIDProcessed(uid uint32) {
	if uid == 0 {
		return
	}
	a.mu.Lock()
	if _, ok := a.processedUIDs[uid]; ok {
		a.mu.Unlock()
		return
	}
	a.processedUIDs[uid] = struct{}{}
	a.mu.Unlock()
	if err := a.saveState(); err != nil {
		log.Printf("Warning: failed to persist processed UID state: %v", err)
	}
}

func (a *Agent) isUIDProcessed(uid uint32) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.processedUIDs[uid]
	return ok
}

func (a *Agent) getMaxConcurrentTasks() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.maxConcurrentTasks
}

func (a *Agent) getStatusReportConfig() (bool, time.Duration, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	enabled := a.reportEnabled
	if a.reportEnabledOVR != nil {
		enabled = *a.reportEnabledOVR
	}
	interval := a.reportInterval
	if a.reportIntervalOVR > 0 {
		interval = a.reportIntervalOVR
	}
	return enabled, interval, append([]string(nil), a.reportRecipients...)
}

func (a *Agent) isSubAgentEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.useSubAgent
}

func (a *Agent) getSubAgentQueueSize() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.subagentQueueSize
}

func (a *Agent) enqueueSubTask(msg *channel.Message) error {
	a.mu.Lock()
	queue := a.taskQueue
	a.mu.Unlock()
	if queue == nil {
		return fmt.Errorf("task queue is not initialized")
	}

	select {
	case queue <- msg:
		log.Printf("Task enqueued to subagent queue: subject=%s, queued=%d", msg.Subject, len(queue))
		return nil
	case <-time.After(3 * time.Second):
		return fmt.Errorf("subagent queue timeout")
	}
}

func (a *Agent) setSubTaskRunning(delta int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runningSubtasks += delta
	if a.runningSubtasks < 0 {
		a.runningSubtasks = 0
	}
}

func (a *Agent) incrementCompletedSubtasks() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.completedSubtasks++
}

func (a *Agent) getLLMClient() llm.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.llmClient
}

func (a *Agent) getRuntimeStats() (uint64, uint64, uint64, uint64, uint64, int, int, int, int, uint64, uint64, uint64, uint64, time.Time, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	queueLen := 0
	if a.taskQueue != nil {
		queueLen = len(a.taskQueue)
	}
	return a.totalTasks, a.successTasks, a.failedTasks, a.cacheHitTasks, a.skippedByWhitelist, len(a.pendingTasks), len(a.toolCache), a.runningSubtasks, queueLen, a.completedSubtasks, a.totalInputTokens, a.totalOutputTokens, a.totalTokens, a.lastTaskAt, a.lastTaskSummary
}

func (a *Agent) recordTaskStart(summary string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.totalTasks++
	a.lastTaskAt = time.Now()
	a.lastTaskSummary = summary
}

func (a *Agent) recordTaskSuccess(cacheHit bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.successTasks++
	if cacheHit {
		a.cacheHitTasks++
	}
}

func (a *Agent) recordTaskFailed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failedTasks++
}

func (a *Agent) recordTokenUsage(u *llm.Usage) {
	if u == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if u.InputTokens > 0 {
		a.totalInputTokens += uint64(u.InputTokens)
	}
	if u.OutputTokens > 0 {
		a.totalOutputTokens += uint64(u.OutputTokens)
	}
	if u.TotalTokens > 0 {
		a.totalTokens += uint64(u.TotalTokens)
	} else {
		a.totalTokens += uint64(u.InputTokens + u.OutputTokens)
	}
}

func (a *Agent) recordWhitelistSkip() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.skippedByWhitelist++
}

func (a *Agent) getPendingTask(taskKey string) (*PendingTask, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending, ok := a.pendingTasks[taskKey]
	if !ok || pending == nil {
		return nil, false
	}
	copy := *pending
	return &copy, true
}

func (a *Agent) getCachedTool(cacheKey string) (*CachedTool, bool) {
	if !a.cacheEnabled {
		return nil, false
	}

	a.mu.Lock()
	entry, ok := a.toolCache[cacheKey]
	if !ok || entry == nil {
		a.mu.Unlock()
		return nil, false
	}

	now := time.Now()
	if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
		delete(a.toolCache, cacheKey)
		a.mu.Unlock()
		_ = a.saveState()
		return nil, false
	}
	copy := *entry
	a.mu.Unlock()
	return &copy, true
}

func (a *Agent) getCachedToolByKeys(keys []string) (string, *CachedTool, bool) {
	for _, key := range keys {
		if key == "" {
			continue
		}
		if entry, ok := a.getCachedTool(key); ok {
			return key, entry, true
		}
	}

	// 兜底：历史缓存可能还没有 intent key，按意图启发式复用
	for _, key := range keys {
		if strings.HasPrefix(key, "intent:weather:") {
			if matchedKey, entry, ok := a.findWeatherCacheFallback(); ok {
				return matchedKey, entry, true
			}
		}
	}

	return "", nil, false
}

func (a *Agent) isCacheLLMValidationEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cacheEnabled && a.cacheLLMValidate
}

func (a *Agent) tryLLMSelectCachedTool(msg *channel.Message) (string, *CachedTool, *llm.Usage, bool) {
	if msg == nil {
		return "", nil, nil, false
	}

	type candidate struct {
		Key       string
		Language  string
		Summary   string
		UpdatedAt time.Time
		Entry     *CachedTool
	}

	now := time.Now()
	candidates := make([]candidate, 0, 10)
	a.mu.Lock()
	for key, entry := range a.toolCache {
		if entry == nil {
			continue
		}
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			continue
		}
		summary := strings.TrimSpace(entry.Explanation)
		if summary == "" {
			summary = strings.TrimSpace(entry.Code)
		}
		if len(summary) > 180 {
			summary = summary[:180]
		}
		candidates = append(candidates, candidate{
			Key:       key,
			Language:  entry.Language,
			Summary:   summary,
			UpdatedAt: entry.UpdatedAt,
			Entry:     entry,
		})
	}
	a.mu.Unlock()

	if len(candidates) == 0 {
		return "", nil, nil, false
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
	})
	if len(candidates) > 8 {
		candidates = candidates[:8]
	}

	var sb strings.Builder
	sb.WriteString("请判断以下缓存脚本是否可复用于当前任务。")
	sb.WriteString("\n如果可复用，请仅返回最合适的 key；如果都不合适，返回 NONE。")
	sb.WriteString("\n输出格式：用 markdown 代码块只输出一行 key 或 NONE。")
	sb.WriteString("\n\n当前任务：\n")
	sb.WriteString("主题：")
	sb.WriteString(msg.Subject)
	sb.WriteString("\n正文：\n")
	sb.WriteString(msg.Body)
	sb.WriteString("\n\n候选缓存：\n")
	for i, c := range candidates {
		sb.WriteString(fmt.Sprintf("%d) key=%s lang=%s summary=%s\n", i+1, c.Key, c.Language, c.Summary))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, code, usage, err := a.getLLMClient().GenerateCode(ctx, sb.String())
	if err != nil {
		log.Printf("LLM cache validation failed: %v", err)
		return "", nil, usage, false
	}
	selection := strings.TrimSpace(strings.Split(code, "\n")[0])
	selection = strings.Trim(selection, "` ")
	if selection == "" || strings.EqualFold(selection, "none") {
		return "", nil, usage, false
	}

	for _, c := range candidates {
		if c.Key == selection {
			cp := *c.Entry
			return c.Key, &cp, usage, true
		}
	}
	return "", nil, usage, false
}

func (a *Agent) findWeatherCacheFallback() (string, *CachedTool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	var bestKey string
	var best *CachedTool
	for key, entry := range a.toolCache {
		if entry == nil {
			continue
		}
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			continue
		}
		sample := strings.ToLower(entry.Code + "\n" + entry.Explanation)
		if !strings.Contains(sample, "weather") &&
			!strings.Contains(sample, "天气") &&
			!strings.Contains(sample, "wttr.in") &&
			!strings.Contains(sample, "open-meteo") {
			continue
		}
		if best == nil || entry.UpdatedAt.After(best.UpdatedAt) {
			cp := *entry
			best = &cp
			bestKey = key
		}
	}
	if best == nil {
		return "", nil, false
	}
	return bestKey, best, true
}

func (a *Agent) storeToolCache(cacheKey, explanation, code, language string) {
	if !a.cacheEnabled {
		return
	}
	if strings.TrimSpace(code) == "" || strings.TrimSpace(language) == "" {
		return
	}

	now := time.Now()
	a.mu.Lock()
	a.toolCache[cacheKey] = &CachedTool{
		Explanation: explanation,
		Code:        code,
		Language:    language,
		UpdatedAt:   now,
		ExpiresAt:   now.Add(a.cacheTTL),
		HitCount:    0,
	}

	a.trimToolCache()
	a.mu.Unlock()
	if err := a.saveState(); err != nil {
		log.Printf("Warning: failed to persist tool cache state: %v", err)
	}
}

func (a *Agent) storeToolCacheForKeys(keys []string, explanation, code, language string) {
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		a.storeToolCache(key, explanation, code, language)
	}
}

func (a *Agent) touchToolCache(cacheKey string) {
	a.mu.Lock()
	entry, ok := a.toolCache[cacheKey]
	if !ok || entry == nil {
		a.mu.Unlock()
		return
	}
	entry.HitCount++
	entry.UpdatedAt = time.Now()
	entry.ExpiresAt = entry.UpdatedAt.Add(a.cacheTTL)
	a.mu.Unlock()
	if err := a.saveState(); err != nil {
		log.Printf("Warning: failed to persist tool cache hit state: %v", err)
	}
}

func (a *Agent) removeToolCache(cacheKey string) {
	a.mu.Lock()
	delete(a.toolCache, cacheKey)
	a.mu.Unlock()
	if err := a.saveState(); err != nil {
		log.Printf("Warning: failed to persist tool cache eviction: %v", err)
	}
}

func (a *Agent) trimToolCache() {
	if a.cacheMaxItems <= 0 || len(a.toolCache) <= a.cacheMaxItems {
		return
	}

	type item struct {
		key       string
		updatedAt time.Time
	}

	items := make([]item, 0, len(a.toolCache))
	for k, v := range a.toolCache {
		when := time.Time{}
		if v != nil {
			when = v.UpdatedAt
		}
		items = append(items, item{key: k, updatedAt: when})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].updatedAt.Before(items[j].updatedAt)
	})

	toDelete := len(a.toolCache) - a.cacheMaxItems
	for i := 0; i < toDelete; i++ {
		delete(a.toolCache, items[i].key)
	}
}

func (a *Agent) loadState() error {
	data, err := os.ReadFile(a.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}

	a.mu.Lock()
	for _, uid := range st.ProcessedUIDs {
		if uid != 0 {
			a.processedUIDs[uid] = struct{}{}
		}
	}
	if st.PendingTasks != nil {
		a.pendingTasks = st.PendingTasks
	}
	if st.ToolCache != nil {
		now := time.Now()
		for key, tool := range st.ToolCache {
			if tool == nil {
				continue
			}
			if !tool.ExpiresAt.IsZero() && now.After(tool.ExpiresAt) {
				continue
			}
			a.toolCache[key] = tool
		}
	}
	a.totalInputTokens = st.TotalInput
	a.totalOutputTokens = st.TotalOutput
	a.totalTokens = st.TotalTokens
	a.reportEnabledOVR = st.ReportEnabled
	if strings.TrimSpace(st.ReportInterval) != "" {
		if d, err := time.ParseDuration(st.ReportInterval); err == nil && d > 0 {
			a.reportIntervalOVR = d
		}
	}
	processedCount := len(a.processedUIDs)
	pendingCount := len(a.pendingTasks)
	cacheCount := len(a.toolCache)
	a.mu.Unlock()

	log.Printf("Loaded state: %d processed UID(s), %d pending task(s), %d cached tool(s)", processedCount, pendingCount, cacheCount)
	return nil
}

func (a *Agent) saveState() error {
	if err := os.MkdirAll(filepath.Dir(a.statePath), 0755); err != nil {
		return err
	}

	a.mu.Lock()
	uids := make([]uint32, 0, len(a.processedUIDs))
	for uid := range a.processedUIDs {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	pendingTasks := make(map[string]*PendingTask, len(a.pendingTasks))
	for k, v := range a.pendingTasks {
		if v == nil {
			continue
		}
		cp := *v
		pendingTasks[k] = &cp
	}
	toolCache := make(map[string]*CachedTool, len(a.toolCache))
	for k, v := range a.toolCache {
		if v == nil {
			continue
		}
		cp := *v
		toolCache[k] = &cp
	}
	totalInput := a.totalInputTokens
	totalOutput := a.totalOutputTokens
	totalTokens := a.totalTokens
	reportEnabledOVR := a.reportEnabledOVR
	reportIntervalOVR := a.reportIntervalOVR
	a.mu.Unlock()

	st := persistedState{
		ProcessedUIDs: uids,
		PendingTasks:  pendingTasks,
		ToolCache:     toolCache,
		TotalInput:    totalInput,
		TotalOutput:   totalOutput,
		TotalTokens:   totalTokens,
		ReportEnabled: reportEnabledOVR,
	}
	if reportIntervalOVR > 0 {
		st.ReportInterval = reportIntervalOVR.String()
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}

	tmp := a.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, a.statePath)
}

func buildTaskKey(from, subject string) string {
	addr := strings.ToLower(strings.TrimSpace(normalizeEmailAddress(from)))
	sub := strings.ToLower(strings.TrimSpace(normalizeThreadSubject(subject)))
	return addr + "|" + sub
}

func buildToolCacheKey(msg *channel.Message) string {
	if msg == nil {
		return ""
	}
	base := strings.ToLower(strings.TrimSpace(normalizeThreadSubject(msg.Subject)))
	body := normalizeForCache(msg.Body)
	sum := sha256.Sum256([]byte(base + "\n" + body))
	return hex.EncodeToString(sum[:])
}

func buildToolCacheKeys(msg *channel.Message) []string {
	if msg == nil {
		return nil
	}

	primary := buildToolCacheKey(msg)
	body := strings.ToLower(normalizeForCache(msg.Body))
	bodyOnly := ""
	if body != "" {
		sum := sha256.Sum256([]byte(body))
		bodyOnly = "body:" + hex.EncodeToString(sum[:])
	}

	intent := buildIntentCacheKey(msg.Subject, msg.Body)

	keys := make([]string, 0, 3)
	if primary != "" {
		keys = append(keys, primary)
	}
	if bodyOnly != "" {
		keys = append(keys, bodyOnly)
	}
	if intent != "" {
		keys = append(keys, intent)
	}
	return keys
}

func buildIntentCacheKey(subject, body string) string {
	text := strings.ToLower(strings.TrimSpace(subject + "\n" + body))
	if text == "" {
		return ""
	}

	if strings.Contains(text, "天气") || strings.Contains(text, "weather") {
		target := extractWeatherTarget(text)
		if target == "" {
			target = "default"
		}
		return "intent:weather:" + target
	}

	return ""
}

func extractWeatherTarget(text string) string {
	patterns := []string{
		`(?:查询|查|看|获取)?\s*([\p{Han}A-Za-z]{2,20})\s*的?\s*天气`,
		`([\p{Han}A-Za-z]{2,20})\s*天气`,
	}

	stopwords := map[string]struct{}{
		"查询": {}, "天气": {}, "今天": {}, "明天": {}, "现在": {}, "实时": {},
		"weather": {}, "query": {}, "check": {}, "the": {},
	}

	for _, p := range patterns {
		re := regexp.MustCompile(p)
		matches := re.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			candidate := strings.TrimSpace(strings.ToLower(m[1]))
			if candidate == "" {
				continue
			}
			if _, ok := stopwords[candidate]; ok {
				continue
			}
			return candidate
		}
	}
	return ""
}

func normalizeForCache(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	return strings.Join(strings.Fields(trimmed), " ")
}

func normalizeThreadSubject(subject string) string {
	s := strings.TrimSpace(subject)
	for {
		lower := strings.ToLower(s)
		switch {
		case strings.HasPrefix(lower, "re:"):
			s = strings.TrimSpace(s[3:])
		case strings.HasPrefix(lower, "fw:"):
			s = strings.TrimSpace(s[3:])
		case strings.HasPrefix(lower, "fwd:"):
			s = strings.TrimSpace(s[4:])
		default:
			return s
		}
	}
}

type statusCommand struct {
	Action   string
	Interval time.Duration
}

func (a *Agent) tryHandleStatusCommand(msg *channel.Message) bool {
	if msg == nil || !a.isStatusCommander(msg.From) {
		return false
	}

	cmd, ok, err := parseStatusCommand(msg.Subject, msg.Body)
	if !ok {
		return false
	}
	if err != nil {
		a.sendReply(
			msg.From,
			"状态汇报命令解析失败",
			fmt.Sprintf("%s<h3>错误</h3><pre>%s</pre><h3>可用命令</h3><pre>status show\nstatus now\nstatus on\nstatus off\nstatus interval 1m</pre>", a.formatTaskContext(msg), err.Error()),
			msg.ReplyToID,
		)
		return true
	}

	switch cmd.Action {
	case "show":
		enabled, interval, recipients := a.getStatusReportConfig()
		a.sendReply(
			msg.From,
			"状态汇报当前配置",
			fmt.Sprintf("%s<h3>当前状态</h3><p>enabled=%t</p><p>interval=%s</p><p>recipients=%s</p>", a.formatTaskContext(msg), enabled, interval, strings.Join(recipients, ", ")),
			msg.ReplyToID,
		)
	case "now":
		_, _, recipients := a.getStatusReportConfig()
		if len(recipients) == 0 {
			recipients = []string{normalizeEmailAddress(msg.From)}
		}
		a.sendStatusReport(recipients)
		a.sendReply(
			msg.From,
			"已触发立即汇报",
			fmt.Sprintf("%s<p>已立即发送一次状态汇报。</p>", a.formatTaskContext(msg)),
			msg.ReplyToID,
		)
	case "on":
		v := true
		a.mu.Lock()
		a.reportEnabledOVR = &v
		a.mu.Unlock()
		_ = a.saveState()
		a.wakeStatusReportLoop()
		a.sendReply(
			msg.From,
			"状态汇报已开启",
			fmt.Sprintf("%s<p>状态汇报已开启。</p>", a.formatTaskContext(msg)),
			msg.ReplyToID,
		)
	case "off":
		v := false
		a.mu.Lock()
		a.reportEnabledOVR = &v
		a.mu.Unlock()
		_ = a.saveState()
		a.wakeStatusReportLoop()
		a.sendReply(
			msg.From,
			"状态汇报已关闭",
			fmt.Sprintf("%s<p>状态汇报已关闭。</p>", a.formatTaskContext(msg)),
			msg.ReplyToID,
		)
	case "interval":
		a.mu.Lock()
		a.reportIntervalOVR = cmd.Interval
		a.mu.Unlock()
		_ = a.saveState()
		a.wakeStatusReportLoop()
		enabled, interval, _ := a.getStatusReportConfig()
		a.sendReply(
			msg.From,
			"状态汇报频率已更新",
			fmt.Sprintf("%s<p>新的汇报间隔：<strong>%s</strong></p><p>enabled=%t</p>", a.formatTaskContext(msg), interval, enabled),
			msg.ReplyToID,
		)
	case "reset":
		a.mu.Lock()
		a.reportEnabledOVR = nil
		a.reportIntervalOVR = 0
		a.mu.Unlock()
		_ = a.saveState()
		a.wakeStatusReportLoop()
		enabled, interval, _ := a.getStatusReportConfig()
		a.sendReply(
			msg.From,
			"状态汇报已恢复配置文件设置",
			fmt.Sprintf("%s<p>已清除邮件命令覆盖。</p><p>enabled=%t, interval=%s</p>", a.formatTaskContext(msg), enabled, interval),
			msg.ReplyToID,
		)
	default:
		return false
	}

	return true
}

func (a *Agent) isStatusCommander(from string) bool {
	sender := strings.ToLower(strings.TrimSpace(normalizeEmailAddress(from)))
	if sender == "" {
		return false
	}
	a.mu.Lock()
	recipients := append([]string(nil), a.reportRecipients...)
	a.mu.Unlock()
	for _, r := range recipients {
		if strings.ToLower(strings.TrimSpace(r)) == sender {
			return true
		}
	}
	return false
}

func parseStatusCommand(subject, body string) (statusCommand, bool, error) {
	raw := strings.TrimSpace(subject + "\n" + body)
	if raw == "" {
		return statusCommand{}, false, nil
	}
	lower := strings.ToLower(raw)

	if strings.Contains(lower, "status show") || strings.Contains(raw, "汇报状态") {
		return statusCommand{Action: "show"}, true, nil
	}
	if strings.Contains(lower, "status now") || strings.Contains(raw, "立即汇报") || strings.Contains(raw, "马上汇报") {
		return statusCommand{Action: "now"}, true, nil
	}
	if strings.Contains(lower, "status on") || strings.Contains(raw, "开启汇报") || strings.Contains(raw, "汇报开启") {
		return statusCommand{Action: "on"}, true, nil
	}
	if strings.Contains(lower, "status off") || strings.Contains(raw, "关闭汇报") || strings.Contains(raw, "停止汇报") {
		return statusCommand{Action: "off"}, true, nil
	}
	if strings.Contains(lower, "status reset") || strings.Contains(raw, "恢复汇报配置") || strings.Contains(raw, "重置汇报配置") {
		return statusCommand{Action: "reset"}, true, nil
	}

	if strings.Contains(lower, "status interval") || strings.Contains(lower, "report every") || strings.Contains(raw, "汇报频率") || strings.Contains(raw, "汇报间隔") {
		d, ok := parseCommandDuration(raw)
		if !ok {
			return statusCommand{}, true, fmt.Errorf("未识别到有效时间间隔，请使用如：status interval 1m / 汇报频率 1小时")
		}
		if d < time.Minute {
			return statusCommand{}, true, fmt.Errorf("最小支持 1 分钟，当前: %s", d)
		}
		return statusCommand{Action: "interval", Interval: d}, true, nil
	}

	return statusCommand{}, false, nil
}

func parseCommandDuration(text string) (time.Duration, bool) {
	re := regexp.MustCompile(`(?i)(\d+(?:\.\d+)?(?:ms|s|m|h))`)
	if m := re.FindStringSubmatch(text); len(m) == 2 {
		if d, err := time.ParseDuration(strings.ToLower(strings.TrimSpace(m[1]))); err == nil {
			return d, true
		}
	}

	zh := regexp.MustCompile(`(\d+)\s*(分钟|分|小时|时|天)`)
	if m := zh.FindStringSubmatch(text); len(m) == 3 {
		n := 0
		fmt.Sscanf(m[1], "%d", &n)
		if n <= 0 {
			return 0, false
		}
		switch m[2] {
		case "分钟", "分":
			return time.Duration(n) * time.Minute, true
		case "小时", "时":
			return time.Duration(n) * time.Hour, true
		case "天":
			return time.Duration(n) * 24 * time.Hour, true
		}
	}
	return 0, false
}

func (a *Agent) isSenderAllowed(from string) bool {
	if len(a.config.Email.AllowedSenders) == 0 {
		return true
	}

	sender := strings.ToLower(strings.TrimSpace(normalizeEmailAddress(from)))
	if sender == "" {
		return false
	}

	for _, allowed := range a.config.Email.AllowedSenders {
		if strings.ToLower(strings.TrimSpace(allowed)) == sender {
			return true
		}
	}

	return false
}

// normalizeEmailAddress 将 "Name <a@b.com>" 转换为 "a@b.com"
func normalizeEmailAddress(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return input
	}

	if addr, err := mail.ParseAddress(input); err == nil && addr.Address != "" {
		return addr.Address
	}

	if matches := regexp.MustCompile(`<([^<>]+)>`).FindStringSubmatch(input); len(matches) == 2 {
		return strings.TrimSpace(matches[1])
	}

	return input
}
