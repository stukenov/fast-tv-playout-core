package main

import (
    "fmt"
    "net/http"
    "os"
    "sort"
    "strings"
    "testing"

    "github.com/go-chi/chi/v5"
    "gopkg.in/yaml.v3"

    "ftvx/pkg/auth"
    "ftvx/pkg/logger"
)

type spec struct {
    Paths map[string]map[string]yaml.Node `yaml:"paths"`
}

type operation struct {
    Service []string `yaml:"x-service"`
    Scope   string   `yaml:"x-scope"`
}

// specOperations lists "METHOD path" of the operations that a service implements, with their scopes.
func specOperations(t *testing.T, service string) (map[string]string, error) {
    t.Helper()
    raw, err := os.ReadFile("../../api/openapi.yaml")
    if err != nil { return nil, err }
    var s spec
    if err := yaml.Unmarshal(raw, &s); err != nil { return nil, err }
    out := map[string]string{}
    for path, item := range s.Paths {
        for method, node := range item {
            if !isMethod(method) { continue }
            var op operation
            if err := node.Decode(&op); err != nil { return nil, err }
            for _, sv := range op.Service {
                if sv == service { out[strings.ToUpper(method)+" "+path] = op.Scope }
            }
        }
    }
    return out, nil
}

func isMethod(m string) bool {
    switch m { case "get", "post", "put", "patch", "delete", "head", "options": return true }
    return false
}

func routerOperations(t *testing.T, h http.Handler) map[string]bool {
    t.Helper()
    got := map[string]bool{}
    err := chi.Walk(h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
        got[method+" "+strings.TrimSuffix(route, "/")] = true
        return nil
    })
    if err != nil { t.Fatal(err) }
    return got
}

// The description of the API and the router say the same: nothing that is routed is undocumented, nothing documented is missing.
func TestOpenAPI_DescribesExactlyTheRoutesOfControlAPI(t *testing.T) {
    keys, _ := auth.Parse("", "some-key-1234")
    h := newServer(nil, keys, logger.Setup(), 0).routes()
    routed := routerOperations(t, h)
    documented, err := specOperations(t, "control-api")
    if err != nil { t.Fatal(err) }
    var missing, extra []string
    for op := range routed { if _, ok := documented[op]; !ok { missing = append(missing, op) } }
    for op := range documented { if !routed[op] { extra = append(extra, op) } }
    sort.Strings(missing)
    sort.Strings(extra)
    if len(missing) > 0 { t.Errorf("routes that api/openapi.yaml does not describe: %v", missing) }
    if len(extra) > 0 { t.Errorf("operations in api/openapi.yaml that control-api does not have: %v", extra) }
    if len(routed) < 10 { t.Fatalf("only %d routes were found: the test proves nothing", len(routed)) }
}

// Every documented operation names its scope, and it is one that exists.
func TestOpenAPI_EveryProtectedOperationNamesAScope(t *testing.T) {
    ops, err := specOperations(t, "control-api")
    if err != nil { t.Fatal(err) }
    open := map[string]bool{"GET /healthz": true, "GET /readyz": true, "GET /metrics": true}
    for op, scope := range ops {
        switch {
        case open[op] && scope != "": t.Errorf("%s is open and must not name a scope", op)
        case !open[op] && scope != "read" && scope != "schedule" && scope != "command" && scope != "admin": t.Errorf("%s: scope %q", op, scope)
        }
    }
    _ = fmt.Sprint
}
