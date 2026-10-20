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

package oauth

// Regression tests for client names on the consent page: a name is the
// client's own claim, so it must not reorder, hide or imitate the client
// ID and redirect host shown beside it. Ported from go-steer/steward#17.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// spoofName carries a right-to-left override, an isolate pair, zero-width
// characters, a line separator, a newline, a no-break space, a tab and a
// fake client ID.
const spoofName = "Good\u202E App\u2066x\u2069\u200B\u200D\u2028\nnext\u00A0\t(https://evil.example)"

// spoofClean is spoofName cleaned.
const spoofClean = "Good Appx next (https://evil.example)"

// spoofRunes are spoofName's invisible runes; none may reach the page.
var spoofRunes = []rune{'\u202E', '\u2066', '\u2069', '\u200B', '\u200D', '\u2028'}

var bdiRE = regexp.MustCompile(`(?s)<bdi>(.*?)</bdi>`)

// consentBody fetches the consent page for clientID with set applied.
func (e *testEnv) consentBody(clientID string, set map[string]string) string {
	e.t.Helper()
	code, body := get(e.t, e.authorizeURL(clientID, rand.Text()+rand.Text(), set))
	if code != http.StatusOK || !hiddenRequestRE.MatchString(body) {
		e.t.Fatalf("no consent page: HTTP %d: %s", code, body)
	}
	return body
}

// checkConsentName asserts the page names the client spoofClean, in an
// element of its own holding nothing else, and carries no spoofing rune.
func checkConsentName(t *testing.T, body string) {
	t.Helper()
	for _, r := range spoofRunes {
		if strings.ContainsRune(body, r) {
			t.Errorf("consent page carries %U", r)
		}
	}
	if !strings.Contains(body, "An MCP client calling itself <b><bdi>"+spoofClean+"</bdi></b> wants") {
		t.Errorf("consent page doesn't name the client %q in its own <bdi>:\n%s", spoofClean, body)
	}
}

func TestConsentCIMDNameCannotSpoofClientID(t *testing.T) {
	var docURL string
	doc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"client_id": docURL, "client_name": spoofName, "redirect_uris": []string{clientRedirect}})
	}))
	t.Cleanup(doc.Close)
	// The client ID itself carries an override, which url.Parse keeps.
	docURL = doc.URL + "/\u202Eclient.json"
	e := newTestEnv(t, false, func(c *Config) { c.CIMDClient = doc.Client() })
	body := e.consentBody(docURL, nil)
	checkConsentName(t, body)
	shown := strings.ReplaceAll(docURL, "\u202E", "%E2%80%AE")
	if !strings.Contains(body, `<p>Its client ID is <code dir="ltr">`+shown+`</code>.</p>`) {
		t.Errorf("consent page doesn't show the client ID %q in its own element:\n%s", shown, body)
	}
	for _, m := range bdiRE.FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[1], doc.URL) {
			t.Errorf("the client's name element holds its client ID: %q", m[1])
		}
	}
	// Resolution cleans it too, which keeps it clean in the log
	// (clientLabel).
	if c, err := e.as.resolveClient(context.Background(), docURL); err != nil {
		t.Error(err)
	} else if c.Name != spoofClean {
		t.Errorf("resolved CIMD client: name %q, want %q", c.Name, spoofClean)
	}
}

// The page cleans the name itself, whatever reaches it.
func TestConsentPageCleansName(t *testing.T) {
	w := httptest.NewRecorder()
	consentPage(w, &client{ID: sealPrefix + "x", Name: spoofName}, clientRedirect, "form")
	checkConsentName(t, w.Body.String())
}

func TestConsentDCRNameCannotSpoofRedirect(t *testing.T) {
	e := newTestEnv(t, false, nil)
	// A redirect host whose escaped override url.Parse decodes.
	const redirect = "https://ev%E2%80%AEil.example/cb"
	reg, _ := json.Marshal(map[string]any{"client_name": spoofName, "redirect_uris": []string{redirect}})
	resp, err := http.Post(e.srv.URL+"/register", "application/json", strings.NewReader(string(reg)))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ClientID   string `json:"client_id"`
		ClientName string `json:"client_name"`
	}
	err = json.NewDecoder(resp.Body).Decode(&m)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || err != nil {
		t.Fatalf("register: %s, %v", resp.Status, err)
	}
	if m.ClientName != spoofClean {
		t.Errorf("registered client_name = %q, want %q", m.ClientName, spoofClean)
	}
	body := e.consentBody(m.ClientID, map[string]string{"redirect_uri": redirect})
	checkConsentName(t, body)
	if !strings.Contains(body, `<code dir="ltr">https://ev%E2%80%AEil.example</code>`) {
		t.Errorf("consent page doesn't show the redirect host escaped in its own element:\n%s", body)
	}
	if strings.Contains(body, "Its client ID is") || !strings.Contains(body, "<p>It registered itself with kode-gopher, so nothing vouches for its name.</p>") {
		t.Errorf("consent page shows a sealed client ID, or doesn't say the name is unvouched:\n%s", body)
	}
}

// A registration sealed before names were cleaned still displays safely.
func TestConsentOldDCRRegistration(t *testing.T) {
	e := newTestEnv(t, false, nil)
	id, err := e.as.keys.seal(purposeClient, dcrClient{RedirectURIs: []string{clientRedirect}, Name: spoofName, IssuedAt: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	checkConsentName(t, e.consentBody(id, nil))
	// Resolution cleans it too, which keeps it clean in the log.
	if c, err := e.as.resolveClient(context.Background(), id); err != nil {
		t.Error(err)
	} else if c.Name != spoofClean {
		t.Errorf("resolved old registration: name %q, want %q", c.Name, spoofClean)
	}
	// A name that is nothing but invisible characters is no name.
	id, err = e.as.keys.seal(purposeClient, dcrClient{RedirectURIs: []string{clientRedirect}, Name: "\u202E\u200B \n\u2066", IssuedAt: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	body := e.consentBody(id, nil)
	if !strings.Contains(body, "<p>An unnamed MCP client wants") || strings.Contains(body, "<bdi>") {
		t.Errorf("an invisible name isn't shown as unnamed:\n%s", body)
	}
}

// A static client's name, operator-supplied and shown in logs, is cleaned
// too.
func TestStaticClientNameCleaned(t *testing.T) {
	e := newTestEnv(t, false, func(c *Config) {
		c.Clients = []StaticClient{{ID: "static", Name: spoofName, RedirectURIs: []string{clientRedirect}}}
	})
	c, err := e.as.resolveClient(context.Background(), "static")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != spoofClean {
		t.Errorf("static client name = %q, want %q", c.Name, spoofClean)
	}
}

func TestClientName(t *testing.T) {
	acute := "\u0301"
	marks := "e" + strings.Repeat(acute, 10)
	capped := "e" + strings.Repeat(acute, maxMarks)
	cases := []struct{ in, want string }{
		{"", ""},
		{"Claude Code", "Claude Code"},
		{spoofName, spoofClean},
		{"  \t lead and trail \r\n ", "lead and trail"},
		{"a\u00ADb\u2060c\uFEFFd\u180Ee", "abcde"},                                   // soft hyphen, word joiner, BOM, Mongolian vowel separator
		{"x\u202Ay\u202Bz\u202Cw\u202Dv\u2067u\u2068t", "xyzwvut"},                   // LRE, RLE, PDF, LRO, RLI, FSI
		{"a\u200Eb\u200Fc\u061Cd", "abcd"},                                           // LRM, RLM, ALM
		{"tag\U000E0041\U000E007F", "tag"},                                           // tag characters
		{"pua\uE000\U000F0000", "pua"},                                               // private use
		{"none\U000EFFFF\uFDD0", "none"},                                             // unassigned, noncharacter
		{"fill\u3164\u115F\uFFA0er", "filler"},                                       // Hangul fillers
		{"vs\uFE0F\U000E0100", "vs"},                                                 // variation selectors
		{"para\u2029graph", "para graph"},                                            // paragraph separator is whitespace
		{"bell\x07nul\x00del\x7F", "bellnuldel"},                                     // C0 and DEL
		{"c1\u0085\u009Bx", "c1 x"},                                                  // NEL is whitespace; CSI is a control
		{"bad\xffutf8", "bad\uFFFDutf8"},                                             // invalid UTF-8 shows as U+FFFD
		{"\u05E2\u05D1\u05E8\u05D9\u05EA App", "\u05E2\u05D1\u05E8\u05D9\u05EA App"}, // RTL letters stay, isolated by <bdi>
		{"\u65E5\u672C\u8A9E \u30AF\u30E9", "\u65E5\u672C\u8A9E \u30AF\u30E9"},
		{"Caf\u00E9 n\u0303", "Caf\u00E9 n\u0303"},
		{marks, capped},
		{marks + " " + marks, capped + " " + capped},
		{strings.Repeat("n", 300), strings.Repeat("n", maxClientName)},
		// Truncation after cleaning, on a character boundary, with no
		// trailing space.
		{strings.Repeat("\u200B", 200) + strings.Repeat("a", 99) + "\u00E9", strings.Repeat("a", 99)},
		{strings.Repeat("a", 99) + " b", strings.Repeat("a", 99)},
	}
	for _, tc := range cases {
		got := clientName(tc.in)
		if got != tc.want {
			t.Errorf("clientName(%+q) = %+q, want %+q", tc.in, got, tc.want)
		}
		if again := clientName(got); again != got {
			t.Errorf("clientName isn't idempotent on %+q: %+q", got, again)
		}
		if len(got) > maxClientName || !utf8.ValidString(got) {
			t.Errorf("clientName(%+q) = %+q: too long or not UTF-8", tc.in, got)
		}
	}
}

func TestDisplayURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://client.example/meta.json": "https://client.example/meta.json",
		"https://ev\u202Eil.example":       "https://ev%E2%80%AEil.example",
		"https://\u05E2\u05D1.example/x y": "https://%D7%A2%D7%91.example/x%20y",
		"com.example.app:/cb\n":            "com.example.app:/cb%0A",
	} {
		if got := displayURL(in); got != want {
			t.Errorf("displayURL(%+q) = %q, want %q", in, got, want)
		}
	}
}
