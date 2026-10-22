package main

import (
	"strings"
	"testing"
)

func TestParseHostsCSVCommentsSkippedNotParsed(t *testing.T) {
	in := "name,ip\n# node00,192.0.2.9\nnode01,192.0.2.1\n\n# trailing\n"
	res, err := ParseHostsCSV(strings.NewReader(in))
	if err != nil || len(res.Errors) != 0 {
		t.Fatalf("err=%v errs=%v", err, res.Errors)
	}
	if len(res.Hosts) != 1 || res.Hosts["node01"].IP != "192.0.2.1" {
		t.Errorf("hosts=%v", res.Hosts)
	}
	if res.Skipped != 4 { // header, 2 comments, 1 blank
		t.Errorf("skipped=%d", res.Skipped)
	}
}
