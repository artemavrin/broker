package integration

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/artemavrin/broker/internal/admin"
)

const adminToken = "test-admin-token-1234567890"

var reRID = regexp.MustCompile(`<code id="rid">([^<]+)</code>`)
var reSEC = regexp.MustCompile(`<code id="rsec">([^<]+)</code>`)

// adminServer mounts the dashboard over the same service/signer/hub the env
// uses, so secrets minted in the UI authenticate against the participant API.
func (e *testEnv) adminServer(t testing.TB) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	dash := admin.New(e.svc, e.signer, e.hub, adminToken, time.Hour, log)
	srv := httptest.NewServer(dash.Routes())
	t.Cleanup(srv.Close)
	return srv
}

// TestAdminOnboardInitiator exercises the headline flow: sign in, create an
// initiator from the dashboard, and confirm the one-time secret it reveals
// actually authenticates — then revoke it and confirm it stops working.
func TestAdminOnboardInitiator(t *testing.T) {
	e := newEnv(t)
	dash := e.adminServer(t)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // don't auto-follow; assert on 303s
		},
	}

	// Unauthenticated dashboard redirects to login.
	if code := get(t, client, dash.URL+"/admin/"); code != http.StatusSeeOther {
		t.Fatalf("unauthed /admin/ expected 303, got %d", code)
	}

	// Wrong token is rejected.
	if code := postForm(t, client, dash.URL+"/admin/login", url.Values{"token": {"nope"}}); code != http.StatusUnauthorized {
		t.Fatalf("bad login expected 401, got %d", code)
	}

	// Correct token logs in and sets the session cookie.
	if code := postForm(t, client, dash.URL+"/admin/login", url.Values{"token": {adminToken}}); code != http.StatusSeeOther {
		t.Fatalf("login expected 303, got %d", code)
	}

	// Create an initiator (Post/Redirect/Get).
	if code := postForm(t, client, dash.URL+"/admin/initiators", nil); code != http.StatusSeeOther {
		t.Fatalf("create initiator expected 303, got %d", code)
	}

	// The redirected-to page reveals the id and one-time secret exactly once.
	body := getBody(t, client, dash.URL+"/admin/initiators")
	rid := match(t, reRID, body, "initiator id")
	rsec := match(t, reSEC, body, "initiator secret")

	// The revealed secret authenticates against the participant API.
	if code := postJSON(t, client, e.server.URL+"/v1/auth/token", `{"secret":"`+rsec+`"}`); code != http.StatusOK {
		t.Fatalf("revealed secret should authenticate, got %d", code)
	}

	// Refreshing the page must NOT show the secret again (single-use reveal).
	if reSEC.MatchString(getBody(t, client, dash.URL+"/admin/initiators")) {
		t.Fatalf("secret should only be revealed once")
	}

	// Revoke the initiator from the dashboard.
	if code := postForm(t, client, dash.URL+"/admin/initiators/"+rid+"/revoke", nil); code != http.StatusSeeOther {
		t.Fatalf("revoke expected 303, got %d", code)
	}

	// After revocation the same secret no longer authenticates.
	if code := postJSON(t, client, e.server.URL+"/v1/auth/token", `{"secret":"`+rsec+`"}`); code != http.StatusUnauthorized {
		t.Fatalf("revoked initiator secret should be rejected, got %d", code)
	}
}

// ---- tiny HTTP helpers ----

func get(t testing.TB, c *http.Client, u string) int {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("get %s: %v", u, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func getBody(t testing.TB, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("get %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func postForm(t testing.TB, c *http.Client, u string, form url.Values) int {
	t.Helper()
	resp, err := c.Post(u, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("post %s: %v", u, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func postJSON(t testing.TB, c *http.Client, u, body string) int {
	t.Helper()
	resp, err := c.Post(u, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", u, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func match(t testing.TB, re *regexp.Regexp, body, what string) string {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if len(m) < 2 || m[1] == "" {
		t.Fatalf("could not find %s in response", what)
	}
	return m[1]
}
