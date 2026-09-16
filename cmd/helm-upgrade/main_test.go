package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

func TestWantsHelp(t *testing.T) {
	cases := map[bool][][]string{
		true: {
			{"helm-upgrade"},
			{"helm-upgrade", "--help"},
			{"helm-upgrade", "list", "-h"},
			{"helm-upgrade", "help"},
		},
		false: {
			{"helm-upgrade", "list"},
			{"helm-upgrade", "upgrade", "--chart", "nginx", "--version", "2.0.0"},
		},
	}
	for want, argsets := range cases {
		for _, args := range argsets {
			if got := wantsHelp(args); got != want {
				t.Fatalf("wantsHelp(%v) = %v, want %v", args, got, want)
			}
		}
	}
}

func TestConfigureLogging(t *testing.T) {
	if err := configureLogging("debug", "json"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if logrus.GetLevel() != logrus.DebugLevel {
		t.Fatalf("level = %v, want debug", logrus.GetLevel())
	}
	if err := configureLogging("info", "text"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var ue usageError
	err := configureLogging("loud", "text")
	if err == nil || !errors.As(err, &ue) {
		t.Fatalf("invalid level should be a usage error, got %v", err)
	}
	err = configureLogging("info", "xml")
	if err == nil || !errors.As(err, &ue) {
		t.Fatalf("invalid format should be a usage error, got %v", err)
	}
}

func TestUsageErrorUnwrap(t *testing.T) {
	inner := errors.New("inner")
	err := error(usageError{inner})
	if !errors.Is(err, inner) {
		t.Fatal("usageError must unwrap to its cause")
	}
}

func TestResolvePassword(t *testing.T) {
	newCtx := func(args ...string) *cli.Context {
		set := flag.NewFlagSet("upgrade", flag.ContinueOnError)
		set.String("password", "", "")
		set.Bool("password-stdin", false, "")
		set.Bool("yes", false, "")
		set.Bool("dry-run", false, "")
		if err := set.Parse(args); err != nil {
			t.Fatalf("parsing %v: %v", args, err)
		}
		return cli.NewContext(cli.NewApp(), set, nil)
	}

	if got, err := newPasswordFrom(newCtx("-password", "hunter2"), ""); err != nil || got != "hunter2" {
		t.Fatalf("plain --password: %q, %v", got, err)
	}

	var ue usageError
	if _, err := newPasswordFrom(newCtx("-password", "x", "-password-stdin", "-yes"), "y"); err == nil || !errors.As(err, &ue) {
		t.Fatalf("mutually exclusive flags should be a usage error, got %v", err)
	}
	if _, err := newPasswordFrom(newCtx("-password-stdin"), "secret\n"); err == nil || !errors.As(err, &ue) {
		t.Fatalf("--password-stdin without --yes/--dry-run should be a usage error, got %v", err)
	}
	if _, err := newPasswordFrom(newCtx("-password-stdin", "-yes"), "\n"); err == nil || !errors.As(err, &ue) {
		t.Fatalf("empty stdin should be a usage error, got %v", err)
	}
	if got, err := newPasswordFrom(newCtx("-password-stdin", "-dry-run"), "secret\r\n"); err != nil || got != "secret" {
		t.Fatalf("stdin password = %q, %v; want \"secret\"", got, err)
	}
}

// newPasswordFrom runs resolvePassword with stdin replaced by the given content.
func newPasswordFrom(c *cli.Context, stdin string) (string, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	go func() {
		defer w.Close()
		_, _ = io.WriteString(w, stdin)
	}()
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig; r.Close() }()
	return resolvePassword(c)
}

func TestFirstOrEmpty(t *testing.T) {
	if firstOrEmpty(nil) != "" {
		t.Fatal("nil slice should yield an empty string")
	}
	if firstOrEmpty([]string{"a", "b"}) != "a" {
		t.Fatal("expected the first element")
	}
}

func TestAppFlagsWiring(t *testing.T) {
	app := newApp(&appState{})
	names := map[string]bool{}
	for _, cmd := range app.Commands {
		names[cmd.Name] = true
	}
	for _, want := range []string{"list", "upgrade", "version"} {
		if !names[want] {
			t.Fatalf("command %q is not registered", want)
		}
	}
	if err := app.Run([]string{"helm-upgrade", "version"}); err != nil {
		t.Fatalf("version command failed: %v", err)
	}
}
