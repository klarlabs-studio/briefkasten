package application

import (
	"context"
	"strings"

	"go.klarlabs.de/briefkasten/domain"
)

// Composer answers and passes on mail that is already in the mailbox.
//
// Every entry point takes the id of the message being answered, never a
// recipient list. That is the whole design: a caller cannot hand-assemble
// who a reply goes to, so the arithmetic — Reply-To over From, To and Cc
// into Cc, never Bcc, never ourselves, never the same address twice —
// happens once, in the domain, where it is tested. An interface that
// built its own list would be a second implementation of the rule that
// matters most, drifting from the first.
//
// Deriving and sending are two calls, not one. The confirmation gate has
// to state the audience, and the audience is not known until the
// original has been read; a gate asked before the derivation could only
// name the id the caller passed, which is exactly the number that does
// not describe what is about to happen.
type Composer struct {
	svc *Service
	ob  *Outbox
}

// NewComposer binds the mailbox the originals are read from to the
// outbox the answers leave through.
func NewComposer(svc *Service, ob *Outbox) *Composer {
	return &Composer{svc: svc, ob: ob}
}

// ReplyRequest names the message being answered and what to say.
type ReplyRequest struct {
	Account string
	Folder  string
	// ID is the message being replied to.
	ID string
	// All widens the reply to everyone the original could already see:
	// its To and its Cc, never its Bcc.
	All         bool
	Body        string
	HTMLBody    string
	Attachments []domain.Attachment
}

// ForwardRequest names the message being passed on and who to.
type ForwardRequest struct {
	Account string
	Folder  string
	// ID is the message being forwarded.
	ID string
	// To are the new recipients — the caller's, not the original's.
	To       []string
	Body     string
	HTMLBody string
}

// PlanReply derives the reply without queuing it: recipients, subject
// and threading headers, with the caller's body and the original quoted
// beneath it.
func (c *Composer) PlanReply(ctx context.Context, req ReplyRequest) (domain.OutboundMessage, error) {
	orig, err := c.original(ctx, req.Account, req.Folder, req.ID)
	if err != nil {
		return domain.OutboundMessage{}, err
	}
	msg, err := domain.DeriveReply(orig, c.ob.From(), req.All)
	if err != nil {
		return domain.OutboundMessage{}, err
	}
	msg.Body = appendBlock(req.Body, domain.Quote(orig))
	msg.HTMLBody = appendHTMLBlock(req.HTMLBody, domain.Quote(orig))
	msg.Attachments = req.Attachments
	return msg, nil
}

// PlanForward derives the forward without queuing it. The original rides
// along as a message/rfc822 attachment, so its own attachments survive
// byte for byte; the body carries the customary header block for a human
// reading without opening it.
func (c *Composer) PlanForward(ctx context.Context, req ForwardRequest) (domain.OutboundMessage, error) {
	orig, err := c.original(ctx, req.Account, req.Folder, req.ID)
	if err != nil {
		return domain.OutboundMessage{}, err
	}
	msg, err := domain.DeriveForward(orig, req.To)
	if err != nil {
		return domain.OutboundMessage{}, err
	}
	msg.Body = appendBlock(req.Body, domain.ForwardIntro(orig))
	msg.HTMLBody = appendHTMLBlock(req.HTMLBody, domain.ForwardIntro(orig))
	return msg, nil
}

// Send queues a message the caller has already had confirmed. It is
// deliberately the plain outbox Enqueue and not a second path: a reply
// is an outbound message like any other once it exists, and it goes
// through the same validation, the same lifecycle and the same worker.
func (c *Composer) Send(msg domain.OutboundMessage) (string, error) {
	return c.ob.Enqueue(msg)
}

// original reads and parses the message being answered.
func (c *Composer) original(ctx context.Context, account, folder, id string) (domain.Original, error) {
	raw, err := c.svc.Read(ctx, account, folder, id)
	if err != nil {
		return domain.Original{}, err
	}
	return domain.ParseOriginal(raw)
}

// appendBlock puts the caller's text above the quoted original, the
// order every mail client has trained people to read.
func appendBlock(body, block string) string {
	if strings.TrimSpace(block) == "" {
		return body
	}
	if body == "" {
		return block
	}
	return body + "\n\n" + block
}

// appendHTMLBlock is appendBlock for the HTML alternative, and only when
// the caller supplied one: an html_body invented here would be a second,
// differently-worded copy of the message rather than an alternative
// rendering of it.
//
// The quoted original is escaped and wrapped in a blockquote. It is text
// pulled from someone else's message, so it is quoted as text — passing
// it through as markup would let a correspondent's mail write into ours.
func appendHTMLBlock(htmlBody, block string) string {
	if htmlBody == "" || strings.TrimSpace(block) == "" {
		return htmlBody
	}
	return htmlBody + "\n<blockquote>\n" + htmlEscapeLines(block) + "</blockquote>\n"
}

// htmlEscapeLines escapes a plain-text block and turns its line breaks
// into <br>.
func htmlEscapeLines(text string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n") {
		b.WriteString(replacer.Replace(line) + "<br>\n")
	}
	return b.String()
}
