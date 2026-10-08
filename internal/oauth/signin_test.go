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

import (
	"crypto/rand"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// noFollow is b's client, stopping at the first redirect.
func noFollow(b *browser) *http.Client {
	hc := *b.hc
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hc
}

// consentForm loads the consent page in b and returns its sealed form.
func consentForm(t *testing.T, b *browser, u string) string {
	t.Helper()
	resp, err := noFollow(b).Get(u)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m := hiddenRequestRE.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("no consent form: HTTP %d %q", resp.StatusCode, body)
	}
	return html.UnescapeString(m[1])
}

// approve posts a consent form from b and returns where it sends the
// browser (Google sign-in), or the page it stopped on.
func approve(t *testing.T, b *browser, srvURL, form string) (string, string) {
	t.Helper()
	resp, err := noFollow(b).PostForm(srvURL+"/authorize", url.Values{"request": {form}, "action": {"approve"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.Header.Get("Location"), string(body)
}

// Two sign-ins started in one browser (a retry, a copied link opened
// alongside the client's tab, a prefetch) both complete.
func TestTwoSignInsOneBrowser(t *testing.T) {
	e := newTestEnv(t, true, nil)
	id := e.register()
	b := newBrowser(t)
	v := rand.Text() + rand.Text()
	formA := consentForm(t, b, e.authorizeURL(id, v, nil))
	formB := consentForm(t, b, e.authorizeURL(id, v, nil))
	googleA, pageA := approve(t, b, e.srv.URL, formA)
	googleB, pageB := approve(t, b, e.srv.URL, formB)
	if googleA == "" || googleB == "" {
		t.Fatalf("consent refused: A %q, B %q", pageA, pageB)
	}
	for _, g := range []string{googleA, googleB} {
		loc, page := b.visit(g)
		if loc == nil || loc.Query().Get("code") == "" {
			t.Errorf("sign-in %s: redirect %v, page %q", g, loc, page)
		}
	}
}

// Each reason a flow can't continue is named on the page.
func TestSignInFailureReasons(t *testing.T) {
	e := newTestEnv(t, true, nil)
	id := e.register()
	v := rand.Text() + rand.Text()

	victim := newBrowser(t)
	form := consentForm(t, newBrowser(t), e.authorizeURL(id, v, nil))
	_, noCookie := approve(t, victim, e.srv.URL, form)

	// The victim has a sign-in of its own, so a cookie, but not this one.
	consentForm(t, victim, e.authorizeURL(id, v, nil))
	_, otherFlow := approve(t, victim, e.srv.URL, form)

	_, unreadable := approve(t, victim, e.srv.URL, "kg1.k1.AAAA")

	old, err := e.as.keys.seal(purposeConsent, browserBound{Req: authRequest{ClientID: id}, Exp: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	_, expired := approve(t, victim, e.srv.URL, old)

	// The Google callback reports the same way.
	resp, err := victim.hc.Get(e.srv.URL + "/callback?state=garbage&code=x")
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	for name, tc := range map[string]struct{ page, want string }{
		"no cookie":  {noCookie, whyNoCookie},
		"other flow": {otherFlow, whyOtherFlow},
		"unreadable": {unreadable, whyUnreadable},
		"expired":    {expired, whyExpired},
		"callback":   {string(cb), whyUnreadable},
	} {
		if !strings.Contains(tc.page, html.EscapeString(tc.want)) {
			t.Errorf("%s: page %q, want %q", name, tc.page, tc.want)
		}
	}
}

// On https the flow cookies carry the __Host- prefix, so they can't be
// planted over http or from another host; bindBrowser reuses only those.
func TestFlowCookiesHostPrefix(t *testing.T) {
	s := &Server{cfg: Config{Issuer: "https://kg.example"}}
	planted := strings.Repeat("A", 26)
	r := httptest.NewRequest(http.MethodGet, "https://kg.example/authorize", nil)
	r.AddCookie(&http.Cookie{Name: cookieBrowser, Value: planted})
	w := httptest.NewRecorder()
	if got := s.bindBrowser(w, r); got == hashCookie(planted) {
		t.Error("bindBrowser reused a cookie without the __Host- prefix")
	}
	set := w.Result().Cookies()
	if len(set) != 1 || set[0].Name != "__Host-"+cookieBrowser || !set[0].Secure || set[0].Path != "/" || set[0].Domain != "" {
		t.Fatalf("Set-Cookie %+v, want a Secure __Host- cookie on /", set)
	}

	r.AddCookie(&http.Cookie{Name: "__Host-" + cookieBrowser, Value: planted})
	if got := s.bindBrowser(httptest.NewRecorder(), r); got != hashCookie(planted) {
		t.Error("bindBrowser didn't reuse the browser's __Host- cookie")
	}
}

// A malformed binding cookie isn't reused, and approving the consent
// page renews the cookie for the Google leg.
func TestBindingCookieHygiene(t *testing.T) {
	e := newTestEnv(t, true, nil)
	r := httptest.NewRequest(http.MethodGet, "/authorize", nil)
	r.AddCookie(&http.Cookie{Name: cookieBrowser, Value: "short"})
	if got := e.as.bindBrowser(httptest.NewRecorder(), r); got == hashCookie("short") {
		t.Error("bindBrowser reused a malformed cookie")
	}

	id := e.register()
	b := newBrowser(t)
	form := consentForm(t, b, e.authorizeURL(id, rand.Text()+rand.Text(), nil))
	resp, err := noFollow(b).PostForm(e.srv.URL+"/authorize", url.Values{"request": {form}, "action": {"approve"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	renewed := false
	for _, c := range resp.Cookies() {
		renewed = renewed || (c.Name == cookieBrowser && c.MaxAge > 0)
	}
	if !renewed {
		t.Errorf("approve didn't renew the binding cookie: %v", resp.Cookies())
	}
}
