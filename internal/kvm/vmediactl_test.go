package kvm

import "testing"

func TestVMediaService(t *testing.T) {
	wants := map[string]string{
		"cd": "CDMEDIA",
		"fd": "FDMEDIA",
		"hd": "HDMEDIA",
	}
	for kind, want := range wants {
		got, err := vmediaService(kind)
		if err != nil {
			t.Fatalf("vmediaService(%q): %v", kind, err)
		}
		if got != want {
			t.Errorf("vmediaService(%q) = %q, want %q", kind, got, want)
		}
	}
	if _, err := vmediaService("tape"); err == nil {
		t.Fatal("unknown media kind was accepted")
	}
}
