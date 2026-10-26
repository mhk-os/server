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
}
