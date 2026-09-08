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

// visible keeps the entries an installation should see: everything untagged, plus
// the tagged ones when `on`.
export const visible = (entries, on) => (entries || []).filter((e) => !e?.experimental || on)

// visibleGroups is the same for a catalog of { title, items } categories. A group
// can be tagged as a whole, and one whose every item is experimental disappears
// with them rather than leaving an empty heading behind.
export const visibleGroups = (groups, on) =>
  visible(groups, on)
    .map((g) => ({ ...g, items: visible(g.items, on) }))
    .filter((g) => g.items.length > 0)
