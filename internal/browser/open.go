// Package browser opens URLs in the user's default browser.
//
// Login should feel like "a page opened and I signed in", not "copy this
// string somewhere". Failure here is never fatal: the caller always prints the
// URL as well, so a headless or locked-down machine still works.
package browser

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func lookupEnv(name string) string { return os.Getenv(name) }

// Open launches url in the default browser. It returns an error when no
// opener is available, which callers should treat as informational.
func Open(url string) error {
	name, args := opener(url)
	if name == "" {
		return errNoOpener
	}

	cmd := exec.Command(name, args...)
	// Detach stdio: some openers are chatty, and their output would corrupt
	// the login prompt the user is reading.
	cmd.Stdout, cmd.Stderr, cmd.Stdin = nil, nil, nil
	return cmd.Start()
}

// Available reports whether a browser can be opened at all, so callers can
// word their prompt before trying.
func Available() bool {
	name, _ := opener("https://example.com")
	return name != ""
}

func opener(url string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{url}
	case "windows":
		// rundll32 avoids cmd.exe's metacharacter handling, which mangles the
		// & separating query parameters.
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		// Try the desktop-agnostic opener first, then common fallbacks, so
		// this works outside a full desktop environment too.
		for _, candidate := range []string{"xdg-open", "gio", "gnome-open", "kde-open", "wslview"} {
			if path, err := exec.LookPath(candidate); err == nil {
				if candidate == "gio" {
					return path, []string{"open", url}
				}
				return path, []string{url}
			}
		}
		return "", nil
	}
}

type openerError string

func (e openerError) Error() string { return string(e) }

const errNoOpener = openerError("no browser opener available on this system")

// IsHeadless reports whether this looks like a machine with no desktop, which
// is worth saying out loud before printing a URL nobody can click.
func IsHeadless() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return strings.TrimSpace(envAny("DISPLAY", "WAYLAND_DISPLAY")) == ""
}

func envAny(names ...string) string {
	for _, n := range names {
		if v := lookupEnv(n); v != "" {
			return v
		}
	}
	return ""
}
