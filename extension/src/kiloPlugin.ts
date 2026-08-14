import * as vscode from 'vscode';
import * as fs from 'fs';
import * as path from 'path';
import * as os from 'os';
import { execFile } from 'child_process';
import { promisify } from 'util';

const execFileAsync = promisify(execFile);

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

// Persisted dismissal so we don't nag on every activation.

/**
 * Detects the plugin by reading Kilo's global plugin manifest at
 * `~/.config/kilo/package.json`, whose `dependencies` is how `kilo plugin -g`
 * records an install.
 *
 * A missing or unreadable file means 'missing', not an error — the user may
 * simply not have run Kilo yet.
 */
export async function detect(): Promise<PluginState> {
  const manifestPath = getKiloManifestPath();
  let manifest: any;
  try {
    const buf = fs.readFileSync(manifestPath, 'utf8');
    manifest = JSON.parse(buf);
  } catch {
    return { kind: 'missing' };
  }

  const deps = manifest.dependencies ?? manifest.devDependencies ?? {};
  const entry = deps[PACKAGE_NAME];
  if (!entry) {
    return { kind: 'missing' };
  }

  // Extract version from semver range like "^0.1.0" or "0.1.0".
  const version = entry.replace(/^[^0-9]+/, '');
  if (version === REQUIRED_VERSION) {
    return { kind: 'installed', version };
  }
  return { kind: 'outdated', version };
}

/**
 * Installs the plugin after explicit confirmation.
 *
 *  - show a modal that says plainly this affects **all** projects, not just
 *    this workspace, before running anything
 *  - run `kilo plugin -g @yyewolf/prh-plugin@<REQUIRED_VERSION>`
 *  - surface failures; never retry silently in a loop
 */
export async function install(): Promise<void> {
  const choice = await vscode.window.showWarningMessage(
    `Install ${PACKAGE_NAME}@${REQUIRED_VERSION} globally? ` +
    'This loads the plugin into every Kilo session on this machine, for every project.',
    { modal: true },
    'Install',
  );
  if (choice !== 'Install') return;

  try {
    const kiloBin = await resolveKiloBinary();
    await execFileAsync(kiloBin, ['plugin', '-g', `${PACKAGE_NAME}@${REQUIRED_VERSION}`]);
    vscode.window.showInformationMessage(`${PACKAGE_NAME} installed.`);
  } catch (e: any) {
    vscode.window.showErrorMessage(`Plugin install failed: ${e.message ?? e}`);
  }
}

/**
 * Removes the plugin.
 *
 * Also global, so it stops the harness in every window — say so before
 * doing it.
 */
export async function remove(): Promise<void> {
  const choice = await vscode.window.showWarningMessage(
    `Remove ${PACKAGE_NAME} globally? This stops the harness in every VSCode window.`,
    { modal: true },
    'Remove',
  );
  if (choice !== 'Remove') return;

  try {
    const kiloBin = await resolveKiloBinary();
    await execFileAsync(kiloBin, ['plugin', '-g', '--remove', PACKAGE_NAME]);
    vscode.window.showInformationMessage(`${PACKAGE_NAME} removed.`);
  } catch (e: any) {
    vscode.window.showErrorMessage(`Plugin removal failed: ${e.message ?? e}`);
  }
}

/**
 * Nudges the user when the plugin is missing or stale.
 *
 * Non-modal and dismissible: a harness that nags on every window activation
 * is one people disable. It must never install as a side effect of noticing.
 */
export async function promptIfNeeded(state: PluginState): Promise<void> {
  if (state.kind === 'installed') {
    return;
  }

  const dismissed = vscode.workspace.getConfiguration('prh').get<boolean>('pluginDismissed', false);
  if (dismissed) return;

  const msg = state.kind === 'outdated'
    ? `${PACKAGE_NAME} is at ${state.version}; ${REQUIRED_VERSION} is required.`
    : `${PACKAGE_NAME} is not installed. The harness needs it to receive prompts.`;

  const choice = await vscode.window.showInformationMessage(msg, 'Install', 'Dismiss');
  if (choice === 'Install') {
    await install();
  } else if (choice === 'Dismiss') {
    await vscode.workspace.getConfiguration('prh').update('pluginDismissed', true,
      vscode.ConfigurationTarget.Global);
  }
}

// -- helpers --------------------------------------------------------------

function getKiloManifestPath(): string {
  const xdg = process.env.XDG_CONFIG_HOME;
  if (xdg) return path.join(xdg, 'kilo', 'package.json');
  return path.join(os.homedir(), '.config', 'kilo', 'package.json');
}

async function resolveKiloBinary(): Promise<string> {
  // Check the configured path first.
  const configured = vscode.workspace.getConfiguration('prh').get<string>('kiloBinaryPath', '');
  if (configured && fs.existsSync(configured)) return configured;

  // Look in the VSCode extension directory.
  const home = os.homedir();
  const candidates = [
    path.join(home, '.vscode-server', 'extensions', 'kilocode.kilo-code-*', 'bin', 'kilo'),
    path.join(home, '.vscode', 'extensions', 'kilocode.kilo-code-*', 'bin', 'kilo'),
  ];

  for (const pattern of candidates) {
    const dir = path.dirname(path.dirname(pattern));
    if (fs.existsSync(dir)) {
      const entries = fs.readdirSync(dir).filter((e) => e.startsWith('kilocode.kilo-code-'));
      if (entries.length > 0) {
        const bin = path.join(dir, entries[0], 'bin', 'kilo');
        if (fs.existsSync(bin)) return bin;
      }
    }
  }

  // Fallback to PATH.
  return 'kilo';
}
