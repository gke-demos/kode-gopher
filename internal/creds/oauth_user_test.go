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

package creds

import (
	"context"
	"testing"
	"time"
)

func TestOAuthUser(t *testing.T) {
	ctx := context.Background()
	u := &OAuthUser{Token: "ya29.x", Expiry: time.Now().Add(time.Hour), Email: "a@example.com", Project: "p", QuotaProject: "q"}
	var _ TokenMinter = u
	files, env, err := u.Materialize(ctx)
	if err != nil || len(files) != 0 || len(env) != 0 {
		t.Errorf("Materialize = %v, %v, %v; want nothing", files, env, err)
	}
	tok, err := u.AccessToken(ctx)
	if err != nil || tok.Token != "ya29.x" || tok.Email != "a@example.com" || tok.QuotaProject != "q" {
		t.Errorf("AccessToken = %+v, %v", tok, err)
	}
	if id, _ := u.Identity(ctx); id.Mode != "oauth" || id.Email != "a@example.com" || id.ProjectID != "p" {
		t.Errorf("Identity = %+v", id)
	}
	u.ServiceAccount = true
	if id, _ := u.Identity(ctx); id.Mode != "service" || id.CredType != "service_account" {
		t.Errorf("client-credentials Identity = %+v", id)
	}
	u.Expiry = time.Now().Add(-time.Second)
	if _, err := u.AccessToken(ctx); err == nil {
		t.Error("expired token handed out")
	}
}
