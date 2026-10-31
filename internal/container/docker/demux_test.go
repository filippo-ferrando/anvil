package docker

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"
)

func frame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func TestDemuxLogsBatches(t *testing.T) {
	var all bytes.Buffer
	var want bytes.Buffer
	for i := range 1000 {
		line := "line " + string(rune('a'+i%26)) + "\n"
		all.Write(frame(byte(1+i%2), line))
		want.WriteString(line)
	}
	var sends [][]byte
	if err := demuxLogs(&all, func(b []byte) error { sends = append(sends, b); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := bytes.Join(sends, nil); !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("demuxed %d bytes, want %d in order", len(got), want.Len())
	}
	if len(sends) > 3 {
		t.Errorf("%d sends for 1000 buffered frames, want them batched", len(sends))
	}
}

// A followed log: a frame with nothing behind it goes out without waiting for more.
func TestDemuxLogsFollowNoDelay(t *testing.T) {
	pr, pw := io.Pipe()
	got := make(chan []byte, 4)
	go func() { _ = demuxLogs(pr, func(b []byte) error { got <- b; return nil }) }()
	go func() { _, _ = pw.Write(frame(1, "ready\n")) }()
	select {
	case b := <-got:
		if string(b) != "ready\n" {
			t.Errorf("got %q", b)
		}
	case <-time.After(time.Second):
		t.Fatal("a lone frame waited for more data")
	}
	pw.Close()
}
