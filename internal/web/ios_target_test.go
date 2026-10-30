package web

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// Two deployment targets on purpose, and the rule that keeps them apart.
//
// App Store Connect refuses an upload below 15.0 from April 2027
// (ITMS-90068), so the Xcode project and the Codemagic build that feeds
// TestFlight are 15.0. The GitLab .ipa is unsigned and never goes through
// App Store Connect -- whoever sideloads it re-signs it themselves -- so it
// stays at 13.0 and keeps older devices working.
//
// The invariant that matters in BOTH tracks: the Go framework's minimum must
// never be ABOVE the app's, or it will not link. Each pipeline therefore has
// to move its two numbers together, which is what this checks.
const (
	appleIOSTarget  = 15 // Xcode project + Codemagic: what the App Store reads
	legacyIOSTarget = 13 // GitLab's unsigned .ipa
)

var (
	reDeploymentTarget = regexp.MustCompile(`IPHONEOS_DEPLOYMENT_TARGET\s*=\s*(\d+)\.`)
	reIOSVersion       = regexp.MustCompile(`-iosversion=(\d+)\.`)
)

func readIfPresent(t *testing.T, path string) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// all returns every number the pattern finds, so a file that declares the
// value more than once cannot drift between its own copies.
func all(re *regexp.Regexp, s string) []int {
	var out []int
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func TestAppleIOSTargetIsFifteen(t *testing.T) {
	proj, ok := readIfPresent(t, "../../ios/Thefeed.xcodeproj/project.pbxproj")
	if !ok {
		t.Skip("no Xcode project in this checkout")
	}
	got := all(reDeploymentTarget, proj)
	if len(got) == 0 {
		t.Fatal("the Xcode project declares no deployment target")
	}
	for _, v := range got {
		if v != appleIOSTarget {
			t.Errorf("the Xcode project declares iOS %d, want %d -- this is the "+
				"minimum the App Store reads, and uploads below %d are refused "+
				"from April 2027 (ITMS-90068)", v, appleIOSTarget, appleIOSTarget)
		}
	}

	// Codemagic builds the framework that goes to TestFlight, so it has to
	// match the app exactly: below is pointless, above will not link.
	if cm, ok := readIfPresent(t, "../../codemagic.yaml"); ok {
		for _, v := range all(reIOSVersion, cm) {
			if v != appleIOSTarget {
				t.Errorf("codemagic.yaml builds the framework for iOS %d, but the "+
					"app is %d", v, appleIOSTarget)
			}
		}
	}
}

// The GitLab pipeline is deliberately behind, and both of its numbers have to
// say so -- the framework it binds AND the app minimum it overrides.
func TestGitLabIPAStaysOnTheOlderTarget(t *testing.T) {
	gl, ok := readIfPresent(t, "../../.gitlab-ci.yml")
	if !ok {
		t.Skip("no GitLab CI file in this checkout")
	}
	fw := all(reIOSVersion, gl)
	if len(fw) == 0 {
		t.Fatal("the GitLab pipeline binds no framework")
	}
	for _, v := range fw {
		if v != legacyIOSTarget {
			t.Errorf("GitLab binds the framework for iOS %d, want %d", v, legacyIOSTarget)
		}
	}

	// Without this override the archive would inherit the project's 15.0 and
	// the .ipa would silently stop installing on the devices it exists for.
	ov := regexp.MustCompile(`IPHONEOS_DEPLOYMENT_TARGET=(\d+)\.`).FindStringSubmatch(gl)
	if ov == nil {
		t.Fatalf("GitLab does not override IPHONEOS_DEPLOYMENT_TARGET, so its "+
			"archive inherits the project's %d while its framework is built for "+
			"%d -- the framework would be below the app, and the .ipa would no "+
			"longer install on iOS %d", appleIOSTarget, legacyIOSTarget, legacyIOSTarget)
	}
	if n, _ := strconv.Atoi(ov[1]); n != legacyIOSTarget {
		t.Errorf("GitLab overrides the app to iOS %d but binds the framework for %d",
			n, legacyIOSTarget)
	}
}

// Every other pipeline follows the project, and none may build a framework
// ABOVE the app it links into.
func TestNoFrameworkIsBuiltAboveItsApp(t *testing.T) {
	proj, ok := readIfPresent(t, "../../ios/Thefeed.xcodeproj/project.pbxproj")
	if !ok {
		t.Skip("no Xcode project in this checkout")
	}
	appMin := appleIOSTarget
	if m := reDeploymentTarget.FindStringSubmatch(proj); m != nil {
		appMin, _ = strconv.Atoi(m[1])
	}
	for _, path := range []string{"../../Makefile", "../../.github/workflows/build.yml"} {
		s, ok := readIfPresent(t, path)
		if !ok {
			continue
		}
		for _, v := range all(reIOSVersion, s) {
			if v > appMin {
				t.Errorf("%s builds the framework for iOS %d, above the app's %d -- "+
					"it will not link", path, v, appMin)
			}
		}
	}
}
