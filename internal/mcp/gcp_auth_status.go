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
	"context"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// GCPAuthStatusArgs is empty — no inputs.
type GCPAuthStatusArgs struct{}

// GCPAuthStatusOutput mirrors creds.Identity — the LLM can read this
// to decide whether the sandbox is properly credentialed before
// spending a tool call on execute_go_code.
type GCPAuthStatusOutput struct {
	Mode      string `json:"mode"                       jsonschema:"'forwarded' (host ADC copied into sandbox), 'access-token' (a short-lived token minted by kode-gopher, served to each run by a metadata emulator in the sandbox), 'oauth' (the signed-in user's own short-lived Google token, served the same way), 'service' (a short-lived token for the service account this deployment or OAuth client runs snippets as, served the same way), or 'none' (no credentials configured)."`
	CredType  string `json:"credential_type,omitempty"  jsonschema:"For forwarded and access-token modes: 'authorized_user' or 'service_account' (access-token also 'metadata', when kode-gopher's own identity is Workload Identity). oauth: 'authorized_user'. service: 'service_account'."`
	Email     string `json:"email,omitempty"            jsonschema:"Best-effort identity email. Empty means lookup failed or wasn't possible."`
	ProjectID string `json:"project_id,omitempty"       jsonschema:"Google Cloud project for these credentials: the deployment's configured project, the ADC file's quota project, or the service account's own project. Snippets also see it as $GOOGLE_CLOUD_PROJECT when kode-gopher passes it on."`
}

func (s *Server) handleGCPAuthStatus(ctx context.Context, req *sdk.CallToolRequest, _ GCPAuthStatusArgs) (*sdk.CallToolResult, *GCPAuthStatusOutput, error) {
	src := s.credentialsFor(req)
	if src == nil {
		out := &GCPAuthStatusOutput{Mode: "none"}
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "mode=none (no credentials source configured)"}},
		}, out, nil
	}
	id, err := src.Identity(ctx)
	out := &GCPAuthStatusOutput{
		Mode:      id.Mode,
		CredType:  id.CredType,
		Email:     id.Email,
		ProjectID: id.ProjectID,
	}
	if err != nil {
		// Partial data with an IsError marker so the LLM can distinguish
		// "auth definitively broken" from "auth uncertain — retry or ask".
		text := fmt.Sprintf("identity lookup failed: %v\npartial: mode=%s credential_type=%s email=%s project_id=%s",
			err, out.Mode, out.CredType, out.Email, out.ProjectID)
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: text}},
			IsError: true,
		}, out, nil
	}
	text := fmt.Sprintf("mode=%s credential_type=%s email=%s project_id=%s",
		out.Mode, out.CredType, out.Email, out.ProjectID)
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: text}},
	}, out, nil
}

const gcpAuthStatusDescription = `Report the sandbox's GCP credential identity: mode (forwarded/access-token/oauth/service/none), credential type (authorized_user/service_account/metadata), identity email, and project ID. Zero-arg tool. Use before execute_go_code to confirm the sandbox will authenticate as the expected identity — especially useful after a token revocation or when debugging a "why did my API call return PermissionDenied" scenario.`
