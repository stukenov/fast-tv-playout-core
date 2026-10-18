// Package auth gives the HTTP APIs keys with scopes: a key may be limited to some actions and to some channels.
package auth

import (
    "context"
    "crypto/sha256"
    "crypto/subtle"
    "encoding/hex"
    "errors"
    "fmt"
    "net/http"
    "strings"

    "ftvx/pkg/httpx"
)

// Scope is a kind of action. Admin includes all the others.
type Scope string

const (
    Read     Scope = "read"     // look at channels, assets, schedule, status, events
    Schedule Scope = "schedule" // replace the schedule of a channel
    Command  Scope = "command"  // act on the air now: cue an ad break, skip, insert
    Admin    Scope = "admin"    // create and delete channels and assets, everything else
)

var allScopes = []Scope{Read, Schedule, Command, Admin}

// Key is one API key. The secret is kept only as a SHA-256 hash.
type Key struct {
    Name        string
    hash        [32]byte
    scopes      map[Scope]bool
    channels    map[string]bool
    allChannels bool
}

// Allows tells whether the key may do the action on the channel (channel "" means an action that is not about one channel).
func (k *Key) Allows(s Scope, channel string) bool {
    if !k.scopes[s] && !k.scopes[Admin] { return false }
    if channel == "" || k.allChannels { return true }
    return k.channels[channel]
}

// Keys is the set of keys of a service.
type Keys struct{ keys []*Key }

// Parse reads the key list and the older single key.
//
//	API_KEYS="ops=SECRET:schedule+command+read:c1+c2;viewer=SECRET2:read:*"
//	name=secret:scopes:channels   scopes joined by "+", "*" for all; channels joined by "+", "*" for all
//
// A secret may be given as "sha256:<hex>" so that the plain secret is not in the environment. A single
// API_KEY (the older way) is a key named "default" that may do everything.
func Parse(list, single string) (*Keys, error) {
    ks := &Keys{}
    for _, part := range strings.FieldsFunc(list, func(r rune) bool { return r == ';' || r == '\n' }) {
        part = strings.TrimSpace(part)
        if part == "" { continue }
        k, err := parseKey(part)
        if err != nil { return nil, err }
        ks.keys = append(ks.keys, k)
    }
    if single != "" {
        ks.keys = append(ks.keys, &Key{Name: "default", hash: sha256.Sum256([]byte(single)), scopes: map[Scope]bool{Admin: true}, allChannels: true})
    }
    if len(ks.keys) == 0 { return nil, errors.New("no API keys: set API_KEY or API_KEYS") }
    seen := map[string]bool{}
    for _, k := range ks.keys {
        if seen[k.Name] { return nil, fmt.Errorf("two keys are named %q", k.Name) }
        seen[k.Name] = true
    }
    return ks, nil
}

func parseKey(s string) (*Key, error) {
    name, rest, ok := strings.Cut(s, "=")
    if !ok || name == "" { return nil, fmt.Errorf("key %q: want name=secret:scopes:channels", s) }
    fields := strings.Split(rest, ":")
    // a sha256 secret has a colon of its own: "sha256:<hex>"
    var secret string
    if len(fields) > 0 && fields[0] == "sha256" && len(fields) >= 2 {
        secret, fields = "sha256:"+fields[1], fields[2:]
    } else {
        secret, fields = fields[0], fields[1:]
    }
    if len(fields) != 2 { return nil, fmt.Errorf("key %q: want name=secret:scopes:channels", name) }
    k := &Key{Name: strings.TrimSpace(name), scopes: map[Scope]bool{}, channels: map[string]bool{}}
    if strings.HasPrefix(secret, "sha256:") {
        raw, err := hex.DecodeString(strings.TrimPrefix(secret, "sha256:"))
        if err != nil || len(raw) != 32 { return nil, fmt.Errorf("key %q: sha256 secret must be 64 hex digits", name) }
        copy(k.hash[:], raw)
    } else {
        if len(secret) < 8 { return nil, fmt.Errorf("key %q: the secret must be at least 8 characters", name) }
        k.hash = sha256.Sum256([]byte(secret))
    }
    for _, sc := range strings.Split(fields[0], "+") {
        sc = strings.TrimSpace(sc)
        if sc == "*" { for _, a := range allScopes { k.scopes[a] = true }; continue }
        found := false
        for _, a := range allScopes { if string(a) == sc { k.scopes[a], found = true, true } }
        if !found { return nil, fmt.Errorf("key %q: unknown scope %q", name, sc) }
    }
    for _, ch := range strings.Split(fields[1], "+") {
        ch = strings.TrimSpace(ch)
        if ch == "*" { k.allChannels = true } else if ch != "" { k.channels[ch] = true }
    }
    if len(k.scopes) == 0 { return nil, fmt.Errorf("key %q has no scopes", name) }
    if !k.allChannels && len(k.channels) == 0 { return nil, fmt.Errorf("key %q has no channels", name) }
    return k, nil
}

// Find returns the key for the secret. All keys are compared, so the time does not tell which one was close.
func (ks *Keys) Find(secret string) *Key {
    if secret == "" { return nil }
    h := sha256.Sum256([]byte(secret))
    var found *Key
    for _, k := range ks.keys {
        if subtle.ConstantTimeCompare(h[:], k.hash[:]) == 1 { found = k }
    }
    return found
}

func secretOf(r *http.Request) string {
    if v := r.Header.Get("X-API-Key"); v != "" { return v }
    if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") { return strings.TrimSpace(strings.TrimPrefix(a, "Bearer ")) }
    return ""
}

type ctxKey struct{}

// From returns the key that was used for the request (nil outside Require).
func From(ctx context.Context) *Key {
    k, _ := ctx.Value(ctxKey{}).(*Key)
    return k
}

// Require is a middleware: 401 without a valid key, 403 when the key may not do this to this channel.
// channel extracts the channel of the request ("" for requests that are not about one channel).
func (ks *Keys) Require(s Scope, channel func(*http.Request) string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            k := ks.Find(secretOf(r))
            if k == nil {
                w.Header().Set("WWW-Authenticate", `Bearer realm="ftvx"`)
                httpx.WriteProblem(w, 401, "unauthorized", "a valid API key is required (X-API-Key or Authorization: Bearer)")
                return
            }
            ch := ""
            if channel != nil { ch = channel(r) }
            if !k.Allows(s, ch) {
                httpx.WriteProblem(w, 403, "forbidden", fmt.Sprintf("this key may not do %q on %s", s, describe(ch)))
                return
            }
            next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, k)))
        })
    }
}

func describe(ch string) string {
    if ch == "" { return "this resource" }
    return "channel " + ch
}
