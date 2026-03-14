package email

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-sasl"
)

// EmailMessage 表示一封邮件
type EmailMessage struct {
	ID          uint32
	UID         uint32
	From        string
	To          string
	Subject     string
	Body        string
	HTMLBody    string
	Attachments []Attachment
	ReceivedAt  time.Time
	Seen        bool
	MessageId   string
}

// Attachment 表示邮件附件
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// IMAPClient IMAP 客户端
type IMAPClient struct {
	host     string
	port     int
	username string
	password string
	useSSL   bool
	client   *client.Client
}

// NewIMAPClient 创建 IMAP 客户端
func NewIMAPClient(host string, port int, username, password string, useSSL bool) *IMAPClient {
	return &IMAPClient{
		host:     host,
		port:     port,
		username: username,
		password: password,
		useSSL:   useSSL,
	}
}

// Connect 连接到 IMAP 服务器
func (c *IMAPClient) Connect() error {
	var err error
	if c.useSSL {
		c.client, err = client.DialTLS(fmt.Sprintf("%s:%d", c.host, c.port), nil)
	} else {
		c.client, err = client.Dial(fmt.Sprintf("%s:%d", c.host, c.port))
	}
	if err != nil {
		return fmt.Errorf("failed to connect to IMAP server: %w", err)
	}

	auth := sasl.NewPlainClient("", c.username, c.password)
	if err := c.client.Authenticate(auth); err != nil {
		return fmt.Errorf("failed to authenticate: %w", err)
	}

	return nil
}

// Close 关闭连接
func (c *IMAPClient) Close() error {
	if c.client != nil {
		return c.client.Logout()
	}
	return nil
}

// FetchUnseenMessages 获取未读邮件
func (c *IMAPClient) FetchUnseenMessages(mailbox string) ([]*EmailMessage, error) {
	_, err := c.client.Select(mailbox, false)
	if err != nil {
		return nil, fmt.Errorf("failed to select mailbox: %w", err)
	}

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}

	ids, err := c.client.Search(criteria)
	if err != nil {
		return nil, fmt.Errorf("failed to search messages: %w", err)
	}

	if len(ids) == 0 {
		return nil, nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(ids...)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid, section.FetchItem()}

	var messages []*EmailMessage
	var mu sync.Mutex
	var fetchErr error
	var wg sync.WaitGroup

	ch := make(chan *imap.Message, 100)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for m := range ch {
			msg := c.parseMessage(m)
			if msg != nil {
				mu.Lock()
				messages = append(messages, msg)
				mu.Unlock()
			}
		}
	}()

	// Fetch 会在完成后自动关闭 channel
	fetchErr = c.client.Fetch(seqset, items, ch)

	wg.Wait()

	if fetchErr != nil {
		return nil, fmt.Errorf("failed to fetch messages: %w", fetchErr)
	}

	return messages, nil
}

// FetchLatestMessages 获取邮箱中最新 N 封邮件（不区分已读/未读）
func (c *IMAPClient) FetchLatestMessages(mailbox string, limit uint32) ([]*EmailMessage, error) {
	status, err := c.client.Select(mailbox, false)
	if err != nil {
		return nil, fmt.Errorf("failed to select mailbox: %w", err)
	}

	if status.Messages == 0 {
		return nil, nil
	}

	if limit == 0 {
		limit = 50
	}

	start := uint32(1)
	if status.Messages > limit {
		start = status.Messages - limit + 1
	}

	seqset := new(imap.SeqSet)
	seqset.AddRange(start, status.Messages)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid, section.FetchItem()}

	var messages []*EmailMessage
	var mu sync.Mutex
	var fetchErr error
	var wg sync.WaitGroup

	ch := make(chan *imap.Message, 100)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for m := range ch {
			msg := c.parseMessage(m)
			if msg != nil {
				mu.Lock()
				messages = append(messages, msg)
				mu.Unlock()
			}
		}
	}()

	fetchErr = c.client.Fetch(seqset, items, ch)
	wg.Wait()

	if fetchErr != nil {
		return nil, fmt.Errorf("failed to fetch latest messages: %w", fetchErr)
	}

	return messages, nil
}

// parseMessage 解析邮件内容
func (c *IMAPClient) parseMessage(m *imap.Message) *EmailMessage {
	msg := &EmailMessage{
		ID:   m.SeqNum,
		UID:  m.Uid,
		Seen: containsFlag(m.Flags, imap.SeenFlag),
	}

	if m.Envelope != nil {
		msg.ReceivedAt = m.Envelope.Date
		msg.Subject = m.Envelope.Subject
		msg.MessageId = m.Envelope.MessageId

		if len(m.Envelope.From) > 0 {
			msg.From = formatAddress(m.Envelope.From[0])
		}

		if len(m.Envelope.To) > 0 {
			var tos []string
			for _, to := range m.Envelope.To {
				tos = append(tos, formatAddress(to))
			}
			msg.To = strings.Join(tos, ", ")
		}
	}

	for _, value := range m.Body {
		if value != nil {
			body := c.parseBody(value)
			if body != "" {
				msg.Body = body
			}
		}
	}

	return msg
}

// parseBody 解析邮件体
func (c *IMAPClient) parseBody(literal imap.Literal) string {
	if literal == nil {
		return ""
	}

	data, err := io.ReadAll(literal)
	if err != nil {
		return ""
	}

	return c.parseRawMessage(data)
}

// parseRawMessage 解析原始邮件数据
func (c *IMAPClient) parseRawMessage(data []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return string(data)
	}

	return c.readMessageContent(msg)
}

// readMessageContent 读取邮件内容
func (c *IMAPClient) readMessageContent(msg *mail.Message) string {
	contentType := msg.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		body, _ := io.ReadAll(msg.Body)
		return string(body)
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		return c.readMultipart(msg.Body, params["boundary"])
	}

	return c.readPart(msg.Body, mediaType, msg.Header.Get("Content-Transfer-Encoding"))
}

// readMultipart 读取 multipart 邮件
func (c *IMAPClient) readMultipart(body io.Reader, boundary string) string {
	mr := multipart.NewReader(body, boundary)
	var textParts []string

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		contentType := part.Header.Get("Content-Type")
		encoding := part.Header.Get("Content-Transfer-Encoding")
		content := c.readPart(part, contentType, encoding)

		if strings.Contains(contentType, "text/plain") {
			textParts = append(textParts, content)
		}
	}

	return strings.Join(textParts, "\n\n")
}

// readPart 读取邮件部分
func (c *IMAPClient) readPart(part io.Reader, contentType, encoding string) string {
	var reader io.Reader = part

	switch strings.ToLower(encoding) {
	case "quoted-printable":
		reader = quotedprintable.NewReader(part)
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, part)
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		return ""
	}

	return string(content)
}

// MarkAsRead 标记邮件为已读
func (c *IMAPClient) MarkAsRead(seqNum uint32) error {
	seqset := new(imap.SeqSet)
	seqset.AddNum(seqNum)

	flags := []any{imap.SeenFlag}
	err := c.client.Store(seqset, imap.AddFlags, flags, nil)
	if err != nil {
		return fmt.Errorf("failed to mark message as read: %w", err)
	}

	return nil
}

// containsFlag 检查是否包含标志
func containsFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

// formatAddress 格式化邮件地址
func formatAddress(addr *imap.Address) string {
	if addr == nil {
		return ""
	}
	if addr.PersonalName != "" {
		return fmt.Sprintf("%s <%s@%s>", addr.PersonalName, addr.MailboxName, addr.HostName)
	}
	return fmt.Sprintf("%s@%s", addr.MailboxName, addr.HostName)
}

// SMTPClient SMTP 客户端
type SMTPClient struct {
	host     string
	port     int
	username string
	password string
	useSSL   bool
}

// NewSMTPClient 创建 SMTP 客户端
func NewSMTPClient(host string, port int, username, password string, useSSL bool) *SMTPClient {
	return &SMTPClient{
		host:     host,
		port:     port,
		username: username,
		password: password,
		useSSL:   useSSL,
	}
}

// SendEmail 发送邮件
func (c *SMTPClient) SendEmail(to, subject, body string, inReplyTo string) error {
	var msg bytes.Buffer
	msg.WriteString(fmt.Sprintf("From: %s\r\n", c.username))
	msg.WriteString(fmt.Sprintf("To: %s\r\n", to))
	msg.WriteString(fmt.Sprintf("Subject: =?utf-8?Q?%s?=\r\n", encodeQ(subject)))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	msg.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	if inReplyTo != "" {
		msg.WriteString(fmt.Sprintf("In-Reply-To: %s\r\n", inReplyTo))
	}
	msg.WriteString("\r\n")

	qp := quotedprintable.NewWriter(&msg)
	qp.Write([]byte(body))
	qp.Close()

	auth := smtp.PlainAuth("", c.username, c.password, c.host)

	if c.useSSL {
		return c.sendMailWithTLS(auth, c.username, []string{to}, msg.Bytes())
	}

	return smtp.SendMail(fmt.Sprintf("%s:%d", c.host, c.port), auth, c.username, []string{to}, msg.Bytes())
}

// sendMailWithTLS 使用 TLS 发送邮件
func (c *SMTPClient) sendMailWithTLS(auth smtp.Auth, from string, to []string, data []byte) error {
	host, _, _ := net.SplitHostPort(fmt.Sprintf("%s:%d", c.host, c.port))

	conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", c.host, c.port), &tls.Config{
		ServerName: host,
	})
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	defer conn.Close()

	clientConn, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	defer clientConn.Close()

	if err = clientConn.Auth(auth); err != nil {
		return fmt.Errorf("failed to auth: %w", err)
	}

	if err = clientConn.Mail(from); err != nil {
		return fmt.Errorf("failed to mail: %w", err)
	}

	for _, t := range to {
		if err = clientConn.Rcpt(t); err != nil {
			return fmt.Errorf("failed to rcpt: %w", err)
		}
	}

	w, err := clientConn.Data()
	if err != nil {
		return fmt.Errorf("failed to data: %w", err)
	}
	defer w.Close()

	_, err = w.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write: %w", err)
	}

	if err = w.Close(); err != nil {
		return fmt.Errorf("failed to close: %w", err)
	}

	return clientConn.Quit()
}

// encodeQ Q 编码
func encodeQ(s string) string {
	var buf bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' {
			buf.WriteByte('_')
		} else if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", rune(c)) {
			buf.WriteByte(c)
		} else {
			fmt.Fprintf(&buf, "= %02X", c)
		}
	}
	return buf.String()
}
