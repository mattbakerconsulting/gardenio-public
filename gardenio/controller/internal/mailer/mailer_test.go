package mailer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
)

func validMessage() Message {
	return Message{
		Kind:    KindApplicationReceived,
		To:      []string{"tenant@example.com"},
		Subject: "We have your application",
		Text:    "Hello.",
		HTML:    "<p>Hello.</p>",
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Message)
		ok   bool
	}{
		{"valid", func(*Message) {}, true},
		{"named recipient", func(m *Message) { m.To = []string{"Tenant <tenant@example.com>"} }, true},
		{"no recipient", func(m *Message) { m.To = nil }, false},
		{"bad recipient", func(m *Message) { m.To = []string{"not an address"} }, false},
		{"header injection in recipient", func(m *Message) { m.To = []string{"a@example.com\r\nBcc: b@example.com"} }, false},
		{"too many recipients", func(m *Message) { m.To = make([]string, MaxRecipients+1) }, false},
		{"empty subject", func(m *Message) { m.Subject = " " }, false},
		{"subject line break", func(m *Message) { m.Subject = "hi\nBcc: b@example.com" }, false},
		{"empty text", func(m *Message) { m.Text = "" }, false},
		{"no html is fine", func(m *Message) { m.HTML = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validMessage()
			tc.edit(&m)
			err := Validate(m)
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("Validate = %v, want ErrInvalidMessage", err)
			}
		})
	}
}

func TestConfigFromEnvOffByDefault(t *testing.T) {
	for _, k := range []string{EnvEnabled, EnvFrom, EnvReplyTo, EnvRegion, EnvConfigurationSet, EnvBaseURL, EnvCredentialsDir} {
		t.Setenv(k, "")
	}
	cfg := ConfigFromEnv()
	if cfg.Enabled {
		t.Fatal("mail is on with an empty environment")
	}
	m, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if Enabled(m) {
		t.Fatalf("New with mail off returned %T, want Disabled", m)
	}
	if err := m.Send(context.Background(), validMessage()); err != nil {
		t.Fatalf("Disabled.Send: %v", err)
	}
	if err := m.Send(context.Background(), Message{}); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("Disabled.Send of an invalid message = %v, want ErrInvalidMessage", err)
	}

	t.Setenv(EnvEnabled, "maybe")
	if ConfigFromEnv().Enabled {
		t.Fatal("an unparsable MAILER_ENABLED turned mail on")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvFrom, "Gardenio Lands <lands@example.com>")
	t.Setenv(EnvReplyTo, "support@example.com")
	t.Setenv(EnvRegion, "us-east-1")
	t.Setenv(EnvConfigurationSet, "lands")
	t.Setenv(EnvBaseURL, "https://gardenio.example.com/")
	t.Setenv(EnvCredentialsDir, " /var/run/secrets/gardenio-mailer-ses ")
	got := ConfigFromEnv()
	want := Config{
		Enabled:          true,
		From:             "Gardenio Lands <lands@example.com>",
		ReplyTo:          "support@example.com",
		Region:           "us-east-1",
		ConfigurationSet: "lands",
		BaseURL:          "https://gardenio.example.com",
		CredentialsDir:   "/var/run/secrets/gardenio-mailer-ses",
	}
	if got != want {
		t.Fatalf("ConfigFromEnv = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// sesEnv isolates the AWS default chain to env credentials that stand in for
// the controller's in-box MinIO keys, with no shared files or IMDS.
func sesEnv(t *testing.T) {
	t.Helper()
	none := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_CONFIG_FILE", none)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", none)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "minio-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "minio-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
}

func retrieveKeys(t *testing.T, cfg Config) aws.Credentials {
	t.Helper()
	awsCfg, err := loadAWSConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("loadAWSConfig: %v", err)
	}
	creds, err := awsCfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	return creds
}

func TestCredentialsWithoutDirUseDefaultChain(t *testing.T) {
	sesEnv(t)
	creds := retrieveKeys(t, Config{Region: "us-east-1"})
	if creds.AccessKeyID != "minio-access" || creds.SecretAccessKey != "minio-secret" {
		t.Fatalf("credentials = %q/%q, want the default chain's env keys", creds.AccessKeyID, creds.SecretAccessKey)
	}
}

func TestCredentialsDirOverridesDefaultChain(t *testing.T) {
	sesEnv(t)
	dir := t.TempDir()
	for name, v := range map[string]string{AccessKeyIDFile: "AKIASES\n", SecretAccessKeyFile: "ses-secret\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	creds := retrieveKeys(t, Config{Region: "us-east-1", CredentialsDir: dir})
	if creds.AccessKeyID != "AKIASES" || creds.SecretAccessKey != "ses-secret" || creds.SessionToken != "" {
		t.Fatalf("credentials = %q/%q/%q, want the mounted SES keys", creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)
	}

	m, err := New(context.Background(), Config{
		Enabled: true, From: "lands@example.com", BaseURL: "https://gardenio.example.com",
		Region: "us-east-1", CredentialsDir: dir,
	})
	if err != nil || !Enabled(m) {
		t.Fatalf("New with a credentials dir = %T, %v; want SES", m, err)
	}
}

func TestCredentialsDirIncompleteFailsClosed(t *testing.T) {
	sesEnv(t)
	for name, files := range map[string]map[string]string{
		"missing dir":   nil,
		"no secret":     {AccessKeyIDFile: "AKIASES"},
		"empty key":     {AccessKeyIDFile: " \n", SecretAccessKeyFile: "ses-secret"},
		"no access key": {SecretAccessKeyFile: "ses-secret"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ses")
			if files != nil {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for f, v := range files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o400); err != nil {
					t.Fatal(err)
				}
			}
			_, err := loadAWSConfig(context.Background(), Config{Region: "us-east-1", CredentialsDir: dir})
			if err == nil || !strings.Contains(err.Error(), EnvCredentialsDir) {
				t.Fatalf("loadAWSConfig = %v, want an error naming %s (never the default chain's keys)", err, EnvCredentialsDir)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{Enabled: true, From: "lands@example.com", BaseURL: "https://gardenio.example.com"}
	cases := []struct {
		name string
		edit func(*Config)
		ok   bool
	}{
		{"valid", func(*Config) {}, true},
		{"disabled needs nothing", func(c *Config) { *c = Config{} }, true},
		{"no sender", func(c *Config) { c.From = "" }, false},
		{"bad sender", func(c *Config) { c.From = "lands" }, false},
		{"bad reply-to", func(c *Config) { c.ReplyTo = "support" }, false},
		{"no base url", func(c *Config) { c.BaseURL = "" }, false},
		{"plain http base url", func(c *Config) { c.BaseURL = "http://gardenio.example.com" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			tc.edit(&c)
			err := c.Validate()
			if tc.ok != (err == nil) {
				t.Fatalf("Validate = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	if _, err := New(context.Background(), Config{Enabled: true}); err == nil {
		t.Fatal("New accepted an enabled Config with no sender")
	}
}

func TestEnabled(t *testing.T) {
	if Enabled(nil) || Enabled(Disabled{}) || Enabled(&Disabled{}) {
		t.Fatal("Enabled is true for a mailer that sends nothing")
	}
	if !Enabled(NewRecorder()) || !Enabled(NewSES(&fakeSES{}, Config{})) {
		t.Fatal("Enabled is false for a mailer that delivers")
	}
}

type fakeSES struct {
	in  []*sesv2.SendEmailInput
	err error
}

func (f *fakeSES) SendEmail(_ context.Context, in *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	f.in = append(f.in, in)
	if f.err != nil {
		return nil, f.err
	}
	return &sesv2.SendEmailOutput{MessageId: aws.String("m-1")}, nil
}

func TestSESSend(t *testing.T) {
	fake := &fakeSES{}
	s := NewSES(fake, Config{
		From:             "Gardenio Lands <lands@example.com>",
		ReplyTo:          "support@example.com",
		ConfigurationSet: "lands",
	})
	if err := s.Send(context.Background(), validMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(fake.in) != 1 {
		t.Fatalf("SendEmail called %d times, want 1", len(fake.in))
	}
	in := fake.in[0]
	if got := aws.ToString(in.FromEmailAddress); got != "Gardenio Lands <lands@example.com>" {
		t.Errorf("From = %q", got)
	}
	if got := in.Destination.ToAddresses; len(got) != 1 || got[0] != "tenant@example.com" {
		t.Errorf("To = %v", got)
	}
	if len(in.Destination.CcAddresses)+len(in.Destination.BccAddresses) != 0 {
		t.Errorf("unexpected cc/bcc: %+v", in.Destination)
	}
	if got := in.ReplyToAddresses; len(got) != 1 || got[0] != "support@example.com" {
		t.Errorf("ReplyTo = %v", got)
	}
	if got := aws.ToString(in.ConfigurationSetName); got != "lands" {
		t.Errorf("ConfigurationSetName = %q", got)
	}
	msg := in.Content.Simple
	if aws.ToString(msg.Subject.Data) != "We have your application" || aws.ToString(msg.Body.Text.Data) != "Hello." || aws.ToString(msg.Body.Html.Data) != "<p>Hello.</p>" {
		t.Errorf("content = %q / %q / %q", aws.ToString(msg.Subject.Data), aws.ToString(msg.Body.Text.Data), aws.ToString(msg.Body.Html.Data))
	}
	if aws.ToString(msg.Subject.Charset) != "UTF-8" || aws.ToString(msg.Body.Text.Charset) != "UTF-8" {
		t.Error("charset is not UTF-8")
	}
	if len(in.EmailTags) != 1 || aws.ToString(in.EmailTags[0].Name) != "kind" || aws.ToString(in.EmailTags[0].Value) != string(KindApplicationReceived) {
		t.Errorf("EmailTags = %+v", in.EmailTags)
	}
}

func TestSESSendMinimal(t *testing.T) {
	fake := &fakeSES{}
	m := validMessage()
	m.HTML, m.Kind = "", ""
	if err := NewSES(fake, Config{From: "lands@example.com"}).Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	in := fake.in[0]
	if in.ReplyToAddresses != nil || in.ConfigurationSetName != nil || in.EmailTags != nil || in.Content.Simple.Body.Html != nil {
		t.Errorf("optional fields set on a minimal send: %+v", in)
	}
}

func TestSESSendRefusesInvalidAndWrapsErrors(t *testing.T) {
	fake := &fakeSES{}
	s := NewSES(fake, Config{From: "lands@example.com"})
	if err := s.Send(context.Background(), Message{}); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("Send of an invalid message = %v", err)
	}
	if len(fake.in) != 0 {
		t.Fatal("an invalid message reached SES")
	}
	boom := errors.New("throttled")
	fake.err = boom
	if err := s.Send(context.Background(), validMessage()); !errors.Is(err, boom) || !strings.Contains(err.Error(), string(KindApplicationReceived)) {
		t.Fatalf("Send = %v, want the SES error wrapped with the kind", err)
	}
}

func TestRecorder(t *testing.T) {
	r := NewRecorder()
	ctx := context.Background()
	m := validMessage()
	if err := r.Send(ctx, m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	m.To[0] = "changed@example.com"
	if got := r.Sent()[0].To[0]; got != "tenant@example.com" {
		t.Fatalf("Recorder kept the caller's slice: %q", got)
	}
	other := validMessage()
	other.Kind = KindLandReady
	if err := r.Send(ctx, other); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n := len(r.SentKind(KindLandReady)); n != 1 {
		t.Fatalf("SentKind = %d messages, want 1", n)
	}
	if err := r.Send(ctx, Message{}); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("Send of an invalid message = %v", err)
	}
	boom := errors.New("boom")
	r.FailWith(boom)
	if err := r.Send(ctx, validMessage()); !errors.Is(err, boom) {
		t.Fatalf("Send after FailWith = %v", err)
	}
	if n := len(r.Sent()); n != 2 {
		t.Fatalf("Sent = %d messages, want 2 (failed and invalid sends record nothing)", n)
	}
	r.FailWith(nil)
	r.Reset()
	if n := len(r.Sent()); n != 0 {
		t.Fatalf("Sent after Reset = %d", n)
	}
}

// goldenData fills every field a template can read, so each golden shows
// the full mail.
func goldenData(k Kind) Data {
	d := Data{
		Name:          "octocat",
		ApplicationID: "gd-AbCdEf1234",
		Land:          "hello-world",
		Repo:          "octocat/hello-world",
		GrantedUSD:    50,
		RemainingUSD:  9.5,
	}
	switch k {
	case KindApplicationDenied:
		d.Note = "We are only opening a few plots this month.\nPlease try again in November."
	case KindApplicationQuestion:
		d.Note = "What will the agents build in this repo?"
	case KindLandFrozen:
		d.Note = "An agent opened 40 pull requests in an hour. Write in #support and we will look at it together."
	case KindLandClosed:
		d.Note = "The land was closed at your request."
	case KindLandReady:
		d.RemainingUSD = 50
	case KindPurseEmpty:
		d.RemainingUSD = 0
	case KindInvite:
		d.InvitedBy = "operator@example.com"
	}
	return d
}

// TestTemplatesGolden renders every Kind and compares it with
// testdata/<kind>.txt and testdata/<kind>.html. Review the words there.
// Regenerate with:
//
//	GB_UPDATE_GOLDEN=1 go test ./internal/mailer/ -run TestTemplatesGolden
func TestTemplatesGolden(t *testing.T) {
	tpl := NewTemplates(Config{BaseURL: "https://gardenio.example.com/", ReplyTo: "support@example.com"})
	update := os.Getenv("GB_UPDATE_GOLDEN") == "1"
	for _, k := range Kinds() {
		t.Run(string(k), func(t *testing.T) {
			m, err := tpl.Render(k, "octocat@example.com", goldenData(k))
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if err := Validate(m); err != nil {
				t.Fatalf("rendered message is invalid: %v", err)
			}
			if m.Kind != k || len(m.To) != 1 || m.To[0] != "octocat@example.com" {
				t.Fatalf("Kind/To = %q/%v", m.Kind, m.To)
			}
			text := "Subject: " + m.Subject + "\n\n" + m.Text
			for name, got := range map[string]string{string(k) + ".txt": text, string(k) + ".html": m.HTML} {
				path := filepath.Join("testdata", name)
				if update {
					if err := os.MkdirAll("testdata", 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read golden (regenerate with GB_UPDATE_GOLDEN=1): %v", err)
				}
				if got != string(want) {
					t.Errorf("%s differs from the golden; regenerate with GB_UPDATE_GOLDEN=1 and review.\n--- got ---\n%s", name, got)
				}
			}
		})
	}
}

func TestKindsAllHaveTemplates(t *testing.T) {
	if len(Kinds()) != len(defs) {
		t.Fatalf("Kinds() lists %d kinds, defs has %d", len(Kinds()), len(defs))
	}
	seen := map[Kind]bool{}
	for _, k := range Kinds() {
		if _, ok := defs[k]; !ok {
			t.Errorf("%s has no template", k)
		}
		if seen[k] {
			t.Errorf("%s listed twice", k)
		}
		seen[k] = true
		if !sesTagValue.MatchString(string(k)) {
			t.Errorf("%s is not a valid SES tag value", k)
		}
	}
}

func TestRenderRequiresData(t *testing.T) {
	tpl := NewTemplates(Config{BaseURL: "https://gardenio.example.com"})
	cases := []struct {
		kind Kind
		data Data
		want string
	}{
		{KindApplicationReceived, Data{Land: "x", Repo: "o/r"}, "ApplicationID"},
		{KindApplicationApproved, Data{ApplicationID: "a", Land: "x", Repo: "o/r"}, "GrantedUSD"},
		{KindApplicationQuestion, Data{ApplicationID: "a", Land: "x", Repo: "o/r", Note: "  "}, "Note"},
		{KindLandReady, Data{Land: "x"}, "Repo"},
		{KindPurseEmpty, Data{}, "Land"},
		{KindInvite, Data{Land: "x", InvitedBy: " "}, "InvitedBy"},
	}
	for _, tc := range cases {
		_, err := tpl.Render(tc.kind, "a@example.com", tc.data)
		if !errors.Is(err, ErrMissingData) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Render(%s) = %v, want ErrMissingData naming %s", tc.kind, err, tc.want)
		}
	}
	if _, err := tpl.Render("nope", "a@example.com", Data{}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("Render of an unknown kind = %v", err)
	}
	if _, err := NewTemplates(Config{}).Render(KindPurseEmpty, "a@example.com", Data{Land: "x"}); err == nil {
		t.Error("Render with no base URL succeeded")
	}
	// The optional fields really are optional.
	if _, err := tpl.Render(KindApplicationDenied, "a@example.com", Data{ApplicationID: "a", Land: "x", Repo: "o/r"}); err != nil {
		t.Errorf("Render denied without a note: %v", err)
	}
}

// A sign-in invite (gd-ycE484kLyk) says who invited them, links to the Lands
// front door, and tells them to sign in with GitHub using this address. One
// that names no land says so without a land.
func TestInviteMail(t *testing.T) {
	tpl := NewTemplates(Config{BaseURL: "https://gardenio.clonk.tv"})
	m, err := tpl.Render(KindInvite, "someone@example.com", Data{InvitedBy: "operator@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"operator@example.com invited you to Gardenio Lands.",
		"sign in with GitHub using this email address",
		"Sign in to Gardenio Lands: https://gardenio.clonk.tv/lands/\n",
	} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("invite text lacks %q:\n%s", want, m.Text)
		}
	}
	if strings.Contains(m.Text, "the land") {
		t.Errorf("a land-less invite names a land:\n%s", m.Text)
	}
	if !strings.Contains(m.HTML, `href="https://gardenio.clonk.tv/lands/"`) {
		t.Errorf("invite HTML does not link to /lands/:\n%s", m.HTML)
	}
	m, err = tpl.Render(KindInvite, "someone@example.com", Data{InvitedBy: "operator@example.com", Land: "acme"})
	if err != nil || !strings.Contains(m.Text, "invited you to Gardenio Lands, to the land acme.") {
		t.Errorf("land invite = %v\n%s", err, m.Text)
	}
}

func TestRenderEscapesAndSanitizes(t *testing.T) {
	tpl := NewTemplates(Config{BaseURL: "https://gardenio.example.com"})
	m, err := tpl.Render(KindLandFrozen, "a@example.com", Data{
		Land: "x\r\nBcc: evil@example.com",
		Note: `<script>alert(1)</script> & <a href="https://evil.example">click</a>`,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		t.Errorf("subject kept a line break: %q", m.Subject)
	}
	if strings.Contains(m.HTML, "<script>") || strings.Contains(m.HTML, `<a href="https://evil.example">`) {
		t.Errorf("HTML body carries the note's markup unescaped:\n%s", m.HTML)
	}
	if !strings.Contains(m.HTML, "&lt;script&gt;") {
		t.Errorf("HTML body lost the escaped note:\n%s", m.HTML)
	}
	if !strings.Contains(m.Text, "<script>alert(1)</script>") {
		t.Errorf("text body should carry the note verbatim:\n%s", m.Text)
	}
	if !strings.Contains(m.Text, "https://gardenio.example.com/garden?land=x%0D%0ABcc%3A+evil%40example.com") {
		t.Errorf("land link is not query-escaped:\n%s", m.Text)
	}
	if strings.Contains(m.Text, "Reply to this email") {
		t.Errorf("mail says reply with no reply-to configured:\n%s", m.Text)
	}
}

func TestFormatUSD(t *testing.T) {
	for v, want := range map[float64]string{0: "$0", 50: "$50", 9.5: "$9.50", 12.345: "$12.35", 0.01: "$0.01", -5: "-$5", -2.5: "-$2.50"} {
		if got := formatUSD(v); got != want {
			t.Errorf("formatUSD(%v) = %q, want %q", v, got, want)
		}
	}
}
