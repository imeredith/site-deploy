// Package archive extracts deployment bundles into empty staging directories.
package archive

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits bounds the work and disk space used by an expanded deployment bundle.
// MaxFiles counts both ZIP entries and their implicit parent directories.
// Zero-valued fields use DefaultLimits; negative values are invalid.
type Limits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// DefaultLimits permits up to 10,000 paths, 128 MiB per file, and 512 MiB total.
func DefaultLimits() Limits {
	return Limits{MaxFiles: 10_000, MaxFileBytes: 128 << 20, MaxTotalBytes: 512 << 20}
}

type entry struct {
	file *zip.File
	name string
	dir  bool
}

type node struct {
	dir      bool
	explicit bool
}

// Extract expands a ZIP into an existing, empty directory. It rejects unsafe
// names, links, special files, conflicting paths, and excessive expanded data.
// File permissions are 0644 plus the source executable bits; directories are
// 0755. Destination permissions are preserved. On an extraction error callers
// should remove the staging directory, which may contain partial output.
// The caller must prevent concurrent modification of its staging directory.
func Extract(zipPath, destination string, limits Limits) error {
	limits, err := normalizedLimits(limits)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve extraction destination: %w", err)
	}
	if err := emptyDirectory(root); err != nil {
		return err
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open deployment ZIP: %w", err)
	}
	defer zr.Close()

	entries, err := validate(zr.File, limits)
	if err != nil {
		return err
	}
	remaining := limits.MaxTotalBytes
	buffer := make([]byte, 32*1024)
	for _, item := range entries {
		if item.dir {
			if err := verifyEmptyDirectory(item.file, buffer); err != nil {
				return fmt.Errorf("verify directory %q: %w", item.name, err)
			}
			if err := makeDirectories(root, item.name); err != nil {
				return fmt.Errorf("create directory %q: %w", item.name, err)
			}
			continue
		}
		if err := makeDirectories(root, path.Dir(item.name)); err != nil {
			return fmt.Errorf("create parent directory for %q: %w", item.name, err)
		}
		limit := limits.MaxFileBytes
		if remaining < limit {
			limit = remaining
		}
		n, err := extractFile(item, filepath.Join(root, filepath.FromSlash(item.name)), limit, buffer)
		if err != nil {
			return fmt.Errorf("extract %q: %w", item.name, err)
		}
		remaining -= n
	}
	return nil
}

func normalizedLimits(limits Limits) (Limits, error) {
	if limits.MaxFiles < 0 || limits.MaxFileBytes < 0 || limits.MaxTotalBytes < 0 {
		return Limits{}, errors.New("archive limits must not be negative")
	}
	defaults := DefaultLimits()
	if limits.MaxFiles == 0 {
		limits.MaxFiles = defaults.MaxFiles
	}
	if limits.MaxFileBytes == 0 {
		limits.MaxFileBytes = defaults.MaxFileBytes
	}
	if limits.MaxTotalBytes == 0 {
		limits.MaxTotalBytes = defaults.MaxTotalBytes
	}
	return limits, nil
}

func emptyDirectory(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect extraction destination: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("extraction destination must be a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read extraction destination: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("extraction destination must be empty")
	}
	return nil
}

func validate(files []*zip.File, limits Limits) ([]entry, error) {
	if len(files) > limits.MaxFiles {
		return nil, fmt.Errorf("archive exceeds the %d path limit", limits.MaxFiles)
	}
	entries := make([]entry, 0, len(files))
	nodes := make(map[string]node, len(files))
	remaining := uint64(limits.MaxTotalBytes)
	for _, file := range files {
		name, err := safeName(file.Name)
		if err != nil {
			return nil, err
		}
		mode := file.Mode()
		if mode.Type()&^fs.ModeDir != 0 {
			return nil, fmt.Errorf("archive entry %q is a link or special file", file.Name)
		}
		dir := mode.IsDir()
		if dir && file.UncompressedSize64 != 0 {
			return nil, fmt.Errorf("archive directory %q contains data", file.Name)
		}
		// archive/zip skips checksum verification for names ending in '/'.
		// An empty directory's CRC must still be the empty-content CRC, zero.
		if dir && file.CRC32 != 0 {
			return nil, fmt.Errorf("archive directory %q: %w", file.Name, zip.ErrChecksum)
		}
		if !dir {
			if file.UncompressedSize64 > uint64(limits.MaxFileBytes) {
				return nil, fmt.Errorf("archive file %q exceeds the %d byte file limit", file.Name, limits.MaxFileBytes)
			}
			if file.UncompressedSize64 > remaining {
				return nil, fmt.Errorf("archive exceeds the %d byte expanded limit", limits.MaxTotalBytes)
			}
			remaining -= file.UncompressedSize64
		}

		// Record parents as well, so deeply nested names cannot evade the path
		// limit and a regular file cannot later become a parent directory.
		components := strings.Split(name, "/")
		if len(components) > limits.MaxFiles {
			return nil, fmt.Errorf("archive exceeds the %d path limit", limits.MaxFiles)
		}
		prefix := ""
		for index, component := range components {
			if index == 0 {
				prefix = component
			} else {
				prefix += "/" + component
			}
			leaf := index == len(components)-1
			previous, exists := nodes[prefix]
			if exists {
				if leaf && previous.explicit {
					return nil, fmt.Errorf("archive contains duplicate path %q", prefix)
				}
				if !previous.dir || (leaf && !dir) {
					return nil, fmt.Errorf("archive contains a file-directory conflict at %q", prefix)
				}
			}
			if !exists && len(nodes) >= limits.MaxFiles {
				return nil, fmt.Errorf("archive exceeds the %d path limit", limits.MaxFiles)
			}
			if !exists || leaf {
				nodes[prefix] = node{dir: !leaf || dir, explicit: leaf}
			}
		}
		entries = append(entries, entry{file: file, name: name, dir: dir})
	}
	return entries, nil
}

func safeName(name string) (string, error) {
	// Reject Windows separators and drive-qualified names even on Unix. The
	// length cap also bounds the memory required to account for parent paths.
	unsafe := name == "" || len(name) > 4096 || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name)
	for _, component := range strings.Split(name, "/") {
		if component == ".." {
			unsafe = true
		}
	}
	clean := path.Clean(name)
	if len(clean) >= 2 && clean[1] == ':' && ((clean[0] >= 'a' && clean[0] <= 'z') || (clean[0] >= 'A' && clean[0] <= 'Z')) {
		unsafe = true
	}
	if unsafe || clean == "." {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return clean, nil
}

func makeDirectories(root, relative string) error {
	if relative == "." {
		return nil
	}
	current := root
	for _, component := range strings.Split(relative, "/") {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0755); err != nil {
				return err
			}
			if err := os.Chmod(current, 0755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %q is not a real directory", current)
		}
	}
	return nil
}

func verifyEmptyDirectory(file *zip.File, buffer []byte) error {
	source, err := file.Open()
	if err != nil {
		return err
	}
	defer source.Close()
	_, err = io.CopyBuffer(&boundedWriter{writer: io.Discard}, source, buffer)
	return err
}

func extractFile(item entry, target string, limit int64, buffer []byte) (int64, error) {
	source, err := item.file.Open()
	if err != nil {
		return 0, err
	}
	defer source.Close()
	permission := fs.FileMode(0644) | item.file.Mode().Perm()&0111
	// O_EXCL prevents replacing existing files or following a destination link.
	destination, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, permission)
	if err != nil {
		return 0, err
	}
	writer := &boundedWriter{writer: destination, remaining: limit}
	n, copyErr := io.CopyBuffer(writer, source, buffer)
	if copyErr == nil {
		copyErr = destination.Chmod(permission)
	}
	closeErr := destination.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return n, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return n, closeErr
	}
	return n, nil
}

type boundedWriter struct {
	writer    io.Writer
	remaining int64
}

func (writer *boundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.remaining {
		return 0, errors.New("archive expanded data exceeds extraction limit")
	}
	n, err := writer.writer.Write(data)
	writer.remaining -= int64(n)
	return n, err
}
