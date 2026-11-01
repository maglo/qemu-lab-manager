package peer

import (
	"testing"
)

func TestParseFlag(t *testing.T) {
	p, err := ParseFlag("kvm01=https://lab.example/labview/")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "kvm01" {
		t.Errorf("Name = %q", p.Name)
	}
	if p.URL != "https://lab.example/labview" {
		t.Errorf("URL = %q", p.URL)
	}
}

func TestParseFlagRejects(t *testing.T) {
	for _, v := range []string{
		"",
		"noequals",
		"=http://x",
		"kvm01=",
		"kvm01=ftp://x",
		"kvm01=not-a-url",
		"bad name=http://x",
	} {
		if _, err := ParseFlag(v); err == nil {
			t.Errorf("%q was accepted", v)
		}
	}
}

func TestJoinSplitID(t *testing.T) {
	id := JoinID("kvm01", "el9-build")
	if id != "kvm01:el9-build" {
		t.Fatalf("JoinID = %q", id)
	}
	peer, local, ok := SplitID(id)
	if !ok || peer != "kvm01" || local != "el9-build" {
		t.Fatalf("SplitID = %q %q %v", peer, local, ok)
	}
	if _, _, ok := SplitID("el9-build"); ok {
		t.Fatal("a local id was treated as composite")
	}
	if _, _, ok := SplitID("a:b:c"); ok {
		t.Fatal("a triple was accepted")
	}
}

func TestRewriteMachinePath(t *testing.T) {
	got := rewriteMachinePath("/api/machines/kvm01:el9/screenshot", "kvm01:el9", "el9")
	if got != "/api/machines/el9/screenshot" {
		t.Errorf("got %q", got)
	}
	got = rewriteMachinePath("/api/machines/kvm01:el9", "kvm01:el9", "el9")
	if got != "/api/machines/el9" {
		t.Errorf("got %q", got)
	}
}
