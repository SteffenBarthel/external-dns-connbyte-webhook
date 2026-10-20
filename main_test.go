package main

import (
	"testing"
	"time"
)

func TestCheckAPIURL(t *testing.T) {
	for _, u := range []string{"", "https://api.connbyte.com", "http://127.0.0.1:18080", "http://localhost:8080", "http://[::1]:80"} {
		if err := checkAPIURL(u); err != nil {
			t.Errorf("%q: unexpected error %v", u, err)
		}
	}
	for _, u := range []string{"http://api.connbyte.com", "ftp://api.connbyte.com", "api.connbyte.com", "http://localhost.evil.com"} {
		if err := checkAPIURL(u); err == nil {
			t.Errorf("%q: expected error", u)
		}
	}
}

func TestParseConfig(t *testing.T) {
	t.Setenv("CONNBYTE_API_TOKEN", "secret")
	t.Setenv("REQUEST_TIMEOUT", "9s")
	c, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.requestTimeout != 9*time.Second || c.token != "secret" {
		t.Fatalf("unexpected config %+v", c)
	}

	t.Setenv("REQUEST_TIMEOUT", "")
	c, err = parseConfig(nil)
	if err != nil || c.requestTimeout != 14*time.Second {
		t.Fatalf("default timeout must be 14s, got %v (%v)", c.requestTimeout, err)
	}

	if _, err := parseConfig([]string{"--connbyte-api-url=http://api.connbyte.com"}); err == nil {
		t.Fatal("plain http to a remote host must be rejected")
	}
}
