// repo.js — where this project's own documentation lives, in one place.
//
// The app links out to its docs from three places (the API page, the What's New dialog, and
// wherever the next one turns up), and the URL was being written out longhand at each of them.
// That is fine until the repository moves or is renamed, at which point the links that were not
// updated go on looking like links and start 404ing — which nothing in a build would catch.

// docsRepo is the repository the running build's documentation belongs to.
export const docsRepo = 'https://github.com/jaimesicam/dbcanvas'

// docURL is a path in the repository, as a link somebody can open: docURL('docs/CLI.md').
export const docURL = (path) => `${docsRepo}/blob/main/${String(path).replace(/^\/+/, '')}`
