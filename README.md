# go-clipboard

![logo](golang-clipboard.png)

A cross-platform clipboard utility for [Go](go.dev).

It offers abilities for copying and pasting plain text, and for syncing the
clipboard between two machines over the network.

## installation

```
go get github.com/edickens09/go-clipboard/clipboard
```

## documentation

[https://pkg.go.dev/github.com/edickens09/go-clipboard](https://pkg.go.dev/github.com/edickens09/go-clipboard)

## platforms

| OS | Supported copy tools | Supported paste tools |
|----------|----------|----------|
| Darwin | `pbcopy` | `pbpaste` |
| Windows | `clip.exe` | `powershell` |
| Linux/FreeBSD/NetBSD/OpenBSD/Dragonfly| X11: `xsel`, `xclip` <br> Wayland: `wl-copy` | X11: `xsel`, `xclip` <br> Wayland: `wl-paste` |
| Solaris | X11: `xsel`, `xclip`| X11: `xsel`, `xclip` |
| Android (via Termux) | `termux-clipboard-set`| `termux-clipboard-get` |

## syncing the clipboard between two systems

The `clipsync` command keeps the clipboards of two machines in sync over TCP.
Build the same binary once and install it on both machines:

```
go build -o bin/ ./cmd/clipsync

# or install it into $GOBIN / $GOPATH/bin:
go install github.com/edickens09/go-clipboard/cmd/clipsync@latest
```

On the first machine, start a listener:

```
bin/clipsync -listen :9876 -token "$(openssl rand -hex 16)" -v
```

On the second machine, connect to it with the same token:

```
bin/clipsync -connect 192.168.1.10:9876 -token <same token> -v
```

Both peers poll their local clipboard (`-interval`, default `500ms`), send
changes to the other peer, and write changes received from the peer to their
local clipboard. The connector reconnects automatically with exponential
backoff if the connection is lost. By default the connecting peer sends its
current clipboard when the session starts, which also makes the peers converge
after a reconnect; pass `-sync-initial=false` to disable that.

Options:

| Flag | Description |
|------|-------------|
| `-listen ADDR` | listen for a peer on `ADDR` (use exactly one of `-listen` or `-connect`) |
| `-connect ADDR` | connect to the listening peer at `ADDR` |
| `-token SECRET` | shared secret used to authenticate the peer; defaults to `$CLIPSYNC_TOKEN` |
| `-interval DURATION` | how often the local clipboard is checked (default `500ms`) |
| `-primary` | sync the X11/Wayland primary selection instead of the clipboard (Unix only) |
| `-sync-initial` | send the connector's clipboard when a session starts (default `true`) |
| `-v` | enable verbose logging |

The same functionality is available as the `clipsync` package if you want to
embed it in another program.

Notes:

- Traffic is plaintext JSON over TCP. The shared token authenticates peers but
  does not encrypt anything, so run this on a trusted network or tunnel it
  through SSH, WireGuard, Tailscale, etc.
- One side must listen and the other must connect. The listener accepts
  multiple peers, but the usual setup is one listener and one connector.
- The platform clipboard tools listed above must be installed on each machine.
- Run the command under systemd, launchd, or Task Scheduler if it should start
  at boot.

## examples

### copy

[examples/copy/copy.go](examples/copy/copy.go)

```
package main

import (
	"fmt"
	"os"

	clipboard "github.com/edickens09/go-clipboard/clipboard"
)

func main() {
	text := "some text"
	c := clipboard.New()
	if err := c.CopyText(text); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("text \"%s\" was copied into clipboard. Paste it elsewhere.\n", text)
}
```

### paste

[examples/paste/paste.go](examples/paste/paste.go)

```
package main

import (
	"fmt"
	"os"

	clipboard "github.com/edickens09/go-clipboard/clipboard"
)

func main() {
	c := clipboard.New()
	text, err := c.PasteText()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("text from clipboard: %v\n", text)
}

```

## unit tests

### *nix

```
make test
```

### Windows

```
run-tasks.bat test
```
