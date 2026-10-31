// telseal prints the linker flags that embed the bundled Telegram app
// credentials, read from TEL_API_ID and TEL_API_HASH (CI secrets): the sealed
// blob and the fresh key it opens with, each into its own variable. Release
// builds append the output to -ldflags:
//
//	TEL_LDFLAG=$(go run ./cmd/telseal)
//	go build -ldflags="... ${TEL_LDFLAG}" ./cmd/client
//
// With both unset (or empty, as CI passes a missing secret) it prints nothing
// and exits 0: the build goes on without a default and the app asks users for
// their own pair. Anything half-set or malformed fails the build instead --
// shipping a pair Telegram will refuse, or silently none, is worse.
package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/sartoopjj/thefeed/internal/telcreds"
)

// Telegram's api_hash is 32 lowercase hex digits.
var hashRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

const (
	blobVar = "github.com/sartoopjj/thefeed/internal/web.telDefaultBlob"
	keyVar  = "github.com/sartoopjj/thefeed/internal/telcreds.buildKey"
)

// sealFromEnv decides what a build embeds, as linker flags. Both empty is a
// build without a default: "" and no error. Anything else must be a complete,
// well-formed pair.
func sealFromEnv(idStr, hash string) (string, error) {
	idStr, hash = strings.TrimSpace(idStr), strings.TrimSpace(hash)
	switch {
	case idStr == "" && hash == "":
		return "", nil
	case idStr == "":
		return "", errors.New("TEL_API_HASH is set but TEL_API_ID is not")
	case hash == "":
		return "", errors.New("TEL_API_ID is set but TEL_API_HASH is not")
	}
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		return "", errors.New("TEL_API_ID is not a positive number")
	}
	if !hashRe.MatchString(hash) {
		return "", errors.New("TEL_API_HASH is not 32 lowercase hex characters")
	}
	blob, key, err := telcreds.Seal(id, hash)
	if err != nil {
		return "", err
	}
	return "-X " + blobVar + "=" + blob + " -X " + keyVar + "=" + key, nil
}

// report says, in the build log, what this build carries. On GitHub it is an
// annotation, so it shows on the run's summary page without opening a log.
// Never the values themselves.
func report(level, msg string) {
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		fmt.Fprintf(os.Stderr, "::%s title=Telegram default key::%s\n", level, msg)
		// Also on the run's Summary page: stdout carries the blob, so the
		// annotation above can only go to stderr.
		if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
			if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
				fmt.Fprintf(f, "**Telegram default key** (%s): %s\n\n", level, msg)
				f.Close()
			}
		}
		return
	}
	fmt.Fprintf(os.Stderr, "telseal [%s]: %s\n", level, msg)
}

func main() {
	flags, err := sealFromEnv(os.Getenv("TEL_API_ID"), os.Getenv("TEL_API_HASH"))
	if err != nil {
		report("error", err.Error()+"; fix the TEL_API_ID / TEL_API_HASH secrets")
		os.Exit(1)
	}
	if flags == "" {
		report("warning", "TEL_API_ID / TEL_API_HASH are not set: this build has NO default key, users must enter their own")
		return
	}
	report("notice", "default key embedded in this build")
	fmt.Print(flags)
}
