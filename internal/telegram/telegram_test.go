package telegram

import "testing"

func TestSessionListMarkupNilWhenNoSessions(t *testing.T) {
	t.Parallel()

	if got := SessionListMarkup(nil); got != nil {
		t.Fatalf("SessionListMarkup(nil) = %#v, want nil", got)
	}
}

func TestSessionListMarkupWithSessions(t *testing.T) {
	t.Parallel()

	got := SessionListMarkup([]int{1})
	if got == nil || len(got.InlineKeyboard) != 1 || len(got.InlineKeyboard[0]) != 2 {
		t.Fatalf("SessionListMarkup([1]) = %#v", got)
	}
}
