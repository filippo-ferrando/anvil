package events

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncodeParseRoundTrip(t *testing.T) {
	var b bytes.Buffer
	in := Event{Event: Detached, Log: "/tmp/dev.log", Offset: 42}
	if err := Encode(&b, in); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != `{"event":"detached","log":"/tmp/dev.log","offset":42}`+"\n" {
		t.Fatalf("encoded %q", got)
	}
	out, ok := Parse(b.Bytes())
	if !ok || out != in {
		t.Fatalf("parsed %+v %v", out, ok)
	}
}

func TestParseSkipsOtherLines(t *testing.T) {
	for _, l := range []string{"", "app output", "{not json", `{"text":"no kind"}`, "anvil: failed"} {
		if _, ok := Parse([]byte(l)); ok {
			t.Errorf("%q parsed as an event", l)
		}
	}
}

func TestStepWriter(t *testing.T) {
	var b bytes.Buffer
	w := &StepWriter{W: &b}
	_, _ = w.Write([]byte("restoring the clone\nstart"))
	_, _ = w.Write([]byte("ing the app\n\n"))
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %q", b.String())
	}
	for i, want := range []string{"restoring the clone", "starting the app"} {
		e, ok := Parse([]byte(lines[i]))
		if !ok || e.Event != Step || e.String() != want {
			t.Errorf("line %d: %+v", i, e)
		}
	}
}

func TestString(t *testing.T) {
	cases := map[string]Event{
		"exported /a.desktop":                    {Event: Exported, Path: "/a.desktop"},
		"started in the background, log: /x.log": {Event: Detached, Log: "/x.log"},
		"firefox runs in dev":                    {Event: Running, App: "firefox", Where: "dev", Text: "firefox runs in dev"},
		"boom":                                   {Event: Error, Message: "boom"},
		"":                                       {Event: Done},
	}
	for want, e := range cases {
		if got := e.String(); got != want {
			t.Errorf("%+v: %q, want %q", e, got, want)
		}
	}
}
