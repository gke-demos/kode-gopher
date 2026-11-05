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

package executor

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func samplePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sampleJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sampleWebP() []byte {
	// Minimal RIFF....WEBPVP8L container header recognized by net/http and detectImageMIME.
	return []byte("RIFF\x14\x00\x00\x00WEBPVP8L\x08\x00\x00\x00\x2f\x00\x00\x00\x00\x00\x00\x00")
}

func runFetchInDir(t *testing.T, dir string) (*Result, string, []Attachment, []string) {
	t.Helper()
	shellBin := "sh"
	if dashPath, err := exec.LookPath("dash"); err == nil {
		// Prefer dash to match debian:bookworm-slim's /bin/sh in the sandbox image.
		shellBin = dashPath
	}
	nonce := rand.Text()
	cmd := exec.Command(shellBin, "-c", fetchCmd(nonce))
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fetchCmd (%s) failed: %v", shellBin, err)
	}
	return parseFetchStdout(string(out), nonce)
}

func TestFetchDashCleanWarningsAndDotfiles(t *testing.T) {
	dir := t.TempDir()
	attDir := filepath.Join(dir, AttachmentsDir)
	if err := os.MkdirAll(attDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pngBytes := samplePNG(t)
	if err := os.WriteFile(filepath.Join(attDir, "chart.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attDir, "..two-dot.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	_, resErr, atts, warnings := runFetchInDir(t, dir)
	if resErr != "" || len(warnings) != 0 {
		t.Fatalf("resErr=%q warnings=%v, want zero warnings under dash", resErr, warnings)
	}
	if len(atts) != 2 || atts[0].Name != "..two-dot.png" || atts[1].Name != "chart.png" {
		t.Fatalf("atts = %+v, want ..two-dot.png and chart.png", atts)
	}
}

func TestFetchNoAttachmentsPreservesResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".kode-gopher"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit trailing newline to verify separator framing doesn't corrupt result.json.
	if err := os.WriteFile(filepath.Join(dir, resultPath), []byte(`{"kind":"ok","value":{"count":3}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, resErr, atts, warnings := runFetchInDir(t, dir)
	if resErr != "" || len(warnings) != 0 || len(atts) != 0 {
		t.Fatalf("resErr=%q warnings=%v atts=%v", resErr, warnings, atts)
	}
	if res == nil || res.Kind != "ok" || string(res.Value) != `{"count":3}` {
		t.Fatalf("res = %+v", res)
	}
}

func TestFetchValidImagesAndSpoofedFormats(t *testing.T) {
	dir := t.TempDir()
	attDir := filepath.Join(dir, AttachmentsDir)
	if err := os.MkdirAll(attDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pngBytes := samplePNG(t)
	jpgBytes := sampleJPEG(t)
	webpBytes := sampleWebP()

	files := map[string][]byte{
		"c-diagram.webp": webpBytes,
		"a-chart.png":    pngBytes,
		"b-photo.jpg":    jpgBytes,
		"d-spoof.png":    []byte("not a png at all"),
		"e-vector.svg":   []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
		"f-empty.png":    {},
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(attDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, resErr, atts, warnings := runFetchInDir(t, dir)
	if resErr != "" {
		t.Fatalf("unexpected resErr: %q", resErr)
	}
	if len(atts) != 3 {
		t.Fatalf("len(atts) = %d, want 3 (warnings=%v)", len(atts), warnings)
	}
	if atts[0].Name != "a-chart.png" || atts[0].MIMEType != "image/png" || !bytes.Equal(atts[0].Data, pngBytes) {
		t.Errorf("atts[0] = %q %q (%d bytes)", atts[0].Name, atts[0].MIMEType, len(atts[0].Data))
	}
	if atts[1].Name != "b-photo.jpg" || atts[1].MIMEType != "image/jpeg" || !bytes.Equal(atts[1].Data, jpgBytes) {
		t.Errorf("atts[1] = %q %q (%d bytes)", atts[1].Name, atts[1].MIMEType, len(atts[1].Data))
	}
	if atts[2].Name != "c-diagram.webp" || atts[2].MIMEType != "image/webp" || !bytes.Equal(atts[2].Data, webpBytes) {
		t.Errorf("atts[2] = %q %q (%d bytes)", atts[2].Name, atts[2].MIMEType, len(atts[2].Data))
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{`"d-spoof.png"`, `"e-vector.svg"`, `"f-empty.png"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %s:\n%s", want, joined)
		}
	}
}

func TestFetchRejectsSymlinksFIFOsAndControlNames(t *testing.T) {
	dir := t.TempDir()
	attDir := filepath.Join(dir, AttachmentsDir)
	if err := os.MkdirAll(attDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real PNG outside attachments; symlink inside attachments must not read it.
	outsidePNG := filepath.Join(dir, "secret.png")
	if err := os.WriteFile(outsidePNG, samplePNG(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePNG, filepath.Join(attDir, "escape.png")); err != nil {
		t.Fatal(err)
	}
	// Subdirectory and named FIFO inside attachments must not block or be read.
	if err := os.MkdirAll(filepath.Join(attDir, "nested.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(attDir, "fifo.png"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported on test filesystem: %v", err)
	}
	// Filename containing a newline and colons must neither forge markers nor pass validation.
	if err := os.WriteFile(filepath.Join(attDir, "bad\n::name::.png"), samplePNG(t), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, atts, warnings := runFetchInDir(t, dir)
	if len(atts) != 0 {
		t.Fatalf("expected 0 accepted attachments, got %d", len(atts))
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"symlinks are not allowed", "only regular files", "invalid file name"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q:\n%s", want, joined)
		}
	}
}

func TestFetchRejectsDirectoryAndResultSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".kode-gopher"), 0o755); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "chart.png"), samplePNG(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetDir, filepath.Join(dir, AttachmentsDir)); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(secret, []byte(`{"kind":"ok","value":"secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, resultPath)); err != nil {
		t.Fatal(err)
	}

	res, resErr, atts, warnings := runFetchInDir(t, dir)
	if res != nil || !strings.Contains(resErr, "is a symlink") {
		t.Errorf("res=%v resErr=%q, want result symlink rejection", res, resErr)
	}
	if len(atts) != 0 || !strings.Contains(strings.Join(warnings, "\n"), "attachments is a symlink") {
		t.Errorf("atts=%v warnings=%v, want directory symlink warning", atts, warnings)
	}

	// Replacing .kode-gopher itself with a symlink must also be refused before reading children.
	rootSymDir := t.TempDir()
	outsideRoot := filepath.Join(rootSymDir, "outside")
	if err := os.MkdirAll(filepath.Join(outsideRoot, "attachments"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideRoot, "result.json"), []byte(`{"kind":"ok","value":"escaped"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideRoot, "attachments", "leak.png"), samplePNG(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideRoot, filepath.Join(rootSymDir, ".kode-gopher")); err != nil {
		t.Fatal(err)
	}
	res2, resErr2, atts2, _ := runFetchInDir(t, rootSymDir)
	if res2 != nil || len(atts2) != 0 || !strings.Contains(resErr2, ".kode-gopher is a symlink") {
		t.Errorf("root symlink: res=%v atts=%d resErr=%q", res2, len(atts2), resErr2)
	}
}

func TestFetchMissingDelimiterAndUnreadableFile(t *testing.T) {
	// Empty stdout without delimiter (e.g. truncated stream) must surface resultErr, never silent nil.
	res, resErr, atts, _ := parseFetchStdout("", "NONCE123")
	if res != nil || len(atts) != 0 || !strings.Contains(resErr, "truncated") {
		t.Errorf("empty stdout without delimiter: res=%v atts=%v resErr=%q", res, atts, resErr)
	}

	if os.Geteuid() == 0 {
		t.Skip("root bypasses 0000 read permissions")
	}
	dir := t.TempDir()
	attDir := filepath.Join(dir, AttachmentsDir)
	if err := os.MkdirAll(attDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(attDir, "locked.png")
	if err := os.WriteFile(unreadable, samplePNG(t), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })

	_, _, gotAtts, warnings := runFetchInDir(t, dir)
	if len(gotAtts) != 0 || !strings.Contains(strings.Join(warnings, "\n"), "not readable") {
		t.Errorf("atts=%v warnings=%v, want unreadable warning", gotAtts, warnings)
	}
}

func TestFetchEnforcesSizeCountAndOversizeResultCaps(t *testing.T) {
	dir := t.TempDir()
	attDir := filepath.Join(dir, AttachmentsDir)
	if err := os.MkdirAll(attDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Oversize result.json (600 KiB > 256 KiB) must be flagged without starving attachments.
	bigResult := `{"kind":"ok","value":"` + strings.Repeat("r", 600<<10) + `"}`
	if err := os.WriteFile(filepath.Join(dir, resultPath), []byte(bigResult), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write 10 valid small PNGs (exceeding MaxAttachments = 8) plus 1 oversize PNG (> 2 MiB).
	small := samplePNG(t)
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("img-%02d.png", i)
		if err := os.WriteFile(filepath.Join(attDir, name), small, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oversize := append(slicesClone(pngSignature), make([]byte, MaxAttachmentBytes+10)...)
	if err := os.WriteFile(filepath.Join(attDir, "z-huge.png"), oversize, 0o644); err != nil {
		t.Fatal(err)
	}

	res, resErr, atts, warnings := runFetchInDir(t, dir)
	if res != nil || !strings.Contains(resErr, "over the 256 KiB limit") {
		t.Errorf("res=%v resErr=%q, want oversize result error", res, resErr)
	}
	if len(atts) != MaxAttachments {
		t.Fatalf("len(atts) = %d, want %d (warnings=%v)", len(atts), MaxAttachments, warnings)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "2 additional attachment(s)") {
		t.Errorf("warnings missing maxcount message:\n%s", joined)
	}
	if !strings.Contains(joined, `"z-huge.png"`) || !strings.Contains(joined, "per-attachment limit") {
		t.Errorf("warnings missing oversize message:\n%s", joined)
	}
}

func TestFetchTotalAttachmentBudget(t *testing.T) {
	dir := t.TempDir()
	attDir := filepath.Join(dir, AttachmentsDir)
	if err := os.MkdirAll(attDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two 1.2 MiB PNG-header payloads: each < 2 MiB, combined 2.4 MiB > 2 MiB total cap.
	chunk := append(slicesClone(pngSignature), bytes.Repeat([]byte{1}, (1200<<10)-len(pngSignature))...)
	if err := os.WriteFile(filepath.Join(attDir, "01.png"), chunk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attDir, "02.png"), chunk, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, atts, warnings := runFetchInDir(t, dir)
	if len(atts) != 1 || atts[0].Name != "01.png" {
		t.Fatalf("atts = %v, want only 01.png", atts)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "per-run limit") {
		t.Errorf("warnings = %v, want per-run limit warning", warnings)
	}
}

func slicesClone(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
