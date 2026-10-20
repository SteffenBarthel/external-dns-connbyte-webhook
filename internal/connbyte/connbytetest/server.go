// Package connbytetest provides an in-memory mock of the ConnByte DNS API for tests.
// It follows the response format of https://api.connbyte.com/ (status/data/field).
package connbytetest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
)

// Record is a record stored by the mock. Name is the FQDN, as the real API returns it.
type Record struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	TTL      int    `json:"ttl"`
	Priority int    `json:"priority"`
	Disabled int    `json:"disabled"`
}

// Call is one request received by the mock.
type Call struct {
	Path string
	Body map[string]any
}

// Server is a mock ConnByte API.
type Server struct {
	*httptest.Server
	Token string

	mu      sync.Mutex
	zones   map[string]string
	records map[string][]Record
	nextID  int64
	calls   []Call
	// FailPath makes requests to this path fail with FailStatus.
	FailPath   string
	FailStatus int
	// FailTimes limits how many requests fail; 0 means all of them.
	FailTimes int
	failed    int
}

// New starts a mock server that accepts the given bearer token.
func New(token string) *Server {
	s := &Server{Token: token, zones: map[string]string{}, records: map[string][]Record{}, nextID: 100}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// AddZone adds a zone.
func (s *Server) AddZone(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.zones[id] = name
}

// AddRecord stores a record in a zone and returns its ID. name is relative ("" is the apex).
func (s *Server) AddRecord(zoneID, name, typ, content string, ttl, priority int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(zoneID, name, typ, content, ttl, priority)
}

func (s *Server) add(zoneID, name, typ, content string, ttl, priority int) int64 {
	fqdn := s.zones[zoneID]
	if name != "" && name != "@" {
		fqdn = name + "." + fqdn
	}
	if typ == "TXT" && !strings.HasPrefix(content, `"`) {
		content = `"` + content + `"` // the real backend returns TXT content quoted
	}
	s.nextID++
	s.records[zoneID] = append(s.records[zoneID], Record{ID: s.nextID, Name: fqdn, Type: typ, Content: content, TTL: ttl, Priority: priority})
	return s.nextID
}

// Disable marks a record as disabled.
func (s *Server) Disable(zoneID string, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records[zoneID] {
		if s.records[zoneID][i].ID == id {
			s.records[zoneID][i].Disabled = 1
		}
	}
}

// Records returns a sorted copy of the records of a zone.
func (s *Server) Records(zoneID string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Record(nil), s.records[zoneID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Calls returns the requests received so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CountCalls returns how many requests were sent to path.
func (s *Server) CountCalls(path string) int {
	n := 0
	for _, c := range s.Calls() {
		if c.Path == path {
			n++
		}
	}
	return n
}

// ResetCalls clears the call log.
func (s *Server) ResetCalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

func status(code int, path string) map[string]any {
	title, msg := "OK", "Data were transferred."
	if code != http.StatusOK {
		title, msg = http.StatusText(code), "Transferred data are incorrect."
	}
	return map[string]any{
		"code": code, "title": title, "message": msg, "id": "01DXF6DT00ZZZHQPP3JFJS8VEE",
		"titleOriginal": title, "messageOriginal": msg, "logId": "01KQW9AVCV3K0M23ERMC84RG9N",
		"path": path, "time": 1775425496, "version": "4.6.0",
	}
}

func reply(w http.ResponseWriter, code int, path string, data any, field []map[string]string) {
	body := map[string]any{"status": status(code, path)}
	if data != nil {
		body["data"] = data
	}
	if field != nil {
		body["field"] = field
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := r.URL.Path
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reply(w, 400, path, nil, []map[string]string{{"body": "invalid json"}})
		return
	}
	s.calls = append(s.calls, Call{Path: path, Body: body})

	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		reply(w, 400, path, nil, []map[string]string{{"authorization": "'Authorization' must be set."}})
		return
	}
	if r.Method != http.MethodPost {
		reply(w, 404, path, nil, nil)
		return
	}
	if s.FailPath == path && (s.FailTimes == 0 || s.failed < s.FailTimes) {
		s.failed++
		reply(w, s.FailStatus, path, nil, nil)
		return
	}

	zoneID, _ := body["zoneId"].(string)
	_, zoneOK := s.zones[zoneID]

	switch path {
	case "/ns/zone/list":
		ids := make([]string, 0, len(s.zones))
		for id := range s.zones {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out := []map[string]any{}
		for _, id := range ids {
			out = append(out, map[string]any{"zone": map[string]any{
				"id": id, "projectId": "01P", "title": "", "zone": s.zones[id],
				"createdAt": "2026-04-05T21:43:40.000Z", "updatedAt": nil,
			}})
		}
		reply(w, 200, path, out, nil)
	case "/ns/record/list":
		if !zoneOK {
			reply(w, 400, path, nil, []map[string]string{{"zoneId": "Zone not found."}})
			return
		}
		out := []map[string]any{}
		for _, rec := range s.records[zoneID] {
			out = append(out, map[string]any{"record": rec})
		}
		reply(w, 200, path, out, nil)
	case "/ns/record/create":
		if !zoneOK {
			reply(w, 400, path, nil, []map[string]string{{"zoneId": "Zone not found."}})
			return
		}
		for _, k := range []string{"name", "type", "content", "ttl", "priority", "disabled"} {
			if _, ok := body[k]; !ok {
				reply(w, 400, path, nil, []map[string]string{{k: "Field must be set."}})
				return
			}
		}
		ttl := int(body["ttl"].(float64))
		if ttl < 60 || ttl > 86400 {
			reply(w, 400, path, nil, []map[string]string{{"ttl": "Value is out of range."}})
			return
		}
		name, _ := body["name"].(string)
		typ, _ := body["type"].(string)
		content, _ := body["content"].(string)
		id := s.add(zoneID, name, typ, content, ttl, int(body["priority"].(float64)))
		fqdn := s.records[zoneID][len(s.records[zoneID])-1].Name
		reply(w, 200, path, map[string]any{"id": id, "record": fqdn}, nil)
	case "/ns/record/delete":
		if !zoneOK {
			reply(w, 400, path, nil, []map[string]string{{"zoneId": "Zone not found."}})
			return
		}
		id, _ := body["recordId"].(float64)
		recs := s.records[zoneID]
		for i, rec := range recs {
			if rec.ID == int64(id) {
				s.records[zoneID] = append(recs[:i], recs[i+1:]...)
				reply(w, 200, path, map[string]any{}, nil)
				return
			}
		}
		reply(w, 400, path, nil, []map[string]string{{"recordId": "Record not found."}})
	default:
		reply(w, 404, path, nil, nil)
	}
}
