package main

import "testing"

func TestClientKey(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"203.0.113.9":          "203.0.113.9",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "2001:db8:1:3::/64",
		"::ffff:203.0.113.9":   "203.0.113.9",
		"not:an:address":       "not:an:address",
	}
	for in, want := range cases {
		r := &Request{}
		r.h[hCfConnectingIP] = in
		if got := clientKey(r); got != want {
			t.Errorf("clientKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIPLimit(t *testing.T) {
	l := newIPLimit(2)
	if !l.enter("a") || !l.enter("a") {
		t.Fatal("under the limit must enter")
	}
	if l.enter("a") {
		t.Fatal("over the limit must not enter")
	}
	if !l.enter("b") {
		t.Fatal("another client has its own count")
	}
	l.leave("a")
	if !l.enter("a") {
		t.Fatal("must enter again after leaving")
	}
	// Docker の中から (鍵なし) は数えない
	for i := 0; i < 10; i++ {
		if !l.enter("") {
			t.Fatal("no key must not be limited")
		}
		l.leave("")
	}
	l.leave("a")
	l.leave("a")
	l.leave("b")
	if len(l.counts) != 0 {
		t.Fatalf("counts must be empty: %v", l.counts)
	}
}
