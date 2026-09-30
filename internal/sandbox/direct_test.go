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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostExecuteSendsCredentials(t *testing.T) {
	expiry := time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC)
	var got struct {
		Command     string `json:"command"`
		Credentials struct {
			AccessToken string    `json:"access_token"`
			Expiry      time.Time `json:"expiry"`
			Email       string    `json:"email"`
			Project     string    `json:"project"`
		} `json:"credentials"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/execute" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Write([]byte(`{"stdout":"hi\n","stderr":"","exit_code":3}`))
	}))
	defer ts.Close()

	res, err := postExecute(context.Background(), ts.Client(), ts.URL, "./run", &Credentials{
		AccessToken: "ya29.secret", Expiry: expiry, Email: "u@example.com", Project: "p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hi\n" || res.ExitCode != 3 {
		t.Errorf("result = %+v", res)
	}
	c := got.Credentials
	if got.Command != "./run" || c.AccessToken != "ya29.secret" || !c.Expiry.Equal(expiry) || c.Email != "u@example.com" || c.Project != "p" {
		t.Errorf("request = %+v", got)
	}
}

func TestPostExecuteNoRetryAndNoTokenInErrors(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"message":"boom"}`, http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	_, err := postExecute(context.Background(), ts.Client(), ts.URL, "./run", &Credentials{
		AccessToken: "ya29.secret", Expiry: time.Now().Add(time.Hour),
	})
	if err == nil {
		t.Fatal("want error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server saw %d requests, want exactly 1", n)
	}
	if strings.Contains(err.Error(), "ya29.secret") {
		t.Errorf("error leaks the token: %v", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error lacks status: %v", err)
	}
}

func TestRunWithCredentialsNeedsInCluster(t *testing.T) {
	s := &Session{}
	if _, err := s.runWithCredentials(context.Background(), "true", &Credentials{AccessToken: "x"}); err == nil ||
		!strings.Contains(err.Error(), "in-cluster") {
		t.Errorf("err = %v, want in-cluster error", err)
	}
}
