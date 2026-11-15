package matrixfmt

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

// stripMarker removes the "(i/n)\n" marker of a part, failing if it's not the expected one.
func stripMarker(t *testing.T, chunks []string, i int) string {
	t.Helper()
	marker := fmt.Sprintf("(%d/%d)\n", i+1, len(chunks))
	if !strings.HasPrefix(chunks[i], marker) {
		t.Fatalf("part %d doesn't start with %q: %.40q", i+1, marker, chunks[i])
	}
	return strings.TrimPrefix(chunks[i], marker)
}

func requireChunkSizes(t *testing.T, chunks []string) {
	t.Helper()
	for i, chunk := range chunks {
		if n := utf8.RuneCountInString(chunk); n > MaxSplitChars {
			t.Fatalf("part %d has %d characters", i+1, n)
		}
	}
}

func TestSplit12000CharactersKeepsCodeBlocks(t *testing.T) {
	var buf strings.Builder
	var codes []string
	for i := 0; buf.Len() < 12000; i++ {
		fmt.Fprintf(&buf, "<p>Párrafo %03d. %s</p>", i, strings.Repeat("Texto de relleno. ", 20))
		if i%4 == 3 {
			code := fmt.Sprintf("bloque %03d\n%s", i, strings.Repeat("linea de codigo\n", 30))
			codes = append(codes, strings.TrimRight(code, "\n"))
			fmt.Fprintf(&buf, "<pre><code>%s</code></pre>", code)
		}
	}
	block := New2(nil, nil).Parse(context.Background(), buf.String(), nil, nil)
	chunks := SplitText(block)
	if len(chunks) < 4 {
		t.Fatalf("expected at least 4 parts for 12000 characters, got %d", len(chunks))
	}
	requireChunkSizes(t, chunks)
	var all strings.Builder
	for i := range chunks {
		body := stripMarker(t, chunks, i)
		if strings.Count(body, "```")%2 != 0 {
			t.Fatalf("part %d cuts a code block: %q", i+1, body)
		}
		all.WriteString(body + "\n")
	}
	for _, code := range codes {
		if !strings.Contains(all.String(), "```\n"+code+"\n```") {
			t.Fatalf("code block %.10q not kept whole", code)
		}
	}
	last := -1
	for i := 0; strings.Contains(buf.String(), fmt.Sprintf("Párrafo %03d.", i)); i++ {
		at := strings.Index(all.String(), fmt.Sprintf("Párrafo %03d.", i))
		if at <= last {
			t.Fatalf("paragraph %d missing or out of order", i)
		}
		last = at
	}
}

func TestSplitPlainTextKeepsFencedCode(t *testing.T) {
	code := "```\n" + strings.Repeat("x := 1\n", 300) + "```"
	text := strings.Repeat("Una frase de un párrafo largo. ", 100) + "\n\n" + code + "\n\nFin."
	chunks := SplitText(New2(nil, nil).ParseText(context.Background(), text, nil, nil))
	requireChunkSizes(t, chunks)
	found := false
	for i := range chunks {
		body := stripMarker(t, chunks, i)
		found = found || strings.Contains(body, code)
	}
	if !found {
		t.Fatalf("the fenced code block was cut: %q", chunks)
	}
}

func TestSplitPlainTextOneLineFenceIsNotABlock(t *testing.T) {
	// A one-line block doesn't open a code block, and text after an opening fence is code.
	text := "```inline code```\n" + strings.Repeat("Prosa larga. ", 400) + "\n```\nreal\n```\n\n```primera linea\n" +
		strings.Repeat("codigo\n", 600) + "```"
	chunks := SplitText(New2(nil, nil).ParseText(context.Background(), text, nil, nil))
	requireChunkSizes(t, chunks)
	var all strings.Builder
	for i := range chunks {
		body := stripMarker(t, chunks, i)
		if strings.Count(body, "```")%2 != 0 {
			t.Fatalf("part %d has unpaired fences: %.80q", i+1, body)
		}
		all.WriteString(body)
	}
	for _, want := range []string{"```inline code```", "```\nreal\n```", "```\nprimera linea\ncodigo"} {
		if !strings.Contains(all.String(), want) {
			t.Fatalf("lost %q", want)
		}
	}
}

func TestSplitCodeBlockLongerThanAPart(t *testing.T) {
	var lines []string
	for i := range 400 {
		lines = append(lines, fmt.Sprintf("linea %03d de un bloque de codigo enorme", i))
	}
	block := slack.NewRichTextBlock("", slack.NewRichTextPreformatted(0, "", slack.NewRichTextSectionTextElement(strings.Join(lines, "\n"), nil)))
	chunks := SplitText(block)
	requireChunkSizes(t, chunks)
	var got []string
	for i := range chunks {
		body := stripMarker(t, chunks, i)
		if !strings.HasPrefix(body, "```\n") || !strings.HasSuffix(body, "\n```") {
			t.Fatalf("part %d isn't a complete code block: %.40q", i+1, body)
		}
		got = append(got, strings.TrimSuffix(strings.TrimPrefix(body, "```\n"), "\n```"))
	}
	if strings.Join(got, "\n") != strings.Join(lines, "\n") {
		t.Fatal("the code block lost or reordered lines")
	}
}

func TestSplitShortMessageIsOnePartWithoutMarker(t *testing.T) {
	block := New2(nil, nil).Parse(context.Background(), `<h3>Título</h3><p>Hola <b>equipo</b> &amp; <a href="https://example.com">web</a></p>`+
		`<ol start="3"><li>uno</li><li>dos</li></ol><blockquote>cita</blockquote>`, nil, nil)
	chunks := SplitText(block)
	want := "*### Título*\nHola *equipo* &amp; <https://example.com|web>\n3. uno\n4. dos\n> cita"
	if len(chunks) != 1 || chunks[0] != want {
		t.Fatalf("got %q, want %q", chunks, want)
	}
	if chunks := SplitText(slack.NewRichTextBlock("")); len(chunks) != 0 {
		t.Fatalf("an empty message should have no parts, got %q", chunks)
	}
}
