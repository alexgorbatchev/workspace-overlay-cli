package overlayfs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

const (
	projectText   = "# Project\n- project rule\n"
	workspaceText = "# Workspace\n- workspace rule\n"
	alphaText     = "# Alpha\n- alpha rule\n"
)

// markedDocument is a mounted project whose AGENTS.md is joined, with
// markers, from the project and from each of the named overlays.
type markedDocument struct {
	view *View
	// file is AGENTS.md as seen through the mount.
	file    string
	sources map[string]string
}

func mountMarkedDocument(t *testing.T, overlays ...string) markedDocument {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "alpha")
	scratch.Write(t, filepath.Join(project, "AGENTS.md"), projectText)
	projectRoot, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	v := &View{}
	v.SetMarkers(defaultMarkerRules)
	v.AddProject(projectRoot)
	doc := markedDocument{view: v, file: filepath.Join(project, "AGENTS.md"), sources: make(map[string]string)}
	texts := map[string]string{"workspace": workspaceText, "alpha": alphaText}
	for _, name := range overlays {
		source := filepath.Join(root, ".ai", name)
		doc.sources[name] = scratch.Write(t, filepath.Join(source, "AGENTS.md"), texts[name])
		sourceRoot, err := os.OpenRoot(source)
		if err != nil {
			t.Fatal(err)
		}
		v.AddOverlay(sourceRoot, name, "**/*")
	}
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	server, err := v.Mount(project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		v.Close()
	})
	// The test reads the mount from the process that serves it.
	if err := server.WaitMount(); err != nil {
		t.Fatal(err)
	}
	return doc
}

// projectCopy returns the project's own AGENTS.md, which the mount hides, and
// whether it exists.
func (d markedDocument) projectCopy(t *testing.T) (string, bool) {
	t.Helper()
	data, err := d.view.Project().ReadFile("AGENTS.md")
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

func section(name, text string) string {
	return "<!-- BEGIN WORKSPACE-OVERLAY: " + name + " (../.ai/" + name + "/AGENTS.md) -->\n" + text + "<!-- END WORKSPACE-OVERLAY: " + name + " (../.ai/" + name + "/AGENTS.md) -->\n"
}

// saveLikeVim replaces file with content the way Vim does with its default
// 'backupcopy' when it takes the file for an ordinary one: it moves the file
// aside, and when that is refused it copies it aside, removes it, and writes
// a new file under its name.
func saveLikeVim(t *testing.T, file, content string) {
	t.Helper()
	backup := file + "~"
	if err := os.Rename(file, backup); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("moving a merged file aside = %v, want EPERM", err)
	}
	scratch.Write(t, backup, string(scratch.Read(t, file)))
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("writing the new file = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
}

// A program that removes a merged file and writes it again must end up with
// every section where it belongs. Before, the project copy stayed deleted and
// its text, with the marker lines, was saved into an overlay source.
func TestRemoveAndRewriteSavesEverySectionToItsSource(t *testing.T) {
	tests := []struct {
		name     string
		overlays []string
		// edit names the section the save changes.
		edit string
	}{
		{"one overlay, project section edited", []string{"workspace"}, "project"},
		{"one overlay, overlay section edited", []string{"workspace"}, "workspace"},
		{"two overlays, project section edited", []string{"workspace", "alpha"}, "project"},
		{"two overlays, last section edited", []string{"workspace", "alpha"}, "alpha"},
		{"two overlays, nothing edited", []string{"workspace", "alpha"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mountMarkedDocument(t, tt.overlays...)
			want := map[string]string{"project": projectText, "workspace": workspaceText, "alpha": alphaText}
			if tt.edit != "" {
				want[tt.edit] += "- added while editing\n"
			}
			content := want["project"]
			for _, name := range tt.overlays {
				content += section(name, want[name])
			}

			saveLikeVim(t, doc.file, content)

			if got, _ := doc.projectCopy(t); got != want["project"] {
				t.Errorf("project copy = %q, want %q", got, want["project"])
			}
			for _, name := range tt.overlays {
				if got := string(scratch.Read(t, doc.sources[name])); got != want[name] {
					t.Errorf("%s source = %q, want %q", name, got, want[name])
				}
			}
			if got := string(scratch.Read(t, doc.file)); got != content {
				t.Errorf("document = %q, want %q", got, content)
			}
			if _, err := os.Lstat(doc.file + "~"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("backup file was left behind: %v", err)
			}
		})
	}
}

// Removing the project copy empties the project section and nothing else:
// the overlays stay marked and protected, and text written into the empty
// section becomes the project copy again.
func TestRemovedProjectCopyLeavesItsSectionEmpty(t *testing.T) {
	doc := mountMarkedDocument(t, "workspace")
	if err := os.Remove(doc.file); err != nil {
		t.Fatal(err)
	}

	if _, exists := doc.projectCopy(t); exists {
		t.Fatal("project copy survived its removal")
	}
	remaining := section("workspace", workspaceText)
	if got := string(scratch.Read(t, doc.file)); got != remaining {
		t.Fatalf("document after removal = %q, want %q", got, remaining)
	}
	info, err := os.Stat(doc.file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(remaining)) {
		t.Errorf("size = %d, want %d", info.Size(), len(remaining))
	}
	if err := os.Remove(doc.file); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("removing the overlay's section = %v, want EPERM", err)
	}

	// Saving the document unchanged does not bring an empty project copy back.
	scratch.Write(t, doc.file, remaining)
	if _, exists := doc.projectCopy(t); exists {
		t.Fatal("an empty project section created a project copy")
	}

	scratch.Write(t, doc.file, "# Back\n"+remaining)
	if got, _ := doc.projectCopy(t); got != "# Back\n" {
		t.Fatalf("project copy = %q, want the text written into its section", got)
	}
	if got := string(scratch.Read(t, doc.sources["workspace"])); got != workspaceText {
		t.Fatalf("workspace source = %q, want it untouched", got)
	}
}

// A project copy that comes back some other way ends the special case.
func TestProjectCopyCreatedBehindTheMountIsUsed(t *testing.T) {
	doc := mountMarkedDocument(t, "workspace")
	if err := os.Remove(doc.file); err != nil {
		t.Fatal(err)
	}
	if err := doc.view.Project().WriteFile("AGENTS.md", []byte("# Checked out\n"), 0644); err != nil {
		t.Fatal(err)
	}

	want := "# Checked out\n" + section("workspace", workspaceText)
	if got := string(scratch.Read(t, doc.file)); got != want {
		t.Fatalf("document = %q, want %q", got, want)
	}
}

// A merged file reports one link per layer that contributes to it. Vim, with
// its default settings, writes a file that has more than one link in place
// instead of removing it and writing a new one.
func TestMergedFileReportsOneLinkPerLayer(t *testing.T) {
	doc := mountMarkedDocument(t, "workspace", "alpha")
	scratch.Write(t, filepath.Join(filepath.Dir(doc.sources["alpha"]), "alone.md"), "alone\n")
	if err := doc.view.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	links := func(info os.FileInfo) uint64 { return uint64(info.Sys().(*syscall.Stat_t).Nlink) }

	merged, err := os.Stat(doc.file)
	if err != nil {
		t.Fatal(err)
	}
	if links(merged) != 3 {
		t.Errorf("file joined from three layers reports %d links, want 3", links(merged))
	}
	alone, err := os.Stat(filepath.Join(filepath.Dir(doc.file), "alone.md"))
	if err != nil {
		t.Fatal(err)
	}
	if links(alone) != 1 {
		t.Errorf("file from one layer reports %d links, want 1", links(alone))
	}
	// An open file answers for itself, and must agree.
	f, err := os.OpenFile(doc.file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() // nothing was written
	open, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if links(open) != 3 {
		t.Errorf("open merged file reports %d links, want 3", links(open))
	}
	if err := f.Chmod(0600); err != nil {
		t.Fatal(err)
	}
	if open, err = f.Stat(); err != nil || links(open) != 3 {
		t.Errorf("open merged file reports %d links after a mode change (%v), want 3", links(open), err)
	}
}

// Text written after a section's end marker has no markers of its own. It
// joins the section it follows, so a line appended to the document reaches
// the last overlay instead of being dropped.
func TestTextAfterASectionJoinsIt(t *testing.T) {
	doc := mountMarkedDocument(t, "workspace", "alpha")
	f, err := os.OpenFile(doc.file, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("- appended\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := string(scratch.Read(t, doc.sources["alpha"])); got != alphaText+"- appended\n" {
		t.Fatalf("alpha source = %q, want the appended line at its end", got)
	}

	between := projectText + section("workspace", workspaceText) + "- between\n" + section("alpha", alphaText+"- appended\n")
	scratch.Write(t, doc.file, between)

	if got := string(scratch.Read(t, doc.sources["workspace"])); got != workspaceText+"- between\n" {
		t.Errorf("workspace source = %q, want the line that followed its section", got)
	}
	if got := string(scratch.Read(t, doc.sources["alpha"])); got != alphaText+"- appended\n" {
		t.Errorf("alpha source = %q, want it unchanged", got)
	}
	if got, _ := doc.projectCopy(t); got != projectText {
		t.Errorf("project copy = %q, want it unchanged", got)
	}
}

// The built-in rules mark common text formats by their file names.
func TestDefaultMarkerRulesMatchFileNames(t *testing.T) {
	v := &View{}
	v.SetMarkers(DefaultMarkerRules())
	tests := []struct {
		name, start string
	}{
		{"AGENTS.md", "<!-- BEGIN"},
		{"docs/nested/notes.md", "<!-- BEGIN"},
		{"index.html", "<!-- BEGIN"},
		{"config.yaml", "# BEGIN"},
		{".gitignore", "# BEGIN"},
		{".env.local", "# BEGIN"},
		{"Dockerfile", "# BEGIN"},
		{"deploy/Dockerfile.dev", "# BEGIN"},
		{"Makefile", "# BEGIN"},
		{"main.go", "// BEGIN"},
		{"notes.txt", ""},
		{"data.json", ""},
	}
	for _, tt := range tests {
		rule := v.findMarkerRule(tt.name)
		switch {
		case tt.start == "" && rule != nil:
			t.Errorf("%s is marked with %q, want no markers", tt.name, rule.Start)
		case tt.start != "" && (rule == nil || !strings.HasPrefix(rule.Start, tt.start)):
			t.Errorf("%s: rule = %+v, want markers starting with %q", tt.name, rule, tt.start)
		}
	}
}

// saveByRename replaces file with content the way programs do that save
// atomically: they write a temporary file beside it and rename that over it.
func saveByRename(t *testing.T, file, content string) error {
	t.Helper()
	staged := scratch.Write(t, file+".tmp", content)
	return os.Rename(staged, file)
}

// A file renamed over a marked document is that document, saved whole. Each
// section goes to its own source, as it does when the document is written in
// place. Before, everything after the project's text, marker lines included,
// was written into an overlay source.
func TestRenameOverMarkedDocumentSavesEverySectionToItsSource(t *testing.T) {
	tests := []struct {
		name     string
		overlays []string
		edit     string
	}{
		{"one overlay, project section edited", []string{"workspace"}, "project"},
		{"one overlay, overlay section edited", []string{"workspace"}, "workspace"},
		{"two overlays, project section edited", []string{"workspace", "alpha"}, "project"},
		{"two overlays, first overlay edited", []string{"workspace", "alpha"}, "workspace"},
		{"two overlays, last overlay edited", []string{"workspace", "alpha"}, "alpha"},
		{"two overlays, nothing edited", []string{"workspace", "alpha"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mountMarkedDocument(t, tt.overlays...)
			want := map[string]string{"project": projectText, "workspace": workspaceText, "alpha": alphaText}
			if tt.edit != "" {
				want[tt.edit] += "- added while editing\n"
			}
			content := want["project"]
			for _, name := range tt.overlays {
				content += section(name, want[name])
			}

			if err := saveByRename(t, doc.file, content); err != nil {
				t.Fatalf("rename over the document = %v", err)
			}

			if got, _ := doc.projectCopy(t); got != want["project"] {
				t.Errorf("project copy = %q, want %q", got, want["project"])
			}
			for _, name := range tt.overlays {
				if got := string(scratch.Read(t, doc.sources[name])); got != want[name] {
					t.Errorf("%s source = %q, want %q", name, got, want[name])
				}
			}
			if got := string(scratch.Read(t, doc.file)); got != content {
				t.Errorf("document = %q, want %q", got, content)
			}
			if _, err := os.Lstat(doc.file + ".tmp"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("temporary file was left behind: %v", err)
			}
		})
	}
}

// A file without the document's markers cannot be split into its sections,
// so it is refused and nothing is changed.
func TestRenameOverMarkedDocumentWithoutMarkersIsRefused(t *testing.T) {
	doc := mountMarkedDocument(t, "workspace", "alpha")

	err := saveByRename(t, doc.file, projectText+section("workspace", workspaceText)+"no alpha section\n")

	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("rename of a document that lost a section = %v, want EPERM", err)
	}
	if got, _ := doc.projectCopy(t); got != projectText {
		t.Errorf("project copy = %q, want it unchanged", got)
	}
	for name, text := range map[string]string{"workspace": workspaceText, "alpha": alphaText} {
		if got := string(scratch.Read(t, doc.sources[name])); got != text {
			t.Errorf("%s source = %q, want it unchanged", name, got)
		}
	}
}

// The project section of a document whose project copy was removed takes a
// renamed-over save as well.
func TestRenameOverDocumentWithRemovedProjectCopy(t *testing.T) {
	doc := mountMarkedDocument(t, "workspace")
	if err := os.Remove(doc.file); err != nil {
		t.Fatal(err)
	}

	if err := saveByRename(t, doc.file, "# Back\n"+section("workspace", workspaceText+"- edited\n")); err != nil {
		t.Fatalf("rename over the document = %v", err)
	}

	if got, _ := doc.projectCopy(t); got != "# Back\n" {
		t.Errorf("project copy = %q, want the text of its section", got)
	}
	if got := string(scratch.Read(t, doc.sources["workspace"])); got != workspaceText+"- edited\n" {
		t.Errorf("workspace source = %q, want its edited section", got)
	}
}

// git runs Git in the mounted project and returns its output.
func (d markedDocument) git(t *testing.T, args ...string) string {
	t.Helper()
	identity := []string{"-C", filepath.Dir(d.file), "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}
	out, err := exec.Command("git", append(identity, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// Git replaces a file by removing it and creating it again. It is shown only
// the project's files, so the overlay's copy must not stand in its way.
// Before, the removal went through and the creation was refused, which left
// the project copy deleted.
func TestGitRestoresFileThatAnOverlayAlsoHas(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, doc markedDocument)
	}{
		{"modified", func(t *testing.T, doc markedDocument) {
			scratch.Write(t, doc.file, "# Changed\n"+section("workspace", workspaceText))
		}},
		{"removed", func(t *testing.T, doc markedDocument) {
			if err := os.Remove(doc.file); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mountMarkedDocument(t, "workspace")
			doc.git(t, "init", "--quiet", "--initial-branch=main")
			doc.git(t, "add", "AGENTS.md")
			doc.git(t, "commit", "--quiet", "-m", "project file")
			tt.change(t, doc)
			if status := doc.git(t, "status", "--porcelain"); status == "" {
				t.Fatal("Git does not see the change the test made")
			}

			doc.git(t, "checkout", "--", "AGENTS.md")

			if got, _ := doc.projectCopy(t); got != projectText {
				t.Errorf("project copy = %q, want the committed text", got)
			}
			if status := doc.git(t, "status", "--porcelain"); status != "" {
				t.Errorf("git status after the checkout = %q, want it clean", status)
			}
			if got, want := string(scratch.Read(t, doc.file)), projectText+section("workspace", workspaceText); got != want {
				t.Errorf("document = %q, want %q", got, want)
			}
			if got := string(scratch.Read(t, doc.sources["workspace"])); got != workspaceText {
				t.Errorf("workspace source = %q, want it untouched", got)
			}
		})
	}
}

// Opening a file with O_TRUNC, as a shell redirection and most programs that
// write a whole file do, empties it before the new content arrives. Content
// shorter than the old must not be saved with the end of the old content.
func TestShorterContentReplacesMergedFile(t *testing.T) {
	t.Run("marked", func(t *testing.T) {
		doc := mountMarkedDocument(t, "workspace")
		content := "# P\n" + section("workspace", "# W\n")

		scratch.Write(t, doc.file, content)

		if got, _ := doc.projectCopy(t); got != "# P\n" {
			t.Errorf("project copy = %q, want %q", got, "# P\n")
		}
		if got := string(scratch.Read(t, doc.sources["workspace"])); got != "# W\n" {
			t.Errorf("workspace source = %q, want %q", got, "# W\n")
		}
		if got := string(scratch.Read(t, doc.file)); got != content {
			t.Errorf("document = %q, want %q", got, content)
		}
	})
	t.Run("joined without markers", func(t *testing.T) {
		doc := mountMarkedDocument(t, "workspace")
		dir := filepath.Dir(doc.file)
		if err := doc.view.Project().WriteFile("notes.txt", []byte("project\n"), 0644); err != nil {
			t.Fatal(err)
		}
		source := scratch.Write(t, filepath.Join(filepath.Dir(doc.sources["workspace"]), "notes.txt"), "a long overlay contribution\n")
		if err := doc.view.RefreshPaths(); err != nil {
			t.Fatal(err)
		}

		scratch.Write(t, filepath.Join(dir, "notes.txt"), "project\nshort\n")

		if got := string(scratch.Read(t, source)); got != "short\n" {
			t.Errorf("overlay source = %q, want %q", got, "short\n")
		}
		if got := string(scratch.Read(t, filepath.Join(dir, "notes.txt"))); got != "project\nshort\n" {
			t.Errorf("file = %q, want %q", got, "project\nshort\n")
		}
	})
}
