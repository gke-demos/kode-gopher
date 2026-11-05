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
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// AttachmentsDir is the directory relative to /app where snippets
	// and full programs write binary image attachments to surface as
	// MCP ImageContent blocks.
	AttachmentsDir = ".kode-gopher/attachments"

	// MaxAttachments is the maximum number of image attachments a single
	// run may return.
	MaxAttachments = 8

	// MaxAttachmentBytes caps each individual image attachment (2 MiB).
	MaxAttachmentBytes = 2 << 20

	// MaxTotalAttachmentBytes caps the combined raw byte size of all
	// image attachments in one run (2 MiB), keeping base64-framed output
	// comfortably inside sandbox-server's 4 MiB stdout cap.
	MaxTotalAttachmentBytes = 2 << 20

	maxAttachmentNameBytes = 128
	maxInspectedEntries    = 32
	maxShippedCandidates   = 16
	maxShippedBytes        = 2<<20 + 256<<10 // 2.25 MiB raw cap before base64
)

// Attachment is a validated image file produced by a sandbox run under
// /app/.kode-gopher/attachments/.
type Attachment struct {
	Name     string
	MIMEType string
	Data     []byte
}

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// detectImageMIME verifies supported image formats by magic bytes
// (matching net/http.DetectContentType signatures) rather than trusting
// file extensions.
func detectImageMIME(data []byte) (string, bool) {
	switch {
	case len(data) >= len(pngSignature) && bytes.Equal(data[:len(pngSignature)], pngSignature):
		return "image/png", true
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg", true
	case len(data) >= 14 && string(data[0:4]) == "RIFF" && string(data[8:14]) == "WEBPVP":
		return "image/webp", true
	default:
		return "", false
	}
}

// fetchCmd builds the Phase 3 POSIX shell script that reads both
// .kode-gopher/result.json and .kode-gopher/attachments/ in a single
// sandbox round-trip. nonce is generated after Phase 2 exits so neither
// result.json nor attachment filenames can forge framing markers.
func fetchCmd(nonce string) string {
	return fmt.Sprintf(`LC_ALL=C
n=%s
if [ -L .kode-gopher ]; then
  printf '::%%s:root_symlink::\n' "$n"
  printf '\n::%%s:attachments::\n' "$n"
  exit 0
fi
if [ -L %s ]; then
  printf '::%%s:result_symlink::\n' "$n"
elif [ -f %s ]; then
  if [ ! -r %s ]; then
    printf '::%%s:result_unreadable::\n' "$n"
  else
    rsize=$(wc -c < %s | tr -d '[:space:]')
    if [ "${rsize:-0}" -gt %d ]; then
      printf '::%%s:result_oversize:%%s::\n' "$n" "$rsize"
    else
      head -c %d -- %s 2>/dev/null || true
    fi
  fi
fi
printf '\n::%%s:attachments::\n' "$n"
if [ -L %s ]; then
  printf '::%%s:dir_symlink::\n' "$n"
elif [ -d %s ]; then
  if [ ! -r %s ] || [ ! -x %s ]; then
    printf '::%%s:dir_unreadable::\n' "$n"
    exit 0
  fi
  seen=0
  shipped=0
  total=0
  for f in %s/* %s/.[!.]* %s/..?*; do
    [ -e "$f" ] || [ -L "$f" ] || continue
    seen=$((seen + 1))
    if [ "$seen" -gt %d ]; then
      printf '::%%s:truncated_dir::\n' "$n"
      break
    fi
    name=${f#%s/}
    bname=$(printf '%%s' "$name" | base64 | tr -d '\n')
    if [ -L "$f" ]; then
      printf '::%%s:skip:symlink:0:%%s::\n' "$n" "$bname"
      continue
    fi
    if [ ! -f "$f" ]; then
      printf '::%%s:skip:nonregular:0:%%s::\n' "$n" "$bname"
      continue
    fi
    if [ ! -r "$f" ]; then
      printf '::%%s:skip:unreadable:0:%%s::\n' "$n" "$bname"
      continue
    fi
    size=$(wc -c < "$f" | tr -d '[:space:]')
    size=${size:-0}
    if [ "$size" -eq 0 ]; then
      printf '::%%s:skip:empty:0:%%s::\n' "$n" "$bname"
      continue
    fi
    if [ "$size" -gt %d ]; then
      printf '::%%s:skip:oversize:%%s:%%s::\n' "$n" "$size" "$bname"
      continue
    fi
    if [ "$shipped" -ge %d ]; then
      printf '::%%s:skip:maxcount:%%s:%%s::\n' "$n" "$size" "$bname"
      continue
    fi
    if [ $((total + size)) -gt %d ]; then
      printf '::%%s:skip:maxtotal:%%s:%%s::\n' "$n" "$size" "$bname"
      continue
    fi
    shipped=$((shipped + 1))
    total=$((total + size))
    printf '::%%s:file:%%s:%%s::\n' "$n" "$size" "$bname"
    head -c %d -- "$f" | base64
    printf '\n::%%s:end::\n' "$n"
  done
fi
`,
		shellQuote(nonce),
		resultPath, resultPath, resultPath, resultPath, maxResultBytes, maxResultBytes+1, resultPath,
		AttachmentsDir, AttachmentsDir, AttachmentsDir, AttachmentsDir,
		AttachmentsDir, AttachmentsDir, AttachmentsDir,
		maxInspectedEntries, AttachmentsDir,
		MaxAttachmentBytes, maxShippedCandidates, maxShippedBytes, MaxAttachmentBytes+1,
	)
}

type rawAttachmentEntry struct {
	name    string
	rawName string
	kind    string // "file", "symlink", "nonregular", "empty", "oversize", "maxcount", "maxtotal"
	size    int64
	b64     string
}

func parseFetchStdout(stdout, nonce string) (result *Result, resultErr string, attachments []Attachment, warnings []string) {
	sep := "\n::" + nonce + ":attachments::\n"
	resPart, attPart, ok := strings.Cut(stdout, sep)
	if !ok {
		if after, hasPrefix := strings.CutPrefix(stdout, "::"+nonce+":attachments::\n"); hasPrefix {
			resPart, attPart, ok = "", after, true
		}
	}
	result, resultErr = parseFetchedResult(resPart, nonce)
	if !ok {
		if resultErr == "" {
			resultErr = "couldn't fetch the result: fetch output was truncated"
		} else {
			warnings = append(warnings, "couldn't read attachments: fetch output was truncated")
		}
		return result, resultErr, nil, warnings
	}
	attachments, attWarnings := parseAttachmentSection(attPart, nonce)
	warnings = append(warnings, attWarnings...)
	return result, resultErr, attachments, warnings
}

func parseFetchedResult(body, nonce string) (*Result, string) {
	trimmed := strings.TrimSpace(body)
	switch trimmed {
	case "::" + nonce + ":root_symlink::":
		return nil, ".kode-gopher is a symlink and was ignored; keep .kode-gopher as a regular directory"
	case "::" + nonce + ":result_symlink::":
		return nil, fmt.Sprintf("%s is a symlink and was ignored; write a regular file", resultPath)
	case "::" + nonce + ":result_unreadable::":
		return nil, fmt.Sprintf("%s is not readable", resultPath)
	}
	if rest, ok := strings.CutPrefix(trimmed, "::"+nonce+":result_oversize:"); ok {
		if numStr, ok := strings.CutSuffix(rest, "::"); ok {
			if n, err := strconv.ParseInt(numStr, 10, 64); err == nil {
				return nil, fmt.Sprintf("the result is %d KiB, over the %d KiB limit, so it was dropped: return less data (filter, summarize or page through it in the program)",
					n>>10, maxResultBytes>>10)
			}
		}
	}
	return parseResult(body)
}

func parseAttachmentSection(section, nonce string) ([]Attachment, []string) {
	if strings.TrimSpace(section) == "" {
		return nil, nil
	}
	entries, warnings := scanAttachmentEntries(section, nonce)
	slices.SortFunc(entries, func(a, b rawAttachmentEntry) int {
		return strings.Compare(a.rawName, b.rawName)
	})
	var out []Attachment
	totalBytes := 0
	extraCount := 0
	for _, e := range entries {
		att, warn, countedOver := evaluateAttachmentEntry(e, len(out), totalBytes)
		if countedOver {
			extraCount++
			continue
		}
		if warn != "" {
			warnings = append(warnings, warn)
			continue
		}
		totalBytes += len(att.Data)
		out = append(out, att)
	}
	if extraCount > 0 {
		warnings = append(warnings, fmt.Sprintf("%d additional attachment(s) in %s were skipped: at most %d attachments are returned per run",
			extraCount, AttachmentsDir, MaxAttachments))
	}
	return out, warnings
}

func scanAttachmentEntries(section, nonce string) ([]rawAttachmentEntry, []string) {
	prefix := "::" + nonce + ":"
	lines := strings.Split(section, "\n")
	var entries []rawAttachmentEntry
	var warnings []string

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		if line == "" {
			continue
		}
		body, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		body, ok = strings.CutSuffix(body, "::")
		if !ok {
			continue
		}
		switch {
		case body == "dir_symlink":
			warnings = append(warnings, fmt.Sprintf("%s is a symlink and was ignored; create a regular directory", AttachmentsDir))
		case body == "dir_unreadable":
			warnings = append(warnings, fmt.Sprintf("%s is not readable; ensure directory permissions allow reading", AttachmentsDir))
		case body == "truncated_dir":
			warnings = append(warnings, fmt.Sprintf("%s has more than %d entries; remaining entries were skipped", AttachmentsDir, maxInspectedEntries))
		case strings.HasPrefix(body, "skip:"):
			if e, ok := parseSkipHeader(strings.TrimPrefix(body, "skip:")); ok {
				entries = append(entries, e)
			}
		case strings.HasPrefix(body, "file:"):
			e, nextIdx, warn := parseFileBlock(strings.TrimPrefix(body, "file:"), lines, i+1, prefix+"end::")
			i = nextIdx
			if warn != "" {
				warnings = append(warnings, warn)
				continue
			}
			entries = append(entries, e)
		}
	}
	return entries, warnings
}

func parseSkipHeader(rest string) (rawAttachmentEntry, bool) {
	// Format: <kind>:<size>:<bname>
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 {
		return rawAttachmentEntry{}, false
	}
	size, _ := strconv.ParseInt(parts[1], 10, 64)
	rawName, disp := decodeEntryName(parts[2])
	return rawAttachmentEntry{name: disp, rawName: rawName, kind: parts[0], size: size}, true
}

func parseFileBlock(rest string, lines []string, startIdx int, endMarker string) (rawAttachmentEntry, int, string) {
	// Format: <size>:<bname>
	sizeStr, bname, ok := strings.Cut(rest, ":")
	if !ok {
		return rawAttachmentEntry{}, startIdx, ""
	}
	size, _ := strconv.ParseInt(sizeStr, 10, 64)
	rawName, disp := decodeEntryName(bname)
	var b64 strings.Builder
	for j := startIdx; j < len(lines); j++ {
		trimmed := strings.TrimSuffix(lines[j], "\r")
		if trimmed == endMarker {
			return rawAttachmentEntry{name: disp, rawName: rawName, kind: "file", size: size, b64: b64.String()}, j, ""
		}
		b64.WriteString(trimmed)
	}
	return rawAttachmentEntry{}, len(lines), fmt.Sprintf("attachment %s was dropped: transfer output was truncated", disp)
}

func evaluateAttachmentEntry(e rawAttachmentEntry, acceptedCount, totalBytes int) (Attachment, string, bool) {
	switch e.kind {
	case "symlink":
		return Attachment{}, fmt.Sprintf("attachment %s was skipped: symlinks are not allowed in %s", e.name, AttachmentsDir), false
	case "nonregular":
		return Attachment{}, fmt.Sprintf("attachment %s was skipped: only regular files are allowed in %s", e.name, AttachmentsDir), false
	case "unreadable":
		return Attachment{}, fmt.Sprintf("attachment %s was skipped: file is not readable", e.name), false
	case "empty":
		return Attachment{}, fmt.Sprintf("attachment %s was skipped: file is empty", e.name), false
	case "oversize":
		return Attachment{}, fmt.Sprintf("attachment %s (%d KiB) exceeds the %d KiB per-attachment limit and was skipped",
			e.name, e.size>>10, MaxAttachmentBytes>>10), false
	case "maxcount":
		return Attachment{}, "", true
	case "maxtotal":
		return Attachment{}, fmt.Sprintf("attachment %s (%d KiB) was skipped: total attachments exceed the %d KiB per-run limit",
			e.name, e.size>>10, MaxTotalAttachmentBytes>>10), false
	}

	if !validAttachmentName(e.rawName) {
		return Attachment{}, fmt.Sprintf("attachment %s was skipped: invalid file name", e.name), false
	}
	data, err := base64.StdEncoding.DecodeString(e.b64)
	if err != nil {
		return Attachment{}, fmt.Sprintf("attachment %s could not be decoded: %v", e.name, err), false
	}
	if len(data) == 0 {
		return Attachment{}, fmt.Sprintf("attachment %s was skipped: file is empty", e.name), false
	}
	if len(data) > MaxAttachmentBytes {
		return Attachment{}, fmt.Sprintf("attachment %s (%d KiB) exceeds the %d KiB per-attachment limit and was skipped",
			e.name, len(data)>>10, MaxAttachmentBytes>>10), false
	}
	mimeType, ok := detectImageMIME(data)
	if !ok {
		return Attachment{}, fmt.Sprintf("attachment %s was skipped: unsupported or invalid image format (supported: image/png, image/jpeg, image/webp)", e.name), false
	}
	if acceptedCount >= MaxAttachments {
		return Attachment{}, "", true
	}
	if totalBytes+len(data) > MaxTotalAttachmentBytes {
		return Attachment{}, fmt.Sprintf("attachment %s (%d KiB) was skipped: total attachments exceed the %d KiB per-run limit",
			e.name, len(data)>>10, MaxTotalAttachmentBytes>>10), false
	}
	return Attachment{Name: e.rawName, MIMEType: mimeType, Data: data}, "", false
}

func decodeEntryName(b64 string) (rawName, quoted string) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", strconv.QuoteToASCII(b64)
	}
	raw := string(b)
	return raw, strconv.QuoteToASCII(raw)
}

func validAttachmentName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > maxAttachmentNameBytes || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r == '/' || r == 0 || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
