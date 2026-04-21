# Architecture Alternatives: TUI Frameworks & tmux Approach

## Why We're Looking at Alternatives

Bubble Tea's diff-based renderer skips rewriting unchanged terminal lines. Kitty graphics protocol places images as a layer on top of the text layer. When the image changes, stale text underneath is briefly exposed as a "ghost." This is a fundamental architectural mismatch between line-diffing TUI renderers and layered graphics protocols.

---

## Option 1: Alternative Go TUI Frameworks

### tview (github.com/rivo/tview)
- **Renderer:** Uses tcell underneath, which abstracts terminal control through terminfo
- **Raw escape support:** Limited -- high-level widget library, doesn't expose raw escape sequences
- **Kitty graphics:** No integration. kittyimg (github.com/dolmen-go/kittyimg) can be used alongside but same diff-renderer issue applies since tcell is underneath
- **Verdict:** Same fundamental problem. Widget abstraction makes it harder to work around, not easier

### tcell (github.com/gdamore/tcell)
- **Renderer:** Lower-level terminal abstraction via terminfo
- **Raw escape support:** Can access underlying file descriptor but not designed for graphics passthrough
- **Kitty graphics:** No built-in support
- **Verdict:** More control than tview but would require building a full TUI from scratch. The core diff-rendering conflict would need to be solved manually

### Bubble Tea v2 (charmbracelet/bubbletea/v2)
- **Renderer:** New "Cursed Renderer" based on ncurses algorithm, ~100x faster
- **Kitty graphics:** Still NO explicit graphics protocol support. Issue #163 (image display) open since 2021, unresolved
- **Key improvements:** Better terminal state management, kitty keyboard protocol fixes
- **Verdict:** Better performance but the diff-renderer / graphics protocol conflict is NOT addressed in v2

### Ratatui (Rust, for reference)
- Has `Cell::set_skip()` method specifically designed for this problem -- tells the diff renderer to skip cells occupied by graphics
- ratatui-image library provides Image and StatefulImage widgets that work with kitty protocol
- **Verdict:** Rust ecosystem has already solved this problem. If we were in Rust, this would be trivial. Go has no equivalent

### Key Insight
The diff-based renderer vs. layered graphics conflict is well-known. The Rust ecosystem solved it by adding `set_skip()` to the renderer. The Go ecosystem (Bubble Tea, tview, tcell) has not. No Go TUI framework currently handles kitty graphics natively.

---

## Option 2: tmux Split-Pane Architecture

### Concept
```
+---tmux session------------------------------------------+
|                          |                               |
|  Pane 1: Bubble Tea TUI  |  Pane 2: Image viewer process |
|  (search, browse, queue)  |  (kitty graphics, standalone) |
|                          |                               |
+--------------------------+-------------------------------+
|  Pane 3: Player status process                           |
|  (now-playing, progress bar, controls)                   |
+----------------------------------------------------------+
```

Each pane is a separate process. The navigator TUI sends commands to the other panes via IPC (unix socket or named pipe).

### Kitty Graphics in tmux
- **Supported** since tmux 3.3+ via APC passthrough
- Kitty-capable terminals (Ghostty, Kitty, WezTerm) relay graphics through tmux panes
- Some limitations: buffer size constraints, image cleanup on scroll
- Unicode placeholder approach (U+10EEEE) is an alternative that works more reliably in tmux

### Go Libraries for tmux Control
- **gotmux** (github.com/GianlucaP106/gotmux) -- comprehensive: Split(), SplitVertical(), SendKeys()
- **gomux** (github.com/wricardo/gomux) -- session/window/pane creation with Vsplit()

### IPC Between Panes
- **Unix domain sockets:** Fast, efficient, structured communication. Go's net package supports them natively
- **Named pipes (FIFOs):** Simple file-based. Each pane reads from its FIFO
- **File watching:** Write cover art path to a file, image viewer watches for changes. Simplest approach

### Pros
- **Completely eliminates the ghost bug** -- kitty graphics run in a dedicated pane with no TUI framework interference
- Clean separation of concerns: navigation, image display, and player status are independent
- User can resize panes manually if desired
- The image viewer pane can use tools like `timg` or `chafa` directly, or a simple Go program with kittyimg
- Each component can be developed and tested independently

### Cons
- **tmux is a required dependency** -- users must have tmux installed and be running inside tmux (or the app spawns tmux)
- tmux 3.3+ required for kitty graphics passthrough
- Focus handling: keyboard input goes to the focused pane. Need `tmux send-keys` to route commands, or keep focus on the TUI pane and use IPC
- More complex distribution/packaging (multiple binaries or a launcher script)
- Image cleanup issues when tmux scrollback triggers
- Slight latency on IPC-based communication (cover art update is not instantaneous)

---

## Option 3: Hybrid Single-Process with Raw Escape Passthrough

### Concept
Keep Bubble Tea for the TUI, but bypass it entirely for the cover art region. Write kitty APC sequences directly to `os.Stdout` from a separate goroutine, positioned at the cover art coordinates using ANSI cursor positioning.

### How It Would Work
1. Bubble Tea renders the left side (navigator) and bottom (now-playing) as normal
2. The cover art region (top-right, CoverCols x CoverRows) is left blank in Bubble Tea's output
3. A separate goroutine writes kitty sequences directly to stdout, using `\x1b[{row};{col}H` (cursor positioning) to target the cover art region
4. The goroutine manages its own image state (no interaction with Bubble Tea's renderer)

### Implementation
```go
// Separate goroutine for cover art
go func() {
    for update := range coverArtChan {
        // Position cursor at cover art region
        fmt.Fprintf(os.Stdout, "\x1b[1;%dH", navW+1)
        // Write kitty sequence directly
        os.Stdout.Write([]byte(kittySequence))
    }
}()
```

### Pros
- Single process, no external dependencies
- Bubble Tea handles all text rendering without interference
- Kitty sequences bypass the diff renderer entirely
- No tmux dependency

### Cons
- **Stdout contention:** Bubble Tea and the cover art goroutine both write to stdout. Need mutex or careful coordination to avoid interleaved writes
- **Cursor position conflicts:** After the goroutine moves the cursor, Bubble Tea needs it back at the right position
- Fragile: any Bubble Tea internal change to how it manages the cursor could break this
- Bubble Tea's alt-screen management could interfere with direct stdout writes

---

## Option 4: Separate Image Viewer Process (No tmux)

### Concept
Similar to tmux approach but without tmux. The main Bubble Tea process spawns a separate image viewer process that renders cover art using kitty protocol. Communication via unix socket.

### How It Would Work
1. Main process: Bubble Tea TUI occupying the left portion of the terminal
2. Image viewer: Separate process that writes kitty graphics to a specific terminal region
3. The image viewer uses ANSI cursor positioning to render at the right coordinates
4. Both processes share the same terminal (same stdout/PTY)

### This is essentially how these tools work:
- **ueberzug** (Python) -- spawns a separate process for image rendering
- **image.nvim** (Neovim plugin) -- renders images separately from the editor's TUI

### Pros
- No tmux dependency
- Clean separation
- Works with any terminal that supports kitty protocol

### Cons
- Same stdout contention issues as Option 3
- Two processes sharing one terminal is fragile
- Hard to coordinate cursor positions between processes

---

## Recommendation

### Short-term (current approach is acceptable)
The `tea.ClearScreen()` workaround for album changes works. The resize ghost is minor (clears on next interaction). Keep iterating on the current single-process Bubble Tea architecture.

### Medium-term (best ROI)
**Option 2: tmux split-pane architecture.** This is the cleanest solution and has precedent (ranger, neovim image plugins). The ghost bug is eliminated entirely because kitty graphics run in their own pane. The main risk is the tmux dependency, but the target audience (terminal power users running a Tidal TUI) almost certainly has tmux installed.

Implementation path:
1. Write a launcher (`rising-tide` binary) that creates a tmux session with three panes
2. Pane 1: `rising-tide --tui` (Bubble Tea navigator, no image rendering)
3. Pane 2: `rising-tide --cover` (image viewer, watches unix socket for cover art URL updates)
4. Pane 3: `rising-tide --player` (player status, watches unix socket for playback state)
5. IPC via unix domain socket at `~/.local/share/rising-tide/rising-tide.sock`

### Long-term (if the project grows)
Consider porting to Rust with Ratatui, which has native `Cell::set_skip()` support for kitty graphics. Or wait for Bubble Tea to add graphics protocol support (issue #163).

---

## References

- [Kitty Graphics Protocol](https://sw.kovidgoyal.net/kitty/graphics-protocol/)
- [Bubble Tea Issue #163 (Images)](https://github.com/charmbracelet/bubbletea/issues/163)
- [Ratatui Cell::set_skip()](https://docs.rs/ratatui-image/latest/ratatui_image/)
- [go-termimg](https://github.com/blacktop/go-termimg)
- [kittyimg](https://github.com/dolmen-go/kittyimg)
- [gotmux](https://github.com/GianlucaP106/gotmux)
- [tmux Kitty Graphics (Issue #4902)](https://github.com/tmux/tmux/issues/4902)
- [image.nvim](https://github.com/3rd/image.nvim)
- [ranger Image Previews](https://github.com/ranger/ranger/wiki/Image-Previews)
- [timg](https://github.com/hzeller/timg)
- [rmpc (Rust MPD client with album art)](https://github.com/mierak/rmpc)
