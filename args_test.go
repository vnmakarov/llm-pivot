package main

import (
	"strings"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`a b c`, []string{"a", "b", "c"}},
		{`  a   b  `, []string{"a", "b"}},
		{`--msg "hello world"`, []string{"--msg", "hello world"}},
		{`--msg 'hello world'`, []string{"--msg", "hello world"}},
		{`--msg "say \"hi\""`, []string{"--msg", `say "hi"`}},
		{`--path /a\ b`, []string{"--path", "/a b"}},
		{`--empty "" x`, []string{"--empty", "", "x"}},
		{`a"b"c`, []string{"abc"}},
		{``, nil},
	}
	for _, c := range cases {
		got, err := SplitArgs(c.in)
		if err != nil {
			t.Fatalf("SplitArgs(%q): %v", c.in, err)
		}
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("SplitArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	for _, bad := range []string{`"unterminated`, `'nope`, `trailing\`} {
		if _, err := SplitArgs(bad); err == nil {
			t.Errorf("SplitArgs(%q) should have failed", bad)
		}
	}
}

func TestSplitArgsRealEngineCommand(t *testing.T) {
	cmd := `/home/v/llama-server --models-max 1 --jinja on ` +
		`--reasoning-budget-message "Thinking time expired. Formulate your final response now." ` +
		`--device Vulkan1`
	got, err := SplitArgs(cmd)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/home/v/llama-server", "--models-max", "1", "--jinja", "on",
		"--reasoning-budget-message", "Thinking time expired. Formulate your final response now.",
		"--device", "Vulkan1",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestMergeArgsOverridesInPlace(t *testing.T) {
	base, _ := SplitArgs(`--ctx-size 64000 --jinja on --device Vulkan1 --flash-attn on`)
	over, _ := SplitArgs(`--ctx-size 8192 -m /models/x.gguf`)
	got := QuoteArgs(MergeArgs(base, over))
	want := `--ctx-size 8192 --jinja on --device Vulkan1 --flash-attn on -m /models/x.gguf`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestMergeArgsDropsRepeatedBaseFlag(t *testing.T) {
	base, _ := SplitArgs(`--device Vulkan0 --n-gpu-layers 99 --device Vulkan1`)
	over, _ := SplitArgs(`--device CUDA0`)
	got := QuoteArgs(MergeArgs(base, over))
	want := `--device CUDA0 --n-gpu-layers 99`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestMergeArgsMultiValueAndInlineForms(t *testing.T) {
	base, _ := SplitArgs(`--lora a.gguf b.gguf --temp 0.8`)
	over, _ := SplitArgs(`--lora c.gguf --top-k=40`)
	got := QuoteArgs(MergeArgs(base, over))
	want := `--lora c.gguf --temp 0.8 --top-k=40`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestMergeArgsNegativeNumberIsNotAFlag(t *testing.T) {
	base, _ := SplitArgs(`--seed -1 --threads 8`)
	over, _ := SplitArgs(`--threads 16`)
	got := QuoteArgs(MergeArgs(base, over))
	want := `--seed -1 --threads 16`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestHasFlag(t *testing.T) {
	args := []string{"--port", "8080", "--host=1.2.3.4", "-v"}
	for _, f := range []string{"--port", "--host", "-v"} {
		if !HasFlag(args, f) {
			t.Errorf("HasFlag(%q) = false", f)
		}
	}
	if HasFlag(args, "--ctx-size") {
		t.Error("HasFlag(--ctx-size) = true")
	}
	if HasFlag([]string{"--seed", "-1"}, "-1") {
		t.Error("a negative number must not count as a flag")
	}
}

func TestBuildArgvInjectsPort(t *testing.T) {
	e := &Engine{Name: "llama.cpp", Port: 8080, Command: "/bin/srv --jinja on", PortFlag: "--port"}
	m := &Model{Name: "x", Options: "-m /models/x.gguf"}
	argv, err := buildArgv(e, m)
	if err != nil {
		t.Fatal(err)
	}
	want := `/bin/srv --jinja on -m /models/x.gguf --port 8080`
	if got := QuoteArgs(argv); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	// An explicit --port in the command wins; nothing is injected.
	e.Command = "/bin/srv --port 9000"
	argv, _ = buildArgv(e, &Model{Name: "x"})
	if got := QuoteArgs(argv); got != `/bin/srv --port 9000` {
		t.Errorf("got %s", got)
	}

	// port_flag "-" disables injection entirely.
	e.Command, e.PortFlag = "/bin/srv", "-"
	argv, _ = buildArgv(e, &Model{Name: "x"})
	if got := QuoteArgs(argv); got != `/bin/srv` {
		t.Errorf("got %s", got)
	}
}

func TestExpandVars(t *testing.T) {
	e := &Engine{Name: "eng", Port: 8080}
	m := &Model{Name: "mod"}
	got := expandVars("http://127.0.0.1:{{port}}/m?e={{engine}}&m={{model}}", e, m, "")
	want := "http://127.0.0.1:8080/m?e=eng&m=mod"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
