package main

import (
	"os"
	"regexp"
	"testing"
)

// A setting written "true" must not silently leave a feature off.
func TestEnvBoolAcceptsTheUsualSpellings(t *testing.T) {
	for val, want := range map[string][2]bool{
		"1": {true, true}, "true": {true, true}, "TRUE": {true, true}, " yes ": {true, true}, "on": {true, true},
		"0": {false, true}, "false": {false, true}, "No": {false, true}, "off": {false, true},
		"": {false, false}, "maybe": {false, false},
	} {
		t.Setenv("THEFEED_TEST_BOOL", val)
		v, ok := envBool("THEFEED_TEST_BOOL")
		if v != want[0] || ok != want[1] {
			t.Errorf("%q: got %v,%v want %v,%v", val, v, ok, want[0], want[1])
		}
	}
}

// Every boolean setting goes through envBool.
func TestNoBareBooleanEnvChecks(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`Getenv\("[A-Z_]+"\)\)? == "[01]"|(env|v) == "[01]"`).FindAll(src, -1); len(m) > 0 {
		t.Errorf("boolean settings read without envBool: %q", m)
	}
}
