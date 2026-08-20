package main

import "testing"

func TestParseCurrentVersion(t *testing.T) {
	good := []byte(`{"script_name":"counter","version":"d733f37ce356fc35","prefix":"deploy/counter/d733f37ce356fc35","rollout":{"percent":100}}`)
	if got := parseCurrentVersion(good); got != "d733f37ce356fc35" {
		t.Errorf("got %q, want d733f37ce356fc35", got)
	}
	for _, bad := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"version":""}`),
		[]byte(`not json`),
		[]byte(`<?xml version="1.0"?><Error/>`),
	} {
		if got := parseCurrentVersion(bad); got != "unknown" {
			t.Errorf("parseCurrentVersion(%s) = %q, want unknown", bad, got)
		}
	}
}

func TestS3EscapePath(t *testing.T) {
	cases := map[string]string{
		"counter/deploy/current.json": "counter/deploy/current.json",
		"a b/c+d.json":                "a%20b/c%2Bd.json",
	}
	for in, want := range cases {
		if got := s3EscapePath(in); got != want {
			t.Errorf("s3EscapePath(%q) = %q, want %q", in, got, want)
		}
	}
}
