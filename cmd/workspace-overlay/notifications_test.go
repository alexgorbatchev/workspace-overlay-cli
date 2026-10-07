package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestNativeNotifications(t *testing.T) {
	v, base, shared, _ := setupTestView(t)
	initGitRepoWithCommit(t, base)
	n, err := newNotifications(context.Background(), mountPlan{target: base, view: v}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := n.close(); err != nil {
			t.Error(err)
		}
	})
	git, source := nativePath(n.roots[0].file), nativePath(n.roots[1].file)
	cases := []struct {
		event fsnotify.Event
		want  bool
	}{
		{fsnotify.Event{Name: filepath.Join(git, "refs/heads/main"), Op: fsnotify.Write}, false},
		{fsnotify.Event{Name: filepath.Join(git, "worktrees"), Op: fsnotify.Create}, true},
		{fsnotify.Event{Name: filepath.Join(git, "worktrees/one/gitdir"), Op: fsnotify.Write}, true},
		{fsnotify.Event{Name: filepath.Join(git, "worktrees/one/locked"), Op: fsnotify.Remove}, true},
		{fsnotify.Event{Name: filepath.Join(source, "file"), Op: fsnotify.Write}, true},
		{fsnotify.Event{Name: filepath.Join(source, "file"), Op: fsnotify.Chmod}, false},
		{fsnotify.Event{Name: "/unrelated", Op: fsnotify.Create}, false},
	}
	for _, tt := range cases {
		if got := n.relevant(tt.event); got != tt.want {
			t.Errorf("relevant(%v)=%v, want %v", tt.event, got, tt.want)
		}
	}
	writeFixture(t, filepath.Join(shared, "added.md"), "live")
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-n.watcher.Events:
			if n.relevant(event) {
				return
			}
		case err := <-n.watcher.Errors:
			t.Fatal(err)
		case <-timer.C:
			t.Fatal("native source notification not delivered")
		}
	}
}

func TestNotificationErrors(t *testing.T) {
	cases := []string{"closed source", "corrupt git", "closed backing"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			v, base, _, _ := setupTestView(t)
			switch name {
			case "closed source":
				if err := v.layers[1].root.Close(); err != nil {
					t.Fatal(err)
				}
			case "corrupt git":
				writeFixture(t, filepath.Join(base, ".git"), "invalid")
			case "closed backing":
				if err := v.layers[0].root.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := newNotifications(context.Background(), mountPlan{target: base, view: v}, true); err == nil {
				n.close()
				t.Fatal("invalid backing handles accepted")
			}
		})
	}
}
