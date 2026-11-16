// mautrix-slack - A Matrix-Slack puppeting bridge.
// Copyright (C) 2026 Intelia Technologies
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package matrixfmt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

// MaxSplitChars is the most characters of each message when a message that Slack rejected
// is resent as split text.
const MaxSplitChars = 3500

// codeFence opens and closes a code block in Slack mrkdwn.
const codeFence = "```"

// ErrCodeBlockTooLong is returned by SplitText when a code block doesn't fit in a single
// part: code blocks are never cut, so the message can't be sent as split text.
var ErrCodeBlockTooLong = errors.New("a code block is longer than a message part")

// SplitText renders a rich text block as Slack mrkdwn text in chunks of at most
// MaxSplitChars characters, marked "(1/n)", "(2/n)"… when there's more than one.
func SplitText(block *slack.RichTextBlock) ([]string, error) {
	return splitText(block, MaxSplitChars)
}

// textUnit is a paragraph, list, quote or code block of the split text. Chunks only break
// between units; a unit longer than a chunk is split at its paragraphs, lines and words,
// but a code block is never split.
type textUnit struct {
	text string
	code bool
}

func splitText(block *slack.RichTextBlock, limit int) ([]string, error) {
	var units []textUnit
	for _, elem := range block.Elements {
		if unit := renderUnit(elem); strings.TrimSpace(unit.text) != "" {
			units = append(units, unit)
		}
	}
	// A message that fits in one part goes without a marker.
	if chunks, err := packUnits(units, limit); err == nil && len(chunks) <= 1 {
		return chunks, nil
	}
	for digits := 1; ; digits++ {
		// Room for the "(i/n)\n" marker with n of this many digits.
		chunks, err := packUnits(units, limit-len("(/)\n")-2*digits)
		if err != nil {
			return nil, err
		}
		if len(strconv.Itoa(len(chunks))) <= digits {
			for i, chunk := range chunks {
				chunks[i] = fmt.Sprintf("(%d/%d)\n%s", i+1, len(chunks), chunk)
			}
			return chunks, nil
		}
	}
}

func packUnits(units []textUnit, limit int) ([]string, error) {
	var chunks []string
	var cur strings.Builder
	curLen := 0
	flush := func() {
		if chunk := strings.TrimRight(cur.String(), "\n"); chunk != "" {
			chunks = append(chunks, chunk)
		}
		cur.Reset()
		curLen = 0
	}
	add := func(text string) {
		n := utf8.RuneCountInString(text)
		if curLen > 0 && curLen+1+n > limit {
			flush()
		}
		if curLen > 0 {
			cur.WriteByte('\n')
			curLen++
		}
		cur.WriteString(text)
		curLen += n
	}
	for _, unit := range units {
		if utf8.RuneCountInString(unit.text) <= limit {
			add(unit.text)
			continue
		}
		pieces, err := splitUnit(unit, limit)
		if err != nil {
			return nil, err
		}
		for _, piece := range pieces {
			add(piece)
		}
	}
	flush()
	return chunks, nil
}

// splitUnit splits a unit longer than limit into pieces of at most limit characters.
func splitUnit(unit textUnit, limit int) ([]string, error) {
	if unit.code {
		return nil, ErrCodeBlockTooLong
	}
	// Plain text may carry its own code blocks between fences: keep them whole too.
	var pieces []string
	var para []string
	flushPara := func() {
		if len(para) > 0 {
			// The trailing line break keeps the blank line between paragraphs.
			pieces = append(pieces, splitBy(strings.Join(para, "\n"), limit, "\n", " ")...)
			pieces[len(pieces)-1] += "\n"
			para = nil
		}
	}
	lines := strings.Split(unit.text, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case opensCodeBlock(line):
			flushPara()
			end := i + 1
			for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), codeFence) {
				end++
			}
			end = min(end, len(lines)-1)
			code := strings.Join(lines[i:end+1], "\n")
			if utf8.RuneCountInString(code) > limit {
				return nil, ErrCodeBlockTooLong
			}
			pieces = append(pieces, code)
			i = end
		case strings.TrimSpace(line) == "":
			flushPara()
		default:
			para = append(para, line)
		}
	}
	flushPara()
	if len(pieces) > 0 {
		pieces[len(pieces)-1] = strings.TrimRight(pieces[len(pieces)-1], "\n")
	}
	return pieces, nil
}

// opensCodeBlock reports whether a line of plain text opens a code block: it starts with a
// fence that isn't closed on the same line.
func opensCodeBlock(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, codeFence) && !strings.Contains(line[len(codeFence):], codeFence)
}

// splitBy splits text into pieces of at most limit characters, joining its parts separated
// by seps[0] while they fit. Parts that alone don't fit are split by the next separators,
// and cut where they reach limit when no separator is left.
func splitBy(text string, limit int, seps ...string) []string {
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}
	if len(seps) == 0 {
		return hardCut(text, limit)
	}
	sep := seps[0]
	var out []string
	var cur strings.Builder
	curLen, hasCur := 0, false
	flush := func() {
		if hasCur {
			out = append(out, cur.String())
		}
		cur.Reset()
		curLen, hasCur = 0, false
	}
	for _, part := range strings.Split(text, sep) {
		n := utf8.RuneCountInString(part)
		if n > limit {
			flush()
			out = append(out, splitBy(part, limit, seps[1:]...)...)
			continue
		}
		if hasCur && curLen+len(sep)+n > limit {
			flush()
		}
		if hasCur {
			cur.WriteString(sep)
			curLen += len(sep)
		}
		cur.WriteString(part)
		curLen += n
		hasCur = true
	}
	flush()
	return out
}

func hardCut(text string, limit int) []string {
	var out []string
	runes := []rune(text)
	for len(runes) > limit {
		out = append(out, string(runes[:limit]))
		runes = runes[limit:]
	}
	return append(out, string(runes))
}

func renderUnit(elem slack.RichTextElement) textUnit {
	switch e := elem.(type) {
	case *slack.RichTextSection:
		return textUnit{text: renderInline(e.Elements)}
	case slack.RichTextSection:
		return textUnit{text: renderInline(e.Elements)}
	case *slack.RichTextList:
		return textUnit{text: renderList(e)}
	case slack.RichTextList:
		return textUnit{text: renderList(&e)}
	case *slack.RichTextQuote:
		lines := strings.Split(renderInline(e.Elements), "\n")
		for i, line := range lines {
			lines[i] = "> " + line
		}
		return textUnit{text: strings.Join(lines, "\n")}
	case *slack.RichTextPreformatted:
		var buf strings.Builder
		for _, se := range e.Elements {
			switch s := se.(type) {
			case *slack.RichTextSectionTextElement:
				buf.WriteString(escapeMrkdwn(s.Text))
			case *slack.RichTextSectionLinkElement:
				if s.Text != "" {
					buf.WriteString(escapeMrkdwn(s.Text))
				} else {
					buf.WriteString(s.URL)
				}
			default:
				buf.WriteString(renderInline([]slack.RichTextSectionElement{se}))
			}
		}
		return textUnit{text: codeFence + "\n" + strings.TrimRight(buf.String(), "\n") + "\n" + codeFence, code: true}
	}
	return textUnit{}
}

func renderList(list *slack.RichTextList) string {
	lines := make([]string, 0, len(list.Elements))
	for i, item := range list.Elements {
		bullet := "• "
		if list.Style == slack.RTEListOrdered {
			bullet = strconv.Itoa(list.Offset+i+1) + ". "
		}
		var text string
		switch it := item.(type) {
		case slack.RichTextSection:
			text = renderInline(it.Elements)
		case *slack.RichTextSection:
			text = renderInline(it.Elements)
		}
		lines = append(lines, strings.Repeat("    ", list.Indent)+bullet+text)
	}
	return strings.Join(lines, "\n")
}

func renderInline(elems []slack.RichTextSectionElement) string {
	var buf strings.Builder
	for i := 0; i < len(elems); i++ {
		switch e := elems[i].(type) {
		case *slack.RichTextSectionTextElement:
			// Adjacent text with the same style goes inside the same markers ("*### Título*").
			text := e.Text
			for i+1 < len(elems) {
				next, ok := elems[i+1].(*slack.RichTextSectionTextElement)
				if !ok || !sameStyle(e.Style, next.Style) {
					break
				}
				text += next.Text
				i++
			}
			buf.WriteString(styleMrkdwn(escapeMrkdwn(text), e.Style))
		case *slack.RichTextSectionLinkElement:
			link := "<" + e.URL + ">"
			if e.Text != "" && e.Text != e.URL {
				link = "<" + e.URL + "|" + escapeMrkdwn(e.Text) + ">"
			}
			buf.WriteString(styleMrkdwn(link, e.Style))
		case *slack.RichTextSectionUserElement:
			buf.WriteString("<@" + e.UserID + ">")
		case *slack.RichTextSectionChannelElement:
			buf.WriteString("<#" + e.ChannelID + ">")
		case *slack.RichTextSectionUserGroupElement:
			buf.WriteString("<!subteam^" + e.UsergroupID + ">")
		case *slack.RichTextSectionBroadcastElement:
			buf.WriteString("<!" + string(e.Range) + ">")
		case *slack.RichTextSectionEmojiElement:
			buf.WriteString(":" + e.Name + ":")
		}
	}
	return buf.String()
}

func sameStyle(a, b *slack.RichTextSectionTextStyle) bool {
	return a == b || (a != nil && b != nil && *a == *b)
}

var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeMrkdwn(text string) string {
	return mrkdwnEscaper.Replace(text)
}

// styleMrkdwn wraps text in the mrkdwn markers of its style, leaving the spaces around it
// outside the markers so that Slack applies them.
func styleMrkdwn(text string, style *slack.RichTextSectionTextStyle) string {
	core := strings.TrimSpace(text)
	if style == nil || core == "" {
		return text
	}
	start := strings.Index(text, core)
	lead, trail := text[:start], text[start+len(core):]
	if style.Code {
		core = "`" + core + "`"
	}
	if style.Strike {
		core = "~" + core + "~"
	}
	if style.Italic {
		core = "_" + core + "_"
	}
	if style.Bold {
		core = "*" + core + "*"
	}
	return lead + core + trail
}
