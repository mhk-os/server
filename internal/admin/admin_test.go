package admin

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgsInterleaved(t *testing.T) {
	for _, args := range [][]string{
		{"bob", "--reason", "spam bot"},
		{"--reason", "spam bot", "bob"},
		{"--reason=spam bot", "bob"},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		reason := fs.String("reason", "", "")
		pos, err := parseArgs(fs, args)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(pos, []string{"bob"}) || *reason != "spam bot" {
			t.Fatalf("%v: pos %v reason %q", args, pos, *reason)
		}
	}
	var ue *UsageError
	if _, err := parseArgs(flag.NewFlagSet("t", flag.ContinueOnError), []string{"--nope"}); !errors.As(err, &ue) {
		t.Fatalf("unknown flag: %v", err)
	}
}

func TestDispatchUsage(t *testing.T) {
	var out bytes.Buffer
	env := &Env{Out: &out}
	var ue *UsageError
	if err := Run(context.Background(), env, []string{"account", "frobnicate"}); !errors.As(err, &ue) {
		t.Fatalf("unknown command: %v", err)
	}
	if !strings.Contains(out.String(), "admin account disable <username> --reason TEXT") {
		t.Fatalf("usage:\n%s", out.String())
	}
	if err := Run(context.Background(), env, []string{"account", "delete", "bob"}); !errors.As(err, &ue) {
		t.Fatalf("delete without --yes: %v", err)
	}
	out.Reset()
	Root.PrintUsage(&out, "admin")
	for _, want := range []string{"admin run list [--account U] [--track T] [--limit N]", "admin run delete <id> --yes",
		"admin run hide <id> --reason TEXT", "admin run reports <id>", "admin board list [--track T]"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage lacks %q:\n%s", want, out.String())
		}
	}
	for _, args := range [][]string{{"run", "show", "abc"}, {"run", "delete", "0", "--yes"}, {"run", "show"}} {
		if err := Run(context.Background(), env, args); !errors.As(err, &ue) {
			t.Errorf("%v: %v", args, err)
		}
	}
	if formatMs(96_252) != "1:36.252" || formatMs(5_007) != "0:05.007" {
		t.Error("formatMs")
	}
}
