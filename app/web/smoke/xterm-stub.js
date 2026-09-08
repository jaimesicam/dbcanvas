// A stand-in for @xterm/* during the SSR render smoke test.
//
// TerminalProvider imports xterm at module scope, and xterm reaches for browser
// globals the moment it is loaded — so importing the provider under node fails
// before any component renders. The smoke test needs the provider only for its
// React context (the manager calls useTerminals()), never for a real terminal,
// so the build aliases the xterm packages to this.
export class Terminal {
  constructor() { this.cols = 80; this.rows = 24 }
  open() {}
  write() {}
  writeln() {}
  onData() { return { dispose() {} } }
  onResize() { return { dispose() {} } }
  loadAddon() {}
  focus() {}
  dispose() {}
}
export class FitAddon {
  activate() {}
  fit() {}
  dispose() {}
}
export default { Terminal, FitAddon }
