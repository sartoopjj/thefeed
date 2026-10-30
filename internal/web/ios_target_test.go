package web

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// iOS 13 is the minimum, and Apple accepts it until April 2027. Raising it to
// 15 was tried and reverted: it bought nothing the app uses and it threw away
// the cross-compile caches, so every bind started from scratch.
//
// Two files have to agree: the Xcode project, which is the minimum the App
// Store actually reads, and the Codemagic bind that produces the Go framework
// it links against.
//
// The framework's minimum must not be HIGHER than the app's, or the link
// fails; equal is what the Apple path uses. Other pipelines (the local
// Makefile, GitLab) build the same framework for development and may sit
// lower -- a framework with a lower minimum links into a higher-minimum app
// perfectly well.
func TestAppleIOSTargetIsThirteen(t *testing.T) {
	const want = 13

	type check struct {
		path string
		re   *regexp.Regexp
		what string
	}
	checks := []check{
		{"../../ios/Thefeed.xcodeproj/project.pbxproj",
			regexp.MustCompile(`IPHONEOS_DEPLOYMENT_TARGET = (\d+)\.`),
			"the app's declared minimum, which is what the App Store reads"},
		{"../../codemagic.yaml",
			regexp.MustCompile(`-iosversion=(\d+)\.`),
			"the framework Codemagic links into that app"},
	}

	seen := 0
	for _, c := range checks {
		raw, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		for _, m := range c.re.FindAllStringSubmatch(string(raw), -1) {
			got, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			seen++
			if got != want {
				t.Errorf("%s declares iOS %d, want %d -- %s", c.path, got, want, c.what)
			}
		}
	}
	if seen == 0 {
		t.Skip("no Apple build files in this checkout")
	}

	// And the framework must never be built for a HIGHER minimum than the app.
	proj, err := os.ReadFile("../../ios/Thefeed.xcodeproj/project.pbxproj")
	if err != nil {
		return
	}
	appMin := want
	if m := regexp.MustCompile(`IPHONEOS_DEPLOYMENT_TARGET = (\d+)\.`).FindSubmatch(proj); m != nil {
		appMin, _ = strconv.Atoi(string(m[1]))
	}
	for _, path := range []string{"../../Makefile", "../../.gitlab-ci.yml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, m := range regexp.MustCompile(`-iosversion=(\d+)\.`).FindAllStringSubmatch(string(raw), -1) {
			got, _ := strconv.Atoi(m[1])
			if got > appMin {
				t.Errorf("%s builds the framework for iOS %d, above the app's %d -- it will not link",
					path, got, appMin)
			}
		}
	}
	t.Logf("%d Apple declarations checked, app minimum %d", seen, appMin)
}
