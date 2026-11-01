package trace

import (
	"reflect"
	"strings"
	"testing"
)

func TestRedactArgv(t *testing.T) {
	args := map[string]any{"argv": []any{"env", "HF_TOKEN=abc", "PATH=/bin", "curl", "--token", "abc", "--api-key=k", "-H", "Bearer xyz", "--name", "ok"}}
	before := HashArgs(args)
	got, ok := RedactArgv(args)
	if !ok {
		t.Fatal("no argv")
	}
	want := []string{"env", "HF_TOKEN=***", "PATH=/bin", "curl", "--token", "***", "--api-key=***", "-H", "Bearer ***", "--name", "ok"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	if HashArgs(args) != before || args["argv"].([]any)[1] != "HF_TOKEN=abc" {
		t.Fatal("args mutated")
	}
}

func TestRedactArgvCaps(t *testing.T) {
	many := make([]any, 100)
	for i := range many {
		many[i] = "a"
	}
	got, _ := RedactArgv(map[string]any{"argv": many})
	if len(got) != maxArgvElems+1 || got[len(got)-1] != argvMarker {
		t.Fatalf("elem cap: %d", len(got))
	}
	got, _ = RedactArgv(map[string]any{"argv": []any{strings.Repeat("x", 1000)}})
	if len(got[0]) != maxArgvElemBytes+len(argvMarker) {
		t.Fatalf("byte cap: %d", len(got[0]))
	}
	big := make([]any, 40)
	for i := range big {
		big[i] = strings.Repeat("y", 250)
	}
	got, _ = RedactArgv(map[string]any{"argv": big})
	if got[len(got)-1] != argvMarker || len(got) > 40 {
		t.Fatalf("total cap: %d", len(got))
	}
	if _, ok := RedactArgv(map[string]any{"path": "x"}); ok {
		t.Fatal("expected !ok")
	}
}

func TestRedactArgvBearer(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"curl", "-H", "Authorization: Bearer abc"}, []string{"curl", "-H", "Authorization: Bearer ***"}},
		{[]string{"x", "authorization: bEaReR abc"}, []string{"x", "authorization: bEaReR ***"}},
		{[]string{"curl", "-H", "Bearer", "abc", "url"}, []string{"curl", "-H", "Bearer", "***", "url"}},
		{[]string{"BEARER", "abc"}, []string{"BEARER", "***"}},
		{[]string{"echo", "bearers"}, []string{"echo", "bearers"}},
	}
	for _, c := range cases {
		in := make([]any, len(c.in))
		for i, s := range c.in {
			in[i] = s
		}
		got, ok := RedactArgv(map[string]any{"argv": in})
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("RedactArgv(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
