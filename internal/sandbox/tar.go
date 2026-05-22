/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sandbox

import (
	"archive/tar"
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"
)

// needsTar reports whether the Files map spans subdirectories. Any
// key containing "/" forces the tar path because the agent-sandbox
// client's Write doesn't accept path separators.
func needsTar(files map[string][]byte) bool {
	for name := range files {
		if strings.Contains(name, "/") {
			return true
		}
	}
	return false
}

// buildTar packs files into an in-memory POSIX tar archive. Keys are
// used as relative paths within the archive. Parent directory entries
// are emitted before their contained files. Keys are sorted for
// deterministic output — helps when an agent diff-checks repeated
// builds.
func buildTar(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		if err := validatePath(n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	mtime := time.Unix(0, 0)
	written := map[string]bool{}

	for _, name := range names {
		for _, dir := range parentDirs(name) {
			if written[dir] {
				continue
			}
			if err := w.WriteHeader(&tar.Header{
				Name:     dir + "/",
				Mode:     0o755,
				Typeflag: tar.TypeDir,
				ModTime:  mtime,
			}); err != nil {
				return nil, err
			}
			written[dir] = true
		}
		content := files[name]
		if err := w.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
			ModTime:  mtime,
		}); err != nil {
			return nil, err
		}
		if _, err := w.Write(content); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty file path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("file path %q must be relative", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("file path %q contains invalid segment %q", p, seg)
		}
	}
	return nil
}

func parentDirs(p string) []string {
	parts := strings.Split(p, "/")
	if len(parts) <= 1 {
		return nil
	}
	dirs := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		dirs = append(dirs, strings.Join(parts[:i], "/"))
	}
	return dirs
}
