package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// TestEachCmd_IteratesWithFailureIsolation is the core scheduler contract:
// every registered KB runs, and one KB erroring neither aborts the loop nor
// fails the tick.
func TestEachCmd_IteratesWithFailureIsolation(t *testing.T) {
	isolateUserConfig(t)
	a := makeKBRoot(t, "a")
	b := makeKBRoot(t, "b")
	writeUserCfg(t, "kbs:\n  - "+a+"\n  - "+b+"\n")

	var ran []string
	prev := eachRunner
	eachRunner = func(root string, args []string) error {
		ran = append(ran, root+"|"+strings.Join(args, " "))
		if root == a {
			return errors.New("boom")
		}
		return nil
	}
	t.Cleanup(func() { eachRunner = prev })

	if err := (&EachCmd{Args: []string{"sync", "--max", "2"}}).Run(); err != nil {
		t.Fatalf("a failing KB must not fail the tick; got %v", err)
	}
	if len(ran) != 2 {
		t.Fatalf("expected both KBs to run, got %v", ran)
	}
	var sawB bool
	for _, r := range ran {
		if strings.HasPrefix(r, b+"|") {
			sawB = true
			if !strings.HasSuffix(r, "sync --max 2") {
				t.Errorf("args not forwarded verbatim: %q", r)
			}
		}
	}
	if !sawB {
		t.Error("b did not run after a failed — failure isolation broken")
	}
}

func TestEachCmd_NoRegistryErrors(t *testing.T) {
	isolateUserConfig(t)
	writeUserCfg(t, "")
	if err := (&EachCmd{Args: []string{"sync"}}).Run(); err == nil {
		t.Error("expected an error when no KBs are registered")
	}
}

func TestEachCmd_NoArgsErrors(t *testing.T) {
	if err := (&EachCmd{}).Run(); err == nil {
		t.Error("expected a usage error with no subcommand")
	}
}

// TestEachCmd_KongPassthrough pins that `each -- <sub> <flags>` reaches Args
// verbatim, including flags that would otherwise be parsed by kong.
func TestEachCmd_KongPassthrough(t *testing.T) {
	var cli struct {
		Each EachCmd `cmd:""`
	}
	parser, err := kong.New(&cli)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"each", "--", "sync", "--max", "2"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// effectiveArgs strips the `--` kong's passthrough preserves.
	if got := strings.Join(cli.Each.effectiveArgs(), " "); got != "sync --max 2" {
		t.Errorf("effectiveArgs = %q, want 'sync --max 2'", got)
	}
}
