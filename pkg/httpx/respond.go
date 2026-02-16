package httpx

import (
    "encoding/json"
    "io"
    "net/http"
)

type Problem struct {
    Type   string `json:"type,omitempty"`
    Title  string `json:"title"`
    Status int    `json:"status"`
    Detail string `json:"detail,omitempty"`
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(v)
}

func WriteProblem(w http.ResponseWriter, status int, title, detail string) {
    w.Header().Set("Content-Type", "application/problem+json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(Problem{Title: title, Status: status, Detail: detail})
}

func DecodeJSON(r *http.Request, dst any) error {
    defer r.Body.Close()
    dec := json.NewDecoder(io.LimitReader(r.Body, 10<<20)) // 10MB
    dec.DisallowUnknownFields()
    return dec.Decode(dst)
}


