// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package md_test

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/caic-xyz/md"
)

func ExampleContainer() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client, err := md.New(os.Stdout)
	if err != nil {
		log.Fatal(err)
	}
	base, err := client.Container(md.Repo{GitRoot: ".", Branch: "main"})
	if err != nil {
		log.Fatal(err)
	}

	run := &md.Container{
		Client: base.Client,
		Repos:  append([]md.Repo(nil), base.Repos...),
		Name:   base.Name + "-run",
	}
	opts := &md.StartOpts{}
	if err = run.Launch(ctx, os.Stdout, os.Stderr, opts); err != nil {
		log.Fatal(err)
	}
	if _, err = run.Connect(ctx, os.Stdout, os.Stderr, opts); err == nil {
		sshArgs := run.SSHCommand(nil, "cd "+shellQuote(run.Repos[0].MountedPath)+" && go test ./...")
		cmd := exec.CommandContext(ctx, sshArgs[0], sshArgs[1:]...) //nolint:gosec // SSH target and command are built from md container state.
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
	}
	if err = errors.Join(err, run.Purge(ctx, os.Stdout, os.Stderr)); err != nil {
		log.Fatal(err)
	}
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') &&
			c != '@' && c != '%' && c != '+' && c != '=' && c != ':' && c != ',' && c != '.' &&
			c != '/' && c != '-' && c != '_' {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
