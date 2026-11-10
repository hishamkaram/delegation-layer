package terminal

import "testing"

func TestStoredTextCannotControlTerminal(t *testing.T) {
	input := "task\x1b]52;c;private\x07 done\x1b[2J\x1b[31m red\x1b[0m\n\t世界\x00\u202e"
	if got := Safe(input); got != "task done red\n\t世界" {
		t.Fatalf("safe=%q", got)
	}
}

func TestSplitSequencesRemainSuppressed(t *testing.T) {
	var text Text
	chunks := []string{"a\x1b]52;c;", "private", "\x1b", "\\b\x1b[", "2Jc"}
	var got string
	for _, chunk := range chunks {
		got += text.Write(chunk)
	}
	if got != "abc" {
		t.Fatalf("safe chunks=%q", got)
	}
}
