package cli

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// safeWriter drops from what it writes the characters that give a terminal
// commands: the escape sequences, the other control characters and the
// invisible formatting ones that reorder text. A message, a fact or a detail
// of the server is shown through it, so a server, or a name it was given, cannot
// recolor the screen or hide what it says. New lines and tabs stay.
//
// A character that a write ends in the middle of is held back until the next
// write completes it.
type safeWriter struct {
	w    io.Writer
	tail []byte
}

func (s *safeWriter) Write(p []byte) (int, error) {
	data := slices.Concat(s.tail, p)
	s.tail = nil

	// The last bytes may be the start of a character the next write finishes.
	for i := 1; i < utf8.UTFMax && i <= len(data); i++ {
		if !utf8.RuneStart(data[len(data)-i]) {
			continue
		}

		if !utf8.FullRune(data[len(data)-i:]) {
			s.tail = bytes.Clone(data[len(data)-i:])
			data = data[:len(data)-i]
		}

		break
	}

	clean := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}

		return r
	}, string(data))

	if _, err := io.WriteString(s.w, clean); err != nil {
		return 0, err
	}

	return len(p), nil
}

// safe returns w, or w behind a safeWriter.
func safe(w io.Writer) io.Writer {
	if _, ok := w.(*safeWriter); ok {
		return w
	}

	return &safeWriter{w: w}
}
