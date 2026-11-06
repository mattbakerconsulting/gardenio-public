package mailer

import (
	"context"
	"fmt"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// SESAPI is the one SES v2 call the mailer makes. *sesv2.Client satisfies it;
// tests pass a fake.
type SESAPI interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// SES sends mail through Amazon SES v2 as cfg.From. The IAM role behind its
// client needs only ses:SendEmail on the sender identity.
type SES struct {
	client           SESAPI
	from             string
	replyTo          string
	configurationSet string
}

// NewSES returns an SES mailer that sends through client with cfg's sender,
// reply-to and configuration set. It does not validate cfg; New does.
func NewSES(client SESAPI, cfg Config) *SES {
	return &SES{
		client:           client,
		from:             cfg.From,
		replyTo:          cfg.ReplyTo,
		configurationSet: cfg.ConfigurationSet,
	}
}

// sesTagValue is what SES accepts in an email tag value.
var sesTagValue = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// Send validates m and sends it as one SES simple message.
func (s *SES) Send(ctx context.Context, m Message) error {
	if err := Validate(m); err != nil {
		return err
	}
	body := &sestypes.Body{
		Text: &sestypes.Content{Data: aws.String(m.Text), Charset: aws.String("UTF-8")},
	}
	if m.HTML != "" {
		body.Html = &sestypes.Content{Data: aws.String(m.HTML), Charset: aws.String("UTF-8")}
	}
	in := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.from),
		Destination:      &sestypes.Destination{ToAddresses: append([]string(nil), m.To...)},
		Content: &sestypes.EmailContent{
			Simple: &sestypes.Message{
				Subject: &sestypes.Content{Data: aws.String(m.Subject), Charset: aws.String("UTF-8")},
				Body:    body,
			},
		},
	}
	if s.replyTo != "" {
		in.ReplyToAddresses = []string{s.replyTo}
	}
	if s.configurationSet != "" {
		in.ConfigurationSetName = aws.String(s.configurationSet)
	}
	if m.Kind != "" && sesTagValue.MatchString(string(m.Kind)) {
		in.EmailTags = []sestypes.MessageTag{{Name: aws.String("kind"), Value: aws.String(string(m.Kind))}}
	}
	if _, err := s.client.SendEmail(ctx, in); err != nil {
		return fmt.Errorf("mailer: ses send %s: %w", kindOrMessage(m.Kind), err)
	}
	return nil
}

func kindOrMessage(k Kind) string {
	if k == "" {
		return "message"
	}
	return string(k)
}
