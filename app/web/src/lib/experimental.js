// experimental.js — how a feature that is not ready yet stays out of the way.
//
// Tag it. A nav entry, a palette group, an item inside one: anything carrying
// `experimental: true` is shown only where the installation has asked for
// experimental features (EXPERIMENTAL in .env, read by app/experimental.go and
// served on the system settings as `experimental`). Off is the default, and it is
// the default that matters — a half-finished feature can be merged and left hidden
// instead of waiting for a release, and an installation that wants none of them
// never sees one.
//
// It is a presentation filter and nothing more. Hiding an item does not remove the
// page behind it, refuse an API call, or touch a stack that already has one of
// these nodes on its canvas — turning the switch off must never break what somebody
// built while it was on.
//
// Kept as plain functions over plain objects so the catalogs stay declarative and
// the smoke suite can check them without rendering anything.

// showExperimental reads the flag off the system settings the SettingsProvider
// fetched. Undefined — the window before that fetch lands — is false, so an
// experimental entry never flashes into a menu it does not belong in.
export const showExperimental = (system) => system?.experimental === true

// showEOL is the same for the second tag, `eol: true` — releases past their end of
// life (CentOS 7, PMM 2), offered only where EOL is on in .env (app/eol.go). It is a
// separate switch rather than more experimental entries because the two say opposite
// things: an experimental feature is not finished yet, an EOL one is finished for good.
export const showEOL = (system) => system?.eol === true

// visible keeps the entries an installation should see: everything untagged, plus
// the experimental ones when `on` and the end-of-life ones when `eol`.
export const visible = (entries, on, eol = false) =>
  (entries || []).filter((e) => (!e?.experimental || on) && (!e?.eol || eol))

// visibleGroups is the same for a catalog of { title, items } categories. A group
// can be tagged as a whole, and one whose every item is hidden disappears with them
// rather than leaving an empty heading behind.
export const visibleGroups = (groups, on, eol = false) =>
  visible(groups, on, eol)
    .map((g) => ({ ...g, items: visible(g.items, on, eol) }))
    .filter((g) => g.items.length > 0)
