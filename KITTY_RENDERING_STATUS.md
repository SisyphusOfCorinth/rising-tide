# Kitty Image Rendering: Current Status & Remaining Bugs

## The Core Problem

BubbleTea's standard renderer uses line-level diffing: it compares each line of the current frame against the previous frame and skips rewriting identical lines. Kitty protocol images render as a **graphic layer on top of the text layer**. When the image changes, the text layer underneath can contain stale content from earlier frames that BubbleTea never rewrote (because those lines were "unchanged"). This stale text becomes visible during image transitions.

## Fixes Attempted (Chronological)

### 1. Space-padding every line to full terminal width
**Result:** Did not fix the ghost. The ghost is not leftover text from line-length differences.

### 2. ANSI clear-to-end-of-line (`\x1b[K`) on every line
**Result:** Did not fix the ghost. Same reason -- the stale text is in lines that BubbleTea's diff renderer skips entirely.

### 3. `tea.ClearScreen()` on PlaybackStartedMsg
**Result:** Did not fix the ghost. The clear fires before the new image is ready (image fetch is async). By the time the image arrives, the text layer has been rewritten but NOT via a forced repaint -- BubbleTea's diff renderer takes over on subsequent frames.

### 4. Kitty Image IDs (persistent images, no delete-all every frame)
**Result:** Fixed same-album track skips (no image change needed, so no delete, no ghost). Did NOT fix cross-album transitions or resize. The delete of the old image (even targeted by ID) triggers a terminal repaint that exposes stale text.

### 5. Transmit-before-delete ordering in KittySequenceForFrame
**Result:** Did not fix cross-album ghost. The new image is placed before the old is deleted, but BubbleTea hasn't finished writing all rows when the delete fires mid-frame. The terminal repaints the deleted area using partially-stale rows.

### 6. Deferred delete (transmit on frame N, delete on frame N+1)
**Result:** Did not fix the ghost. Even with the delete deferred, the text layer under the image was already stale from frames where BubbleTea's diff renderer skipped those lines.

### 7. Force-diff on transition frames (`\x1b[0m ` appended to every line)
**Result:** Fixed first-track-play and same-album transitions. Did NOT fix cross-album transitions. The force-diff ensures all lines are rewritten on the transmit frame, but the ghost still appears -- possibly because the terminal processes the kitty sequence mid-write and repaints before BubbleTea finishes writing all rows.

### 8. Prevent fallback placeholder layout during fetch (set imageID immediately)
**Result:** Fixed the first-track ghost (the fallback layout was writing full-width text that persisted under the image). Combined with force-diff, this fixed same-album transitions completely.

### 9. `tea.ClearScreen()` in CoverArtMsg handler (when album actually changes)
**Result:** **WORKS for cross-album transitions.** ClearScreen fires on the exact frame where the new image data is available. It erases all terminal text (`\x1b[2J`), moves cursor home, and forces BubbleTea's internal `repaint()` which clears the line cache. All lines are rewritten from scratch. The new kitty image is transmitted as part of the rewrite. Introduces a brief visible flicker.

### 10. `tea.ClearScreen()` + `rerenderCoverArt` batch on resize
**Result:** **WORKS for resize** (no duplicate images). But the player ghost bug still appears on resize because the ClearScreen + full rewrite happens before the re-rendered image arrives (async).

## Current Approach

- **Same album, track skip:** Image persists in terminal memory (kitty image IDs). No ghost, no flicker.
- **Cross-album transition:** `tea.ClearScreen()` fired from `CoverArtMsg` handler. Eliminates ghost. Introduces brief flicker.
- **Resize/move:** `tea.ClearScreen()` + async `rerenderCoverArt`. No duplicate images. Player ghost bug appears during the gap between clear and new image arrival.
- **Image IDs:** Each album gets a unique kitty image ID. The terminal retains images by ID, reducing I/O from ~150KB/frame to ~150KB/album-change.

## Remaining Bugs

### 1. Player ghost bar on resize/move
**Severity:** Cosmetic, clears on next album change or window move.
**Cause:** `tea.ClearScreen()` on resize wipes everything. The re-rendered image arrives 1-2 frames later (async). During the gap, the now-playing bar text is written to the terminal at full width. When the image arrives and is placed, the now-playing text from those gap frames is underneath the image in the text layer. It's invisible while the image is displayed, but would be exposed if the image were ever deleted again.
**The ghost is actually from the gap frames, not from before the resize.**
**Potential fix:** Make `rerenderCoverArt` synchronous (render from cached image inline in the resize handler, not async). This eliminates the gap. The image would be ready on the same frame as the ClearScreen.

### 2. Brief flicker on album change
**Severity:** Cosmetic, ~1 frame flash.
**Cause:** `tea.ClearScreen()` erases the entire terminal before rewriting. For one frame, the screen is blank.
**Potential fix:** Use kitty image IDs to place the new image BEFORE clearing the screen, so the new image is visible during the rewrite. Or use the Kitty protocol's `a=p` (re-place) action to swap images without a clear.

## Architecture Notes

- `CoverArt` struct tracks `imageID`, `prevImageID`, `placed`, `transmitSeq`, `deleteSeq`
- `KittySequenceForFrame()` returns the kitty commands to include in the current frame's output (transmit, delete, or "")
- `SetImage()` installs new image data and prepares transmit/delete sequences
- `HasImage()` returns true when an image is loaded (even if being re-fetched), preventing the fallback placeholder layout
- The kitty sequence is appended to line 0 of the top section in `View()`
- Debug logging: remove the `os` import and the debug block at the end of `View()` when no longer needed

## Files

- `internal/ui/coverart.go` -- Kitty protocol encoder, image ID management, CoverArt state machine
- `internal/ui/app.go` -- View() layout composition, CoverArtMsg/resize handlers, debug logging
- `internal/ui/commands.go` -- `fetchCoverArt`, `rerenderCoverArt` async commands
- `internal/ui/messages.go` -- `CoverArtMsg` with ImageID field
