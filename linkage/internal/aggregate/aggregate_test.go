package aggregate

import (
	"testing"
	"time"
	"unicode/utf8"

	"kernel-hw-linkage/internal/es"
)

func TestAggregateDedupAndTopN(t *testing.T) {
	hits := []es.Hit{
		{Timestamp: time.Now(), Message: "eth0: Link is Down"},
		{Timestamp: time.Now(), Message: "eth0: Link is Down"},
		{Timestamp: time.Now(), Message: "eth0: Link is Down"},
		{Timestamp: time.Now(), Message: "EDAC MC0: 1 CE memory read error"},
		{Timestamp: time.Now(), Message: "EDAC MC0: 1 CE memory read error"},
	}
	res := Aggregate(hits, Options{MaxLines: 10, MaxCharsLine: 100})
	if len(res.Lines) != 2 {
		t.Fatalf("want 2 unique lines, got %d", len(res.Lines))
	}
	// 按出现次数降序：Link is Down(x3) 在前
	if res.Lines[0].Message != "eth0: Link is Down" || res.Lines[0].Count != 3 {
		t.Fatalf("want Link is Down x3 first, got %+v", res.Lines[0])
	}
	if res.Lines[1].Count != 2 {
		t.Fatalf("want EDAC x2, got %+v", res.Lines[1])
	}
}

func TestAggregateTruncate(t *testing.T) {
	long := "x"
	for i := 0; i < 50; i++ {
		long += "x"
	}
	hits := []es.Hit{{Message: long}}
	res := Aggregate(hits, Options{MaxLines: 1, MaxCharsLine: 10})
	if len(res.Lines[0].Message) > 13 {
		t.Fatalf("want truncated <=13 chars, got %d: %q", len(res.Lines[0].Message), res.Lines[0].Message)
	}
}

func TestAggregateTruncateChineseValidUTF8(t *testing.T) {
	// 长中文串按 rune 截断后，输出必须是合法 UTF-8
	msg := "硬件错误网卡链路中断故障排查报告内容较长的补充说明"
	hits := []es.Hit{{Message: msg}}
	res := Aggregate(hits, Options{MaxLines: 1, MaxCharsLine: 5})
	out := res.Lines[0].Message
	if !utf8.ValidString(out) {
		t.Fatalf("truncated message is invalid UTF-8: %q", out)
	}
	if utf8.RuneCountInString(out) > 8 {
		t.Fatalf("want ~5 runes + ..., got %q", out)
	}
}

func TestAggregateTopN(t *testing.T) {
	hits := []es.Hit{
		{Message: "a"}, {Message: "b"}, {Message: "c"}, {Message: "d"},
	}
	res := Aggregate(hits, Options{MaxLines: 2, MaxCharsLine: 100})
	if len(res.Lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(res.Lines))
	}
}
