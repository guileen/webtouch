# Long Page Viewport Routing Contract

> Status: `APPROVED` for D11 / T-30 Phase 3 implementation. This file is the contract for macro viewport routing; code-level ranking and candidate crop rules remain in [`algorithm-kernel.md`](algorithm-kernel.md) §5.0.19.

## Trigger

- Run only when the full document is strictly taller than three CSS viewports.
- Every prior decision candidate must be below `classifier.ThresholdLow`, or the decision pool must be empty. A mid/high confidence candidate prevents this route.
- Short pages and pages without `LongPageScreenshotter` continue through the existing full-page VLM fallback.

## Capture and selection

- Read viewport/document dimensions through read-only layout queries.
- Capture 3–5 evenly spaced, viewport-sized document regions through CDP `Page.captureScreenshot` with `captureBeyondViewport`; each capture must leave scroll position, focus, DOM, and input state unchanged.
- Compose captured regions into a labeled A–E montage and request one label or `NONE`. Keep each label mapped to its document Y coordinate.
- `NONE`, invalid labels, timeout, or capture failure fail soft to the existing full-page VLM route.

## Selected-region follow-up

- Scroll only after a valid macro label is returned. Scrolling is the single permitted page-state change in this route; it must not focus an element, mutate DOM, or dispatch input.
- Capture the selected viewport and resume the existing VLM locator. The macro label selects a region only; it never authorizes a click, input, or extraction by itself.
- A scroll or follow-up capture failure continues through the existing fallback/error handling without guessing a target.

## Acceptance

- Unit coverage verifies the strict height/confidence gates, the 3–5 slice bound, label-to-position mapping, and `NONE` fail-soft behavior.
- Headless Chrome coverage verifies offscreen screenshots reflect the requested document region without scrolling, then verifies explicit selected-region scrolling.
- Product acceptance remains separate: on public real-DOM pages, Top-4 must contain the ground-truth target and the actual visual model must select correctly. Synthetic pages and mock clients do not satisfy that criterion.
- The event shape aligns with DL Lab 202609 C domain as “bounded candidate sequence → select one or abstain”; no DL Lab data or measured accuracy transfers to WebTouch.
