// Package media handles durable files and the controlled FFmpeg process boundary.
package media

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
)

const MaxFragmentBytes = 24 << 20

var ErrTooLarge = errors.New("media exceeds size limit")
var keyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type Files struct {
	root *os.Root
	path string
}

func OpenFiles(folder string) (*Files, error) {
	path, err := filepath.Abs(folder)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	files := &Files{root: root, path: path}
	for _, name := range []string{"media", "work"} {
		if err := root.MkdirAll(name, 0o750); err != nil {
			root.Close()
			return nil, err
		}
	}
	return files, nil
}

func (f *Files) Close() error { return f.root.Close() }

func relative(key string) (string, error) {
	if !keyPattern.MatchString(key) {
		return "", errors.New("invalid media file identifier")
	}
	return filepath.Join("media", key[:2], key), nil
}

func (f *Files) Save(r io.Reader, limit int64) (catalog.Blob, error) {
	random := make([]byte, 16)
	rand.Read(random)
	blob := catalog.Blob{Key: hex.EncodeToString(random)}
	path, _ := relative(blob.Key)
	directory := filepath.Dir(path)
	if err := f.root.MkdirAll(directory, 0o750); err != nil {
		return catalog.Blob{}, err
	}
	temporary := path + ".part"
	file, err := f.root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return catalog.Blob{}, err
	}
	defer f.root.Remove(temporary)
	sum := sha256.New()
	size, writeErr := io.Copy(io.MultiWriter(sum, file), io.LimitReader(r, limit+1))
	if writeErr == nil && size > limit {
		writeErr = ErrTooLarge
	}
	if writeErr == nil && size == 0 {
		writeErr = errors.New("empty media file")
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return catalog.Blob{}, err
	}
	if err := f.root.Rename(temporary, path); err != nil {
		return catalog.Blob{}, err
	}
	if err := f.syncDirectory(directory); err != nil {
		return catalog.Blob{}, err
	}
	if err := f.syncDirectory("media"); err != nil {
		return catalog.Blob{}, err
	}
	blob.Bytes = size
	copy(blob.Digest[:], sum.Sum(nil))
	return blob, nil
}

func (f *Files) syncDirectory(path string) error {
	// Windows flushes the file before rename. Go does not provide a Windows directory flush.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := f.root.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (f *Files) SavePath(path string, limit int64) (catalog.Blob, error) {
	file, err := os.Open(path)
	if err != nil {
		return catalog.Blob{}, err
	}
	defer file.Close()
	return f.Save(file, limit)
}

func (f *Files) Open(blob catalog.Blob) (*os.File, error) {
	path, err := relative(blob.Key)
	if err != nil {
		return nil, err
	}
	file, err := f.root.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != blob.Bytes {
		file.Close()
		return nil, fmt.Errorf("media size or type mismatch for %s", blob.Key)
	}
	return file, nil
}

func (f *Files) Available(blob catalog.Blob) bool {
	file, err := f.Open(blob)
	if err != nil {
		return false
	}
	file.Close()
	return true
}

func (f *Files) Path(blob catalog.Blob, verify bool) (string, error) {
	file, err := f.Open(blob)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if verify {
		sum := sha256.New()
		if _, err := io.Copy(sum, file); err != nil {
			return "", err
		}
		var actual [32]byte
		copy(actual[:], sum.Sum(nil))
		if blob.Digest != actual {
			return "", fmt.Errorf("media checksum mismatch for %s", blob.Key)
		}
	}
	path, _ := relative(blob.Key)
	return filepath.Join(f.path, path), nil
}

func (f *Files) Remove(key string) error {
	path, err := relative(key)
	if err != nil {
		return err
	}
	err = f.root.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.syncDirectory(filepath.Dir(path))
}

func (f *Files) Workspace() (string, func(), error) {
	dir, err := os.MkdirTemp(filepath.Join(f.path, "work"), "job-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

// Orphans returns aged files that may be reconciled with the catalog.
// Include incomplete writes, which cannot have a committed database reference.
func (f *Files) Orphans(before time.Time, visit func(key string, incomplete bool) error) error {
	return fs.WalkDir(f.root.FS(), "media", func(path string, dir fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !dir.Type().IsRegular() {
			return nil
		}
		name := dir.Name()
		key, incomplete := name, false
		if filepath.Ext(name) == ".part" {
			key, incomplete = name[:len(name)-5], true
		}
		expected, err := relative(key)
		if err != nil || filepath.ToSlash(expected) != path && filepath.ToSlash(expected)+".part" != path {
			return nil
		}
		info, err := dir.Info()
		if err != nil || !info.ModTime().Before(before) {
			return err
		}
		return visit(key, incomplete)
	})
}

func (f *Files) RemoveIncomplete(key string) error {
	path, err := relative(key)
	if err != nil {
		return err
	}
	err = f.root.Remove(path + ".part")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
