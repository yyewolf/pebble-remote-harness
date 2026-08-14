import * as vscode from 'vscode';

/**
 * Lifecycle for the Kilo plugin that feeds prh.
 *
 * Installing it is global: it loads into every kilo server on the machine,
 * for every project, including sessions with nothing to do with the harness.
 * That makes it a security-relevant act, so it is never silent — see
 * docs/plugin.md.
 */

/** The version the extension expects. Pinned, never floated. */
export const REQUIRED_VERSION = '0.1.0';

export const PACKAGE_NAME = '@yyewolf/prh-plugin';

export type PluginState =
  | { kind: 'installed'; version: string }
  | { kind: 'outdated'; version: string }
  | { kind: 'missing' };

/**
 * Detects the plugin by reading Kilo's global plugin manifest at
 * `~/.config/kilo/package.json`, whose `dependencies` is how `kilo plugin -g`
 * records an install.
 *
 * TODO: implement. A missing or unreadable file means 'missing', not an
 * error — the user may simply not have run Kilo yet.
 */
export async function detect(): Promise<PluginState> {
  return { kind: 'missing' };
}

/**
 * Installs the plugin after explicit confirmation.
 *
 * TODO: implement.
 *  - show a modal that says plainly this affects **all** projects, not just
 *    this workspace, before running anything
 *  - run `kilo plugin -g @yyewolf/prh-plugin@<REQUIRED_VERSION>`
 *  - hold a lock while doing it: several windows can notice a stale plugin at
 *    the same moment and race to fix it
 *  - surface failures; never retry silently in a loop
 */
export async function install(): Promise<void> {
  await vscode.window.showInformationMessage('Plugin install is not implemented yet.');
}

/**
 * Removes the plugin.
 *
 * TODO: implement. Also global, so it stops the harness in every window —
 * say so before doing it.
 */
export async function remove(): Promise<void> {
  await vscode.window.showInformationMessage('Plugin removal is not implemented yet.');
}

/**
 * Nudges the user when the plugin is missing or stale.
 *
 * Non-modal and dismissible: a harness that nags on every window activation
 * is one people disable. It must never install as a side effect of noticing.
 *
 * TODO: implement, and remember a dismissal for the session.
 */
export async function promptIfNeeded(state: PluginState): Promise<void> {
  if (state.kind === 'installed') {
    return;
  }
  // TODO: showInformationMessage with an "Install" action calling install().
}
