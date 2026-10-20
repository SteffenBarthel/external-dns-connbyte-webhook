package provider_test

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/connbyte"
	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/connbyte/connbytetest"
	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/provider"
	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/webhook"
)

func setup(t *testing.T, cfg provider.Config) (*connbytetest.Server, *provider.Provider) {
	t.Helper()
	srv := connbytetest.New("secret")
	t.Cleanup(srv.Close)
	srv.AddZone("Z1", "example.com")
	srv.AddZone("Z2", "sub.example.com")
	srv.AddZone("Z3", "other.org")
	c, err := connbyte.New(srv.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	return srv, provider.New(c, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func ep(name, typ string, ttl int64, targets ...string) *webhook.Endpoint {
	return &webhook.Endpoint{DNSName: name, RecordType: typ, RecordTTL: ttl, Targets: targets}
}

func TestRecords(t *testing.T) {
	srv, p := setup(t, provider.Config{DomainFilter: []string{"example.com"}})
	srv.AddRecord("Z1", "", "A", "192.0.2.1", 3600, 0)
	srv.AddRecord("Z1", "www", "A", "192.0.2.2", 300, 0)
	srv.AddRecord("Z1", "www", "A", "192.0.2.3", 300, 0)
	srv.AddRecord("Z1", "", "MX", "mail.example.com.", 3600, 10)
	srv.AddRecord("Z1", "a-www", "TXT", "heritage=external-dns,external-dns/owner=default", 300, 0)
	srv.AddRecord("Z1", "", "SOA", "ns1.example.com. hostmaster.example.com. 1 2 3 4 5", 3600, 0)
	srv.AddRecord("Z2", "api", "CNAME", "lb.example.net.", 600, 0)
	srv.AddRecord("Z3", "www", "A", "198.51.100.1", 300, 0)

	got, err := p.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*webhook.Endpoint{
		"example.com/A":             ep("example.com", "A", 3600, "192.0.2.1"),
		"www.example.com/A":         ep("www.example.com", "A", 300, "192.0.2.2", "192.0.2.3"),
		"example.com/MX":            ep("example.com", "MX", 3600, "10 mail.example.com"),
		"a-www.example.com/TXT":     ep("a-www.example.com", "TXT", 300, "heritage=external-dns,external-dns/owner=default"),
		"api.sub.example.com/CNAME": ep("api.sub.example.com", "CNAME", 600, "lb.example.net"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d endpoints, want %d: %+v", len(got), len(want), got)
	}
	for _, e := range got {
		w, ok := want[e.DNSName+"/"+e.RecordType]
		if !ok || !reflect.DeepEqual(e, w) {
			t.Errorf("unexpected endpoint %+v", e)
		}
	}
}

func TestApplyCreateAndIdempotent(t *testing.T) {
	srv, p := setup(t, provider.Config{DomainFilter: []string{"example.com"}})
	ctx := context.Background()
	changes := &webhook.Changes{Create: []*webhook.Endpoint{
		ep("www.example.com", "A", 0, "192.0.2.10", "192.0.2.11"),
		ep("example.com", "MX", 3600, "10 mail.example.com"),
		ep("a-www.example.com", "TXT", 300, `"heritage=external-dns,external-dns/owner=default"`),
		ep("api.sub.example.com", "CNAME", 30, "lb.example.net"),
	}}
	if err := p.ApplyChanges(ctx, changes); err != nil {
		t.Fatal(err)
	}
	z1 := srv.Records("Z1")
	if len(z1) != 4 {
		t.Fatalf("want 4 records in Z1, got %+v", z1)
	}
	for _, r := range z1 {
		switch r.Type {
		case "A":
			if r.Name != "www.example.com" || r.TTL != 300 {
				t.Errorf("bad A record %+v", r)
			}
		case "MX":
			if r.Name != "example.com" || r.Content != "mail.example.com" || r.Priority != 10 {
				t.Errorf("bad MX record %+v", r)
			}
		case "TXT":
			if r.Content != `"heritage=external-dns,external-dns/owner=default"` {
				t.Errorf("TXT double quoted: %+v", r)
			}
		}
	}
	z2 := srv.Records("Z2")
	if len(z2) != 1 || z2[0].Name != "api.sub.example.com" || z2[0].TTL != 60 {
		t.Fatalf("CNAME must go to the most specific zone with clamped TTL: %+v", z2)
	}
	for _, c := range srv.Calls() {
		if c.Path == "/ns/record/create" && c.Body["zoneId"] == "Z2" && c.Body["name"] != "api" {
			t.Errorf("name must be relative to the zone: %+v", c.Body)
		}
	}

	srv.ResetCalls()
	if err := p.ApplyChanges(ctx, changes); err != nil {
		t.Fatal(err)
	}
	if n := srv.CountCalls("/ns/record/create"); n != 0 {
		t.Fatalf("second apply created %d duplicate records", n)
	}
}

func TestApplyUpdateAndDelete(t *testing.T) {
	srv, p := setup(t, provider.Config{DomainFilter: []string{"example.com"}})
	ctx := context.Background()
	keep := srv.AddRecord("Z1", "www", "A", "192.0.2.1", 300, 0)
	srv.AddRecord("Z1", "www", "A", "192.0.2.2", 300, 0)
	srv.AddRecord("Z1", "old", "CNAME", "x.example.net.", 300, 0)
	foreign := srv.AddRecord("Z1", "www", "AAAA", "2001:db8::1", 300, 0)

	err := p.ApplyChanges(ctx, &webhook.Changes{
		UpdateOld: []*webhook.Endpoint{ep("www.example.com", "A", 300, "192.0.2.1", "192.0.2.2")},
		UpdateNew: []*webhook.Endpoint{ep("www.example.com", "A", 300, "192.0.2.1", "192.0.2.3")},
		Delete:    []*webhook.Endpoint{ep("old.example.com", "CNAME", 300, "x.example.net")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	ids := map[int64]bool{}
	for _, r := range srv.Records("Z1") {
		ids[r.ID] = true
		if r.Type == "A" {
			targets = append(targets, r.Content)
		}
		if r.Type == "CNAME" {
			t.Errorf("CNAME not deleted: %+v", r)
		}
	}
	if !reflect.DeepEqual(targets, []string{"192.0.2.1", "192.0.2.3"}) {
		t.Fatalf("unexpected A targets %v", targets)
	}
	if !ids[keep] {
		t.Error("unchanged target was recreated instead of kept")
	}
	if !ids[foreign] {
		t.Error("record of another type was deleted")
	}

	srv.ResetCalls()
	err = p.ApplyChanges(ctx, &webhook.Changes{Delete: []*webhook.Endpoint{ep("old.example.com", "CNAME", 300, "x.example.net")}})
	if err != nil {
		t.Fatalf("deleting a missing record must be a no-op: %v", err)
	}
	if n := srv.CountCalls("/ns/record/delete"); n != 0 {
		t.Fatalf("delete sent %d times for a missing record", n)
	}
}

func TestApplyTTLChange(t *testing.T) {
	srv, p := setup(t, provider.Config{})
	srv.AddRecord("Z1", "www", "A", "192.0.2.1", 300, 0)
	err := p.ApplyChanges(context.Background(), &webhook.Changes{
		UpdateOld: []*webhook.Endpoint{ep("www.example.com", "A", 300, "192.0.2.1")},
		UpdateNew: []*webhook.Endpoint{ep("www.example.com", "A", 900, "192.0.2.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	recs := srv.Records("Z1")
	if len(recs) != 1 || recs[0].TTL != 900 {
		t.Fatalf("TTL not updated: %+v", recs)
	}
}

func TestDomainFilterProtectsOtherZones(t *testing.T) {
	srv, p := setup(t, provider.Config{DomainFilter: []string{"example.com"}, ExcludeDomains: []string{"sub.example.com"}})
	srv.AddRecord("Z3", "www", "A", "198.51.100.1", 300, 0)
	srv.AddRecord("Z2", "api", "A", "198.51.100.2", 300, 0)

	err := p.ApplyChanges(context.Background(), &webhook.Changes{
		Create: []*webhook.Endpoint{ep("evil.other.org", "A", 300, "203.0.113.1")},
		Delete: []*webhook.Endpoint{
			ep("www.other.org", "A", 300, "198.51.100.1"),
			ep("api.sub.example.com", "A", 300, "198.51.100.2"),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "outside the domain filter") {
		t.Fatalf("expected domain filter error, got %v", err)
	}
	if n := srv.CountCalls("/ns/record/delete") + srv.CountCalls("/ns/record/create"); n != 0 {
		t.Fatalf("%d write calls outside the domain filter", n)
	}
	if len(srv.Records("Z3")) != 1 || len(srv.Records("Z2")) != 1 {
		t.Fatal("records outside the filter were changed")
	}
	if got := p.DomainFilter(); !reflect.DeepEqual(got, webhook.DomainFilter{Include: []string{"example.com"}, Exclude: []string{"sub.example.com"}}) {
		t.Fatalf("unexpected domain filter %+v", got)
	}
}

func TestDryRun(t *testing.T) {
	srv, p := setup(t, provider.Config{DryRun: true})
	srv.AddRecord("Z1", "www", "A", "192.0.2.1", 300, 0)
	err := p.ApplyChanges(context.Background(), &webhook.Changes{
		Create: []*webhook.Endpoint{ep("new.example.com", "A", 300, "192.0.2.5")},
		Delete: []*webhook.Endpoint{ep("www.example.com", "A", 300, "192.0.2.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := srv.CountCalls("/ns/record/delete") + srv.CountCalls("/ns/record/create"); n != 0 {
		t.Fatalf("dry run sent %d write calls", n)
	}
}

func TestApplyPartialFailure(t *testing.T) {
	srv, p := setup(t, provider.Config{})
	srv.FailPath, srv.FailStatus = "/ns/record/create", 500
	err := p.ApplyChanges(context.Background(), &webhook.Changes{
		Create: []*webhook.Endpoint{ep("a.example.com", "A", 300, "192.0.2.1"), ep("b.example.com", "A", 300, "192.0.2.2")},
	})
	if err == nil || strings.Count(err.Error(), "create A") != 2 {
		t.Fatalf("expected both errors reported, got %v", err)
	}
}

func TestAdjustEndpoints(t *testing.T) {
	_, p := setup(t, provider.Config{})
	in := []*webhook.Endpoint{
		ep("WWW.Example.com.", "a", 10, "192.0.2.1"),
		ep("example.com", "MX", 100000, "10 mail.example.com."),
		ep("txt.example.com", "TXT", 0, `"v=spf1 -all"`),
		ep("x.example.com", "PTR", 300, "host.example.com"),
	}
	out, err := p.AdjustEndpoints(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []*webhook.Endpoint{
		ep("www.example.com", "A", 60, "192.0.2.1"),
		ep("example.com", "MX", 86400, "10 mail.example.com"),
		ep("txt.example.com", "TXT", 0, "v=spf1 -all"),
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("got %+v", out)
	}
}

func TestInvalidTargetsRejected(t *testing.T) {
	srv, p := setup(t, provider.Config{})
	err := p.ApplyChanges(context.Background(), &webhook.Changes{Create: []*webhook.Endpoint{
		ep("mx.example.com", "MX", 300, "mail.example.com"),
		ep("txt.example.com", "TXT", 300, "a\nb"),
	}})
	if err == nil {
		t.Fatal("expected error")
	}
	if n := srv.CountCalls("/ns/record/create"); n != 0 {
		t.Fatalf("invalid targets were sent: %d", n)
	}
}
