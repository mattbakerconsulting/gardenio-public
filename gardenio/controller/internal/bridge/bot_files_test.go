package bridge

import (
	"testing"

	"github.com/slack-go/slack"
)

func TestSlackFilesToFields_NoFiles(t *testing.T) {
	fields := slackFilesToFields(nil)
	if fields != nil {
		t.Errorf("expected nil for no files, got %v", fields)
	}
}

func TestSlackFilesToFields_WithImages(t *testing.T) {
	files := []slack.File{
		{ID: "F1", Mimetype: "image/png"},
		{ID: "F2", Mimetype: "application/pdf"},
	}

	fields := slackFilesToFields(files)
	if fields == nil {
		t.Fatal("expected non-nil fields")
	}
	if fields["slack_attachment_count"] != "2" {
		t.Errorf("attachment_count = %q, want %q", fields["slack_attachment_count"], "2")
	}
	if fields["slack_has_images"] != "true" {
		t.Errorf("has_images = %q, want %q", fields["slack_has_images"], "true")
	}
}

func TestSlackFilesToFields_NoImages(t *testing.T) {
	files := []slack.File{
		{ID: "F1", Mimetype: "application/pdf"},
	}

	fields := slackFilesToFields(files)
	if fields == nil {
		t.Fatal("expected non-nil fields")
	}
	if fields["slack_attachment_count"] != "1" {
		t.Errorf("attachment_count = %q, want %q", fields["slack_attachment_count"], "1")
	}
	if _, ok := fields["slack_has_images"]; ok {
		t.Error("expected no slack_has_images field for non-image files")
	}
}
