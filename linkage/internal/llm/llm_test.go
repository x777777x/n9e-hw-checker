package llm

import "testing"

func TestExtractJSONFenced(t *testing.T) {
	in := "```json\n{\"结论\":\"网卡故障\",\"紧急度\":\"高\"}\n```"
	got := extractJSON(in)
	if got != `{"结论":"网卡故障","紧急度":"高"}` {
		t.Fatalf("fenced json not extracted: %q", got)
	}
}

func TestExtractJSONWithProse(t *testing.T) {
	in := "分析如下：\n{\"结论\":\"a\"}，请知悉。"
	got := extractJSON(in)
	if got != `{"结论":"a"}` {
		t.Fatalf("json with prose not extracted: %q", got)
	}
}

func TestExtractJSONBare(t *testing.T) {
	in := `{"结论":"a"}`
	if got := extractJSON(in); got != in {
		t.Fatalf("bare json modified: %q", got)
	}
}

func TestExtractJSONWithThinkBlock(t *testing.T) {
	in := " 故障分析中，请稍候  {\"思考\":\"中间过程\"}  \n```json\n{\"结论\":\"网卡故障\"}\n``` "
	if got := extractJSON(in); got != `{"结论":"网卡故障"}` {
		t.Fatalf("think/fence json not extracted correctly: %q", got)
	}
}

func TestExtractJSONThinkBracesIgnored(t *testing.T) {
	// think 块内也有花括号，应跳过并取到真正的 JSON
	in := " 我在思考 {不是JSON} \n {\"结论\":\"ok\"} "
	if got := extractJSON(in); got != `{"结论":"ok"}` {
		t.Fatalf("think braces interfered: %q", got)
	}
}
