# Ghost Rendering Bug: Analysis of Fix Approaches

## The Problem

When cover art changes (track skip/transition), a ghost copy of the now-playing bar text appears at the top of the TUI. Moving the terminal window clears it instantly.

### Root Cause

BubbleTea's standard renderer uses **line-level diffing** to minimize terminal writes. It compares each line of the current frame against the previous frame and **skips rewriting identical lines** (`standard_renderer.go:214`):

```go
canSkip := !flushQueuedMessages &&
    len(r.lastRenderedLines) > i && r.lastRenderedLines[i] == newLines[i]
```

The kitty image renders as a **graphic layer on top of the text layer**. The text underneath the image is invisible while the image is displayed, but it's still there. When the image is deleted (`\x1b_Ga=d,d=a\x1b\\`) before a new image is rendered, the stale text layer becomes momentarily visible. Because the diff renderer never rewrote those lines (they were "unchanged"), the text layer contains content from an earlier frame -- in this case, the now-playing bar.

Moving the window clears the ghost because the terminal performs a full repaint of all layers on expose/move events.

---

## Approach 1: Override BubbleTea's Diff Renderer

**Concept:** Force the renderer to rewrite ALL lines when cover art changes, bypassing the line diff optimization.

### How It Would Work

BubbleTea's renderer has an internal `repaint()` method (`standard_renderer.go:319`) that clears the line cache:

```go
func (r *standardRenderer) repaint() {
    r.lastRender = ""
    r.lastRenderedLines = nil
}
```

On the next flush, all lines would be rewritten because there are no cached lines to compare against.

### Pros

- **100% effective** -- every line gets rewritten, so stale text under the image is always overwritten with correct content before the new image is placed
- **No visual flash** -- unlike `ClearScreen()`, a repaint just rewrites lines in place without clearing the screen first
- **No kitty protocol changes needed** -- the current delete-all + re-render approach stays as-is

### Cons

- **`repaint()` is private** -- there's no public API to trigger it. `repaintMsg` is an internal type in the `tea` package; creating a local duplicate won't match the type assertion in the renderer's message handler
- **`ClearScreen()` is the only public alternative** -- but it sends `\x1b[2J` (erase entire screen) + cursor home before repainting, causing a visible flash/flicker on every track transition
- **`ignoreLines` mechanism exists** (`standard_renderer.go:510`) but is also private and designed for application-managed lines, not for our use case
- **Would require forking BubbleTea** -- to expose `repaint()` publicly, or adding a `ForceRepaint() tea.Cmd` to the library. This creates a maintenance burden on every BubbleTea upgrade
- **Performance regression on every track change** -- all ~40 lines get rewritten instead of just the ~7 that actually changed (bottom bar). Minor, but unnecessary

---

## Approach 2: Kitty Image IDs (Update In-Place)

**Concept:** Assign persistent IDs to kitty images. Instead of deleting all images and re-transmitting on every frame, keep the image in the terminal's memory and only delete+replace when the album actually changes.

### How It Would Work

The kitty protocol supports image IDs (`i=N`) and separate transmit (`a=T`) vs. place (`a=p`) actions:

```
First render:   \x1b_Ga=T,i=1,f=100,c=72,r=33,...;{image_data}\x1b\\
Same album:     (no kitty commands needed -- image persists)
New album:      \x1b_Ga=d,d=i,i=1\x1b\\                              (delete old)
                \x1b_Ga=T,i=2,f=100,c=72,r=33,...;{new_data}\x1b\\   (transmit new)
```

Key parameters:
- `i=N` -- assigns a persistent image ID
- `a=T` -- transmit image data + display
- `a=p,i=N` -- re-display a previously transmitted image (no data re-sent)
- `a=d,d=i,i=N` -- delete only the image with ID N (not all images)

### Pros

- **Eliminates the root cause** -- the ghost happens because we delete all images every frame (`\x1b_Ga=d,d=a\x1b\\`). With image IDs, we only delete when the album changes, so the image is never removed and the stale text layer is never exposed
- **No BubbleTea changes needed** -- purely a kitty protocol change in `coverart.go` and the View() method in `app.go`
- **Better performance** -- the current approach re-transmits ~150KB of base64 PNG data every frame (once per second on tick). With image IDs, we transmit once per album change and the terminal retains the image in memory. Massive reduction in terminal I/O
- **No visual flash** -- the old image stays visible until the new one is ready, creating a seamless transition
- **Cleaner resize handling** -- on `WindowSizeMsg`, we can re-place the existing image at the new position without re-transmitting the data (using `a=p,i=N`)
- **Terminal standard** -- image IDs are a core part of the kitty graphics protocol, supported by all kitty-compatible terminals (Ghostty, Kitty, WezTerm)

### Cons

- **More state to track** -- the `CoverArt` struct needs an `imageID` counter and logic to detect album changes
- **Edge cases with terminal state** -- if the terminal loses image memory (e.g., alt-screen toggle, terminal crash recovery), the image ID reference becomes stale. Need a fallback to re-transmit
- **Slightly more complex kitty sequence generation** -- `kittyChunkedFull()` needs the `i=` parameter added, and the View() method needs conditional logic for when to transmit vs. place vs. delete
- **Resize still requires re-placement** -- when the terminal is resized, we need to re-place the image at the correct position. This is simpler than re-transmitting but still requires logic in the `WindowSizeMsg` handler

---

## Approach 3: Unique Content Per Line (Force Diff Mismatch)

**Concept:** Ensure that every line in the top section has unique content on every frame, so BubbleTea's diff renderer always rewrites them. The stale text layer would be overwritten with correct content before the kitty image covers it.

### How It Would Work

Add a frame counter or timestamp to each line in the top section as invisible ANSI content:

```go
// In the topLines loop:
for i := 0; i < topH; i++ {
    // Append an invisible frame counter so each line is unique every frame
    topLines[i] = navLines[i] + fmt.Sprintf("\x1b[0m%d", frameCounter)
}
```

Or more practically: include the cover art as per-row strips (one kitty sequence per line) instead of a single sequence on line 0. Each strip's data is unique, so the diff renderer always rewrites all cover art lines.

### Pros

- **Works within BubbleTea's existing architecture** -- no renderer changes, no private API access needed
- **Simple to implement** -- either add invisible frame counters or revert to per-row kitty strips
- **Reliable** -- the diff renderer is guaranteed to rewrite lines that differ, so stale text is always overwritten

### Cons

- **Per-row strips had visible gaps** -- we already tried this (the horizontal banding bug from earlier). Each strip's pixel height didn't perfectly match the terminal cell height, creating visible blank lines between strips. The single-image approach (`r=CoverRows`) was adopted specifically to fix this
- **Invisible frame counters are a hack** -- adding `\x1b[0m{counter}` to every line defeats the purpose of the diff renderer (performance optimization). Every line would be rewritten every frame, which is what we're trying to avoid
- **~150KB of kitty data retransmitted every frame** -- if we force all lines to be rewritten, the kitty sequence on line 0 (~150KB of base64 PNG) gets sent to the terminal on every tick (once per second). This is wasteful I/O and could cause lag on slower terminals
- **Doesn't solve the fundamental problem** -- the ghost is caused by the text-behind-image being stale. Forcing rewrites fixes the symptom but doesn't address the architectural mismatch between BubbleTea's text-based renderer and kitty's layered graphics model

---

## Recommendation

**Approach 2 (Kitty Image IDs) is the clear winner.** It eliminates the root cause rather than working around it, reduces terminal I/O by ~150KB/frame, requires no BubbleTea internals access, and uses the kitty protocol as intended. The added state tracking (image ID counter, album change detection) is minimal and the `CoverArt` struct already has `CoverURL` for dedup.

| | Approach 1 (Renderer) | Approach 2 (Image IDs) | Approach 3 (Force Diff) |
|---|---|---|---|
| Fixes root cause | No (works around it) | **Yes** | No (works around it) |
| BubbleTea changes | Fork required | None | None |
| Visual artifacts | Flash on ClearScreen | **None** | Strip gaps or wasted I/O |
| Terminal I/O | Same (~150KB/frame) | **~150KB/album change** | Same or worse |
| Complexity | High | **Medium** | Low (but hacky) |
| Maintainability | Low (fork) | **High** | Medium |
