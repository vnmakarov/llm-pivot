package main

import (
	"os/exec"
	"testing"
)

// TestWebFollowScroll runs web_scroll_test.mjs, which exercises the monitor
// page's own functions over a stub DOM. It is skipped where node is not
// installed: the page is checked by whoever has it, and never unchecked
// silently by whoever does not.
func TestWebFollowScroll(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the monitor page's own tests")
	}
	out, err := exec.Command(node, "web_scroll_test.mjs").CombinedOutput()
	t.Logf("web_scroll_test.mjs:\n%s", out)
	if err != nil {
		t.Fatalf("the monitor page's scrolling is wrong: %v", err)
	}
}
