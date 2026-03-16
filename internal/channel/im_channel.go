package channel

import (
	"fmt"
	"sync"
	"time"
)

// IMConfig IM 通道骨架配置（与 config.IMConfig 对齐）。
type IMConfig struct {
	Platform     string
	Endpoint     string
	BotID        string
	BotToken     string
	PollInterval time.Duration
}

// IMInboundPayload 描述 IM 平台入站消息的通用映射结构（骨架）。
type IMInboundPayload struct {
	MessageID string
	UserID    string
	ChatID    string
	ThreadID  string
	Text      string
	Timestamp time.Time
	Raw       map[string]any
}

// IMOutboundPayload 描述 IM 平台回包消息的通用映射结构（骨架）。
type IMOutboundPayload struct {
	ToUserID   string
	ToChatID   string
	ThreadID   string
	Text       string
	Markdown   string
	InReplyTo  string
	Structured map[string]any
}

// IMChannel 是 IM 通道骨架实现：
// 1) 已实现 Channel 接口，便于无缝接入 Agent
// 2) 目前仅提供 in-memory 队列与映射函数，实际平台 API 调用待实现
type IMChannel struct {
	cfg   IMConfig
	queue chan *Message
	mu    sync.Mutex
}

func NewIMChannel(cfg IMConfig) *IMChannel {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	return &IMChannel{
		cfg:   cfg,
		queue: make(chan *Message, 200),
	}
}

func (c *IMChannel) Connect() error {
	// TODO: 初始化 IM SDK 客户端、鉴权、建链。
	return nil
}

func (c *IMChannel) Close() error {
	return nil
}

func (c *IMChannel) Reconnect() error {
	return c.Connect()
}

func (c *IMChannel) FetchLatestMessages(_ string, limit uint32) ([]*Message, error) {
	if limit == 0 {
		limit = 50
	}

	out := make([]*Message, 0, limit)
	for i := uint32(0); i < limit; i++ {
		select {
		case m := <-c.queue:
			out = append(out, m)
		default:
			return out, nil
		}
	}
	return out, nil
}

func (c *IMChannel) MarkAsRead(_ uint32) error {
	// 对多数 IM 平台，“已读”不一定是必要语义，默认 no-op。
	return nil
}

func (c *IMChannel) SendReply(_ string, _ string, _ string, _ string) error {
	// TODO: 将 HTML/Markdown 内容映射为目标 IM 平台消息格式并发送。
	return fmt.Errorf("im channel skeleton: SendReply is not implemented yet")
}

// PushInbound 将平台入站消息注入通道队列，供 Agent 轮询消费。
// 后续接入真实 IM SDK 时，可在 webhook/长连接回调里调用该方法。
func (c *IMChannel) PushInbound(payload IMInboundPayload) {
	msg := &Message{
		From:       payload.UserID,
		To:         payload.ChatID,
		Subject:    payload.ThreadID,
		Body:       payload.Text,
		ReceivedAt: payload.Timestamp,
		ReplyToID:  payload.MessageID,
	}
	select {
	case c.queue <- msg:
	default:
		// 队列满时丢弃最旧消息，避免阻塞。
		select {
		case <-c.queue:
		default:
		}
		c.queue <- msg
	}
}
