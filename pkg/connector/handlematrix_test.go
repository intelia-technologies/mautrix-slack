package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/slack-go/slack"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-slack/pkg/msgconv"
	"go.mau.fi/mautrix-slack/pkg/msgconv/matrixfmt"
	"go.mau.fi/mautrix-slack/pkg/slackid"
)

// fakeSlack answers each request with the given errors in order ("" is a success), then
// with success. Each success has a new timestamp, ending in the number of the request. The
// method of each request is recorded as "_method" in its form.
func fakeSlack(t *testing.T, errs ...string) (*slack.Client, *[]url.Values) {
	t.Helper()
	var sent []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := r.PostForm
		form.Set("_method", strings.TrimPrefix(r.URL.Path, "/"))
		sent = append(sent, form)
		if len(sent) <= len(errs) && errs[len(sent)-1] != "" {
			_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, errs[len(sent)-1])
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":"1700000000.%06d"}`, len(sent))
	}))
	t.Cleanup(srv.Close)
	return slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/")), &sent
}

func convert(t *testing.T, formattedBody string, threadRoot *database.Message) *msgconv.ConvertedSlackMessage {
	t.Helper()
	return convertMessage(t, formattedBody, threadRoot, nil)
}

func convertMessage(t *testing.T, formattedBody string, threadRoot, editTarget *database.Message) *msgconv.ConvertedSlackMessage {
	t.Helper()
	mc := &msgconv.MessageConverter{MatrixHTMLParser: matrixfmt.New2(nil, nil)}
	content := &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          "mensaje",
		Format:        event.FormatHTML,
		FormattedBody: formattedBody,
	}
	conv, err := mc.ToSlack(context.Background(), nil, nil, content, &event.Event{Type: event.EventMessage}, threadRoot, editTarget, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return conv
}

func TestSendToSlackSplitTextStartsAThread(t *testing.T) {
	// A message that wasn't a reply: the first part goes to the channel and the rest reply to it.
	client, sent := fakeSlack(t, "foo_unknown")
	ts, otherParts, err := (&SlackClient{Client: client}).sendToSlack(context.Background(), "C1", convert(t, reportTables(), nil), nil)
	if err != nil || ts != "1700000000.000002" || len(otherParts) != len(*sent)-2 {
		t.Fatalf("expected the split text to be sent, got %q, %v, %v", ts, otherParts, err)
	}
	parts := (*sent)[1:]
	if parts[0].Has("thread_ts") {
		t.Fatalf("the first part should start the thread, got thread_ts %q", parts[0].Get("thread_ts"))
	}
	for i, part := range parts[1:] {
		if part.Get("thread_ts") != ts {
			t.Fatalf("part %d should reply to the first part %s, got thread_ts %q", i+2, ts, part.Get("thread_ts"))
		}
	}
}

func TestSendToSlackLongEditSplitsText(t *testing.T) {
	// An edit that Slack rejects and doesn't fit in a part: the edited message gets the first
	// part and the rest reply to it, so nothing of the new content is lost.
	client, sent := fakeSlack(t, "foo_unknown")
	editTarget := &database.Message{ID: slackid.MakeMessageID("T1", "C1", "1700000000.000200")}
	ts, otherParts, err := (&SlackClient{Client: client}).sendToSlack(context.Background(), "C1", convertMessage(t, reportTables(), nil, editTarget), nil)
	if err != nil || ts != "1700000000.000002" {
		t.Fatalf("expected the split edit to be sent, got %q, %v", ts, err)
	}
	parts := (*sent)[1:]
	if len(parts) < 2 || len(otherParts) != len(parts)-1 {
		t.Fatalf("expected several parts, got %d and %v", len(parts), otherParts)
	}
	first := parts[0]
	if first.Get("_method") != "chat.update" || first.Get("ts") != "1700000000.000200" || first.Get("blocks") != "[]" ||
		!strings.HasPrefix(first.Get("text"), fmt.Sprintf("(1/%d)\n", len(parts))) {
		t.Fatalf("the first part should update the edited message, got %v", first)
	}
	for i, part := range parts[1:] {
		if part.Get("_method") != "chat.postMessage" || part.Get("thread_ts") != "1700000000.000200" || part.Has("ts") {
			t.Fatalf("part %d should reply to the edited message, got %v", i+2, part)
		}
	}
}

func TestSendToSlackCodeBlockTooLongFailsForGood(t *testing.T) {
	// A code block of 4,000 characters doesn't fit in a part and isn't cut: the message fails.
	client, sent := fakeSlack(t, "foo_unknown")
	html := "<p>Antes</p><pre><code>" + strings.Repeat("x", 4000) + "</code></pre>"
	_, otherParts, err := (&SlackClient{Client: client}).sendToSlack(context.Background(), "C1", convert(t, html, nil), nil)
	var status bridgev2.MessageStatus
	if !errors.As(err, &status) || status.Status != event.MessageStatusFail || !errors.Is(err, matrixfmt.ErrCodeBlockTooLong) {
		t.Fatalf("expected a definitive failure for the code block, got %v", err)
	}
	if !strings.Contains(err.Error(), "foo_unknown") || len(*sent) != 1 || len(otherParts) != 0 {
		t.Fatalf("expected no text sent after the rejection, got %d requests, %v", len(*sent), err)
	}
}

const smallTable = `<p>Antes</p><table><tr><th>A</th></tr><tr><td>1</td></tr></table>`

func TestSendToSlackRetriesWithoutTables(t *testing.T) {
	client, sent := fakeSlack(t, "invalid_blocks")
	ts, otherParts, err := (&SlackClient{Client: client}).sendToSlack(context.Background(), "C1", convert(t, smallTable, nil), nil)
	if len(otherParts) != 0 {
		t.Fatalf("a message in one part has no other parts, got %v", otherParts)
	}
	if err != nil || ts != "1700000000.000002" {
		t.Fatalf("expected the retry to succeed, got %q, %v", ts, err)
	}
	if len(*sent) != 2 || !strings.Contains((*sent)[0].Get("blocks"), `"type":"table"`) || (*sent)[1].Has("blocks") {
		t.Fatalf("expected a native table first and a retry as text, got %v", *sent)
	}
	if text := (*sent)[1].Get("text"); text != "Antes\n```\nA\n-\n1\n```" {
		t.Fatalf("retry should carry the table as a code block, got %q", text)
	}
}

// reportTables is the message that Slack rejected in SF-2316: five 6x4 tables with 40 links
// of 240 characters, between paragraphs.
func reportTables() string {
	var buf strings.Builder
	link := 0
	for table := range 5 {
		fmt.Fprintf(&buf, "<h3>Sección %d</h3><p>%s</p><table><tr><th>Tarea</th><th>Estado</th><th>PR</th><th>Notas</th></tr>",
			table, strings.Repeat("Texto del informe. ", 40))
		for row := range 5 {
			fmt.Fprintf(&buf, "<tr><td>OPSF-%d</td><td>hecha</td>", table*10+row)
			for range 2 {
				if link < 40 {
					target := fmt.Sprintf("https://example.com/%03d/", link) + strings.Repeat("x", 240-len("https://example.com/000/"))
					fmt.Fprintf(&buf, `<td><a href="%s">enlace %d</a></td>`, target, link)
					link++
				} else {
					buf.WriteString("<td>-</td>")
				}
			}
			buf.WriteString("</tr>")
		}
		buf.WriteString("</table>")
	}
	return buf.String()
}

func TestSendToSlackUnknownRejectionSplitsText(t *testing.T) {
	client, sent := fakeSlack(t, "foo_unknown")
	threadRoot := &database.Message{ID: slackid.MakeMessageID("T1", "C1", "1700000000.000100")}
	ts, otherParts, err := (&SlackClient{Client: client}).sendToSlack(context.Background(), "C1", convert(t, reportTables(), threadRoot), nil)
	if err != nil || ts != "1700000000.000002" {
		t.Fatalf("expected the split text to be sent, got %q, %v", ts, err)
	}
	if want := len(*sent) - 2; len(otherParts) != want || otherParts[0] != "1700000000.000003" || otherParts[want-1] != fmt.Sprintf("1700000000.%06d", len(*sent)) {
		t.Fatalf("expected the timestamps of parts 2..n in order, got %v", otherParts)
	}
	parts := (*sent)[1:]
	if len(parts) < 2 {
		t.Fatalf("expected the message split in several parts, got %d", len(parts))
	}
	var all strings.Builder
	for i, part := range parts {
		text := part.Get("text")
		if n := utf8.RuneCountInString(text); n > matrixfmt.MaxSplitChars {
			t.Errorf("part %d has %d characters", i+1, n)
		}
		if marker := fmt.Sprintf("(%d/%d)\n", i+1, len(parts)); !strings.HasPrefix(text, marker) {
			t.Errorf("part %d doesn't start with %q: %.40q", i+1, marker, text)
		}
		if part.Get("thread_ts") != "1700000000.000100" || part.Has("blocks") {
			t.Errorf("part %d should be text in the same thread, got %v", i+1, part)
		}
		if strings.Count(text, "```")%2 != 0 {
			t.Errorf("part %d cuts a code block", i+1)
		}
		all.WriteString(strings.TrimPrefix(text, fmt.Sprintf("(%d/%d)\n", i+1, len(parts))))
	}
	// Every link, in order.
	last := -1
	for link := range 40 {
		at := strings.Index(all.String(), fmt.Sprintf("https://example.com/%03d/", link))
		if at <= last {
			t.Fatalf("link %d missing or out of order", link)
		}
		last = at
	}
}

func TestSendToSlackRejectedTextFailsForGood(t *testing.T) {
	client, sent := fakeSlack(t, "foo_unknown", "msg_too_long")
	_, otherParts, err := (&SlackClient{Client: client}).sendToSlack(context.Background(), "C1", convert(t, smallTable, nil), nil)
	if len(otherParts) != 0 {
		t.Fatalf("no part was sent, got %v", otherParts)
	}
	var status bridgev2.MessageStatus
	if !errors.As(err, &status) || status.Status != event.MessageStatusFail || !status.IsCertain {
		t.Fatalf("expected a definitive failure, got %#v", err)
	}
	if !strings.Contains(err.Error(), "foo_unknown") || !strings.Contains(err.Error(), "msg_too_long") || len(*sent) != 2 {
		t.Fatalf("expected both rejections in the error after 2 requests, got %d: %v", len(*sent), err)
	}
}

func TestSendToSlackPartialSplitTextReturnsSentParts(t *testing.T) {
	// Parts 1 and 2 go out and part 3 is rejected: the message fails, and the parts sent are
	// returned so that their echoes from Slack aren't bridged back.
	client, sent := fakeSlack(t, "foo_unknown", "", "", "msg_too_long")
	_, otherParts, err := (&SlackClient{Client: client}).sendToSlack(context.Background(), "C1", convert(t, reportTables(), nil), nil)
	var status bridgev2.MessageStatus
	if !errors.As(err, &status) || status.Status != event.MessageStatusFail || len(*sent) != 4 {
		t.Fatalf("expected a definitive failure after 4 requests, got %d: %v", len(*sent), err)
	}
	if fmt.Sprint(otherParts) != "[1700000000.000002 1700000000.000003]" {
		t.Fatalf("expected the two parts sent, got %v", otherParts)
	}
}

func TestSendToSlackDoesNotResendOnTransportErrors(t *testing.T) {
	// A failure that isn't Slack rejecting the message may have been posted: don't repeat it.
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	client := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	_, _, err := (&SlackClient{Client: client}).sendToSlack(context.Background(), "C1", convert(t, smallTable, nil), nil)
	if err == nil || requests != 1 {
		t.Fatalf("expected one failed request, got %d requests, err %v", requests, err)
	}
}
