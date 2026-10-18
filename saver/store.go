package saver

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const tempPrefix = ".tmp-"

type hashed struct {
	size  int64
	mtime time.Time
	sum   [32]byte
}

// folderHashes caches content hashes per folder so a duplicate check does not
// rehash the whole folder; an entry is reused only while size and mtime match.
// Not safe for concurrent use: callers hold Module.mu.
type folderHashes map[string]map[string]hashed

// contains reports whether a file with this hash already lives in dir.
func (h folderHashes) contains(dir string, sum [32]byte) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	old := h[dir]
	fresh := make(map[string]hashed, len(entries))
	found := false
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		ent, ok := old[e.Name()]
		if !ok || ent.size != info.Size() || !ent.mtime.Equal(info.ModTime()) {
			s, err := hashFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			ent = hashed{size: info.Size(), mtime: info.ModTime(), sum: s}
		}
		fresh[e.Name()] = ent
		if ent.sum == sum {
			found = true
		}
	}
	h[dir] = fresh
	return found
}

func (h folderHashes) record(path string, sum [32]byte) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if h[dir] == nil {
		h[dir] = map[string]hashed{}
	}
	h[dir][filepath.Base(path)] = hashed{size: info.Size(), mtime: info.ModTime(), sum: sum}
}

func hashFile(path string) ([32]byte, error) {
	var sum [32]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	hh := sha256.New()
	if _, err := io.Copy(hh, f); err != nil {
		return sum, err
	}
	copy(sum[:], hh.Sum(nil))
	return sum, nil
}

// writeAtomic writes data via a temp file in the same folder and a rename, so
// meme-search and the library scanner never see a half-written image.
func writeAtomic(dir, base, ext string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Names carry a hash prefix, so they collide only with a pre-existing file
	// of that exact name; this loop is a safety net.
	target := filepath.Join(dir, base+"."+ext)
	for i := 2; ; i++ {
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			break
		} else if err != nil {
			return "", err
		}
		target = filepath.Join(dir, base+"_"+strconv.Itoa(i)+"."+ext)
		if i > 1000 {
			return "", fmt.Errorf("no free name for %s", base)
		}
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return "", err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		cleanup()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		cleanup()
		return "", err
	}
	return target, nil
}
