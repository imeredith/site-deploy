package hostpaths

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// tempDir returns a temporary directory named without symlinks, as rules must be.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParse(t *testing.T) {
	for value, want := range map[string]Rule{
		"/srv/media":             {Path: "/srv/media"},
		"/srv/media:ro":          {Path: "/srv/media"},
		"/srv/uploads:rw":        {Path: "/srv/uploads", Writable: true},
		"app=/srv/app":           {Path: "/srv/app", Apps: []string{"app"}},
		"a,b-2,a=/srv/shared:rw": {Path: "/srv/shared", Writable: true, Apps: []string{"a", "b-2"}},
		"/srv/a=b":               {Path: "/srv/a=b"},
	} {
		got, err := Parse(value)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", value, got, err, want)
		}
	}
	for _, value := range []string{
		"", "srv/media", "./media", "app=", "app=srv", "=/srv", "App=/srv", "a,,b=/srv",
		"/srv/media/", "/srv/../etc", "/srv/./media", "/srv/media:rx", "/srv/a:b", "/srv/$HOME", `/srv\x`,
	} {
		if rule, err := Parse(value); err == nil {
			t.Errorf("Parse(%q) accepted: %+v", value, rule)
		}
	}
	rule, _ := Parse("a,b=/srv/x:rw")
	if rule.String() != "a,b=/srv/x:rw" {
		t.Errorf("String() = %q", rule.String())
	}
}

func TestValidate(t *testing.T) {
	root := tempDir(t)
	data := filepath.Join(root, "data")
	media := filepath.Join(root, "media")
	run := filepath.Join(root, "run")
	for _, dir := range []string{data, media, run} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(root, "file")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(media, link); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(run, "docker.sock")
	if err := (Policy{{Path: media}}).Validate(data, socket); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(root, "missing"): "no such file",
		file:                           "must be a directory",
		link:                           "use " + media,
		root:                           "overlaps the data directory",
		filepath.Join(data):            "overlaps the data directory",
		run:                            "Docker socket",
		"/":                            "overlaps the data directory",
	} {
		err := (Policy{{Path: media}, {Path: path}}).Validate(data, socket)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %v; want %q", path, err, want)
		}
	}
	inside := filepath.Join(data, "apps")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := (Policy{{Path: inside}}).Validate(data, ""); err == nil {
		t.Error("accepted a path inside the data directory")
	}
}

func TestCheck(t *testing.T) {
	root := tempDir(t)
	media := filepath.Join(root, "media")
	for _, dir := range []string{media, filepath.Join(media, "films"), filepath.Join(root, "media2"), filepath.Join(root, "uploads")} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(media, "list.txt"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(media, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(media, "pipe"), 0644); err != nil {
		t.Fatal(err)
	}
	policy := Policy{
		{Path: media},
		{Path: filepath.Join(root, "uploads"), Writable: true, Apps: []string{"uploader"}},
		{Path: media, Writable: true, Apps: []string{"curator"}},
	}
	allowed := []struct {
		app, source string
		readOnly    bool
		want        string
	}{
		{"web", media, true, media},
		{"web", media + "/films/", true, media + "/films"},
		{"web", media + "//list.txt", true, media + "/list.txt"},
		{"curator", media + "/films", false, media + "/films"},
		{"uploader", root + "/uploads", false, root + "/uploads"},
		{"uploader", media, true, media},
	}
	for _, c := range allowed {
		got, err := policy.Check(c.app, c.source, c.readOnly)
		if err != nil || got != c.want {
			t.Errorf("Check(%s, %s, ro=%v) = %q, %v; want %q", c.app, c.source, c.readOnly, got, err, c.want)
		}
	}
	refused := []struct {
		app, source string
		readOnly    bool
		want        string
	}{
		{"web", media, false, "read-only"},
		{"web", root + "/media2", true, "not allowed for web (allowed: " + media + " read-only)"},
		{"web", root + "/uploads", true, "not allowed"},
		{"web", root, true, "not allowed"},
		{"web", "/etc", true, "not allowed"},
		{"web", media + "/escape", true, "symlink"},
		{"web", media + "/escape/passwd", true, "symlink"},
		{"web", media + "/pipe", true, "regular file or directory"},
		{"web", media + "/missing", true, "no such file"},
		{"web", media + "/../media2", true, "'..'"},
		{"web", "media", true, "absolute"},
		{"web", media + ":x", true, "absolute"},
	}
	for _, c := range refused {
		_, err := policy.Check(c.app, c.source, c.readOnly)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Check(%s, %s, ro=%v) = %v; want %q", c.app, c.source, c.readOnly, err, c.want)
		}
	}
	if _, err := Policy(nil).Check("web", media, true); err == nil || !strings.Contains(err.Error(), "allows it no host paths") {
		t.Errorf("empty policy: %v", err)
	}
}

func TestOverlappingFindsRulesAboveAndBelow(t *testing.T) {
	policy := Policy{{Path: "/srv/media"}, {Path: "/data/volumes/corpus"}}
	for path, want := range map[string]string{
		"/srv/media":       "/srv/media",
		"/srv/media/films": "/srv/media",
		"/data/volumes":    "/data/volumes/corpus",
		"/data":            "/data/volumes/corpus",
		"/srv/mediaother":  "",
		"/data/other":      "",
	} {
		rule, found := policy.Overlapping(path)
		if found != (want != "") || rule.Path != want {
			t.Errorf("Overlapping(%s) = %v %v; want %q", path, rule, found, want)
		}
	}
}
