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
	"context"
	"flag"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/oauth2/google"

	"github.com/gke-demos/kode-gopher/internal/creds"
	"github.com/gke-demos/kode-gopher/internal/oauth"
)

// oauthFlags are serve's --auth=oauth settings: one deployment's
// authorization server (docs/design-in-cluster.md, section 3).
type oauthFlags struct {
	issuer       *string
	googleClient *string
	keyring      *string
	custody      *string
	authProvider *string
	allowDomains *string
	allowGroups  *string
	clientsFile  *string
	openReg      *bool
	project      *string
	quotaProject *string
}

func addOAuthFlags(fs *flag.FlagSet) *oauthFlags {
	return &oauthFlags{
		issuer:       fs.String("oauth-issuer", "", "oauth: public base URL of this server, e.g. https://kg.example.com (the MCP endpoint is <issuer>/mcp)"),
		googleClient: fs.String("oauth-google-client-file", "", "oauth: kode-gopher's Google OAuth client, as the JSON the Cloud console downloads"),
		keyring:      fs.String("oauth-keyring-file", "", "oauth: token-sealing keys, one \"<id> <base64 32 bytes>\" per line, sealing key first"),
		custody:      fs.String("oauth-custody", "vault", "oauth: who holds users' Google grants: vault (Agent Identity credential vault) or sealed (inside kode-gopher's refresh token)"),
		authProvider: fs.String("oauth-vault-auth-provider", "", "oauth, vault custody: projects/<p>/locations/<l>/authProviders/<name>"),
		allowDomains: fs.String("oauth-allow-domains", "", "oauth: comma-separated Workspace domains (the ID token's hd claim) to admit"),
		allowGroups:  fs.String("oauth-allow-groups", "", "oauth: comma-separated Google group emails to admit (nested membership counts; needs the Groups Reader admin role)"),
		clientsFile:  fs.String("oauth-clients-file", "", "oauth: JSON array of pre-registered clients ({client_id, client_secret?, client_name, redirect_uris}, or {client_id, client_secret, service_account} for the client_credentials grant)"),
		openReg:      fs.Bool("oauth-open-registration", true, "oauth: accept dynamically registered (DCR) and Client ID Metadata Document clients, not just pre-registered ones"),
		project:      fs.String("project", "", "oauth: project reported to snippets as GOOGLE_CLOUD_PROJECT"),
		quotaProject: fs.String("quota-project", "", "oauth: quota project reported to snippets as GOOGLE_CLOUD_QUOTA_PROJECT"),
	}
}

var authProviderRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/authProviders/[^/]+$`)

// server builds the authorization server. Calls to the vault, Cloud
// Identity and (for client-credentials clients) the IAM Credentials API
// use kode-gopher's own ADC (Workload Identity in a pod).
func (f *oauthFlags) server(ctx context.Context) (*oauth.Server, error) {
	if *f.issuer == "" || *f.googleClient == "" || *f.keyring == "" {
		return nil, fmt.Errorf("--auth=oauth needs --oauth-issuer, --oauth-google-client-file and --oauth-keyring-file")
	}
	gc, err := oauth.LoadGoogleClient(*f.googleClient)
	if err != nil {
		return nil, err
	}
	keys, err := oauth.LoadKeyring(*f.keyring)
	if err != nil {
		return nil, err
	}
	var custody oauth.Custody
	switch *f.custody {
	case "vault":
		if !authProviderRE.MatchString(*f.authProvider) {
			return nil, fmt.Errorf("--oauth-custody=vault needs --oauth-vault-auth-provider=projects/<p>/locations/<l>/authProviders/<name>")
		}
		hc, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return nil, fmt.Errorf("vault client: %w", err)
		}
		custody = &oauth.VaultCustody{AuthProvider: *f.authProvider, Client: hc}
	case "sealed":
		custody = oauth.SealedCustody{}
	default:
		return nil, fmt.Errorf("--oauth-custody must be vault or sealed, got %q", *f.custody)
	}
	allow := &oauth.AllowList{Domains: splitList(*f.allowDomains), Groups: splitList(*f.allowGroups)}
	if len(allow.Groups) > 0 {
		hc, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-identity.groups.readonly")
		if err != nil {
			return nil, fmt.Errorf("cloud identity client: %w", err)
		}
		allow.GroupChecker = &oauth.CloudIdentityGroups{Client: hc}
	}
	var clients []oauth.StaticClient
	var serviceTokens oauth.ServiceTokens
	if *f.clientsFile != "" {
		if clients, err = oauth.LoadStaticClients(*f.clientsFile); err != nil {
			return nil, err
		}
		if slices.ContainsFunc(clients, func(c oauth.StaticClient) bool { return c.ServiceAccount != "" }) {
			if serviceTokens, err = creds.NewImpersonator(ctx); err != nil {
				return nil, fmt.Errorf("service token minter: %w", err)
			}
		}
	}
	return oauth.New(oauth.Config{
		Issuer:           *f.issuer,
		Google:           gc,
		Custody:          custody,
		Allow:            allow,
		Keys:             keys,
		Clients:          clients,
		ServiceTokens:    serviceTokens,
		OpenRegistration: *f.openReg,
		Project:          *f.project,
		QuotaProject:     *f.quotaProject,
	})
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
