package auth

import (
    "crypto/sha256"
    "encoding/hex"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
)

func mustParse(t *testing.T, list, single string) *Keys {
    t.Helper()
    ks, err := Parse(list, single)
    if err != nil { t.Fatal(err) }
    return ks
}

func status(ks *Keys, scope Scope, channel, header, value string) int {
    h := ks.Require(scope, func(*http.Request) string { return channel })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
    req := httptest.NewRequest("GET", "/x", nil)
    if header != "" { req.Header.Set(header, value) }
    rr := httptest.NewRecorder()
    h.ServeHTTP(rr, req)
    return rr.Code
}

func TestScopesAndChannels(t *testing.T) {
    ks := mustParse(t, "ops=opsecret1:schedule+command+read:c1+c2; viewer=viewsecret:read:*; boss=bosssecret:admin:*", "")
    cases := []struct{ name string; scope Scope; ch, key string; want int }{
        {"ops may schedule c1", Schedule, "c1", "opsecret1", 204},
        {"ops may command c2", Command, "c2", "opsecret1", 204},
        {"ops may not touch c3", Schedule, "c3", "opsecret1", 403},
        {"ops may not administer", Admin, "", "opsecret1", 403},
        {"viewer may read any channel", Read, "anything", "viewsecret", 204},
        {"viewer may not schedule", Schedule, "c1", "viewsecret", 403},
        {"viewer may not command", Command, "c1", "viewsecret", 403},
        {"admin includes every scope", Command, "c9", "bosssecret", 204},
        {"admin may administer", Admin, "", "bosssecret", 204},
        {"a wrong key is 401, not 403", Read, "c1", "nope-nope", 401},
    }
    for _, c := range cases {
        if got := status(ks, c.scope, c.ch, "X-API-Key", c.key); got != c.want { t.Errorf("%s: got %d, want %d", c.name, got, c.want) }
    }
    if got := status(ks, Read, "c1", "", ""); got != 401 { t.Errorf("no key: got %d", got) }
}

func TestBearerHeaderWorksToo(t *testing.T) {
    ks := mustParse(t, "ops=opsecret1:read:*", "")
    if got := status(ks, Read, "", "Authorization", "Bearer opsecret1"); got != 204 { t.Fatalf("bearer: %d", got) }
    if got := status(ks, Read, "", "Authorization", "Basic opsecret1"); got != 401 { t.Fatalf("basic must not work: %d", got) }
}

func TestSingleOldKeyIsAdminForEverything(t *testing.T) {
    ks := mustParse(t, "", "the-old-key")
    for _, s := range []Scope{Read, Schedule, Command, Admin} {
        if got := status(ks, s, "any", "X-API-Key", "the-old-key"); got != 204 { t.Errorf("%s: %d", s, got) }
    }
    if ks.Find("the-old-key").Name != "default" { t.Fatal("the old key is named default") }
}

func TestSecretMayBeGivenAsAHash(t *testing.T) {
    sum := sha256.Sum256([]byte("a-long-secret"))
    ks := mustParse(t, "svc=sha256:"+hex.EncodeToString(sum[:])+":read:*", "")
    if got := status(ks, Read, "", "X-API-Key", "a-long-secret"); got != 204 { t.Fatalf("the plain secret must match its hash: %d", got) }
    if got := status(ks, Read, "", "X-API-Key", hex.EncodeToString(sum[:])); got != 401 { t.Fatalf("the hash itself is not the secret: %d", got) }
}

func TestBadConfigurationIsRefused(t *testing.T) {
    bad := map[string]string{
        "no keys":          "",
        "no secret":        "a=:read:*",
        "short secret":     "a=short:read:*",
        "unknown scope":    "a=longsecret:fly:*",
        "no scopes":        "a=longsecret::*",
        "no channels":      "a=longsecret:read:",
        "missing parts":    "a=longsecret:read",
        "bad hash":         "a=sha256:xyz:read:*",
        "duplicate names":  "a=longsecret1:read:*;a=longsecret2:read:*",
    }
    for name, list := range bad {
        if _, err := Parse(list, ""); err == nil { t.Errorf("%s: %q must be refused", name, list) }
    }
}

func TestAnAnswerNeverContainsTheSecret(t *testing.T) {
    ks := mustParse(t, "ops=opsecret1:read:c1", "")
    h := ks.Require(Schedule, func(*http.Request) string { return "c1" })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
    req := httptest.NewRequest("GET", "/x", nil)
    req.Header.Set("X-API-Key", "opsecret1")
    rr := httptest.NewRecorder()
    h.ServeHTTP(rr, req)
    if rr.Code != 403 || strings.Contains(rr.Body.String(), "opsecret1") { t.Fatalf("%d %s", rr.Code, rr.Body.String()) }
}

func TestTheKeyIsAvailableToTheHandler(t *testing.T) {
    ks := mustParse(t, "ops=opsecret1:read:*", "")
    var name string
    h := ks.Require(Read, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { name = From(r.Context()).Name }))
    req := httptest.NewRequest("GET", "/x", nil)
    req.Header.Set("X-API-Key", "opsecret1")
    h.ServeHTTP(httptest.NewRecorder(), req)
    if name != "ops" { t.Fatalf("name=%q", name) }
}
