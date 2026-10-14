package archive

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureEntry struct {
	name   string
	data   string
	mode   fs.FileMode
	method uint16
}

func fixtureZIP(t *testing.T, entries ...fixtureEntry) string {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: entry.method}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "deployment.zip")
	if err := os.WriteFile(name, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestExtractBundleAndPermissions(t *testing.T) {
	input := fixtureZIP(t,
		fixtureEntry{name: "compose.yaml", data: "services: {}\n", mode: 0666},
		fixtureEntry{name: "./public/index.html", data: "hello\x00world", mode: 0666},
		fixtureEntry{name: "public/", mode: fs.ModeDir | 0777},
		fixtureEntry{name: "bin/start", data: "#!/bin/sh\n", mode: fs.ModeSetuid | fs.ModeSetgid | 0777},
	)
	destination := t.TempDir()
	if err := os.Chmod(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Extract(input, destination, Limits{}); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name string
		data string
		mode fs.FileMode
	}{
		{name: "compose.yaml", data: "services: {}\n", mode: 0644},
		{name: "public/index.html", data: "hello\x00world", mode: 0644},
		{name: "bin/start", data: "#!/bin/sh\n", mode: 0755},
	} {
		content, err := os.ReadFile(filepath.Join(destination, check.name))
		if err != nil || string(content) != check.data {
			t.Errorf("%s content = %q, error = %v", check.name, content, err)
		}
		info, err := os.Stat(filepath.Join(destination, check.name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != check.mode {
			t.Errorf("%s mode = %v, want %v", check.name, info.Mode(), check.mode)
		}
	}
	for name, want := range map[string]fs.FileMode{".": 0700, "public": 0755, "bin": 0755} {
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s permission = %v, want %v", name, info.Mode().Perm(), want)
		}
	}
}

func TestRejectUnsafePathsBeforeWriting(t *testing.T) {
	for _, name := range []string{
		"../escape", "nested/../../escape", "nested/../escape", "/absolute",
		"\\absolute", "nested\\file", "C:/escape", "C:escape", "./C:/escape", "", ".", "./",
		"nul\x00byte", strings.Repeat("a", 4097),
	} {
		t.Run(name, func(t *testing.T) {
			input := fixtureZIP(t, fixtureEntry{name: "safe", data: "safe"}, fixtureEntry{name: name})
			destination := t.TempDir()
			if err := Extract(input, destination, Limits{}); err == nil {
				t.Fatal("accepted unsafe archive path")
			}
			assertEmpty(t, destination)
		})
	}
}

func TestRejectLinksAndSpecialFiles(t *testing.T) {
	for _, mode := range []fs.FileMode{fs.ModeSymlink, fs.ModeNamedPipe, fs.ModeSocket, fs.ModeDevice, fs.ModeDevice | fs.ModeCharDevice} {
		t.Run(mode.String(), func(t *testing.T) {
			input := fixtureZIP(t, fixtureEntry{name: "unsafe", data: "target", mode: mode | 0777})
			destination := t.TempDir()
			if err := Extract(input, destination, Limits{}); err == nil {
				t.Fatal("accepted link or special file")
			}
			assertEmpty(t, destination)
		})
	}
}

func TestRejectDuplicateAndConflictingPaths(t *testing.T) {
	for _, entries := range [][]fixtureEntry{
		{{name: "file"}, {name: "file"}},
		{{name: "file"}, {name: "./file"}},
		{{name: "a//file"}, {name: "a/file"}},
		{{name: "a/"}, {name: "a/"}},
		{{name: "a"}, {name: "a/file"}},
		{{name: "a/file"}, {name: "a"}},
		{{name: "a/"}, {name: "a"}},
		{{name: "a"}, {name: "a/"}},
	} {
		t.Run(entries[0].name+"_"+entries[1].name, func(t *testing.T) {
			destination := t.TempDir()
			if err := Extract(fixtureZIP(t, entries...), destination, Limits{}); err == nil {
				t.Fatal("accepted duplicate or conflicting paths")
			}
			assertEmpty(t, destination)
		})
	}
}

func TestEnforceExtractionLimits(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries []fixtureEntry
		limits  Limits
	}{
		{name: "file size", entries: []fixtureEntry{{name: "file", data: "12345"}}, limits: Limits{MaxFileBytes: 4}},
		{name: "total size", entries: []fixtureEntry{{name: "one", data: "123"}, {name: "two", data: "456"}}, limits: Limits{MaxTotalBytes: 5}},
		{name: "file count", entries: []fixtureEntry{{name: "one"}, {name: "two"}}, limits: Limits{MaxFiles: 1}},
		{name: "implicit directories", entries: []fixtureEntry{{name: "a/b/file"}}, limits: Limits{MaxFiles: 2}},
		{name: "negative files", limits: Limits{MaxFiles: -1}},
		{name: "negative size", limits: Limits{MaxFileBytes: -1}},
		{name: "negative total", limits: Limits{MaxTotalBytes: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := t.TempDir()
			if err := Extract(fixtureZIP(t, test.entries...), destination, test.limits); err == nil {
				t.Fatal("accepted archive exceeding limits")
			}
			assertEmpty(t, destination)
		})
	}
	// Exact limits must permit complete reads, including EOF checksum checking.
	input := fixtureZIP(t, fixtureEntry{name: "a/b/file", data: "12345"})
	if err := Extract(input, t.TempDir(), Limits{MaxFiles: 3, MaxFileBytes: 5, MaxTotalBytes: 5}); err != nil {
		t.Fatalf("exact limit: %v", err)
	}
}

func TestRejectCorruptContents(t *testing.T) {
	for _, corruption := range []string{"checksum", "declared size", "directory checksum"} {
		t.Run(corruption, func(t *testing.T) {
			entry := fixtureEntry{name: "file", data: strings.Repeat("x", 128*1024), method: zip.Deflate}
			if corruption == "directory checksum" {
				entry = fixtureEntry{name: "directory/"}
			}
			input := fixtureZIP(t, entry)
			data, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			limits := Limits{}
			if corruption == "checksum" {
				reader, err := zip.OpenReader(input)
				if err != nil {
					t.Fatal(err)
				}
				offset, err := reader.File[0].DataOffset()
				reader.Close()
				if err != nil {
					t.Fatal(err)
				}
				data[offset] ^= 0xff
			} else {
				index := bytes.Index(data, []byte("PK\x01\x02"))
				if index < 0 {
					t.Fatal("missing central directory")
				}
				if corruption == "directory checksum" {
					binary.LittleEndian.PutUint32(data[index+16:index+20], 1)
				} else {
					binary.LittleEndian.PutUint32(data[index+24:index+28], 4)
					limits.MaxFileBytes = 8
					limits.MaxTotalBytes = 8
				}
			}
			if err := os.WriteFile(input, data, 0600); err != nil {
				t.Fatal(err)
			}
			destination := t.TempDir()
			if err := Extract(input, destination, limits); err == nil {
				t.Fatal("accepted corrupt archive contents")
			}
			assertEmpty(t, destination)
		})
	}
}

func TestRequireEmptyRealDestination(t *testing.T) {
	input := fixtureZIP(t, fixtureEntry{name: "file", data: "contents"})
	t.Run("nonempty", func(t *testing.T) {
		destination := t.TempDir()
		sentinel := filepath.Join(destination, "sentinel")
		if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Extract(input, destination, Limits{}); err == nil {
			t.Fatal("accepted nonempty destination")
		}
		content, err := os.ReadFile(sentinel)
		if err != nil || string(content) != "preserve" {
			t.Fatalf("destination modified: %q, %v", content, err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := Extract(input, link, Limits{}); err == nil {
			t.Fatal("accepted symlink destination")
		}
		assertEmpty(t, target)
	})
	t.Run("missing", func(t *testing.T) {
		if err := Extract(input, filepath.Join(t.TempDir(), "missing"), Limits{}); err == nil {
			t.Fatal("accepted missing destination")
		}
	})
	t.Run("regular file", func(t *testing.T) {
		if err := Extract(input, input, Limits{}); err == nil {
			t.Fatal("accepted regular file destination")
		}
	})
}

func assertEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("destination contains %d entries after rejected extraction", len(entries))
	}
}
