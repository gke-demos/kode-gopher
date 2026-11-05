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

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/kode-gopher/internal/executor"
)

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 200, G: 100, B: 50, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestOutcomeToToolResultNoAttachments(t *testing.T) {
	outcome := &executor.Outcome{
		Phase:         executor.PhaseRun,
		ExitCode:      0,
		Duration:      200 * time.Millisecond,
		BuildDuration: 100 * time.Millisecond,
		Result:        &executor.Result{Kind: "ok", Value: json.RawMessage(`{"ok":true}`)},
	}
	res, out := outcomeToToolResult(outcome, "wrapped")
	if res.IsError {
		t.Fatal("unexpected IsError")
	}
	if len(res.Content) != 1 {
		t.Fatalf("len(res.Content) = %d, want 1", len(res.Content))
	}
	if out.Attachments != nil {
		t.Errorf("out.Attachments = %v, want nil when empty", out.Attachments)
	}
	tc := res.Content[0].(*sdk.TextContent)
	if strings.Contains(tc.Text, "attachments") {
		t.Errorf("renderText unexpectedly mentioned attachments: %q", tc.Text)
	}
}

func TestExecuteGoCodeImageAttachmentsOverMCPTransport(t *testing.T) {
	pngData := testPNGBytes(t)
	webpData := []byte("RIFF\x14\x00\x00\x00WEBPVP8L\x08\x00\x00\x00\x2f\x00\x00\x00\x00\x00\x00\x00")

	srv := sdk.NewServer(&sdk.Implementation{Name: "kg-test", Version: "0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "execute_go_code"}, func(_ context.Context, _ *sdk.CallToolRequest, _ ExecuteGoCodeArgs) (*sdk.CallToolResult, *ExecuteGoCodeOutput, error) {
		outcome := &executor.Outcome{
			Phase:         executor.PhaseRun,
			ExitCode:      0,
			Duration:      500 * time.Millisecond,
			BuildDuration: 250 * time.Millisecond,
			Result:        &executor.Result{Kind: "ok", Value: json.RawMessage(`{"summary":"rendered 2 charts"}`)},
			Attachments: []executor.Attachment{
				{Name: "cpu.png", MIMEType: "image/png", Data: pngData},
				{Name: "mem.webp", MIMEType: "image/webp", Data: webpData},
			},
		}
		res, out := outcomeToToolResult(outcome, "wrapped")
		return res, out, nil
	})

	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()

	cli := sdk.NewClient(&sdk.Implementation{Name: "kg-cli", Version: "0"}, nil)
	cs, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name:      "execute_go_code",
		Arguments: map[string]any{"code": "package snippet"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError=true")
	}
	if len(res.Content) != 3 {
		t.Fatalf("len(res.Content) = %d, want 3 (1 TextContent + 2 ImageContent)", len(res.Content))
	}

	txt, ok := res.Content[0].(*sdk.TextContent)
	if !ok || !strings.Contains(txt.Text, "── attachments ──") || !strings.Contains(txt.Text, "cpu.png (image/png,") {
		t.Errorf("Content[0] = %#v, want TextContent with attachments summary", res.Content[0])
	}

	img1, ok := res.Content[1].(*sdk.ImageContent)
	if !ok || img1.MIMEType != "image/png" || !bytes.Equal(img1.Data, pngData) {
		t.Errorf("Content[1] = %#v, want ImageContent(image/png)", res.Content[1])
	}
	img2, ok := res.Content[2].(*sdk.ImageContent)
	if !ok || img2.MIMEType != "image/webp" || !bytes.Equal(img2.Data, webpData) {
		t.Errorf("Content[2] = %#v, want ImageContent(image/webp)", res.Content[2])
	}

	rawStructured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var structured ExecuteGoCodeOutput
	if err := json.Unmarshal(rawStructured, &structured); err != nil {
		t.Fatal(err)
	}
	if len(structured.Attachments) != 2 || structured.Attachments[0].Name != "cpu.png" || structured.Attachments[0].Bytes != len(pngData) {
		t.Errorf("structured.Attachments = %+v", structured.Attachments)
	}
}
