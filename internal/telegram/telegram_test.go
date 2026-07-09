package telegram

import "testing"

func TestSessionListMarkupNilWhenNoSessions(t *testing.T) {
	t.Parallel()

	if got := SessionListMarkup(nil, nil); got != nil {
		t.Fatalf("SessionListMarkup(nil, nil) = %#v, want nil", got)
	}
}

func TestSessionListMarkupWithSessions(t *testing.T) {
	t.Parallel()

	got := SessionListMarkup([]int{1}, nil)
	if got == nil || len(got.InlineKeyboard) != 1 || len(got.InlineKeyboard[0]) != 2 {
		t.Fatalf("SessionListMarkup([1]) = %#v", got)
	}
}

func TestSessionListMarkupWithAttachTargets(t *testing.T) {
	t.Parallel()

	got := SessionListMarkup(nil, []AttachTarget{{Label: "0:1", Target: "0:1"}})
	if got == nil || len(got.InlineKeyboard) != 1 || got.InlineKeyboard[0][0].CallbackData != "attach:0:1" {
		t.Fatalf("SessionListMarkup attach = %#v", got)
	}
}

func TestMarkdownToHTML(t *testing.T) {
	t.Parallel()

	got := MarkdownToHTML("**Status:** `ok` <raw>")
	want := "<b>Status:</b> <code>ok</code> &lt;raw&gt;"
	if got != want {
		t.Fatalf("MarkdownToHTML = %q, want %q", got, want)
	}
}

func TestMarkdownToHTMLCodeFence(t *testing.T) {
	t.Parallel()

	got := MarkdownToHTML("before\n```\n<a>\n```\nafter")
	want := "before\n<pre>&lt;a&gt;</pre>\nafter"
	if got != want {
		t.Fatalf("MarkdownToHTML code fence = %q, want %q", got, want)
	}
}
