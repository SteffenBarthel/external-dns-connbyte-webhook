package connbyte_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/connbyte"
	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/connbyte/connbytetest"
)

func TestClientRoundTrip(t *testing.T) {
	srv := connbytetest.New("secret")
	defer srv.Close()
	srv.AddZone("Z1", "example.com")
	ctx := context.Background()

	c, err := connbyte.New(srv.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}

	zones, err := c.ListZones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 1 || zones[0].ID != "Z1" || zones[0].Zone != "example.com" {
		t.Fatalf("unexpected zones: %+v", zones)
	}

	id, err := c.CreateRecord(ctx, connbyte.RecordInput{ZoneID: "Z1", Name: "www", Type: "A", Content: "192.0.2.1", TTL: 300})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := c.ListRecords(ctx, "Z1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].ID != id || recs[0].Name != "www.example.com" || recs[0].IsDisabled() {
		t.Fatalf("unexpected records: %+v", recs)
	}

	if err := c.DeleteRecord(ctx, "Z1", id); err != nil {
		t.Fatal(err)
	}
	if got := srv.Records("Z1"); len(got) != 0 {
		t.Fatalf("record not deleted: %+v", got)
	}

	call := srv.Calls()[1]
	if call.Path != "/ns/record/create" || call.Body["zoneId"] != "Z1" || call.Body["disabled"] != false {
		t.Fatalf("unexpected create body: %+v", call)
	}
}

func TestClientAPIError(t *testing.T) {
	srv := connbytetest.New("secret")
	defer srv.Close()

	c, err := connbyte.New(srv.URL, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListZones(context.Background())
	var apiErr *connbyte.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %v", err)
	}
	if apiErr.HTTPStatus != 400 || apiErr.Temporary() {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
	if !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("field details missing: %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("token leaked into error: %v", err)
	}
}

func TestClientServerError(t *testing.T) {
	srv := connbytetest.New("secret")
	defer srv.Close()
	srv.FailPath, srv.FailStatus = "/ns/zone/list", 503

	c, _ := connbyte.New(srv.URL, "secret")
	_, err := c.ListZones(context.Background())
	var apiErr *connbyte.APIError
	if !errors.As(err, &apiErr) || !apiErr.Temporary() {
		t.Fatalf("want temporary APIError, got %v", err)
	}
}

func TestNewRequiresToken(t *testing.T) {
	if _, err := connbyte.New("", " "); err == nil {
		t.Fatal("expected error for empty token")
	}
}
