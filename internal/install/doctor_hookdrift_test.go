package install

import (
	"bytes"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/hookdrift"
)

func TestDoctorHookDrift(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	var buf bytes.Buffer
	d := &doctor{w: &buf}
	d.hookDrift()
	if buf.Len() != 0 || len(d.problems) != 0 {
		t.Fatalf("no drift must print nothing, got %q", buf.String())
	}

	hookdrift.Note("copilot", "rewrite_v9", []string{"toolName", "toolArgs", "cwd"})
	d = &doctor{w: &buf}
	d.hookDrift()
	out := buf.String()
	for _, want := range []string{"copilot hook payload", "1 call(s)", "rewrite_v9", "cwd,toolArgs,toolName"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
	if len(d.problems) != 1 {
		t.Errorf("problems = %v, want one fix line", d.problems)
	}
}
