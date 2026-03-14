package channel

import "time"

// Message 统一的交互消息结构，屏蔽具体通道（Email/IM/其他）差异。
type Message struct {
	ID         uint32
	UID        uint32
	From       string
	To         string
	Subject    string
	Body       string
	ReceivedAt time.Time
	Seen       bool
	ReplyToID  string
}

// Channel 定义统一的消息通道抽象。
type Channel interface {
	Connect() error
	Close() error
	FetchLatestMessages(inbox string, limit uint32) ([]*Message, error)
	MarkAsRead(id uint32) error
	SendReply(to, subject, htmlBody, inReplyTo string) error
}
