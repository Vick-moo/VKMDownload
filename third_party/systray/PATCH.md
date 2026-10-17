# Why this fork exists

This is `fyne.io/systray v1.12.2` with exactly one patch.

`systray_notifier_unix.go` ships **without a `//go:build` constraint**. The
`_unix` filename suffix is not an implicit platform constraint in Go (only
`_linux`, `_windows`, `_darwin`, ... are), so the file compiled on every
platform and pulled `github.com/godbus/dbus/v5` plus
`fyne.io/systray/internal/generated/notifier` into the Windows binary:
**~364 KiB of Linux-only DBus notification plumbing, dead on arrival.**

Measured on the real app build (same flags as `scripts/release.ps1`):

| build                    | size          |
| ------------------------ | ------------- |
| upstream `v1.12.2`       | 13,220,352 B  |
| this patch               | 12,847,104 B  |

The fix is the first line of `systray_notifier_unix.go`:

```go
//go:build (linux || freebsd || openbsd || netbsd) && !android
```

identical to the constraint its sibling `systray_unix.go` already carries. Do
not delete the file instead — Unix builds need `newLeftRightNotifierItem`.

`desktop/go.mod` points at this copy via a `replace` directive. When bumping
the systray version, re-copy the module here and re-apply the one-line patch
(or drop the fork entirely once upstream fixes it; check
https://github.com/fyne-io/systray for `systray_notifier_unix.go` gaining a
build tag).
