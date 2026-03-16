package channel

import (
	"sync"

	"email-automation-agent/internal/email"
)

// EmailChannel 基于 IMAP/SMTP 的通道实现。
type EmailChannel struct {
	imap *email.IMAPClient
	smtp *email.SMTPClient
	mu   sync.Mutex
}

func NewEmailChannel(
	imapHost string, imapPort int, imapUser, imapPass string, imapSSL bool,
	smtpHost string, smtpPort int, smtpUser, smtpPass string, smtpSSL bool,
) *EmailChannel {
	return &EmailChannel{
		imap: email.NewIMAPClient(imapHost, imapPort, imapUser, imapPass, imapSSL),
		smtp: email.NewSMTPClient(smtpHost, smtpPort, smtpUser, smtpPass, smtpSSL),
	}
}

func (c *EmailChannel) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.imap.Connect()
}

func (c *EmailChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.imap.Close()
}

func (c *EmailChannel) Reconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.imap.Close()
	return c.imap.Connect()
}

func (c *EmailChannel) FetchLatestMessages(inbox string, limit uint32) ([]*Message, error) {
	c.mu.Lock()
	messages, err := c.imap.FetchLatestMessages(inbox, limit)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]*Message, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}
		out = append(out, &Message{
			ID:         m.ID,
			UID:        m.UID,
			From:       m.From,
			To:         m.To,
			Subject:    m.Subject,
			Body:       m.Body,
			ReceivedAt: m.ReceivedAt,
			Seen:       m.Seen,
			ReplyToID:  m.MessageId,
		})
	}
	return out, nil
}

func (c *EmailChannel) MarkAsRead(id uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.imap.MarkAsRead(id)
}

func (c *EmailChannel) SendReply(to, subject, htmlBody, inReplyTo string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.smtp.SendEmail(to, subject, htmlBody, inReplyTo)
}

func (c *EmailChannel) UpdateSMTP(host string, port int, username, password string, useSSL bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.smtp = email.NewSMTPClient(host, port, username, password, useSSL)
}
