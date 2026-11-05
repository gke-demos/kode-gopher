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

package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gke-demos/kode-gopher/internal/executor"
)

func TestPrintOutcomeAttachments(t *testing.T) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	printOutcome(&executor.Outcome{
		Phase:         executor.PhaseRun,
		ExitCode:      0,
		Duration:      100 * time.Millisecond,
		BuildDuration: 50 * time.Millisecond,
		Attachments: []executor.Attachment{
			{Name: "plot.png", MIMEType: "image/png", Data: []byte{1, 2, 3, 4}},
		},
	})
	_ = w.Close()
	outBytes, _ := io.ReadAll(r)
	out := string(outBytes)
	if !strings.Contains(out, "── attachments ──\nplot.png (image/png, 4 bytes)") {
		t.Errorf("printOutcome = %q, want attachments section", out)
	}
	if strings.Contains(out, "(no output)") {
		t.Errorf("printOutcome printed (no output) despite attachment: %q", out)
	}
}
