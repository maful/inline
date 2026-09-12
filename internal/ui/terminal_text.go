package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	escapeByte = '\x1b'
	deleteByte = '\x7f'
	csiByte    = '\x9b'
	dcsByte    = '\x90'
	oscByte    = '\x9d'
	sosByte    = '\x98'
	pmByte     = '\x9e'
	apcByte    = '\x9f'
	stByte     = '\x9c'
	belByte    = '\x07'

	maxSGRSequenceBytes = 128
)

// sanitizeLogLine retains printable text, tabs, and conventional SGR styling.
// All other terminal controls are discarded before the line reaches a renderer.
func sanitizeLogLine(value string) string {
	return sanitizeTerminalText(value, true, true)
}

// sanitizeDisplayText produces plain, single-line text for UI labels.
func sanitizeDisplayText(value string) string {
	return sanitizeTerminalText(value, false, false)
}

func sanitizeTerminalText(value string, allowSGR, allowTab bool) string {
	var output strings.Builder
	output.Grow(len(value))
	retainedSGR := false

	for offset := 0; offset < len(value); {
		current := value[offset]
		switch {
		case current == escapeByte:
			end := consumeEscapeSequence(value, offset)
			sequence := value[offset:end]
			if allowSGR && validSGRSequence(sequence) {
				output.WriteString(sequence)
				retainedSGR = true
			}
			offset = end
		case current == csiByte:
			offset = consumeCSISequence(value, offset+1)
		case current == dcsByte || current == sosByte || current == oscByte || current == pmByte || current == apcByte:
			offset = consumeControlString(value, offset+1, current == oscByte)
		case current < utf8.RuneSelf:
			if current == '\t' && allowTab {
				output.WriteByte(current)
			} else if current >= ' ' && current != deleteByte {
				output.WriteByte(current)
			}
			offset++
		default:
			r, size := utf8.DecodeRuneInString(value[offset:])
			if r == utf8.RuneError && size == 1 {
				output.WriteRune(utf8.RuneError)
				offset++
				continue
			}
			if !unicode.IsControl(r) {
				output.WriteString(value[offset : offset+size])
			}
			offset += size
		}
	}

	if retainedSGR && !strings.HasSuffix(output.String(), "\x1b[0m") && !strings.HasSuffix(output.String(), "\x1b[m") {
		output.WriteString("\x1b[0m")
	}
	return output.String()
}

func consumeEscapeSequence(value string, start int) int {
	if start+1 >= len(value) {
		return len(value)
	}

	switch value[start+1] {
	case '[':
		return consumeCSISequence(value, start+2)
	case 'P', 'X', '^', '_':
		return consumeControlString(value, start+2, false)
	case ']':
		return consumeControlString(value, start+2, true)
	}

	offset := start + 1
	for offset < len(value) && value[offset] >= 0x20 && value[offset] <= 0x2f {
		offset++
	}
	if offset < len(value) && value[offset] >= 0x30 && value[offset] <= 0x7e {
		return offset + 1
	}
	return max(start+1, offset)
}

func consumeCSISequence(value string, offset int) int {
	for offset < len(value) {
		current := value[offset]
		offset++
		if current >= 0x40 && current <= 0x7e {
			return offset
		}
	}
	return len(value)
}

func consumeControlString(value string, offset int, osc bool) int {
	for offset < len(value) {
		switch value[offset] {
		case belByte:
			if osc {
				return offset + 1
			}
		case stByte:
			return offset + 1
		case escapeByte:
			if offset+1 < len(value) && value[offset+1] == '\\' {
				return offset + 2
			}
		}
		offset++
	}
	return len(value)
}

func validSGRSequence(sequence string) bool {
	if len(sequence) < 3 || len(sequence) > maxSGRSequenceBytes ||
		!strings.HasPrefix(sequence, "\x1b[") || sequence[len(sequence)-1] != 'm' {
		return false
	}
	for _, current := range sequence[2 : len(sequence)-1] {
		if (current < '0' || current > '9') && current != ';' && current != ':' {
			return false
		}
	}
	return true
}
