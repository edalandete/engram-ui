package main

import "testing"

// The AI assistants' /engram-pull, /engram-push and /engram-sync build this
// command line, so a typo must fail instead of acting on the wrong thing.
func TestParseShareArgs(t *testing.T) {
	for _, args := range [][]string{
		{"push", "-project", "nlo-ai-toolkit"},
		{"pull", "--project=nlo-ai-toolkit"},
		{"sync", "-project", " nlo-ai-toolkit "},
	} {
		action, project, err := parseShareArgs(args)
		if err != nil || action != args[0] || project != "nlo-ai-toolkit" {
			t.Errorf("parseShareArgs(%q) = %q, %q, %v", args, action, project, err)
		}
	}
	for _, args := range [][]string{
		nil,
		{"publish", "-project", "x"},
		{"push"},
		{"push", "-project", " "},
		{"push", "nlo-ai-toolkit"},
		{"push", "-project", "x", "extra"},
		{"push", "-h"},
	} {
		if _, _, err := parseShareArgs(args); err == nil {
			t.Errorf("parseShareArgs(%q) accepted, want an error", args)
		}
	}
}
