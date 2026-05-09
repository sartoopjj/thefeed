//go:build gomobiletools

package mobile

// Pins golang.org/x/mobile/bind* in go.mod so `go mod tidy` doesn't
// drop them between gomobile-bind invocations. The build tag keeps
// these out of normal compilation; the imports exist solely for go
// modules' dependency graph.
import (
	_ "golang.org/x/mobile/bind"
	_ "golang.org/x/mobile/bind/objc"
)
